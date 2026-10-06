package store

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// TestPoolConfigTimezoneUTC guards audit #7 without a live DB: poolConfig must
// pin the session timezone RuntimeParam to UTC so 2-arg date_trunc bucketing
// aligns with the .UTC() labels the Go handlers apply. The DB-gated
// TestPoolSessionTimezoneUTC verifies the same end-to-end when a DB is present.
func TestPoolConfigTimezoneUTC(t *testing.T) {
	cfg, err := poolConfig("postgres://u:p@localhost:5432/db", 2)
	if err != nil {
		t.Fatalf("poolConfig: %v", err)
	}
	if got := cfg.ConnConfig.RuntimeParams["timezone"]; got != "UTC" {
		t.Fatalf("timezone RuntimeParam = %q, want UTC", got)
	}
}

func TestPoolConfigRejectsBadDSN(t *testing.T) {
	_, err := poolConfig("postgres://u:p@localhost:notaport/db", 2)
	if err == nil || !strings.HasPrefix(err.Error(), "parse db dsn: ") {
		t.Fatalf("poolConfig err = %v, want a 'parse db dsn' error", err)
	}
	// NewPool surfaces it unchanged, and never tries to connect.
	pool, err := NewPool(context.Background(), "postgres://u:p@localhost:notaport/db", 2)
	if pool != nil || err == nil || !strings.HasPrefix(err.Error(), "parse db dsn: ") {
		t.Fatalf("NewPool = (%v, %v), want (nil, parse db dsn error)", pool, err)
	}
}

func TestPoolConfigMaxConns(t *testing.T) {
	const dsn = "postgres://u:p@localhost:5432/db"
	def, err := poolConfig(dsn, 0)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := poolConfig(dsn, 7)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxConns != 7 {
		t.Errorf("MaxConns = %d, want 7", cfg.MaxConns)
	}
	if neg, _ := poolConfig(dsn, -3); neg.MaxConns != def.MaxConns {
		t.Errorf("non-positive maxConns changed the driver default: %d vs %d", neg.MaxConns, def.MaxConns)
	}
	if def.ConnConfig.Tracer == nil {
		t.Error("otel tracer not installed")
	}
}

// uuid.Nil maps to SQL NULL, not the all-zeros UUID, so a Nil that slips into a
// parameter fails against NOT NULL / FK instead of matching a sentinel.
func TestUUIDRoundTrip(t *testing.T) {
	if p := UUID(uuid.Nil); p.Valid {
		t.Errorf("UUID(Nil) = %+v, want an invalid (NULL) value", p)
	}
	id := uuid.New()
	p := UUID(id)
	if !p.Valid || p.Bytes != [16]byte(id) {
		t.Errorf("UUID(%s) = %+v", id, p)
	}
	if got := GoUUID(p); got != id {
		t.Errorf("GoUUID round trip = %s, want %s", got, id)
	}
	if got := GoUUID(pgtype.UUID{}); got != uuid.Nil {
		t.Errorf("GoUUID(NULL) = %s, want uuid.Nil", got)
	}
	if got := GoUUID(pgtype.UUID{Bytes: [16]byte(id), Valid: false}); got != uuid.Nil {
		t.Errorf("GoUUID(invalid with bytes) = %s, want uuid.Nil", got)
	}
}

// maxConns comes from user config (max_conns) and is truncated to int32, so
// 1<<32 becomes 0, which pgxpool rejects. NewPool must fail loudly, before any
// connection attempt, rather than start with a bogus pool.
func TestNewPoolRejectsOverflowingMaxConns(t *testing.T) {
	pool, err := NewPool(context.Background(), "postgres://u:p@127.0.0.1:1/db", 1<<32)
	if pool != nil || err == nil || !strings.HasPrefix(err.Error(), "connect db: ") {
		t.Fatalf("NewPool(1<<32) = (%v, %v), want (nil, 'connect db: ' error)", pool, err)
	}
}
