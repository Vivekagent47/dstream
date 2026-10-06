package pipeline

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/deliver"
	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/ingest"
	"github.com/Vivekagent47/dstream/internal/store"
)

// ---- shared helpers for the events / scenarios / bookmarks suites ----

// fullRoutes mounts every handler of events.go, scenarios.go and bookmarks.go.
func fullRoutes(r chi.Router, h Handlers) {
	r.Get("/events", h.ListEvents)
	r.Get("/events/histogram", h.EventsHistogram)
	r.Get("/events/{id}", h.GetEvent)
	r.Post("/events/{id}/retry", h.RetryEvent)
	r.Route("/scenarios", func(r chi.Router) {
		r.Get("/", h.ListScenarios)
		r.Post("/", h.CreateScenario)
		r.Get("/{id}", h.GetScenario)
		r.Patch("/{id}", h.PatchScenario)
		r.Delete("/{id}", h.DeleteScenario)
		r.Post("/{id}/replay-to", h.ReplayScenarioTo)
	})
	r.Route("/bookmarks", func(r chi.Router) {
		r.Get("/", h.ListBookmarks)
		r.Post("/", h.CreateBookmark)
		r.Post("/import", h.ImportBookmark)
		r.Get("/{id}", h.GetBookmark)
		r.Patch("/{id}", h.PatchBookmark)
		r.Delete("/{id}", h.DeleteBookmark)
		r.Post("/{id}/replay", h.ReplayBookmark)
		r.Post("/{id}/replay-to", h.ReplayBookmarkTo)
		r.Get("/{id}/export", h.ExportBookmark)
	})
}

// fullRouter serves fullRoutes over q/pool; replay targets on loopback are
// allowed so an httptest sink is reachable. dq may be nil.
func fullRouter(q *store.Queries, pool *pgxpool.Pool, dq *dqueue.Client) http.Handler {
	return newRouter(q, func(r chi.Router, h Handlers) {
		h.Pool, h.Queue = pool, dq
		h.BodyStore = ingest.NewPostgresBodyStore(q)
		h.Replayer = deliver.NewSafeHTTPClient(5*time.Second, true)
		fullRoutes(r, h)
	})
}

// testQueue is a real dqueue on a private key prefix, wiped on cleanup.
func testQueue(t *testing.T) *dqueue.Client {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr(t)})
	prefix := "cov8-" + uuid.NewString()
	t.Cleanup(func() {
		if keys, err := rdb.Keys(context.Background(), prefix+":*").Result(); err == nil && len(keys) > 0 {
			rdb.Del(context.Background(), keys...)
		}
		_ = rdb.Close()
	})
	return dqueue.NewClient(rdb).WithPrefix(prefix)
}

// pending lists the queued event ids of an org's lane.
func pending(t *testing.T, dq *dqueue.Client, oid uuid.UUID) map[uuid.UUID]int {
	t.Helper()
	items, _, err := dq.Items(context.Background(), "pending", oid.String(), 200)
	if err != nil {
		t.Fatalf("queue items: %v", err)
	}
	out := map[uuid.UUID]int{}
	for _, it := range items {
		out[it.EventID] = it.Attempt
	}
	return out
}

// queuedPayloads decodes the full queue payload of every pending entry of an org.
func queuedPayloads(t *testing.T, dq *dqueue.Client, oid uuid.UUID) map[uuid.UUID]dqueue.Payload {
	t.Helper()
	items, _, err := dq.Items(context.Background(), "pending", oid.String(), 200)
	if err != nil {
		t.Fatalf("queue items: %v", err)
	}
	out := map[uuid.UUID]dqueue.Payload{}
	for _, it := range items {
		var p dqueue.Payload
		if err := json.Unmarshal([]byte(it.Raw), &p); err != nil {
			t.Fatalf("decode payload %q: %v", it.Raw, err)
		}
		out[p.EventID] = p
	}
	return out
}

// failingPool is a real pool on which statements containing marker fail, for
// handlers that also begin transactions.
func failingPool(t *testing.T, marker string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(testDSN(t))
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = 2
	cfg.ConnConfig.Tracer = failTracer{marker: marker}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// closedPool is a pool whose Begin always fails.
func closedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(testDSN(t))
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	pool.Close()
	return pool
}

func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("DSTREAM_TEST_DB_URL")
	if dsn == "" {
		t.Skip("DSTREAM_TEST_DB_URL not set")
	}
	return dsn
}

type cancelKey struct{}

// cancelTracer calls cancel once a statement containing marker has finished,
// so the handler sees its request context die at a known point.
type cancelTracer struct {
	marker string
	cancel context.CancelFunc
}

func (c cancelTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.Contains(strings.ToLower(d.SQL), strings.ToLower(c.marker)) {
		return context.WithValue(ctx, cancelKey{}, true)
	}
	return ctx
}
func (c cancelTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if ctx.Value(cancelKey{}) != nil {
		c.cancel()
	}
}

// cancelingQueries is a real pool whose marker statement cancels cancel() when done.
func cancelingQueries(t *testing.T, marker string, cancel context.CancelFunc) *store.Queries {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(testDSN(t))
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = 2
	cfg.ConnConfig.Tracer = cancelTracer{marker: marker, cancel: cancel}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return store.New(pool)
}

func evIDs(t *testing.T, m map[string]any) []string {
	t.Helper()
	l, ok := m["events"].([]any)
	if !ok {
		t.Fatalf("events must be a list: %v", m)
	}
	out := make([]string, 0, len(l))
	for _, e := range l {
		out = append(out, e.(map[string]any)["id"].(string))
	}
	return out
}

func sameIDs(t *testing.T, what string, got []string, want ...uuid.UUID) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
	for i, w := range want {
		if got[i] != w.String() {
			t.Fatalf("%s[%d] = %s, want %s (all: %v)", what, i, got[i], w, got)
		}
	}
}

// setEvent rewrites an event's status and age.
func setEvent(t *testing.T, pool *pgxpool.Pool, ev store.Event, status string, createdAt time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE events SET status=$2, created_at=$3 WHERE id=$1`, ev.ID, status, createdAt); err != nil {
		t.Fatalf("set event: %v", err)
	}
}

// eventOn seeds a fresh request on src plus one event on conn.
func eventOn(t *testing.T, q *store.Queries, oid uuid.UUID, src store.Source, conn store.Connection) (uuid.UUID, store.Event) {
	t.Helper()
	reqID, _ := seedRequestOn(t, q, src)
	return reqID, seedEvent(t, q, oid, reqID, conn, false)
}

// ---- events.go ----

type eventsFix struct {
	pool           *pgxpool.Pool
	q              *store.Queries
	r              http.Handler
	uid, oid       uuid.UUID
	uidB, oidB     uuid.UUID
	c1, c2         store.Connection
	e1, e2, e3, e4 store.Event // e1..e3 in org A (oldest first), e4 in org B
	now            time.Time
}

func newEventsFix(t *testing.T) eventsFix {
	t.Helper()
	pool := testPool(t)
	q := store.New(pool)
	f := eventsFix{pool: pool, q: q, now: time.Now().UTC()}
	f.uid, f.oid = seedOrg(t, q)
	f.uidB, f.oidB = seedOrg(t, q)
	dropEvents(t, pool, f.oid)
	dropEvents(t, pool, f.oidB)
	src := newSource(t, q, f.oid)
	f.c1 = seedConn(t, q, src, seedDest(t, q, f.oid, "https://sink.example.test/a"))
	f.c2 = seedConn(t, q, src, seedDest(t, q, f.oid, "https://sink.example.test/b"))
	_, f.e1 = eventOn(t, q, f.oid, src, f.c1)
	_, f.e2 = eventOn(t, q, f.oid, src, f.c1)
	_, f.e3 = eventOn(t, q, f.oid, src, f.c2)
	setEvent(t, pool, f.e1, "queued", f.now.Add(-3*time.Hour))
	setEvent(t, pool, f.e2, "delivered", f.now.Add(-2*time.Hour))
	setEvent(t, pool, f.e3, "failed", f.now.Add(-1*time.Hour))
	srcB := newSource(t, q, f.oidB)
	cB := seedConn(t, q, srcB, seedDest(t, q, f.oidB, "https://sink.example.test/c"))
	_, f.e4 = eventOn(t, q, f.oidB, srcB, cB)
	f.r = fullRouter(q, pool, nil)
	return f
}

func (f eventsFix) get(t *testing.T, path string) map[string]any {
	t.Helper()
	rec := do(t, f.r, http.MethodGet, path, f.uid, f.oid, nil)
	wantStatus(t, rec, http.StatusOK)
	return decodeMap(t, rec)
}

func gid(e store.Event) uuid.UUID { return store.GoUUID(e.ID) }

func TestListEvents(t *testing.T) {
	f := newEventsFix(t)
	c1, c2 := store.GoUUID(f.c1.ID).String(), store.GoUUID(f.c2.ID).String()
	ts := func(d time.Duration) string { return url.QueryEscape(f.now.Add(d).Format(time.RFC3339)) }

	t.Run("newest first, own org only, row shape", func(t *testing.T) {
		m := f.get(t, "/api/events")
		sameIDs(t, "events", evIDs(t, m), gid(f.e3), gid(f.e2), gid(f.e1))
		if _, ok := m["next_cursor"]; ok {
			t.Fatalf("short page must not carry a cursor: %v", m)
		}
		first := m["events"].([]any)[0].(map[string]any)
		mustEq(t, "status", first["status"], "failed")
		mustEq(t, "connection_id", first["connection_id"], c2)
		mustEq(t, "is_test", first["is_test"], false)
		mustEq(t, "last_attempt_at", first["last_attempt_at"], nil)
		other := decodeMap(t, do(t, f.r, http.MethodGet, "/api/events", f.uidB, f.oidB, nil))
		sameIDs(t, "org B events", evIDs(t, other), gid(f.e4))
	})

	t.Run("filters", func(t *testing.T) {
		sameIDs(t, "connection c1", evIDs(t, f.get(t, "/api/events?connection_id="+c1)), gid(f.e2), gid(f.e1))
		sameIDs(t, "connection c2", evIDs(t, f.get(t, "/api/events?connection_id="+c2)), gid(f.e3))
		sameIDs(t, "zero connection", evIDs(t, f.get(t, "/api/events?connection_id="+uuid.Nil.String())))
		sameIDs(t, "status", evIDs(t, f.get(t, "/api/events?status=delivered")), gid(f.e2))
		sameIDs(t, "both", evIDs(t, f.get(t, "/api/events?status=queued&connection_id="+c2)))
	})

	t.Run("time window", func(t *testing.T) {
		sameIDs(t, "after -150m", evIDs(t, f.get(t, "/api/events?after="+ts(-150*time.Minute))), gid(f.e3), gid(f.e2))
		sameIDs(t, "after future", evIDs(t, f.get(t, "/api/events?after="+ts(time.Hour))))
	})

	t.Run("paging by cursor", func(t *testing.T) {
		p1 := f.get(t, "/api/events?limit=2")
		sameIDs(t, "page 1", evIDs(t, p1), gid(f.e3), gid(f.e2))
		cur, _ := p1["next_cursor"].(string)
		if cur == "" {
			t.Fatalf("full page must carry a cursor: %v", p1)
		}
		p2 := f.get(t, "/api/events?limit=2&cursor="+cur)
		sameIDs(t, "page 2", evIDs(t, p2), gid(f.e1))
		if _, ok := p2["next_cursor"]; ok {
			t.Fatalf("last page must not carry a cursor: %v", p2)
		}
	})

	t.Run("unusable limit falls back to the default page", func(t *testing.T) {
		// "0", "-4" and "abc" bite the n>0 / parse guards: a limit taken literally would
		// return no rows or fail. The n<=500 bound is NOT pinned here: three rows come
		// back whether 501 is accepted or replaced by the default, and seeding >500
		// rows per run is not worth it.
		for _, l := range []string{"0", "-4", "abc", "501"} {
			sameIDs(t, "limit="+l, evIDs(t, f.get(t, "/api/events?limit="+l)), gid(f.e3), gid(f.e2), gid(f.e1))
		}
	})

	t.Run("rows with the same created_at page by id", func(t *testing.T) {
		src := newSource(t, f.q, f.oid)
		c := seedConn(t, f.q, src, seedDest(t, f.q, f.oid, "https://sink.example.test/tie"))
		_, ta := eventOn(t, f.q, f.oid, src, c)
		_, tb := eventOn(t, f.q, f.oid, src, c)
		same := f.now.Add(-30 * time.Minute)
		setEvent(t, f.pool, ta, "queued", same)
		setEvent(t, f.pool, tb, "queued", same)
		// Fixed ids, strictly ordered, with the HIGHER id on the row inserted first, so
		// insertion order is the opposite of the expected id-descending page order.
		hiID := uuid.MustParse("ffffffff-ffff-7fff-8fff-ffffffffff01")
		loID := uuid.MustParse("00000000-0000-7000-8000-000000000001")
		for old, nu := range map[pgtype.UUID]uuid.UUID{ta.ID: hiID, tb.ID: loID} {
			if _, err := f.pool.Exec(context.Background(), `UPDATE events SET id=$2 WHERE id=$1`, old, nu); err != nil {
				t.Fatalf("pin event id: %v", err)
			}
		}
		hi, lo := store.Event{ID: store.UUID(hiID)}, store.Event{ID: store.UUID(loID)}
		cf := "&connection_id=" + store.GoUUID(c.ID).String()
		p1 := f.get(t, "/api/events?limit=1"+cf)
		sameIDs(t, "page 1", evIDs(t, p1), gid(hi))
		p2 := f.get(t, "/api/events?limit=1"+cf+"&cursor="+p1["next_cursor"].(string))
		sameIDs(t, "page 2", evIDs(t, p2), gid(lo))
		p3 := f.get(t, "/api/events?limit=1"+cf+"&cursor="+p2["next_cursor"].(string))
		sameIDs(t, "page 3", evIDs(t, p3))
	})

	t.Run("bad input", func(t *testing.T) {
		enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
		okTS := f.now.Format(time.RFC3339Nano)
		for path, msg := range map[string]string{
			"/api/events?connection_id=nope":                            "invalid connection_id",
			"/api/events?status=weird":                                  "invalid status",
			"/api/events?after=yesterday":                               "invalid after",
			"/api/events?cursor=%21%21":                                 "invalid cursor",
			"/api/events?cursor=" + enc("no-separator"):                 "invalid cursor",
			"/api/events?cursor=" + enc("not-a-time|"+uuid.NewString()): "invalid cursor",
			"/api/events?cursor=" + enc(okTS+"|not-a-uuid"):             "invalid cursor",
		} {
			wantErr(t, do(t, f.r, http.MethodGet, path, f.uid, f.oid, nil), http.StatusBadRequest, msg)
		}
	})

	t.Run("database failure", func(t *testing.T) {
		r := fullRouter(failingQueries(t, "order by e.created_at desc"), nil, nil)
		wantErr(t, do(t, r, http.MethodGet, "/api/events", f.uid, f.oid, nil), http.StatusInternalServerError, "list")
	})
}

func TestEventScenarioBookmarkHandlersRequireActiveOrg(t *testing.T) {
	h := Handlers{Log: discardLog()}
	for name, fn := range map[string]http.HandlerFunc{
		"ListEvents": h.ListEvents, "EventsHistogram": h.EventsHistogram, "GetEvent": h.GetEvent, "RetryEvent": h.RetryEvent,
		"ListScenarios": h.ListScenarios, "CreateScenario": h.CreateScenario, "GetScenario": h.GetScenario,
		"PatchScenario": h.PatchScenario, "ReplayScenarioTo": h.ReplayScenarioTo, "DeleteScenario": h.DeleteScenario,
		"CreateBookmark": h.CreateBookmark, "PatchBookmark": h.PatchBookmark, "ListBookmarks": h.ListBookmarks,
		"GetBookmark": h.GetBookmark, "DeleteBookmark": h.DeleteBookmark, "ReplayBookmark": h.ReplayBookmark,
		"ReplayBookmarkTo": h.ReplayBookmarkTo, "ExportBookmark": h.ExportBookmark, "ImportBookmark": h.ImportBookmark,
	} {
		for who, p := range map[string]*auth.Principal{"no principal": nil, "no active org": {UserID: uuid.New()}} {
			rec := direct(context.Background(), fn, http.MethodGet, p, uuid.NewString(), `{}`)
			if rec.Code != http.StatusUnauthorized || decodeMap(t, rec)["error"] != "active org required" {
				t.Fatalf("%s (%s): %d %s", name, who, rec.Code, rec.Body.String())
			}
		}
	}
}

func histogram(t *testing.T, r http.Handler, uid, oid uuid.UUID, query string) (bucket string, byTs map[string]map[string]any, order []string) {
	t.Helper()
	rec := do(t, r, http.MethodGet, "/api/events/histogram"+query, uid, oid, nil)
	wantStatus(t, rec, http.StatusOK)
	m := decodeMap(t, rec)
	bs, ok := m["buckets"].([]any)
	if !ok {
		t.Fatalf("buckets must be a list: %v", m)
	}
	byTs = map[string]map[string]any{}
	for _, b := range bs {
		bm := b.(map[string]any)
		ts := bm["ts"].(string)
		byTs[ts] = bm
		order = append(order, ts)
	}
	return m["bucket"].(string), byTs, order
}

// dbTrunc asks Postgres which bucket a moment falls in, so the test agrees
// with the server whatever its time zone.
func dbTrunc(t *testing.T, pool *pgxpool.Pool, unit string, at time.Time) time.Time {
	t.Helper()
	var out time.Time
	if err := pool.QueryRow(context.Background(), `SELECT date_trunc($1, $2::timestamptz)`, unit, at).Scan(&out); err != nil {
		t.Fatalf("date_trunc: %v", err)
	}
	return out.UTC()
}

func sumCounts(bs map[string]map[string]any, status string) (n int) {
	for _, b := range bs {
		if v, ok := b["counts"].(map[string]any)[status]; ok {
			n += int(v.(float64))
		}
	}
	return n
}

func TestEventsHistogram(t *testing.T) {
	f := newEventsFix(t)
	after := url.QueryEscape(f.now.Add(-4 * time.Hour).Format(time.RFC3339))
	c1 := store.GoUUID(f.c1.ID).String()

	t.Run("counts per status over contiguous hourly buckets", func(t *testing.T) {
		bucket, bs, order := histogram(t, f.r, f.uid, f.oid, "?after="+after)
		mustEq(t, "bucket", bucket, "hour")
		mustEq(t, "queued", sumCounts(bs, "queued"), 1)
		mustEq(t, "delivered", sumCounts(bs, "delivered"), 1)
		mustEq(t, "failed", sumCounts(bs, "failed"), 1)
		var total float64
		for _, b := range bs {
			total += b["total"].(float64)
		}
		mustEq(t, "total", total, float64(3))
		for i := 1; i < len(order); i++ {
			a, _ := time.Parse(time.RFC3339, order[i-1])
			b, _ := time.Parse(time.RFC3339, order[i])
			if b.Sub(a) != time.Hour {
				t.Fatalf("buckets %s -> %s are not one hour apart", order[i-1], order[i])
			}
		}
		last := dbTrunc(t, f.pool, "hour", time.Now()).Format(time.RFC3339)
		if order[len(order)-1] != last && order[len(order)-1] != dbTrunc(t, f.pool, "hour", f.now).Format(time.RFC3339) {
			t.Fatalf("series must end at the current hour, ends %s", order[len(order)-1])
		}
	})

	t.Run("same filters as the list", func(t *testing.T) {
		_, bs, _ := histogram(t, f.r, f.uid, f.oid, "?after="+after+"&status=failed")
		mustEq(t, "failed", sumCounts(bs, "failed"), 1)
		mustEq(t, "delivered", sumCounts(bs, "delivered"), 0)
		_, bs, _ = histogram(t, f.r, f.uid, f.oid, "?after="+after+"&connection_id="+c1)
		mustEq(t, "c1 failed", sumCounts(bs, "failed"), 0)
		mustEq(t, "c1 delivered", sumCounts(bs, "delivered"), 1)
		_, bs, _ = histogram(t, f.r, f.uid, f.oid, "?after="+after+"&connection_id="+uuid.Nil.String())
		mustEq(t, "zero connection", sumCounts(bs, "queued")+sumCounts(bs, "failed")+sumCounts(bs, "delivered"), 0)
	})

	t.Run("org B sees none of org A", func(t *testing.T) {
		_, bs, _ := histogram(t, f.r, f.uidB, f.oidB, "?after="+after)
		mustEq(t, "B queued", sumCounts(bs, "queued"), 1) // e4 only
		mustEq(t, "B delivered", sumCounts(bs, "delivered"), 0)
		mustEq(t, "B failed", sumCounts(bs, "failed"), 0)
	})

	t.Run("empty range still yields zeroed buckets", func(t *testing.T) {
		uid, oid := seedOrg(t, f.q)
		_, bs, order := histogram(t, f.r, uid, oid, "?after="+after)
		if len(order) < 4 {
			t.Fatalf("a 4h window must have at least 4 buckets, got %d", len(order))
		}
		for ts, b := range bs {
			if b["total"].(float64) != 0 || len(b["counts"].(map[string]any)) != 0 {
				t.Fatalf("bucket %s not empty: %v", ts, b)
			}
		}
	})

	t.Run("window entirely in the future has no buckets", func(t *testing.T) {
		fut := url.QueryEscape(f.now.Add(2 * time.Hour).Format(time.RFC3339))
		_, _, order := histogram(t, f.r, f.uid, f.oid, "?after="+fut)
		mustEq(t, "buckets", len(order), 0)
	})

	t.Run("default window is the last 24h and the bucket falls back to hour", func(t *testing.T) {
		bucket, bs, order := histogram(t, f.r, f.uid, f.oid, "?bucket=fortnight")
		mustEq(t, "bucket", bucket, "hour")
		if len(order) < 24 || len(order) > 26 {
			t.Fatalf("default window should span ~24 hourly buckets, got %d", len(order))
		}
		mustEq(t, "delivered", sumCounts(bs, "delivered"), 1)
	})

	t.Run("a far-past after is clamped to the bucket ceiling", func(t *testing.T) {
		bucket, _, order := histogram(t, f.r, f.uid, f.oid, "?bucket=minute&after=2000-01-01T00:00:00Z")
		mustEq(t, "bucket", bucket, "minute")
		if len(order) < 1400 || len(order) > maxHistogramBuckets+2 {
			t.Fatalf("clamped series length = %d, want ~%d", len(order), maxHistogramBuckets)
		}
	})

	t.Run("day and week buckets", func(t *testing.T) {
		for _, unit := range []string{"day", "week"} {
			span, step := 3*24*time.Hour, 24*time.Hour
			if unit == "week" {
				span, step = 3*7*24*time.Hour, 7*24*time.Hour
			}
			bucket, bs, order := histogram(t, f.r, f.uid, f.oid, "?bucket="+unit+"&after="+url.QueryEscape(f.now.Add(-span).Format(time.RFC3339)))
			mustEq(t, "bucket", bucket, unit)
			mustEq(t, unit+" failed", sumCounts(bs, "failed"), 1)
			if len(order) < 2 {
				t.Fatalf("%s: want several buckets, got %v", unit, order)
			}
			first, _ := time.Parse(time.RFC3339, order[0])
			mustEq(t, unit+" first bucket aligned", first.Equal(dbTrunc(t, f.pool, unit, first)), true)
			for i := 1; i < len(order); i++ {
				a, _ := time.Parse(time.RFC3339, order[i-1])
				b, _ := time.Parse(time.RFC3339, order[i])
				// calendar steps: a DST change in the server zone may shift a day by an hour
				if d := b.Sub(a); d < step-time.Hour || d > step+time.Hour {
					t.Fatalf("%s buckets %s -> %s are %v apart, want %v", unit, order[i-1], order[i], d, step)
				}
			}
		}
	})

	t.Run("bad input", func(t *testing.T) {
		for path, msg := range map[string]string{
			"?connection_id=nope": "invalid connection_id",
			"?status=weird":       "invalid status",
			"?after=yesterday":    "invalid after",
		} {
			wantErr(t, do(t, f.r, http.MethodGet, "/api/events/histogram"+path, f.uid, f.oid, nil), http.StatusBadRequest, msg)
		}
	})

	t.Run("database failure", func(t *testing.T) {
		r := fullRouter(failingQueries(t, "generate_series"), nil, nil)
		wantErr(t, do(t, r, http.MethodGet, "/api/events/histogram", f.uid, f.oid, nil), http.StatusInternalServerError, "histogram")
	})
}

func TestEventsHistogramSplitsEventsAcrossAPeriodBoundary(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedOrg(t, q)
	dropEvents(t, pool, oid)
	src := newSource(t, q, oid)
	conn := seedConn(t, q, src, seedDest(t, q, oid, "https://sink.example.test/p"))
	r := fullRouter(q, pool, nil)

	boundary := dbTrunc(t, pool, "hour", time.Now().Add(-5*time.Hour))
	before, afterB, stale := boundary.Add(-time.Minute), boundary.Add(time.Minute), boundary.Add(-3*time.Hour)
	_, evBefore := eventOn(t, q, oid, src, conn)
	_, evAfter := eventOn(t, q, oid, src, conn)
	_, evStale := eventOn(t, q, oid, src, conn)
	setEvent(t, pool, evBefore, "delivered", before)
	setEvent(t, pool, evAfter, "failed", afterB)
	setEvent(t, pool, evStale, "failed", stale)

	_, bs, _ := histogram(t, r, uid, oid, "?after="+url.QueryEscape(boundary.Add(-2*time.Hour).Format(time.RFC3339)))
	kBefore := dbTrunc(t, pool, "hour", before).Format(time.RFC3339)
	kAfter := dbTrunc(t, pool, "hour", afterB).Format(time.RFC3339)
	if kBefore == kAfter {
		t.Fatalf("test setup: both events fell in bucket %s", kBefore)
	}
	mustEq(t, "before bucket total", bs[kBefore]["total"], float64(1))
	mustEq(t, "before bucket delivered", bs[kBefore]["counts"].(map[string]any)["delivered"], float64(1))
	mustEq(t, "after bucket total", bs[kAfter]["total"], float64(1))
	mustEq(t, "after bucket failed", bs[kAfter]["counts"].(map[string]any)["failed"], float64(1))
	// the stale event predates `after`, so it is in no bucket
	mustEq(t, "failed overall", sumCounts(bs, "failed"), 1)
}

func TestGetEvent(t *testing.T) {
	f := newEventsFix(t)
	ctx := context.Background()
	src := newSource(t, f.q, f.oid)
	dst := seedDest(t, f.q, f.oid, "https://sink.example.test/detail")
	conn := seedConn(t, f.q, src, dst)
	reqID, ev := eventOn(t, f.q, f.oid, src, conn)
	if _, err := ingest.NewPostgresBodyStore(f.q).Put(ctx, reqID, []byte(`{"order":42}`)); err != nil {
		t.Fatalf("put body: %v", err)
	}
	code, dur, qd, msg := int32(502), int32(31), int32(7), "bad gateway"
	if _, err := f.q.CreateAttempt(ctx, store.CreateAttemptParams{
		EventID: ev.ID, AttemptNum: 1, ResponseStatus: &code, ResponseHeaders: []byte(`{"X-Up":["1"]}`),
		ResponseBody: []byte("upstream said no"), DurationMs: &dur, QueuedInMs: &qd, ErrorMessage: &msg,
	}); err != nil {
		t.Fatalf("seed attempt: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE requests SET headers=$2, body_size=12 WHERE id=$1`, reqID, []byte(`{"X-Src":["yes"]}`)); err != nil {
		t.Fatalf("seed request headers: %v", err)
	}
	retryAt := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	if _, err := f.pool.Exec(ctx,
		`UPDATE events SET status='failed', attempt_count=1, last_attempt_at=now(), next_retry_at=$2 WHERE id=$1`, ev.ID, retryAt); err != nil {
		t.Fatalf("seed attempt state: %v", err)
	}
	id := gid(ev).String()

	t.Run("detail with request body and attempts", func(t *testing.T) {
		m := f.get(t, "/api/events/"+id)
		mustEq(t, "id", m["id"], id)
		mustEq(t, "request_id", m["request_id"], reqID.String())
		mustEq(t, "connection_id", m["connection_id"], store.GoUUID(conn.ID).String())
		mustEq(t, "source_id", m["source_id"], store.GoUUID(src.ID).String())
		mustEq(t, "destination_id", m["destination_id"], store.GoUUID(dst.ID).String())
		mustEq(t, "status", m["status"], "failed")
		mustEq(t, "attempt_count", m["attempt_count"], float64(1))
		mustEq(t, "is_test", m["is_test"], false)
		next, _ := time.Parse(time.RFC3339, m["next_retry_at"].(string))
		if !next.Equal(retryAt) {
			t.Fatalf("next_retry_at = %v, want %v", m["next_retry_at"], retryAt)
		}
		if m["last_attempt_at"] == nil {
			t.Fatal("last_attempt_at missing")
		}
		d := m["destination"].(map[string]any)
		mustEq(t, "destination.type", d["type"], "http")
		mustEq(t, "destination.url", d["url"], "https://sink.example.test/detail")
		rq := m["request"].(map[string]any)
		mustEq(t, "request.method", rq["method"], "POST")
		mustEq(t, "request.path", rq["path"], "/e/x")
		mustEq(t, "request.body", rq["body"], `{"order":42}`)
		mustEq(t, "request.content_type", rq["content_type"], "application/json")
		mustEq(t, "request.body_size", rq["body_size"], float64(12))
		mustEq(t, "request.headers", rq["headers"].(map[string]any)["X-Src"].([]any)[0], "yes")
		at := m["attempts"].([]any)
		mustEq(t, "attempts", len(at), 1)
		a := at[0].(map[string]any)
		mustEq(t, "attempt_num", a["attempt_num"], float64(1))
		mustEq(t, "response_status", a["response_status"], float64(502))
		mustEq(t, "response_body", a["response_body"], "upstream said no")
		mustEq(t, "error_message", a["error_message"], "bad gateway")
		mustEq(t, "duration_ms", a["duration_ms"], float64(31))
		mustEq(t, "queued_in_ms", a["queued_in_ms"], float64(7))
		if ts, _ := time.Parse(time.RFC3339, a["attempted_at"].(string)); time.Since(ts) > time.Hour || ts.After(time.Now().Add(time.Minute)) {
			t.Fatalf("attempted_at = %v, want about now", a["attempted_at"])
		}
		mustEq(t, "response header", a["response_headers"].(map[string]any)["X-Up"].([]any)[0], "1")
	})

	t.Run("a body that is no longer stored is omitted, not an error", func(t *testing.T) {
		m := f.get(t, "/api/events/"+gid(f.e1).String())
		mustEq(t, "request.body", m["request"].(map[string]any)["body"], "")
		mustEq(t, "attempts", len(m["attempts"].([]any)), 0)
		mustEq(t, "status", m["status"], "queued")
	})

	t.Run("refusals", func(t *testing.T) {
		wantErr(t, do(t, f.r, http.MethodGet, "/api/events/not-a-uuid", f.uid, f.oid, nil), http.StatusBadRequest, "invalid id")
		wantErr(t, do(t, f.r, http.MethodGet, "/api/events/"+uuid.NewString(), f.uid, f.oid, nil), http.StatusNotFound, "not found")
		// org B asking for org A's real event learns nothing
		rec := do(t, f.r, http.MethodGet, "/api/events/"+id, f.uidB, f.oidB, nil)
		wantErr(t, rec, http.StatusNotFound, "not found")
		if rec.Body.Len() > 40 {
			t.Fatalf("refusal body leaks detail: %s", rec.Body.String())
		}
		// ...and A cannot read B's event either
		wantErr(t, do(t, f.r, http.MethodGet, "/api/events/"+gid(f.e4).String(), f.uid, f.oid, nil), http.StatusNotFound, "not found")
	})
}

func TestRetryEvent(t *testing.T) {
	f := newEventsFix(t)
	dq := testQueue(t)
	r := fullRouter(f.q, f.pool, dq)
	src := newSource(t, f.q, f.oid)
	conn := seedConn(t, f.q, src, seedDest(t, f.q, f.oid, "https://sink.example.test/retry"))
	// non-default retry settings: they are what a manual retry must carry to the queue
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE connections SET retry_strategy='custom', retry_base_ms=1234, retry_cap_ms=56789, retry_jitter_pct=7, custom_retry_schedule='[100,200,300]' WHERE id=$1`, conn.ID); err != nil {
		t.Fatalf("seed retry settings: %v", err)
	}
	conn, err := f.q.GetConnectionByID(context.Background(), conn.ID)
	if err != nil || conn.RetryStrategy != "custom" {
		t.Fatalf("reload connection: %v %+v", err, conn)
	}
	row := func(ev store.Event) (status string, attempts int, nextRetry *time.Time) {
		t.Helper()
		if err := f.pool.QueryRow(context.Background(),
			`SELECT status, attempt_count, next_retry_at FROM events WHERE id=$1`, ev.ID).Scan(&status, &attempts, &nextRetry); err != nil {
			t.Fatalf("read event: %v", err)
		}
		return
	}
	age := func(ev store.Event, status string) {
		t.Helper()
		if _, err := f.pool.Exec(context.Background(),
			`UPDATE events SET status=$2, attempt_count=3, next_retry_at=now()+interval '1 hour' WHERE id=$1`, ev.ID, status); err != nil {
			t.Fatalf("age event: %v", err)
		}
	}

	// Documents CURRENT behaviour, not contract: RetryEvent has no status guard, so
	// delivered and discarded events are reset and requeued like failed ones (a
	// delivered event is delivered again, with nothing recording it already succeeded).
	t.Run("current behaviour: any status is reset and requeued as a manual retry", func(t *testing.T) {
		want := map[uuid.UUID]bool{}
		for _, st := range []string{"failed", "dead", "delivered", "discarded"} {
			_, ev := eventOn(t, f.q, f.oid, src, conn)
			age(ev, st)
			rec := do(t, r, http.MethodPost, "/api/events/"+gid(ev).String()+"/retry", f.uid, f.oid, nil)
			wantStatus(t, rec, http.StatusAccepted)
			status, attempts, next := row(ev)
			mustEq(t, st+": status", status, "queued")
			mustEq(t, st+": attempt_count kept", attempts, 3)
			if next != nil {
				t.Fatalf("%s: next_retry_at = %v, want NULL", st, next)
			}
			mustEq(t, st+": audit", auditCount(t, f.pool, f.oid, "event.retry", gid(ev)), 1)
			var meta string
			if err := f.pool.QueryRow(context.Background(),
				`SELECT metadata->>'event_id' FROM audit_logs WHERE org_id=$1 AND action='event.retry' AND target_id=$2`, f.oid, gid(ev)).Scan(&meta); err != nil {
				t.Fatalf("audit metadata: %v", err)
			}
			mustEq(t, st+": audit metadata event_id", meta, gid(ev).String())
			want[gid(ev)] = true
		}
		got := queuedPayloads(t, dq, f.oid)
		if len(got) != len(want) {
			t.Fatalf("queue has %d entries, want exactly %d", len(got), len(want))
		}
		for id := range want {
			p, ok := got[id]
			if !ok {
				t.Fatalf("event %s not queued", id)
			}
			mustEq(t, "attempt restarts", p.Attempt, 0)
			mustEq(t, "manual", p.Manual, true)
			mustEq(t, "org", p.OrgID, f.oid)
			mustEq(t, "strategy", p.RetryStrategy, conn.RetryStrategy)
			mustEq(t, "base", p.RetryBaseMs, int32(1234))
			mustEq(t, "cap", p.RetryCapMs, int32(56789))
			mustEq(t, "jitter", p.RetryJitterPct, int32(7))
			if len(p.CustomRetrySchedule) == 0 || !bytes.Equal(p.CustomRetrySchedule, conn.CustomRetrySchedule) {
				t.Fatalf("custom schedule = %q, want %q", p.CustomRetrySchedule, conn.CustomRetrySchedule)
			}
			if p.EnqueuedAt == 0 {
				t.Fatal("enqueued_at not set")
			}
		}
	})

	t.Run("refusals change nothing", func(t *testing.T) {
		before := len(pending(t, dq, f.oid))
		wantErr(t, do(t, r, http.MethodPost, "/api/events/nope/retry", f.uid, f.oid, nil), http.StatusBadRequest, "invalid id")
		wantErr(t, do(t, r, http.MethodPost, "/api/events/"+uuid.NewString()+"/retry", f.uid, f.oid, nil), http.StatusNotFound, "not found")
		// org B retrying org A's real, settled event
		_, ev := eventOn(t, f.q, f.oid, src, conn)
		age(ev, "failed")
		wantErr(t, do(t, r, http.MethodPost, "/api/events/"+gid(ev).String()+"/retry", f.uidB, f.oidB, nil), http.StatusNotFound, "not found")
		status, attempts, next := row(ev)
		mustEq(t, "status", status, "failed")
		mustEq(t, "attempts", attempts, 3)
		if next == nil {
			t.Fatal("next_retry_at was cleared by a foreign retry")
		}
		mustEq(t, "audit", auditCount(t, f.pool, f.oid, "event.retry", gid(ev)), 0)
		mustEq(t, "queue", len(pending(t, dq, f.oid)), before)
		mustEq(t, "queue B", len(pending(t, dq, f.oidB)), 0)
	})

	t.Run("reset failure leaves the event as it was", func(t *testing.T) {
		_, ev := eventOn(t, f.q, f.oid, src, conn)
		age(ev, "failed")
		before := len(pending(t, dq, f.oid))
		rf := fullRouter(failingQueries(t, "next_retry_at = null"), f.pool, dq)
		wantErr(t, do(t, rf, http.MethodPost, "/api/events/"+gid(ev).String()+"/retry", f.uid, f.oid, nil), http.StatusInternalServerError, "retry failed")
		status, _, next := row(ev)
		mustEq(t, "status", status, "failed")
		if next == nil {
			t.Fatal("next_retry_at cleared although the reset failed")
		}
		mustEq(t, "queue", len(pending(t, dq, f.oid)), before)
		mustEq(t, "audit", auditCount(t, f.pool, f.oid, "event.retry", gid(ev)), 0)
	})

	// Documents CURRENT behaviour, not contract: the reset is committed before the
	// connection lookup and the enqueue, so a failure there leaves the event already
	// 'queued' with next_retry_at cleared and NO queue entry (only the reaper recovers it).
	t.Run("current behaviour: connection lookup failure leaves the event reset but unqueued", func(t *testing.T) {
		_, ev := eventOn(t, f.q, f.oid, src, conn)
		age(ev, "failed")
		before := len(pending(t, dq, f.oid))
		rf := fullRouter(failingQueries(t, "from connections where id = $1"), f.pool, dq)
		wantErr(t, do(t, rf, http.MethodPost, "/api/events/"+gid(ev).String()+"/retry", f.uid, f.oid, nil), http.StatusInternalServerError, "load connection")
		status, _, next := row(ev)
		mustEq(t, "status (already reset)", status, "queued")
		if next != nil {
			t.Fatalf("next_retry_at = %v, want NULL (already reset)", next)
		}
		mustEq(t, "queue", len(pending(t, dq, f.oid)), before)
		mustEq(t, "audit", auditCount(t, f.pool, f.oid, "event.retry", gid(ev)), 0)
	})

	// Documents CURRENT behaviour, not contract: same partial state as above.
	t.Run("current behaviour: queue failure leaves the event reset but unqueued", func(t *testing.T) {
		_, ev := eventOn(t, f.q, f.oid, src, conn)
		age(ev, "failed")
		dead := redis.NewClient(&redis.Options{Addr: redisAddr(t)})
		_ = dead.Close()
		rf := fullRouter(f.q, f.pool, dqueue.NewClient(dead).WithPrefix("cov8-dead"))
		wantErr(t, do(t, rf, http.MethodPost, "/api/events/"+gid(ev).String()+"/retry", f.uid, f.oid, nil), http.StatusInternalServerError, "retry failed")
		status, _, next := row(ev)
		mustEq(t, "status (already reset)", status, "queued")
		if next != nil {
			t.Fatalf("next_retry_at = %v, want NULL (already reset)", next)
		}
		mustEq(t, "audit", auditCount(t, f.pool, f.oid, "event.retry", gid(ev)), 0)
		_, ok := pending(t, dq, f.oid)[gid(ev)]
		mustEq(t, "queued", ok, false)
	})
}
