package pipeline

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// TestClampAfterCapsBucketCount covers audit #2: a far-past `after` (the DoS
// input) must be floored so generate_series spans at most maxHistogramBuckets
// of the chosen unit, for every bucket size.
func TestClampAfterCapsBucketCount(t *testing.T) {
	for _, bucket := range []string{"minute", "hour", "day", "week"} {
		far := pgtype.Timestamptz{Time: time.Now().Add(-100 * 365 * 24 * time.Hour), Valid: true}
		got := clampAfter(bucket, far)
		buckets := time.Since(got.Time) / bucketInterval(bucket)
		// +1 tolerance for the partial trailing bucket / elapsed test time.
		if buckets > maxHistogramBuckets+1 {
			t.Fatalf("bucket=%s: window spans %d buckets, want <= %d", bucket, buckets, maxHistogramBuckets)
		}
	}
}

// TestClampAfterLeavesSmallWindowUnchanged: an in-cap window and an absent
// bound must pass through untouched (clamp is invisible to the real UI).
func TestClampAfterLeavesSmallWindowUnchanged(t *testing.T) {
	recent := pgtype.Timestamptz{Time: time.Now().Add(-2 * time.Hour), Valid: true}
	if got := clampAfter("hour", recent); !got.Time.Equal(recent.Time) {
		t.Fatalf("in-cap after was moved: got %v want %v", got.Time, recent.Time)
	}
	if got := clampAfter("hour", pgtype.Timestamptz{}); got.Valid {
		t.Fatal("invalid after should stay invalid")
	}
}
