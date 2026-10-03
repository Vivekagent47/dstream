package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/Vivekagent47/dstream/internal/store"
	"github.com/Vivekagent47/dstream/internal/usage"
)

// --- DB-gated harness (mirrors internal/api/audit_test.go) ---

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DSTREAM_TEST_DB_URL")
	if dsn == "" {
		t.Skip("DSTREAM_TEST_DB_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := store.NewPool(ctx, dsn, 2)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

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
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// quiet keeps the sweep's error/warn lines out of the test output; every test
// here asserts on rows and counters, not on logs.
func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// --- seeding ---

// seedOrg creates an organization with the migration-default quotas (all 0 =
// unlimited) and the default 'month' quota period.
func seedOrg(t *testing.T, pool *pgxpool.Pool, period string) uuid.UUID {
	t.Helper()
	var id pgtype.UUID
	err := pool.QueryRow(context.Background(),
		`INSERT INTO organizations (name, slug, quota_period) VALUES ($1, $2, $3) RETURNING id`,
		"usage-test", "usage-"+uuid.NewString(), period).Scan(&id)
	if err != nil {
		t.Fatalf("seed org: %v", err)
	}
	// Delete the org on the way out; every other row these tests create hangs
	// off it by an ON DELETE CASCADE foreign key, so this one statement takes
	// the destinations, sources, connections, requests, events, attempts,
	// applications, messages and usage_rollups with it. Without this the sweep
	// is not org-scoped, so every org left behind is re-counted by every future
	// sweep in every future run — which is already why the Redis-down test
	// needed PoolSize:1 to stay fast.
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(),
			`DELETE FROM organizations WHERE id = $1`, id); err != nil {
			t.Errorf("cleanup org: %v", err)
		}
	})
	return store.GoUUID(id)
}

// seedTraffic inserts the destination/source/connection chain, one request and
// n events for orgID, every timestamp stamped at `at` so a test can place rows
// on either side of a period boundary. Each call makes its own chain, so two
// calls for one org produce two `requests` rows.
func seedTraffic(t *testing.T, pool *pgxpool.Pool, orgID uuid.UUID, n int, at time.Time, isTest bool) {
	t.Helper()
	tag := uuid.NewString()
	_, err := pool.Exec(context.Background(), `
WITH d AS (
  INSERT INTO destinations (org_id, name, type, url)
  VALUES ($1, 'dest-'||$2::text, 'http', 'https://example.test/sink') RETURNING id
), s AS (
  INSERT INTO sources (org_id, name, type, ingest_token)
  VALUES ($1, 'src-'||$2::text, 'http', 'tok-'||$2::text) RETURNING id
), c AS (
  INSERT INTO connections (source_id, destination_id)
  SELECT s.id, d.id FROM s, d RETURNING id
), r AS (
  INSERT INTO requests (source_id, http_method, http_path, body_hash, body_ref, body_size, received_at)
  SELECT s.id, 'POST', '/e/t', 'hash', 'pg:none', 0, $3 FROM s RETURNING id
)
INSERT INTO events (request_id, connection_id, org_id, status, is_test, created_at)
SELECT r.id, c.id, $1, 'queued', $4, $3 FROM r, c, generate_series(1, $5)`,
		store.UUID(orgID), tag, at, isTest, n)
	if err != nil {
		t.Fatalf("seed traffic: %v", err)
	}
}

// seedAttempts records one delivery attempt against every event the org has.
func seedAttempts(t *testing.T, pool *pgxpool.Pool, orgID uuid.UUID, at time.Time) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO attempts (event_id, attempt_num, attempted_at)
		 SELECT id, 1, $2 FROM events WHERE org_id = $1`,
		store.UUID(orgID), at)
	if err != nil {
		t.Fatalf("seed attempts: %v", err)
	}
}

// seedMessages inserts n outbound messages (with the application they hang off).
func seedMessages(t *testing.T, pool *pgxpool.Pool, orgID uuid.UUID, n int, at time.Time) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
WITH a AS (
  INSERT INTO applications (org_id, name) VALUES ($1, 'app-'||$2::text) RETURNING id
)
INSERT INTO messages (app_id, org_id, event_type, payload_hash, created_at)
SELECT a.id, $1, 'test.event', 'hash', $3 FROM a, generate_series(1, $4)`,
		store.UUID(orgID), uuid.NewString(), at, n)
	if err != nil {
		t.Fatalf("seed messages: %v", err)
	}
}

// usageFor reads one metric out of usage_rollups through the real store query.
// Returns -1 when the row is absent, so "no row at all" is distinguishable
// from a rolled-up zero.
func usageFor(t *testing.T, q *store.Queries, orgID uuid.UUID, metric string, ps time.Time) int64 {
	t.Helper()
	rows, err := q.GetUsageForPeriod(context.Background(), store.GetUsageForPeriodParams{
		OrgID:       store.UUID(orgID),
		PeriodStart: pgtype.Timestamptz{Time: ps, Valid: true},
	})
	if err != nil {
		t.Fatalf("get usage: %v", err)
	}
	for _, r := range rows {
		if r.Metric == metric {
			return r.Count
		}
	}
	return -1
}

// --- tests ---

// The sweep runs once at startup and then on the interval, so a worker restart
// re-runs the current period. If the upsert accumulated instead of converging,
// usage would inflate on every restart — invisibly, until someone was wrongly
// throttled or wrongly billed. Spec §9.1.
func TestRollupIsIdempotent(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	ctx := context.Background()
	cur := usage.PeriodStart(time.Now(), "month")

	orgID := seedOrg(t, pool, "month")
	seedTraffic(t, pool, orgID, 5, time.Now(), false)

	rollupUsage(ctx, q, nil, quiet())
	first := usageFor(t, q, orgID, "events", cur)
	rollupUsage(ctx, q, nil, quiet())
	second := usageFor(t, q, orgID, "events", cur)

	if first != 5 || second != 5 {
		t.Fatalf("counts: first=%d second=%d, want 5 and 5 (upsert must converge)", first, second)
	}
}

// Review ruling #1: the sweep recomputes the CURRENT period only. The Rollup*
// queries have no upper bound and UpsertUsageRollup REPLACES count, so any
// closed period the sweep still re-read would be rewritten from live rows —
// and events.connection_id / requests.source_id are both ON DELETE CASCADE, so
// deleting one connection would silently revise a closed period's billing
// record downward. The previous period must therefore be untouched, not merely
// counted correctly.
func TestRollupWritesOnlyTheCurrentPeriod(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	ctx := context.Background()

	cur := usage.PeriodStart(time.Now(), "month")
	prev := usage.PeriodStart(cur.AddDate(0, 0, -1), "month")

	orgID := seedOrg(t, pool, "month")
	seedTraffic(t, pool, orgID, 3, time.Now(), false)
	// One event a day before this period started — i.e. in the closed period.
	seedTraffic(t, pool, orgID, 1, cur.Add(-24*time.Hour), false)

	rollupUsage(ctx, q, nil, quiet())

	if got := usageFor(t, q, orgID, "events", cur); got != 3 {
		t.Errorf("current period events = %d, want 3 (the backdated row must not count)", got)
	}
	if got := usageFor(t, q, orgID, "events", prev); got != -1 {
		t.Errorf("closed period %s got a row (count %d); the sweep must never write a period older than %s",
			prev.Format(time.RFC3339), got, cur.Format(time.RFC3339))
	}
	// Same for the un-enforced metrics: nothing may land in a closed bucket.
	if got := usageFor(t, q, orgID, "requests", prev); got != -1 {
		t.Errorf("closed period requests row written (count %d)", got)
	}
}

// Review ruling #2: fixture replay is the dev-loop feature dstream sells, and
// is_test is set only by dstream's own replay path, never by an inbound
// webhook — so it is not a quota-evasion vector. Throttling or billing someone
// for exercising the product's own differentiator is the wrong default.
func TestRollupExcludesTestEvents(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	ctx := context.Background()
	cur := usage.PeriodStart(time.Now(), "month")

	orgID := seedOrg(t, pool, "month")
	seedTraffic(t, pool, orgID, 2, time.Now(), false)
	seedTraffic(t, pool, orgID, 3, time.Now(), true) // replayed fixtures

	rollupUsage(ctx, q, nil, quiet())

	if got := usageFor(t, q, orgID, "events", cur); got != 2 {
		t.Errorf("events = %d, want 2 (the 3 is_test events must not be metered)", got)
	}
}

// Every metric must roll up, and one metric's failure must not abort the rest:
// a wrong metric string would violate the usage_rollups CHECK and silently
// leave that metric with no row at all.
func TestRollupCoversAllFourMetrics(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	ctx := context.Background()
	cur := usage.PeriodStart(time.Now(), "month")
	now := time.Now()

	orgID := seedOrg(t, pool, "month")
	seedTraffic(t, pool, orgID, 4, now, false) // 1 request, 4 events
	seedAttempts(t, pool, orgID, now)          // 1 attempt per event
	seedMessages(t, pool, orgID, 2, now)

	rollupUsage(ctx, q, nil, quiet())

	for metric, want := range map[string]int64{
		"requests": 1,
		"events":   4,
		"attempts": 4,
		"messages": 2,
	} {
		if got := usageFor(t, q, orgID, metric, cur); got != want {
			t.Errorf("%s = %d, want %d", metric, got, want)
		}
	}
}

// Spec §9.5: Redis is a cache of the quota decision, never the billing record,
// so a deliberately wrong counter is corrected to the Postgres value by the
// next sweep.
func TestRollupReconcilesRedisCounter(t *testing.T) {
	pool := testPool(t)
	rdb := testRedis(t)
	q := store.New(pool)
	ctx := context.Background()
	cur := usage.PeriodStart(time.Now(), "month")

	orgID := seedOrg(t, pool, "month")
	seedTraffic(t, pool, orgID, 6, time.Now(), false)

	key := usage.CounterKey(orgID, "events", cur)
	t.Cleanup(func() { rdb.Del(context.Background(), key) })
	if err := usage.SetCounter(ctx, rdb, orgID, "events", cur, 999); err != nil {
		t.Fatalf("prime counter: %v", err)
	}

	rollupUsage(ctx, q, rdb, quiet())

	got, err := rdb.Get(ctx, key).Int64()
	if err != nil {
		t.Fatalf("read counter: %v", err)
	}
	if got != 6 {
		t.Errorf("counter = %d, want 6 (Postgres is authoritative)", got)
	}
}

// Review ruling #3: SetCounter returns an error so reconciliation failure is
// observable, but it must never abort the sweep — Postgres is the billing
// record and must still be written when Redis is unreachable.
func TestRollupSurvivesRedisDown(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	ctx := context.Background()
	cur := usage.PeriodStart(time.Now(), "month")

	orgID := seedOrg(t, pool, "month")
	seedTraffic(t, pool, orgID, 2, time.Now(), false)
	seedMessages(t, pool, orgID, 1, time.Now())

	// PoolSize 1 so go-redis stops dialing after the first refusal and every
	// later call returns the cached error immediately — otherwise the sweep
	// pays a dial round per (org, metric) across every org in the shared test
	// database. (The same bound applies in production, at the default pool
	// size: a Redis outage costs PoolSize dials per sweep, not one per org.)
	down := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, PoolSize: 1})
	t.Cleanup(func() { _ = down.Close() })

	rollupUsage(ctx, q, down, quiet())

	if got := usageFor(t, q, orgID, "events", cur); got != 2 {
		t.Errorf("events = %d, want 2 (a Redis outage must not stop the Postgres rollup)", got)
	}
	// And the metric after the failing one still ran.
	if got := usageFor(t, q, orgID, "messages", cur); got != 1 {
		t.Errorf("messages = %d, want 1 (one metric's reconcile failure must not abort the others)", got)
	}
}

// An org on a 'day' quota period must be bucketed by day: enforcement reads the
// counter keyed by PeriodStart(now, org.quota_period), so a sweep that always
// bucketed by month would write a key nobody reads, and the hot-path counter
// would drift upward forever — the exact silent failure ruling #3 exists to
// prevent.
func TestRollupRespectsPerOrgQuotaPeriod(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	ctx := context.Background()

	day := usage.PeriodStart(time.Now(), "day")
	month := usage.PeriodStart(time.Now(), "month")

	orgID := seedOrg(t, pool, "day")
	seedTraffic(t, pool, orgID, 2, time.Now(), false)

	rollupUsage(ctx, q, nil, quiet())

	if got := usageFor(t, q, orgID, "events", day); got != 2 {
		t.Errorf("day bucket events = %d, want 2", got)
	}
	// Only meaningful when the day bucket is not itself the month bucket (i.e.
	// not on the 1st); on the 1st the two period starts coincide.
	if !day.Equal(month) {
		if got := usageFor(t, q, orgID, "events", month); got != -1 {
			t.Errorf("month bucket row written for a 'day' org (count %d)", got)
		}
	}
}
