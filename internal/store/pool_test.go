package store_test

import (
	"context"
	"testing"
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
