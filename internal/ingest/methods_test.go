package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/redis/go-redis/v9"

	"github.com/Vivekagent47/dstream/internal/store"
)

func TestMethodAllowed(t *testing.T) {
	allowed := []string{"POST", "PUT"}
	if !methodAllowed(allowed, "POST") {
		t.Fatal("POST should be allowed")
	}
	if !methodAllowed(allowed, "put") {
		t.Fatal("put (any case) should be allowed")
	}
	if methodAllowed(allowed, "DELETE") {
		t.Fatal("DELETE not in set — should be rejected")
	}
}

// TestParseRemoteAddrIgnoresXFF covers audit #10: parseRemoteAddr must trust
// only r.RemoteAddr (normalized by the TrustedRealIP middleware) and never the
// raw X-Forwarded-For header — otherwise any client could forge the stored
// ingest_ip regardless of DSTREAM_TRUSTED_PROXIES (GHSA-3fxj-6jh8-hvhx).
func TestParseRemoteAddrIgnoresXFF(t *testing.T) {
	// Spoofed XFF must be ignored; the RemoteAddr-derived IP wins.
	r := httptest.NewRequest(http.MethodPost, "/e/tok", nil)
	r.RemoteAddr = "203.0.113.5:1234"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := parseRemoteAddr(r); got == nil || got.String() != "203.0.113.5" {
		t.Fatalf("spoofed XFF: parseRemoteAddr = %v, want 203.0.113.5 (RemoteAddr, not forged XFF)", got)
	}

	// No XFF: still the RemoteAddr IP.
	r2 := httptest.NewRequest(http.MethodPost, "/e/tok", nil)
	r2.RemoteAddr = "203.0.113.5:1234"
	if got := parseRemoteAddr(r2); got == nil || got.String() != "203.0.113.5" {
		t.Fatalf("no XFF: parseRemoteAddr = %v, want 203.0.113.5", got)
	}
}

// TestCaptureHeadersRedactsCredentials covers audit N5: captureHeaders must
// redact credential-bearing headers (Authorization/Cookie/Proxy-Authorization)
// before they're persisted to requests.headers (viewable by org members), while
// keeping the key present and non-credential headers intact.
func TestCaptureHeadersRedactsCredentials(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer secret-token")
	h.Set("Cookie", "session=abc123")
	h.Set("Proxy-Authorization", "Basic Zm9vOmJhcg==")
	h.Set("Content-Type", "application/json")

	var out map[string][]string
	if err := json.Unmarshal(captureHeaders(h), &out); err != nil {
		t.Fatalf("unmarshal captureHeaders: %v", err)
	}

	for _, k := range []string{"Authorization", "Cookie", "Proxy-Authorization"} {
		if got := out[k]; len(got) != 1 || got[0] != "[redacted]" {
			t.Fatalf("%s = %v, want [[redacted]] (key kept, value redacted)", k, got)
		}
	}
	if got := out["Content-Type"]; len(got) != 1 || got[0] != "application/json" {
		t.Fatalf("Content-Type = %v, want [application/json] (non-credential header intact)", got)
	}
}

// noRowsDBTX is a store.DBTX whose QueryRow always scans to pgx.ErrNoRows
// (unknown token), counting how many times the DB was actually hit.
type noRowsDBTX struct{ queryRows int }

func (d *noRowsDBTX) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	panic("unused")
}
func (d *noRowsDBTX) Query(context.Context, string, ...any) (pgx.Rows, error) { panic("unused") }
func (d *noRowsDBTX) QueryRow(context.Context, string, ...any) pgx.Row {
	d.queryRows++
	return noRow{}
}

type noRow struct{}

func (noRow) Scan(...any) error { return pgx.ErrNoRows }

// TestResolveSourceNegativeCache covers audit #12: an unknown ingest token must
// be negative-cached so a flood of the same bad token collapses to one DB hit
// per NegativeSourceCacheTTL instead of one round-trip + pool slot per request.
// Once the negative entry expires, the token is re-queried (so a source created
// after a bad-token probe becomes reachable again).
func TestResolveSourceNegativeCache(t *testing.T) {
	db := &noRowsDBTX{}
	h := &Handler{Queries: store.New(db)}
	const tok = "bad-token"

	// A flood of the same unknown token: every call returns ErrSourceNotFound,
	// but only the first touches Postgres — the rest are negative-cache hits.
	for i := 0; i < 5; i++ {
		if _, err := h.resolveSource(context.Background(), tok); !errors.Is(err, ErrSourceNotFound) {
			t.Fatalf("resolveSource #%d: err=%v, want ErrSourceNotFound", i, err)
		}
	}
	if db.queryRows != 1 {
		t.Fatalf("bad-token flood hit DB %d times, want 1 (negative cache)", db.queryRows)
	}

	// Force-expire the negative entry: the next lookup must re-hit the DB, so a
	// source created after the bad-token probe resolves once the TTL lapses.
	v, _ := h.sourceCache.Load(tok)
	entry := v.(sourceCacheEntry)
	entry.expires = time.Now().Add(-time.Second)
	h.sourceCache.Store(tok, entry)

	if _, err := h.resolveSource(context.Background(), tok); !errors.Is(err, ErrSourceNotFound) {
		t.Fatalf("post-expiry resolveSource: err=%v, want ErrSourceNotFound", err)
	}
	if db.queryRows != 2 {
		t.Fatalf("post-expiry DB hits = %d, want 2 (expired negative entry re-queries)", db.queryRows)
	}
}

// TestCheckDedupRollback covers audit #1: when a request claims the dedup key
// but then fails before events are created, handleIngest rolls the key back so
// the sender's retry is NOT deduped (the webhook isn't silently lost). This
// exercises the exact round-trip — claim, re-check (dup), rollback via dedupKey,
// re-check (not dup) — and so also proves dedupKey matches checkDedup's SetNX key.
func TestCheckDedupRollback(t *testing.T) {
	addr := os.Getenv("DSTREAM_REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skip("no redis at " + addr)
	}
	h := &Handler{Redis: rdb}
	src := uuid.New()
	hash := "audit1-" + src.String()
	defer rdb.Del(context.Background(), dedupKey(src, hash))

	// First sight: not a duplicate (claims the key).
	if dup, err := h.checkDedup(ctx, src, hash); err != nil || dup {
		t.Fatalf("first checkDedup: dup=%v err=%v, want dup=false", dup, err)
	}
	// Second sight without rollback: duplicate.
	if dup, err := h.checkDedup(ctx, src, hash); err != nil || !dup {
		t.Fatalf("second checkDedup: dup=%v err=%v, want dup=true", dup, err)
	}
	// Roll back (what handleIngest does on a pre-commit failure), then retry:
	// the key must be gone so the retry is NOT deduped.
	if err := rdb.Del(context.Background(), dedupKey(src, hash)).Err(); err != nil {
		t.Fatalf("rollback del: %v", err)
	}
	if dup, err := h.checkDedup(ctx, src, hash); err != nil || dup {
		t.Fatalf("post-rollback checkDedup: dup=%v err=%v, want dup=false", dup, err)
	}
}
