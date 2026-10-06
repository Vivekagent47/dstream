package store_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/Vivekagent47/dstream/internal/store"
)

// TestPoolSessionTimezoneUTC guards audit #7: NewPool must pin every session's
// timezone to UTC so 2-arg date_trunc bucketing aligns with the .UTC() labels
// the Go handlers apply. DB-gated via the shared DSTREAM_TEST_DB_URL harness.
func TestPoolSessionTimezoneUTC(t *testing.T) {
	pool := isolationPool(t)
	var tz string
	if err := pool.QueryRow(context.Background(), "SHOW timezone").Scan(&tz); err != nil {
		t.Fatalf("show timezone: %v", err)
	}
	if tz != "UTC" {
		t.Fatalf("session timezone = %q, want UTC", tz)
	}
}

// The pin must WIN over a timezone the DSN itself asks for: a deployment (or a
// dev machine) whose connection string says otherwise still gets UTC sessions.
func TestPoolTimezonePinOverridesDSN(t *testing.T) {
	dsn := os.Getenv("DSTREAM_TEST_DB_URL")
	if dsn == "" {
		t.Skip("DSTREAM_TEST_DB_URL not set")
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	pool, err := store.NewPool(context.Background(), dsn+sep+"timezone=Asia/Kolkata", 1)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	var tz string
	if err := pool.QueryRow(context.Background(), "SHOW timezone").Scan(&tz); err != nil {
		t.Fatalf("show timezone: %v", err)
	}
	if tz != "UTC" {
		t.Fatalf("session timezone = %q with timezone=Asia/Kolkata in the DSN, want UTC", tz)
	}
}
