package usage

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

func testRedis(t *testing.T) *redis.Client {
	t.Helper()
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
	return rdb
}

func TestCheck(t *testing.T) {
	cases := []struct {
		name              string
		count, soft, hard int64
		want              Decision
	}{
		// The default for every org that never configured a quota. A `>=`
		// against 0 would reject every request on an unconfigured deployment.
		{"unlimited default", 1_000_000, 0, 0, Allow},
		{"soft unset, hard set, under", 50, 0, 100, Allow},
		{"soft unset, hard set, over", 150, 0, 100, OverHard},
		{"under soft", 50, 100, 500, Allow},
		{"exactly soft", 100, 100, 500, OverSoft},
		{"between", 200, 100, 500, OverSoft},
		{"exactly hard", 500, 100, 500, OverHard},
		{"over hard", 900, 100, 500, OverHard},
		{"soft set, hard unset, way over", 10_000, 100, 0, OverSoft}, // never OverHard
		// Misconfigured (hard below soft): the ceiling still wins, per §6 —
		// a request at or above hard is rejected whatever soft says.
		{"hard below soft", 300, 500, 100, OverHard},
		// Zero count on a configured org, and a negative limit (nothing writes
		// one, but it must not become a ceiling of -1).
		{"zero count", 0, 100, 500, Allow},
		{"negative limits are unlimited", 10_000, -1, -1, Allow},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Check(c.count, c.soft, c.hard); got != c.want {
				t.Errorf("Check(%d,%d,%d) = %v, want %v", c.count, c.soft, c.hard, got, c.want)
			}
		})
	}
}

// Review Focus #2 and the fail-open rule: a Redis outage must not reject.
func TestIncrFailsOpen(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1}) // refused
	n, err := Incr(context.Background(), rdb, uuid.New(), "events", time.Now(), 1)
	if err == nil {
		t.Fatal("expected an error from an unreachable Redis")
	}
	if n != 0 {
		t.Errorf("count on error: got %d, want 0 so the caller allows", n)
	}
	// The fail-open contract is only honored if Check(0, ...) allows.
	if got := Check(n, 100, 500); got != Allow {
		t.Errorf("Check on a failed read = %v, want Allow", got)
	}
}

func TestPeriodStart(t *testing.T) {
	// 2026-03-17 13:45:09 UTC, and the same instant expressed in +05:30 — the
	// zone must not move the bucket, because the rollup sweep truncates with
	// SQL date_trunc on a pool pinned to timezone=UTC.
	utc := time.Date(2026, 3, 17, 13, 45, 9, 123, time.UTC)
	ist := utc.In(time.FixedZone("IST", 5*3600+1800))

	cases := []struct {
		name   string
		now    time.Time
		period string
		want   time.Time
	}{
		{"month", utc, "month", time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)},
		{"day", utc, "day", time.Date(2026, 3, 17, 0, 0, 0, 0, time.UTC)},
		{"month from a +0530 clock", ist, "month", time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)},
		{"day from a +0530 clock", ist, "day", time.Date(2026, 3, 17, 0, 0, 0, 0, time.UTC)},
		// Local 2026-04-01 02:00+05:30 is still 2026-03-31 20:30 UTC: the
		// instant belongs to March, the month date_trunc would give it.
		{"local month edge stays in the UTC month",
			time.Date(2026, 4, 1, 2, 0, 0, 0, time.FixedZone("IST", 5*3600+1800)),
			"month", time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)},
		// quota_period is CHECK-constrained to day|month; anything else falls
		// back to the column default rather than producing a bogus bucket.
		{"unknown period falls back to month", utc, "", time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := PeriodStart(c.now, c.period)
			if !got.Equal(c.want) {
				t.Errorf("PeriodStart(%s, %q) = %s, want %s", c.now, c.period, got, c.want)
			}
			if got.Location() != time.UTC {
				t.Errorf("PeriodStart location = %v, want UTC", got.Location())
			}
		})
	}
}

// The property the counter depends on: every instant inside one period maps to
// one key, so the hot path reads exactly the key the sweep reconciles.
func TestCounterKeyStableWithinPeriod(t *testing.T) {
	org := uuid.New()
	early := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	late := time.Date(2026, 3, 31, 23, 59, 59, 0, time.UTC)
	next := time.Date(2026, 4, 1, 0, 0, 1, 0, time.UTC)

	k1 := CounterKey(org, "events", PeriodStart(early, "month"))
	k2 := CounterKey(org, "events", PeriodStart(late, "month"))
	if k1 != k2 {
		t.Errorf("same month gave different keys:\n %s\n %s", k1, k2)
	}
	if k3 := CounterKey(org, "events", PeriodStart(next, "month")); k3 == k1 {
		t.Errorf("next month reused the key %s", k3)
	}
	// Shape is load-bearing: Task 2 reconciles against exactly this key.
	want := "usage:" + org.String() + ":events:1772323200" // 2026-03-01T00:00:00Z
	if k1 != want {
		t.Errorf("key = %q, want %q", k1, want)
	}
	// Different metrics for the same org and period must not collide.
	if CounterKey(org, "messages", PeriodStart(early, "month")) == k1 {
		t.Error("events and messages share a key")
	}
}

func TestIncrAndSetCounter(t *testing.T) {
	rdb := testRedis(t)
	ctx := context.Background()
	org := uuid.New()
	ps := PeriodStart(time.Now(), "month")
	key := CounterKey(org, "events", ps)
	t.Cleanup(func() { rdb.Del(context.Background(), key) })

	n, err := Incr(ctx, rdb, org, "events", ps, 1)
	if err != nil || n != 1 {
		t.Fatalf("first Incr = (%d, %v), want (1, nil)", n, err)
	}
	if n, err = Incr(ctx, rdb, org, "events", ps, 2); err != nil || n != 3 {
		t.Fatalf("second Incr = (%d, %v), want (3, nil)", n, err)
	}

	ttl, err := rdb.TTL(ctx, key).Result()
	if err != nil {
		t.Fatal(err)
	}
	if ttl <= 0 {
		t.Errorf("TTL = %v, want a positive expiry so abandoned counters die", ttl)
	}

	// Reconciliation: Postgres is the source of truth, so the sweep's value
	// replaces the drifted counter and later increments build on it.
	if err := SetCounter(ctx, rdb, org, "events", ps, 100); err != nil {
		t.Fatalf("SetCounter: %v", err)
	}
	if n, err = Incr(ctx, rdb, org, "events", ps, 1); err != nil || n != 101 {
		t.Fatalf("Incr after SetCounter = (%d, %v), want (101, nil)", n, err)
	}
	if ttl, err = rdb.TTL(ctx, key).Result(); err != nil || ttl <= 0 {
		t.Errorf("TTL after SetCounter = (%v, %v), want positive", ttl, err)
	}
}

// A failed reconciliation must be observable: if SetCounter swallowed this,
// the counter would drift upward forever (Incr counts even rejected
// requests) and the 429s would have no log line behind them.
func TestSetCounterReportsErrors(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	if err := SetCounter(context.Background(), rdb, uuid.New(), "events", time.Now(), 7); err == nil {
		t.Fatal("expected an error from an unreachable Redis, got nil")
	}
}

// An unknown tier must not render as "allow" — a log that lies about a
// rejection is worse than an ugly one.
func TestDecisionStringNeverLies(t *testing.T) {
	for d, want := range map[Decision]string{Allow: "allow", OverSoft: "over_soft", OverHard: "over_hard", Decision(9): "decision(9)"} {
		if got := d.String(); got != want {
			t.Errorf("Decision(%d).String() = %q, want %q", int(d), got, want)
		}
	}
}
