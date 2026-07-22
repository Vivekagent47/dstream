package store

import "testing"

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
