package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
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
	seedAttempts(t, pool, orgID, time.Now())         // one per event, test and real

	rollupUsage(ctx, q, nil, quiet())

	if got := usageFor(t, q, orgID, "events", cur); got != 2 {
		t.Errorf("events = %d, want 2 (the 3 is_test events must not be metered)", got)
	}

	// The replay also wrote a real `requests` row and real `attempts` rows.
	// Metering those would charge for the dev loop just as surely as metering
	// the events would — the exclusion has to follow the traffic, not stop at
	// the one table that happens to carry the flag.
	//
	// seedTraffic inserts exactly one request per call, so two calls produce
	// two requests and only the non-test one counts.
	if got := usageFor(t, q, orgID, "requests", cur); got != 1 {
		t.Errorf("requests = %d, want 1 (the replay's request row must not be metered)", got)
	}
	if got := usageFor(t, q, orgID, "attempts", cur); got != 2 {
		t.Errorf("attempts = %d, want 2 (attempts against is_test events must not be metered)", got)
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

// --- retention + error branches ---

// logBuf is a log sink the test can read while the sweep goroutine writes.
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func (l *logBuf) logger() *slog.Logger { return slog.New(slog.NewTextHandler(l, nil)) }

// retentionRows are the ids of one org's rows at one age, one per swept table.
type retentionRows struct {
	msg, outboundAttempt, request, inboundAttempt uuid.UUID
	token, invite                                 uuid.UUID
}

// seedRetentionRows inserts a row carrying a body (or an expiry) in every table
// the maintenance sweep touches, all stamped at `at`. Expiry-based tables get
// expires_at = at.
func seedRetentionRows(t *testing.T, pool *pgxpool.Pool, orgID uuid.UUID, at time.Time) retentionRows {
	t.Helper()
	ctx := context.Background()
	var r retentionRows
	tag := uuid.NewString()
	err := pool.QueryRow(ctx, `
WITH app AS (
  INSERT INTO applications (org_id, name) VALUES ($1, 'app-'||$2::text) RETURNING id
), msg AS (
  INSERT INTO messages (app_id, org_id, event_type, payload, payload_hash, created_at)
  SELECT app.id, $1, 'test.event', '\x7b7d'::bytea, 'hash', $3 FROM app RETURNING id
), ep AS (
  INSERT INTO endpoints (app_id, org_id, url, secret)
  SELECT app.id, $1, 'https://example.test/sink', 'whsec_x' FROM app RETURNING id
), del AS (
  INSERT INTO message_deliveries (message_id, endpoint_id, org_id)
  SELECT msg.id, ep.id, $1 FROM msg, ep RETURNING id
), att AS (
  INSERT INTO message_delivery_attempts (delivery_id, attempt_num, response_body, attempted_at)
  SELECT del.id, 1, '\x01'::bytea, $3 FROM del RETURNING id
)
SELECT (SELECT id FROM msg), (SELECT id FROM att)`,
		store.UUID(orgID), tag, at).Scan(&r.msg, &r.outboundAttempt)
	if err != nil {
		t.Fatalf("seed outbound rows: %v", err)
	}
	err = pool.QueryRow(ctx, `
WITH d AS (
  INSERT INTO destinations (org_id, name, type, url)
  VALUES ($1, 'dest-'||$2::text, 'http', 'https://example.test/sink') RETURNING id
), s AS (
  INSERT INTO sources (org_id, name, type, ingest_token)
  VALUES ($1, 'src-'||$2::text, 'http', 'tok-'||$2::text) RETURNING id
), c AS (
  INSERT INTO connections (source_id, destination_id) SELECT s.id, d.id FROM s, d RETURNING id
), r AS (
  INSERT INTO requests (source_id, http_method, http_path, body_hash, body_ref, body_size, received_at)
  SELECT s.id, 'POST', '/e/t', 'hash', 'pg:none', 1, $3 FROM s RETURNING id
), rb AS (
  INSERT INTO request_bodies (request_id, body, stored_at) SELECT r.id, '\x01'::bytea, $3 FROM r
), e AS (
  INSERT INTO events (request_id, connection_id, org_id, status, created_at)
  SELECT r.id, c.id, $1, 'queued', $3 FROM r, c RETURNING id
), a AS (
  INSERT INTO attempts (event_id, attempt_num, response_body, attempted_at)
  SELECT e.id, 1, '\x01'::bytea, $3 FROM e RETURNING id
)
SELECT (SELECT id FROM r), (SELECT id FROM a)`,
		store.UUID(orgID), tag, at).Scan(&r.request, &r.inboundAttempt)
	if err != nil {
		t.Fatalf("seed inbound rows: %v", err)
	}
	email := "ret-" + tag + "@example.test"
	if err := pool.QueryRow(ctx,
		`INSERT INTO magic_link_tokens (email, token_hash, expires_at) VALUES ($1, '\x01', $2) RETURNING id`,
		email, at).Scan(&r.token); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM magic_link_tokens WHERE id = $1`, r.token) })
	var userID uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO users (email) VALUES ($1) RETURNING id`, email).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	// Registered after seedOrg's cleanup, so it runs first: invites reference
	// the user with ON DELETE RESTRICT and must go before it.
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM org_invites WHERE invited_by = $1`, userID)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
	})
	if err := pool.QueryRow(ctx,
		`INSERT INTO org_invites (org_id, email, role, token_hash, invited_by, expires_at)
		 VALUES ($1, $2, 'member', '\x01', $3, $4) RETURNING id`,
		store.UUID(orgID), email, userID, at).Scan(&r.invite); err != nil {
		t.Fatalf("seed invite: %v", err)
	}
	return r
}

// exists reports whether a row is present; hasBody whether its column is non-NULL.
func exists(t *testing.T, pool *pgxpool.Pool, table string, id uuid.UUID) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM "+table+" WHERE id = $1", id).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n == 1
}

func hasBody(t *testing.T, pool *pgxpool.Pool, table, col, idCol string, id uuid.UUID) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM "+table+" WHERE "+idCol+" = $1 AND "+col+" IS NOT NULL", id).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n == 1
}

// bodiesSurvive / bodiesGone assert the four expirable bodies of one age.
func checkBodies(t *testing.T, pool *pgxpool.Pool, label string, r retentionRows, want bool) {
	t.Helper()
	for _, c := range []struct {
		name, table, col, idCol string
		id                      uuid.UUID
	}{
		{"message payload", "messages", "payload", "id", r.msg},
		{"outbound attempt body", "message_delivery_attempts", "response_body", "id", r.outboundAttempt},
		{"request body", "request_bodies", "body", "request_id", r.request},
		{"inbound attempt body", "attempts", "response_body", "id", r.inboundAttempt},
	} {
		if got := hasBody(t, pool, c.table, c.col, c.idCol, c.id); got != want {
			t.Errorf("%s: %s present = %v, want %v", label, c.name, got, want)
		}
	}
}

// runSweep runs runMaintenance until its startup sweep has finished, then
// cancels it and waits for it to return. A sentinel event stamped now is
// seeded for the org; the rollup is the sweep's last step, so once its row
// lands every earlier step (all the retention deletes) has already run.
func runSweep(t *testing.T, pool *pgxpool.Pool, orgID uuid.UUID, log *slog.Logger, retention time.Duration) {
	t.Helper()
	q := store.New(pool)
	seedTraffic(t, pool, orgID, 1, time.Now(), false)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); runMaintenance(ctx, q, nil, log, retention, maintenanceInterval) }()
	cur := usage.PeriodStart(time.Now(), "month")
	waitFor(t, "the startup sweep to roll usage up", func() bool { return usageFor(t, q, orgID, "events", cur) >= 1 })
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runMaintenance did not return after cancel")
	}
}

// Retention must delete only what is past the cutoff: assert both directions,
// the row outside the window gone AND the row inside it untouched.
func TestMaintenanceRetentionHonoursTheCutoff(t *testing.T) {
	pool := testPool(t)
	orgID := seedOrg(t, pool, "month")
	old := seedRetentionRows(t, pool, orgID, time.Now().Add(-48*time.Hour))
	fresh := seedRetentionRows(t, pool, orgID, time.Now().Add(-1*time.Hour))

	logs := &logBuf{}
	runSweep(t, pool, orgID, logs.logger(), 24*time.Hour)

	if exists(t, pool, "magic_link_tokens", old.token) {
		t.Error("magic-link token expired 48h ago survived the purge")
	}
	if !exists(t, pool, "magic_link_tokens", fresh.token) {
		t.Error("magic-link token expired 1h ago was purged inside the 24h debug window")
	}
	if exists(t, pool, "org_invites", old.invite) {
		t.Error("invite expired 48h ago survived the purge")
	}
	if !exists(t, pool, "org_invites", fresh.invite) {
		t.Error("invite expired 1h ago was purged inside the 24h debug window")
	}
	checkBodies(t, pool, "old rows", old, false)
	checkBodies(t, pool, "fresh rows", fresh, true)
	// The rows themselves stay: retention nulls bodies, it does not delete history.
	if !exists(t, pool, "messages", old.msg) {
		t.Error("retention deleted the message row; it must only null the payload")
	}

	out := logs.String()
	for _, want := range []string{
		"purged expired magic-link tokens", "purged expired org invites",
		"expired message payloads", "expired attempt bodies",
		"expired request bodies", "expired inbound attempt bodies",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log is missing %q:\n%s", want, out)
		}
	}
}

// PayloadRetention 0 means keep forever: bodies stay, but the unconditional
// token and invite purge still runs.
func TestMaintenanceZeroRetentionKeepsBodies(t *testing.T) {
	pool := testPool(t)
	orgID := seedOrg(t, pool, "month")
	old := seedRetentionRows(t, pool, orgID, time.Now().Add(-48*time.Hour))

	logs := &logBuf{}
	runSweep(t, pool, orgID, logs.logger(), 0)

	checkBodies(t, pool, "retention=0", old, true)
	if exists(t, pool, "magic_link_tokens", old.token) || exists(t, pool, "org_invites", old.invite) {
		t.Error("expired tokens/invites must be purged regardless of payload retention")
	}
	if out := logs.String(); strings.Contains(out, "expired message payloads") || strings.Contains(out, "expired request bodies") {
		t.Errorf("retention=0 ran the body sweep:\n%s", out)
	}
}

// With nothing past the cutoff the sweep leaves every in-window row alone.
func TestMaintenanceNothingToDelete(t *testing.T) {
	pool := testPool(t)
	orgID := seedOrg(t, pool, "month")
	fresh := seedRetentionRows(t, pool, orgID, time.Now().Add(-1*time.Hour))

	runSweep(t, pool, orgID, quiet(), 24*time.Hour)

	checkBodies(t, pool, "fresh rows", fresh, true)
	if !exists(t, pool, "magic_link_tokens", fresh.token) || !exists(t, pool, "org_invites", fresh.invite) {
		t.Error("a sweep with nothing past the cutoff deleted an in-window token or invite")
	}
}

// Every failing query is logged with its own message and the sweep carries on
// to the rollup instead of aborting at the first error.
func TestMaintenanceLogsEveryFailedQuery(t *testing.T) {
	q := closedQueries(t)

	logs := &logBuf{}
	rctx, rcancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); runMaintenance(rctx, q, nil, logs.logger(), time.Hour, maintenanceInterval) }()
	// "list org quotas" is the sweep's last step.
	waitFor(t, "the sweep to reach the rollup", func() bool { return strings.Contains(logs.String(), "list org quotas") })
	rcancel()
	<-done

	out := logs.String()
	for _, want := range []string{
		"purge magic-link tokens", "purge org invites", "expire payloads",
		"expire attempt bodies", "expire request bodies", "expire inbound attempt bodies",
		"list org quotas",
	} {
		line := ""
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, "maintenance: "+want) {
				line = l
			}
		}
		if line == "" {
			t.Errorf("no error logged for %q:\n%s", want, out)
		} else if !strings.Contains(line, "closed pool") {
			t.Errorf("%q logged without the cause: %s", want, line)
		}
	}
}

// One failing metric query must be reported with its metric name and not stop
// the others from being attempted.
func TestRollupPeriodLogsEachFailedMetric(t *testing.T) {
	q := closedQueries(t)

	logs := &logBuf{}
	rollupPeriod(context.Background(), q, nil, logs.logger(), "month",
		usage.PeriodStart(time.Now(), "month"), map[uuid.UUID]struct{}{})
	out := logs.String()
	for _, metric := range []string{"events", "requests", "messages", "attempts"} {
		if !strings.Contains(out, "rollup query") || !strings.Contains(out, "metric="+metric) {
			t.Errorf("no rollup-query error logged for metric %q:\n%s", metric, out)
		}
	}
	if !strings.Contains(out, "closed pool") {
		t.Errorf("errors logged without the cause:\n%s", out)
	}
}

// Go and SQL disagreeing about where the period starts would make the sweep
// write nothing, silently. The tripwire turns that into a warning, and the
// guard must still refuse to write the mismatched bucket.
func TestRollupWarnsWhenEveryBucketMissesThePeriodStart(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	cur := usage.PeriodStart(time.Now(), "month")
	orgID := seedOrg(t, pool, "month")
	seedTraffic(t, pool, orgID, 3, time.Now(), false)

	logs := &logBuf{}
	// An `after` an hour before the real period start: the rows are still
	// selected (created_at >= after) but their bucket is not `after`.
	rollupPeriod(context.Background(), q, nil, logs.logger(), "month",
		cur.Add(-time.Hour), map[uuid.UUID]struct{}{orgID: {}})

	if out := logs.String(); !strings.Contains(out, "every bucket missed the period start") {
		t.Errorf("tripwire did not fire:\n%s", out)
	}
	if got := usageFor(t, q, orgID, "events", cur); got != -1 {
		t.Errorf("events = %d, want no row: a mismatched bucket must not be written", got)
	}
}

// The hourly tick must run the whole sweep again, not just the startup pass:
// traffic that lands after the startup sweep has to reach usage_rollups.
func TestMaintenanceSweepsAgainOnEachTick(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	orgID := seedOrg(t, pool, "month")
	seedTraffic(t, pool, orgID, 1, time.Now(), false)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); runMaintenance(ctx, q, nil, quiet(), 0, 20*time.Millisecond) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("runMaintenance did not return after cancel")
		}
	})
	cur := usage.PeriodStart(time.Now(), "month")
	waitFor(t, "the startup sweep to roll usage up", func() bool { return usageFor(t, q, orgID, "events", cur) == 1 })

	seedTraffic(t, pool, orgID, 2, time.Now(), false)

	waitFor(t, "a later tick to roll the new events up", func() bool { return usageFor(t, q, orgID, "events", cur) == 3 })
}
