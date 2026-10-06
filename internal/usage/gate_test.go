package usage

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/store"
)

// The gate is the quota decision point: these tests drive it against real
// Postgres (the limits) and real Redis (the counters and alert latches).

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// waitFor polls cond until it holds or the deadline passes, then FAILS.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// failTracer cancels the context of any statement containing marker, so that
// statement fails on a real pool while every other one runs normally.
type failTracer struct{ marker string }

func (f failTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.Contains(strings.ToLower(d.SQL), strings.ToLower(f.marker)) {
		c, cancel := context.WithCancel(ctx)
		cancel()
		return c
	}
	return ctx
}
func (failTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func gatePool(t *testing.T, marker string) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DSTREAM_TEST_DB_URL")
	if dsn == "" {
		t.Skip("DSTREAM_TEST_DB_URL not set")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = 4
	if marker != "" {
		cfg.ConnConfig.Tracer = failTracer{marker: marker}
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

type gateEnv struct {
	pool *pgxpool.Pool
	q    *store.Queries
	rdb  *redis.Client
	org  uuid.UUID
	log  *lockedBuf
	g    *Gate
}

// newGateEnv makes one org with the given limits and a Gate over real Redis and
// the given queries (nil = a clean pool). The org, its counters, latches and the
// per-test queue prefix are removed on cleanup.
func newGateEnv(t *testing.T, plan string, l Limits, qpool *pgxpool.Pool) *gateEnv {
	t.Helper()
	admin := gatePool(t, "")
	rdb := testRedis(t)
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()
	q := store.New(admin)
	o, err := q.CreateOrganization(ctx, store.CreateOrganizationParams{
		Name: "gate " + uuid.NewString(), Slug: "gate-" + uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	org := store.GoUUID(o.ID)
	if _, err := q.UpdateOrgQuota(ctx, store.UpdateOrgQuotaParams{
		ID: o.ID, Plan: plan,
		QuotaEventsSoft: l.EventsSoft, QuotaEventsHard: l.EventsHard,
		QuotaMessagesSoft: l.MessagesSoft, QuotaMessagesHard: l.MessagesHard,
		QuotaPeriod: l.Period,
	}); err != nil {
		t.Fatalf("set quota: %v", err)
	}
	pfx := "gatetest-" + uuid.NewString()
	t.Cleanup(func() {
		c := context.Background()
		for _, pat := range []string{"usage:*" + org.String() + "*", pfx + ":*"} {
			if keys, _ := rdb.Keys(c, pat).Result(); len(keys) > 0 {
				rdb.Del(c, keys...)
			}
		}
		_, _ = admin.Exec(c, `DELETE FROM organizations WHERE id = $1`, o.ID)
	})
	if qpool == nil {
		qpool = admin
	}
	buf := &lockedBuf{}
	g := &Gate{
		Log:     slog.New(slog.NewTextHandler(buf, nil)),
		Queries: store.New(qpool),
		Redis:   rdb,
		Queue:   dqueue.NewClient(rdb).WithPrefix(pfx),
	}
	return &gateEnv{pool: admin, q: q, rdb: rdb, org: org, log: buf, g: g}
}

func (e *gateEnv) reload(t *testing.T) {
	t.Helper()
	if err := e.g.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
}

// ops counts the org's operational messages by event type.
func (e *gateEnv) ops(t *testing.T) map[string]int {
	t.Helper()
	rows, err := e.pool.Query(context.Background(),
		`SELECT event_type, count(*) FROM messages WHERE org_id = $1 GROUP BY event_type`, e.org)
	if err != nil {
		t.Fatalf("count ops: %v", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var et string
		var n int
		if err := rows.Scan(&et, &n); err != nil {
			t.Fatal(err)
		}
		out[et] = n
	}
	return out
}

func (e *gateEnv) counter(t *testing.T, metric, period string) int64 {
	t.Helper()
	key := CounterKey(e.org, metric, PeriodStart(time.Now(), period))
	n, err := e.rdb.Exists(context.Background(), key).Result()
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		return -1
	}
	v, err := e.rdb.Get(context.Background(), key).Int64()
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TestGateLadder walks the whole ladder, for both gated metrics, one request at
// a time: under soft, at soft (accepted but warned), over soft, at the hard
// ceiling (rejected) and over it. soft=2 hard=4, so counts 1..5 are
// Allow, OverSoft, OverSoft, OverHard, OverHard.
func TestGateLadder(t *testing.T) {
	for _, metric := range []string{MetricEvents, MetricMessages} {
		t.Run(metric, func(t *testing.T) {
			e := newGateEnv(t, "custom", Limits{EventsSoft: 2, EventsHard: 4, MessagesSoft: 2, MessagesHard: 4, Period: "month"}, nil)
			e.reload(t)
			check := e.g.CheckIngest
			other := MetricMessages
			warn, exceeded := "usage.quota_warning", "usage.quota_exceeded"
			if metric == MetricMessages {
				check, other = e.g.CheckPublish, MetricEvents
			}
			want := []Decision{Allow, OverSoft, OverSoft, OverHard, OverHard}
			for i, w := range want {
				if got := check(context.Background(), e.org); got != w {
					t.Fatalf("request %d: decision = %v, want %v", i+1, got, w)
				}
			}
			e.g.WaitAlerts()
			if got := e.counter(t, metric, "month"); got != 5 {
				t.Errorf("%s counter = %d, want 5", metric, got)
			}
			if got := e.counter(t, other, "month"); got != -1 {
				t.Errorf("%s counter = %d, want untouched (-1): metrics must not share a counter", other, got)
			}
			// Each tier alerts once per period however many requests land in it:
			// two OverSoft and two OverHard requests, one message each.
			ops := e.ops(t)
			if ops[warn] != 1 || ops[exceeded] != 1 {
				t.Errorf("alerts = %v, want exactly one %s and one %s", ops, warn, exceeded)
			}
		})
	}
}

// 0 means unlimited, independently at each tier.
func TestGateZeroIsUnlimited(t *testing.T) {
	t.Run("both zero: no counter, no alert", func(t *testing.T) {
		e := newGateEnv(t, "enterprise", Limits{Period: "month"}, nil)
		e.reload(t)
		for i := 0; i < 5; i++ {
			if got := e.g.CheckIngest(context.Background(), e.org); got != Allow {
				t.Fatalf("request %d = %v, want allow", i+1, got)
			}
		}
		e.g.WaitAlerts()
		if got := e.counter(t, MetricEvents, "month"); got != -1 {
			t.Errorf("counter = %d; an unlimited org must cost no Redis write", got)
		}
		if ops := e.ops(t); len(ops) != 0 {
			t.Errorf("unlimited org alerted: %v", ops)
		}
	})
	t.Run("soft only: warns, never rejects", func(t *testing.T) {
		e := newGateEnv(t, "custom", Limits{EventsSoft: 1, Period: "month"}, nil)
		e.reload(t)
		for i := 0; i < 4; i++ {
			if got := e.g.CheckIngest(context.Background(), e.org); got != OverSoft {
				t.Fatalf("request %d = %v, want over_soft", i+1, got)
			}
		}
		e.g.WaitAlerts()
		if ops := e.ops(t); ops["usage.quota_warning"] != 1 || ops["usage.quota_exceeded"] != 0 {
			t.Errorf("alerts = %v, want one warning and no exceeded", ops)
		}
	})
	t.Run("hard only: allows until the ceiling", func(t *testing.T) {
		e := newGateEnv(t, "custom", Limits{EventsHard: 3, Period: "month"}, nil)
		e.reload(t)
		for i, w := range []Decision{Allow, Allow, OverHard} {
			if got := e.g.CheckIngest(context.Background(), e.org); got != w {
				t.Fatalf("request %d = %v, want %v", i+1, got, w)
			}
		}
		e.g.WaitAlerts()
		if ops := e.ops(t); ops["usage.quota_warning"] != 0 || ops["usage.quota_exceeded"] != 1 {
			t.Errorf("alerts = %v, want one exceeded and no warning", ops)
		}
	})
	t.Run("messages soft only: warns, never rejects", func(t *testing.T) {
		e := newGateEnv(t, "custom", Limits{MessagesSoft: 1, Period: "month"}, nil)
		e.reload(t)
		for i := 0; i < 4; i++ {
			if got := e.g.CheckPublish(context.Background(), e.org); got != OverSoft {
				t.Fatalf("publish %d = %v, want over_soft", i+1, got)
			}
		}
		e.g.WaitAlerts()
		if ops := e.ops(t); ops["usage.quota_warning"] != 1 || ops["usage.quota_exceeded"] != 0 {
			t.Errorf("alerts = %v, want one warning and no exceeded", ops)
		}
	})
	t.Run("messages hard only: allows until the ceiling", func(t *testing.T) {
		e := newGateEnv(t, "custom", Limits{MessagesHard: 3, Period: "month"}, nil)
		e.reload(t)
		for i, w := range []Decision{Allow, Allow, OverHard} {
			if got := e.g.CheckPublish(context.Background(), e.org); got != w {
				t.Fatalf("publish %d = %v, want %v", i+1, got, w)
			}
		}
		e.g.WaitAlerts()
		if ops := e.ops(t); ops["usage.quota_warning"] != 0 || ops["usage.quota_exceeded"] != 1 {
			t.Errorf("alerts = %v, want one exceeded and no warning", ops)
		}
	})
	t.Run("messages both zero: unlimited, no counter", func(t *testing.T) {
		e := newGateEnv(t, "enterprise", Limits{EventsSoft: 1, EventsHard: 1, Period: "month"}, nil)
		e.reload(t)
		for i := 0; i < 3; i++ {
			if got := e.g.CheckPublish(context.Background(), e.org); got != Allow {
				t.Fatalf("publish %d = %v, want allow", i+1, got)
			}
		}
		if got := e.counter(t, MetricMessages, "month"); got != -1 {
			t.Errorf("messages counter = %d, want none", got)
		}
	})
	t.Run("only the other metric limited: this one is unlimited", func(t *testing.T) {
		e := newGateEnv(t, "custom", Limits{MessagesSoft: 1, MessagesHard: 1, Period: "month"}, nil)
		e.reload(t)
		if got := e.g.CheckIngest(context.Background(), e.org); got != Allow {
			t.Fatalf("ingest = %v, want allow", got)
		}
		if got := e.g.CheckPublish(context.Background(), e.org); got != OverHard {
			t.Fatalf("publish = %v, want over_hard", got)
		}
		e.g.WaitAlerts()
	})
}

// The counter is keyed by the org's OWN period.
func TestGateDayPeriodKeysByDay(t *testing.T) {
	e := newGateEnv(t, "custom", Limits{EventsSoft: 5, EventsHard: 9, Period: "day"}, nil)
	e.reload(t)
	if got := e.g.CheckIngest(context.Background(), e.org); got != Allow {
		t.Fatalf("decision = %v, want allow", got)
	}
	if got := e.counter(t, MetricEvents, "day"); got != 1 {
		t.Errorf("day-keyed counter = %d, want 1", got)
	}
	if time.Now().UTC().Day() != 1 { // on the 1st the day and month period starts coincide
		if got := e.counter(t, MetricEvents, "month"); got != -1 {
			t.Errorf("month-keyed counter = %d, want none for a day org", got)
		}
	}
}

func TestGateNilAndRedislessAllow(t *testing.T) {
	var g *Gate
	if got := g.CheckIngest(context.Background(), uuid.New()); got != Allow {
		t.Errorf("nil gate = %v, want allow", got)
	}
	if got := (&Gate{}).CheckPublish(context.Background(), uuid.New()); got != Allow {
		t.Errorf("gate without Redis = %v, want allow", got)
	}
}

// A gate that has never loaded a snapshot treats the org as unlimited (never
// guesses a limit from a miss), kicks off ONE background reload, and enforces
// once it lands.
func TestGateFirstRequestAllowsThenSnapshotEnforces(t *testing.T) {
	e := newGateEnv(t, "custom", Limits{EventsSoft: 1, EventsHard: 1, Period: "month"}, nil)
	if got := e.g.CheckIngest(context.Background(), e.org); got != Allow {
		t.Fatalf("first request with no snapshot = %v, want allow (cache miss is unlimited)", got)
	}
	waitFor(t, "background reload to publish a snapshot", func() bool { return e.g.snap.Load() != nil && !e.g.refreshing.Load() })
	if got := e.g.CheckIngest(context.Background(), e.org); got != OverHard {
		t.Fatalf("after reload = %v, want over_hard (limit 1, second request)", got)
	}
	e.g.WaitAlerts()
}

// A snapshot older than LimitsTTL is still served while a reload runs, and the
// reload replaces it.
func TestGateStaleSnapshotServedThenRefreshed(t *testing.T) {
	e := newGateEnv(t, "custom", Limits{EventsSoft: 100, EventsHard: 200, Period: "month"}, nil)
	stale := &snapshot{at: time.Now().Add(-2 * LimitsTTL), limits: map[uuid.UUID]Limits{
		e.org: {EventsSoft: 1, EventsHard: 2, Period: "month"}, // older, stricter numbers
	}}
	e.g.snap.Store(stale)
	if got := e.g.CheckIngest(context.Background(), e.org); got != OverSoft {
		t.Fatalf("stale snapshot not served: decision = %v, want over_soft from the stale limit", got)
	}
	waitFor(t, "stale snapshot to be replaced", func() bool { return e.g.snap.Load() != stale && !e.g.refreshing.Load() })
	if lim := e.g.snap.Load().limits[e.org]; lim.EventsSoft != 100 || lim.EventsHard != 200 {
		t.Errorf("reloaded limits = %+v, want soft 100 hard 200", lim)
	}
	e.g.WaitAlerts()
}

// When the reload fails the gate keeps the previous snapshot (stale, never
// wrong), says so, and releases the single-flight latch so the next request
// retries.
func TestGateReloadFailureServesStale(t *testing.T) {
	e := newGateEnv(t, "custom", Limits{EventsSoft: 100, EventsHard: 200, Period: "month"},
		gatePool(t, "from organizations order by name"))
	stale := &snapshot{at: time.Now().Add(-2 * LimitsTTL), limits: map[uuid.UUID]Limits{
		e.org: {EventsSoft: 100, EventsHard: 1, Period: "month"},
	}}
	e.g.snap.Store(stale)
	// Hard 1 in the stale snapshot makes the first request OverHard; zero limits
	// (a fallback) or the DB's 100/200 would both say Allow.
	if got := e.g.CheckIngest(context.Background(), e.org); got != OverHard {
		t.Fatalf("decision = %v, want over_hard from the stale snapshot", got)
	}
	e.g.WaitAlerts()
	waitFor(t, "failed reload to be logged", func() bool {
		return strings.Contains(e.log.String(), "serving stale") && !e.g.refreshing.Load()
	})
	if e.g.snap.Load() != stale {
		t.Error("a failed reload replaced the snapshot; it must keep the previous one")
	}
	if err := e.g.Reload(context.Background()); err == nil {
		t.Error("Reload on a failing query returned nil")
	}
}

// With no Queries the gate cannot reload; refresh must be a no-op and the org
// stays unlimited.
func TestGateWithoutQueriesNeverRefreshes(t *testing.T) {
	e := newGateEnv(t, "custom", Limits{EventsSoft: 1, EventsHard: 1, Period: "month"}, nil)
	e.g.Queries = nil
	for i := 0; i < 3; i++ {
		if got := e.g.CheckIngest(context.Background(), e.org); got != Allow {
			t.Fatalf("request %d = %v, want allow (no snapshot can ever load)", i+1, got)
		}
	}
	if e.g.snap.Load() != nil || e.g.refreshing.Load() {
		t.Error("refresh ran without Queries")
	}
}

// Alerting needs both Queries and Queue; without either the over-limit decision
// still stands but nothing is published and no latch is taken.
func TestGateAlertSkippedWithoutQueueOrQueries(t *testing.T) {
	for name, mut := range map[string]func(g *Gate){
		"no queue":   func(g *Gate) { g.Queue = nil },
		"no queries": func(g *Gate) { g.Queries = nil },
	} {
		t.Run(name, func(t *testing.T) {
			e := newGateEnv(t, "custom", Limits{EventsSoft: 1, EventsHard: 2, Period: "month"}, nil)
			e.g.snap.Store(&snapshot{at: time.Now(), limits: map[uuid.UUID]Limits{e.org: {EventsSoft: 1, EventsHard: 2, Period: "month"}}})
			mut(e.g)
			if got := e.g.CheckIngest(context.Background(), e.org); got != OverSoft {
				t.Fatalf("decision = %v, want over_soft", got)
			}
			e.g.WaitAlerts()
			if keys, _ := e.rdb.Keys(context.Background(), "usage:alerted:"+e.org.String()+":*").Result(); len(keys) != 0 {
				t.Errorf("latch taken without a way to publish: %v", keys)
			}
			if ops := e.ops(t); len(ops) != 0 {
				t.Errorf("published without queries/queue: %v", ops)
			}
		})
	}
}

// A failing publish is logged and dropped; the decision is unaffected.
func TestGateAlertPublishFailureIsLoggedNotFatal(t *testing.T) {
	e := newGateEnv(t, "custom", Limits{EventsSoft: 1, EventsHard: 0, Period: "month"},
		gatePool(t, "insert into messages"))
	e.reload(t)
	if got := e.g.CheckIngest(context.Background(), e.org); got != OverSoft {
		t.Fatalf("decision = %v, want over_soft", got)
	}
	e.g.WaitAlerts()
	if out := e.log.String(); !strings.Contains(out, "publish quota alert (ignored)") || !strings.Contains(out, "usage.quota_warning") {
		t.Errorf("publish failure not logged: %s", out)
	}
	if ops := e.ops(t); len(ops) != 0 {
		t.Errorf("a failed publish left messages behind: %v", ops)
	}
}

// A panic inside the off-path alert goroutine is recovered and logged; the
// request's decision stands and the process lives. A zero store.Queries has no
// database behind it, so the first alert query dereferences nil.
func TestGateAlertPanicIsRecovered(t *testing.T) {
	e := newGateEnv(t, "custom", Limits{EventsSoft: 1, EventsHard: 0, Period: "month"}, nil)
	e.g.Queries = store.New(nil)
	e.g.snap.Store(&snapshot{at: time.Now(), limits: map[uuid.UUID]Limits{e.org: {EventsSoft: 1, Period: "month"}}})
	if got := e.g.CheckIngest(context.Background(), e.org); got != OverSoft {
		t.Fatalf("decision = %v, want over_soft", got)
	}
	e.g.WaitAlerts()
	if out := e.log.String(); !strings.Contains(out, "quota alert panic (ignored)") || !strings.Contains(out, "nil pointer") {
		t.Errorf("panic not logged: %s", out)
	}
}

// If the latch cannot be taken the alert is skipped rather than published once
// per request. The Redis failure is a real one: a user with no SET permission,
// so the INCR still works and only the latch write is refused by the server.
func TestGateLatchFailureSkipsAlert(t *testing.T) {
	e := newGateEnv(t, "custom", Limits{EventsSoft: 1, EventsHard: 0, Period: "month"}, nil)
	ctx := context.Background()
	user := "gate-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	const pass = "pw"
	if err := e.rdb.Do(ctx, "ACL", "SETUSER", user, "on", ">"+pass, "~usage:*", "+incrby", "+expire", "+multi", "+exec", "+discard", "+ping", "+hello", "+client|setinfo").Err(); err != nil {
		t.Fatalf("create restricted redis user: %v", err)
	}
	t.Cleanup(func() { _ = e.rdb.Do(context.Background(), "ACL", "DELUSER", user).Err() })
	opts := e.rdb.Options()
	restricted := redis.NewClient(&redis.Options{Addr: opts.Addr, Username: user, Password: pass})
	t.Cleanup(func() { _ = restricted.Close() })
	e.g.Redis = restricted
	e.g.snap.Store(&snapshot{at: time.Now(), limits: map[uuid.UUID]Limits{e.org: {EventsSoft: 1, Period: "month"}}})

	for i := 0; i < 2; i++ {
		if got := e.g.CheckIngest(ctx, e.org); got != OverSoft {
			t.Fatalf("request %d = %v, want over_soft", i+1, got)
		}
	}
	e.g.WaitAlerts()
	if !strings.Contains(e.log.String(), "quota alert latch unavailable (alert skipped)") {
		t.Errorf("latch failure not logged: %s", e.log.String())
	}
	if ops := e.ops(t); len(ops) != 0 {
		t.Errorf("published without a latch: %v", ops)
	}
	if got := e.counter(t, MetricEvents, "month"); got != 2 {
		t.Errorf("counter = %d, want 2 (counting continues)", got)
	}
}

// An unreachable Redis (a real refused connection) fails open and says so: the
// request is allowed even over the limit, and the outage is logged.
func TestGateRedisDownFailsOpen(t *testing.T) {
	org := uuid.New()
	buf := &lockedBuf{}
	dead := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, PoolSize: 1})
	t.Cleanup(func() { _ = dead.Close() })
	g := &Gate{Log: slog.New(slog.NewTextHandler(buf, nil)), Redis: dead}
	g.snap.Store(&snapshot{at: time.Now(), limits: map[uuid.UUID]Limits{org: {EventsSoft: 1, EventsHard: 1, Period: "month"}}})
	if got := g.CheckIngest(context.Background(), org); got != Allow {
		t.Fatalf("decision = %v, want allow (fail-open)", got)
	}
	if !strings.Contains(buf.String(), "quota counter unavailable (fail-open)") {
		t.Errorf("outage not logged: %s", buf.String())
	}
}
