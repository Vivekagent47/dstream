package pipeline

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"

	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/ingest"
	"github.com/Vivekagent47/dstream/internal/store"
)

func connRoutes(r chi.Router, h Handlers) {
	r.Route("/connections", func(r chi.Router) {
		r.Get("/", h.ListConnections)
		r.Get("/stats", h.AllConnectionStats)
		r.Post("/", h.CreateConnection)
		r.Get("/{id}", h.GetConnection)
		r.Get("/{id}/stats", h.ConnectionStats)
		r.Post("/{id}/test", h.TestConnection)
		r.Patch("/{id}", h.PatchConnection)
		r.Delete("/{id}", h.DeleteConnection)
	})
}

// Bad CEL / JS is rejected 400 before any DB work, so these need only an org
// session — no real source/destination/connection rows.
func TestConnectionBadFilterExpr(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	r := newRouter(q, connRoutes)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPost, "/api/connections", uid, oid, map[string]any{
		"source_id": uuid.NewString(), "destination_id": uuid.NewString(),
		"filter_expr": "payload.x ==",
	}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create bad filter: got %d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPatch, "/api/connections/"+uuid.NewString(), uid, oid, map[string]any{
		"filter_expr": "payload.x ==",
	}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("patch bad filter: got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestConnectionBadTransformJS(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	r := newRouter(q, connRoutes)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPost, "/api/connections", uid, oid, map[string]any{
		"source_id": uuid.NewString(), "destination_id": uuid.NewString(),
		"transform_js": "function(",
	}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create bad js: got %d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPatch, "/api/connections/"+uuid.NewString(), uid, oid, map[string]any{
		"transform_js": "function(",
	}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("patch bad js: got %d body=%s", rec.Code, rec.Body.String())
	}
}

func getConn(t *testing.T, q *store.Queries, oid uuid.UUID, id string) store.Connection {
	t.Helper()
	c, err := q.GetConnectionForOrg(context.Background(), store.GetConnectionForOrgParams{
		ID: store.UUID(uuid.MustParse(id)), OrgID: store.UUID(oid)})
	if err != nil {
		t.Fatalf("get connection: %v", err)
	}
	return c
}

func TestConnectionCRUD(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedOrg(t, q)
	r := newRouter(q, connRoutes)
	src, dst := newSource(t, q, oid), seedDest(t, q, oid, "https://sink.example.test/h")
	src2 := newSource(t, q, oid)

	if l := decodeList(t, do(t, r, http.MethodGet, "/api/connections", uid, oid, nil)); len(l) != 0 {
		t.Fatalf("fresh org lists %d connections", len(l))
	}

	// Create with defaults.
	rec := do(t, r, http.MethodPost, "/api/connections", uid, oid, map[string]any{
		"source_id": store.GoUUID(src.ID).String(), "destination_id": store.GoUUID(dst.ID).String(),
		"filter_expr": "", "transform_js": "",
	})
	wantStatus(t, rec, http.StatusCreated)
	created := decodeMap(t, rec)
	id := created["id"].(string)
	row := getConn(t, q, oid, id)
	mustEq(t, "enabled", row.Enabled, true)
	mustEq(t, "max_retries", row.MaxRetries, int32(1))
	mustEq(t, "strategy", row.RetryStrategy, "exponential")
	mustEq(t, "base", row.RetryBaseMs, int32(30000))
	mustEq(t, "cap", row.RetryCapMs, int32(3600000))
	mustEq(t, "jitter", row.RetryJitterPct, int32(20))
	if row.FilterExpr != nil || row.TransformJs != nil || row.Name != nil {
		t.Fatalf("empty filter/transform/name must store NULL: %v %v %v", row.FilterExpr, row.TransformJs, row.Name)
	}
	mustEq(t, "body source_id", created["source_id"], store.GoUUID(src.ID).String())
	mustEq(t, "body destination_id", created["destination_id"], store.GoUUID(dst.ID).String())
	mustEq(t, "body enabled", created["enabled"], true)
	mustEq(t, "body max_retries", created["max_retries"], float64(1))
	mustEq(t, "body strategy", created["retry_strategy"], "exponential")
	if created["filter_expr"] != nil {
		t.Fatalf("filter_expr = %v, want null", created["filter_expr"])
	}
	mustEq(t, "create audit", auditCount(t, pool, oid, "connection.create", uuid.MustParse(id)), 1)

	// Create with every optional field.
	rec = do(t, r, http.MethodPost, "/api/connections", uid, oid, map[string]any{
		"source_id": store.GoUUID(src2.ID).String(), "destination_id": store.GoUUID(dst.ID).String(),
		"enabled": false, "name": "paid only", "filter_expr": `payload.type == "paid"`,
		"transform_js": "function transform(p, h) { return p; }",
	})
	wantStatus(t, rec, http.StatusCreated)
	id2 := decodeMap(t, rec)["id"].(string)
	row2 := getConn(t, q, oid, id2)
	mustEq(t, "enabled", row2.Enabled, false)
	mustEq(t, "name", *row2.Name, "paid only")
	mustEq(t, "filter", *row2.FilterExpr, `payload.type == "paid"`)
	mustEq(t, "transform", *row2.TransformJs, "function transform(p, h) { return p; }")

	// List and Get.
	l := decodeList(t, do(t, r, http.MethodGet, "/api/connections", uid, oid, nil))
	if !hasID(l, uuid.MustParse(id)) || !hasID(l, uuid.MustParse(id2)) {
		t.Fatalf("list missing connections: %v", l)
	}
	got := decodeMap(t, do(t, r, http.MethodGet, "/api/connections/"+id2, uid, oid, nil))
	mustEq(t, "get name", got["name"], "paid only")
	mustEq(t, "get enabled", got["enabled"], false)

	// Patch everything.
	rec = do(t, r, http.MethodPatch, "/api/connections/"+id, uid, oid, map[string]any{
		"enabled": false, "name": "renamed", "max_retries": 5, "retry_strategy": "custom",
		"retry_base_ms": 1000, "retry_cap_ms": 9000, "retry_jitter_pct": 10,
		"custom_retry_schedule": []int{1000, 2000, 4000},
		"filter_expr":           `payload.n > 1`, "transform_js": "function transform(p, h) { return p; }",
	})
	wantStatus(t, rec, http.StatusOK)
	row = getConn(t, q, oid, id)
	mustEq(t, "enabled", row.Enabled, false)
	mustEq(t, "name", *row.Name, "renamed")
	mustEq(t, "max_retries", row.MaxRetries, int32(5))
	mustEq(t, "strategy", row.RetryStrategy, "custom")
	mustEq(t, "base", row.RetryBaseMs, int32(1000))
	mustEq(t, "cap", row.RetryCapMs, int32(9000))
	mustEq(t, "jitter", row.RetryJitterPct, int32(10))
	mustEq(t, "schedule", strings.ReplaceAll(string(row.CustomRetrySchedule), " ", ""), "[1000,2000,4000]")
	mustEq(t, "filter", *row.FilterExpr, "payload.n > 1")
	if row.TransformJs == nil {
		t.Fatal("transform not stored")
	}
	body := decodeMap(t, rec)
	mustEq(t, "body strategy", body["retry_strategy"], "custom")
	mustEq(t, "body max_retries", body["max_retries"], float64(5))
	mustEq(t, "update audit", auditCount(t, pool, oid, "connection.update", uuid.MustParse(id)), 1)

	// An absent filter_expr leaves it; an empty one clears it; transform likewise.
	wantStatus(t, do(t, r, http.MethodPatch, "/api/connections/"+id, uid, oid, map[string]any{"name": "again"}), http.StatusOK)
	row = getConn(t, q, oid, id)
	mustEq(t, "filter kept", *row.FilterExpr, "payload.n > 1")
	mustEq(t, "max_retries kept", row.MaxRetries, int32(5))
	if row.TransformJs == nil {
		t.Fatal("transform dropped by unrelated patch")
	}
	wantStatus(t, do(t, r, http.MethodPatch, "/api/connections/"+id, uid, oid, map[string]any{"filter_expr": "", "transform_js": ""}), http.StatusOK)
	row = getConn(t, q, oid, id)
	if row.FilterExpr != nil || row.TransformJs != nil {
		t.Fatalf("empty string must clear to NULL: %v %v", row.FilterExpr, row.TransformJs)
	}

	// A no-op patch writes no update audit entry.
	before := auditCount(t, pool, oid, "connection.update", uuid.MustParse(id))
	wantStatus(t, do(t, r, http.MethodPatch, "/api/connections/"+id, uid, oid, map[string]any{"max_retries": 5}), http.StatusOK)
	mustEq(t, "no-op audit", auditCount(t, pool, oid, "connection.update", uuid.MustParse(id)), before)

	// Delete.
	wantStatus(t, do(t, r, http.MethodDelete, "/api/connections/"+id, uid, oid, nil), http.StatusNoContent)
	mustEq(t, "rows", rowCount(t, pool, `SELECT count(*) FROM connections WHERE id=$1`, uuid.MustParse(id)), 0)
	mustEq(t, "delete audit", auditCount(t, pool, oid, "connection.delete", uuid.MustParse(id)), 1)
	wantErr(t, do(t, r, http.MethodGet, "/api/connections/"+id, uid, oid, nil), 404, "not found")
	wantErr(t, do(t, r, http.MethodPatch, "/api/connections/"+id, uid, oid, map[string]any{"name": "x"}), 404, "not found")
	mustEq(t, "source kept", rowCount(t, pool, `SELECT count(*) FROM sources WHERE id=$1`, src.ID), 1)
	mustEq(t, "destination kept", rowCount(t, pool, `SELECT count(*) FROM destinations WHERE id=$1`, dst.ID), 1)
	mustEq(t, "sibling kept", getConn(t, q, oid, id2).Enabled, false)
}

func TestConnectionValidation(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedOrg(t, q)
	_, oidB := seedOrg(t, q)
	r := newRouter(q, connRoutes)
	src, dst := newSource(t, q, oid), seedDest(t, q, oid, "https://sink.example.test/h")
	conn := seedConn(t, q, src, dst)
	srcID, dstID := store.GoUUID(src.ID).String(), store.GoUUID(dst.ID).String()
	cid := store.GoUUID(conn.ID).String()
	foreignSrc := newSource(t, q, oidB)
	foreignDst := seedDest(t, q, oidB, "https://b.example.test/h")
	count := func() int {
		return rowCount(t, pool, `SELECT count(*) FROM connections c JOIN sources s ON s.id=c.source_id WHERE s.org_id=$1`, oid)
	}
	start := count()

	post := func(body map[string]any, status int, msg string) {
		t.Helper()
		wantErr(t, do(t, r, http.MethodPost, "/api/connections", uid, oid, body), status, msg)
	}
	post(map[string]any{}, 400, "source_id and destination_id required")
	post(map[string]any{"source_id": srcID}, 400, "source_id and destination_id required")
	post(map[string]any{"destination_id": dstID}, 400, "source_id and destination_id required")
	// Other org's source/destination are indistinguishable from nonexistent ones.
	post(map[string]any{"source_id": store.GoUUID(foreignSrc.ID).String(), "destination_id": dstID}, 400, "source not found in this org")
	post(map[string]any{"source_id": srcID, "destination_id": store.GoUUID(foreignDst.ID).String()}, 400, "destination not found in this org")
	post(map[string]any{"source_id": uuid.NewString(), "destination_id": dstID}, 400, "source not found in this org")
	post(map[string]any{"source_id": srcID, "destination_id": uuid.NewString()}, 400, "destination not found in this org")
	post(map[string]any{"source_id": srcID, "destination_id": dstID, "filter_expr": "payload.n"}, 400,
		"filter expression must evaluate to bool, got dyn")
	// Documents CURRENT behaviour, not contract: a duplicate pair is a user conflict but reads as 500, not 409.
	post(map[string]any{"source_id": srcID, "destination_id": dstID}, 500, "create")
	mustEq(t, "connections after rejected creates", count(), start)

	rec := httptest.NewRecorder()
	req := sessionReq(t, http.MethodPost, "/api/connections", uid, oid, map[string]any{})
	req.Body = io.NopCloser(strings.NewReader("{nope"))
	r.ServeHTTP(rec, req)
	wantErr(t, rec, 400, "invalid json")
	rec = httptest.NewRecorder()
	req = sessionReq(t, http.MethodPatch, "/api/connections/"+cid, uid, oid, map[string]any{})
	req.Body = io.NopCloser(strings.NewReader("{nope"))
	r.ServeHTTP(rec, req)
	wantErr(t, rec, 400, "invalid json")

	patch := func(id string, body map[string]any, status int, msg string) {
		t.Helper()
		wantErr(t, do(t, r, http.MethodPatch, "/api/connections/"+id, uid, oid, body), status, msg)
	}
	patch(cid, map[string]any{"retry_strategy": "random"}, 400, "invalid retry_strategy")
	patch(cid, map[string]any{"filter_expr": "payload.n"}, 400, "filter expression must evaluate to bool, got dyn")
	patch(cid, map[string]any{"name": "a\u0000b"}, 500, "update") // Postgres rejects NUL in text
	patch("not-a-uuid", map[string]any{"name": "x"}, 400, "invalid id")
	patch(uuid.NewString(), map[string]any{"name": "x"}, 404, "not found")
	for _, p := range []string{"/api/connections/not-a-uuid", "/api/connections/not-a-uuid/stats"} {
		wantErr(t, do(t, r, http.MethodGet, p, uid, oid, nil), 400, "invalid id")
	}
	wantErr(t, do(t, r, http.MethodDelete, "/api/connections/not-a-uuid", uid, oid, nil), 400, "invalid id")
	wantErr(t, do(t, r, http.MethodPost, "/api/connections/not-a-uuid/test", uid, oid, nil), 400, "invalid id")
	wantErr(t, do(t, r, http.MethodGet, "/api/connections/"+uuid.NewString(), uid, oid, nil), 404, "not found")

	after := getConn(t, q, oid, cid)
	mustEq(t, "strategy", after.RetryStrategy, "exponential")
	mustEq(t, "name", after.Name == nil, true)
	if after.FilterExpr != nil {
		t.Fatalf("filter changed to %q", *after.FilterExpr)
	}
	mustEq(t, "connections after rejected patches", count(), start)
}

func TestConnectionCrossOrgIsolation(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uidA, oidA := seedOrg(t, q)
	uidB, oidB := seedOrg(t, q)
	dropEvents(t, pool, oidA)
	r := newRouter(q, connRoutes)
	src, dst := newSource(t, q, oidA), seedDest(t, q, oidA, "https://a.example.test/h")
	conn := seedConn(t, q, src, dst)
	id := store.GoUUID(conn.ID).String()
	reqID, _ := seedRequestOn(t, q, src)
	seedEvent(t, q, oidA, reqID, conn, false)
	reqsBefore := rowCount(t, pool, `SELECT count(*) FROM requests WHERE source_id=$1`, src.ID)

	wantErr(t, do(t, r, http.MethodGet, "/api/connections/"+id, uidB, oidB, nil), 404, "not found")
	wantErr(t, do(t, r, http.MethodGet, "/api/connections/"+id+"/stats", uidB, oidB, nil), 404, "not found")
	wantErr(t, do(t, r, http.MethodPost, "/api/connections/"+id+"/test", uidB, oidB, nil), 404, "not found")
	wantErr(t, do(t, r, http.MethodPatch, "/api/connections/"+id, uidB, oidB, map[string]any{"enabled": false, "name": "hijack"}), 404, "not found")
	// Documents CURRENT behaviour, not contract: a foreign DELETE answers 204 (and
	// audits a delete in the caller's org) although nothing was deleted; sources 404.
	wantStatus(t, do(t, r, http.MethodDelete, "/api/connections/"+id, uidB, oidB, nil), http.StatusNoContent)
	// Org B cannot wire its own source to org A's destination either.
	srcB := newSource(t, q, oidB)
	wantErr(t, do(t, r, http.MethodPost, "/api/connections", uidB, oidB, map[string]any{
		"source_id": store.GoUUID(srcB.ID).String(), "destination_id": store.GoUUID(dst.ID).String()}), 400, "destination not found in this org")

	if l := decodeList(t, do(t, r, http.MethodGet, "/api/connections", uidB, oidB, nil)); len(l) != 0 {
		t.Fatalf("org B lists org A's connections: %v", l)
	}
	if m := decodeMap(t, do(t, r, http.MethodGet, "/api/connections/stats", uidB, oidB, nil)); len(m) != 0 {
		t.Fatalf("org B sees org A's stats: %v", m)
	}

	row := getConn(t, q, oidA, id)
	mustEq(t, "enabled", row.Enabled, true)
	if row.Name != nil {
		t.Fatalf("name changed to %q", *row.Name)
	}
	mustEq(t, "events", rowCount(t, pool, `SELECT count(*) FROM events WHERE connection_id=$1`, conn.ID), 1)
	mustEq(t, "requests on A's source", rowCount(t, pool, `SELECT count(*) FROM requests WHERE source_id=$1`, src.ID), reqsBefore)
	mustEq(t, "connections for dst", rowCount(t, pool, `SELECT count(*) FROM connections WHERE destination_id=$1`, dst.ID), 1)
	if l := decodeList(t, do(t, r, http.MethodGet, "/api/connections", uidA, oidA, nil)); !hasID(l, uuid.MustParse(id)) {
		t.Fatalf("org A lost its connection: %v", l)
	}
}

func TestDeleteConnectionCascadesEvents(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedOrg(t, q)
	dropEvents(t, pool, oid)
	r := newRouter(q, connRoutes)
	src, dst := newSource(t, q, oid), seedDest(t, q, oid, "https://sink.example.test/h")
	conn := seedConn(t, q, src, dst)
	reqID, _ := seedRequestOn(t, q, src)
	ev := seedEvent(t, q, oid, reqID, conn, false)
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO attempts (event_id, attempt_num, response_status, duration_ms) VALUES ($1, 1, 200, 5)`, ev.ID); err != nil {
		t.Fatalf("seed attempt: %v", err)
	}
	mustEq(t, "attempts before", rowCount(t, pool, `SELECT count(*) FROM attempts WHERE event_id=$1`, ev.ID), 1)

	wantStatus(t, do(t, r, http.MethodDelete, "/api/connections/"+store.GoUUID(conn.ID).String(), uid, oid, nil), http.StatusNoContent)

	mustEq(t, "events", rowCount(t, pool, `SELECT count(*) FROM events WHERE id=$1`, ev.ID), 0)
	mustEq(t, "attempts", rowCount(t, pool, `SELECT count(*) FROM attempts WHERE event_id=$1`, ev.ID), 0)
	mustEq(t, "request kept", rowCount(t, pool, `SELECT count(*) FROM requests WHERE id=$1`, reqID), 1)
}

func TestConnectionStats(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedOrg(t, q)
	uidB, oidB := seedOrg(t, q)
	dropEvents(t, pool, oid)
	r := newRouter(q, connRoutes)
	ctx := context.Background()
	dst := seedDest(t, q, oid, "https://sink.example.test/h")
	src := newSource(t, q, oid)
	busy := seedConn(t, q, src, dst)
	idle := seedConn(t, q, newSource(t, q, oid), dst)
	reqID, _ := seedRequestOn(t, q, src)

	mk := func(status string, isTest bool, ageHours int) {
		t.Helper()
		ev := seedEvent(t, q, oid, reqID, busy, isTest)
		if _, err := pool.Exec(ctx, `UPDATE events SET status=$2, created_at = now() - make_interval(hours => $3) WHERE id=$1`,
			ev.ID, status, ageHours); err != nil {
			t.Fatalf("set status: %v", err)
		}
	}
	for _, s := range []string{"delivered", "delivered", "failed", "dead", "queued", "in_flight", "paused"} {
		mk(s, false, 0)
	}
	mk("delivered", true, 0)   // test event: excluded
	mk("delivered", false, 48) // outside the 24h window: excluded

	bid := store.GoUUID(busy.ID).String()
	got := decodeMap(t, do(t, r, http.MethodGet, "/api/connections/"+bid+"/stats", uid, oid, nil))
	want := map[string]any{"delivered": 2.0, "failed": 2.0, "pending": 2.0, "total": 7.0, "window": "24h"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stats = %v, want %v", got, want)
	}

	// A connection with no traffic reports zeros, not an error.
	got = decodeMap(t, do(t, r, http.MethodGet, "/api/connections/"+store.GoUUID(idle.ID).String()+"/stats", uid, oid, nil))
	want = map[string]any{"delivered": 0.0, "failed": 0.0, "pending": 0.0, "total": 0.0, "window": "24h"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("idle stats = %v, want %v", got, want)
	}

	// Org-wide: keyed by connection id, idle connection absent, other orgs excluded.
	all := decodeMap(t, do(t, r, http.MethodGet, "/api/connections/stats", uid, oid, nil))
	wantAll := map[string]any{"delivered": 2.0, "failed": 2.0, "pending": 2.0, "total": 7.0}
	if !reflect.DeepEqual(all[bid], wantAll) {
		t.Fatalf("all[busy] = %v, want %v", all[bid], wantAll)
	}
	if _, ok := all[store.GoUUID(idle.ID).String()]; ok {
		t.Fatalf("idle connection must be absent from org stats: %v", all)
	}
	if len(all) != 1 {
		t.Fatalf("org stats = %v, want only the busy connection", all)
	}
	if other := decodeMap(t, do(t, r, http.MethodGet, "/api/connections/stats", uidB, oidB, nil)); len(other) != 0 {
		t.Fatalf("other org stats = %v", other)
	}
}

// TestTestConnection: the handler only creates and enqueues the test
// event; it never contacts the destination, whatever that destination does.
func TestTestConnection(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedOrg(t, q)
	dropEvents(t, pool, oid)
	ctx := context.Background()
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr(t)})
	t.Cleanup(func() { _ = rdb.Close() })
	prefix := "conntest-" + uuid.NewString()
	dq := dqueue.NewClient(rdb).WithPrefix(prefix)
	t.Cleanup(func() {
		if keys, err := rdb.Keys(context.Background(), prefix+":*").Result(); err == nil && len(keys) > 0 {
			rdb.Del(context.Background(), keys...)
		}
	})

	var hits atomic.Int32
	mkTarget := func(code int) string {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			w.WriteHeader(code)
		}))
		t.Cleanup(s.Close)
		return s.URL
	}
	refused := httptest.NewServer(http.NotFoundHandler())
	refusedURL := refused.URL
	refused.Close() // nothing listens here any more

	// The handler never dials the destination, so all three targets must behave
	// identically; the table proves the target's behaviour is irrelevant here.
	targets := map[string]string{"200": mkTarget(200), "500": mkTarget(500), "refused": refusedURL}
	for name, url := range targets {
		t.Run(name, func(t *testing.T) {
			r := newRouter(q, func(r chi.Router, h Handlers) {
				h.Queue, h.BodyStore = dq, ingest.NewPostgresBodyStore(q)
				connRoutes(r, h)
			})
			src, dst := newSource(t, q, oid), seedDest(t, q, oid, url)
			conn := seedConn(t, q, src, dst)
			cid := store.GoUUID(conn.ID).String()

			rec := do(t, r, http.MethodPost, "/api/connections/"+cid+"/test", uid, oid, nil)
			wantStatus(t, rec, http.StatusCreated)
			evID := uuid.MustParse(decodeMap(t, rec)["event_id"].(string))

			var isTest bool
			var status string
			var connID, reqID pgtype.UUID
			if err := pool.QueryRow(ctx, `SELECT is_test, status, connection_id, request_id FROM events WHERE id=$1`, evID).
				Scan(&isTest, &status, &connID, &reqID); err != nil {
				t.Fatalf("event row: %v", err)
			}
			mustEq(t, "is_test", isTest, true)
			mustEq(t, "status", status, "queued")
			mustEq(t, "connection", connID, conn.ID)

			var path string
			var srcID pgtype.UUID
			if err := pool.QueryRow(ctx, `SELECT http_path, source_id FROM requests WHERE id=$1`, reqID).Scan(&path, &srcID); err != nil {
				t.Fatalf("request row: %v", err)
			}
			mustEq(t, "path", path, "/__dstream_test")
			mustEq(t, "source", srcID, src.ID)
			body, err := ingest.NewPostgresBodyStore(q).Get(ctx, "pg:"+store.GoUUID(reqID).String())
			if err != nil {
				t.Fatalf("body: %v", err)
			}
			if !strings.Contains(string(body), `"connection_id":"`+cid+`"`) || !strings.Contains(string(body), `"dstream":"test.ping"`) {
				t.Fatalf("body = %s", body)
			}

			items, _, err := dq.Items(ctx, "pending", oid.String(), 50)
			if err != nil {
				t.Fatal(err)
			}
			var queued bool
			for _, it := range items {
				queued = queued || it.EventID == evID
			}
			if !queued {
				t.Fatalf("event %s not in the org's pending lane: %+v", evID, items)
			}
			mustEq(t, "audit", auditCount(t, pool, oid, "connection.test", conn.ID.Bytes), 1)
			// The target is never contacted by the handler itself.
			mustEq(t, "target hits", hits.Load(), int32(0))
		})
	}
}

type failingBodyStore struct{}

func (failingBodyStore) Put(context.Context, uuid.UUID, []byte) (string, error) {
	return "", errors.New("disk full")
}
func (failingBodyStore) Get(context.Context, string) ([]byte, error) { return nil, errors.New("n/a") }

func TestTestConnectionFailures(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedOrg(t, q)
	dropEvents(t, pool, oid)
	src, dst := newSource(t, q, oid), seedDest(t, q, oid, "https://sink.example.test/h")
	conn := seedConn(t, q, src, dst)
	path := "/api/connections/" + store.GoUUID(conn.ID).String() + "/test"
	eventCount := func() int { return rowCount(t, pool, `SELECT count(*) FROM events WHERE connection_id=$1`, conn.ID) }

	// Body store down: the request row exists but no event is created or queued.
	r := newRouter(q, func(r chi.Router, h Handlers) {
		h.BodyStore = failingBodyStore{}
		connRoutes(r, h)
	})
	wantErr(t, do(t, r, http.MethodPost, path, uid, oid, nil), 500, "store body")
	mustEq(t, "events after body failure", eventCount(), 0)

	// Redis unreachable: enqueue fails and the handler says so.
	dead := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: time.Second})
	t.Cleanup(func() { _ = dead.Close() })
	r = newRouter(q, func(r chi.Router, h Handlers) {
		h.BodyStore, h.Queue = ingest.NewPostgresBodyStore(q), dqueue.NewClient(dead)
		connRoutes(r, h)
	})
	wantErr(t, do(t, r, http.MethodPost, path, uid, oid, nil), 500, "enqueue")
	mustEq(t, "no audit for failed test", auditCount(t, pool, oid, "connection.test", conn.ID.Bytes), 0)
}

func TestDiffConnection(t *testing.T) {
	old := store.Connection{
		Enabled: true, MaxRetries: 1, RetryStrategy: "exponential",
		RetryBaseMs: 100, RetryCapMs: 1000, RetryJitterPct: 20, CustomRetrySchedule: nil,
	}
	if d := diffConnection(old, old); len(d) != 0 {
		t.Fatalf("identical rows diff = %v", d)
	}
	// nil and empty schedules are the same value.
	empty := old
	empty.CustomRetrySchedule = []byte{}
	if d := diffConnection(old, empty); len(d) != 0 {
		t.Fatalf("nil vs empty schedule diff = %v", d)
	}

	nw := old
	nw.Enabled, nw.MaxRetries, nw.RetryStrategy = false, 3, "custom"
	nw.RetryBaseMs, nw.RetryCapMs, nw.RetryJitterPct = 200, 2000, 5
	nw.CustomRetrySchedule = []byte(`[1]`)
	want := map[string]map[string]any{
		"enabled":               {"from": true, "to": false},
		"max_retries":           {"from": int32(1), "to": int32(3)},
		"retry_strategy":        {"from": "exponential", "to": "custom"},
		"retry_base_ms":         {"from": int32(100), "to": int32(200)},
		"retry_cap_ms":          {"from": int32(1000), "to": int32(2000)},
		"retry_jitter_pct":      {"from": int32(20), "to": int32(5)},
		"custom_retry_schedule": {"changed": true},
	}
	if got := diffConnection(old, nw); !reflect.DeepEqual(got, want) {
		t.Fatalf("diff = %#v\nwant   %#v", got, want)
	}
	// A schedule set to nil counts as changed.
	if got := diffConnection(nw, old); got["custom_retry_schedule"]["changed"] != true {
		t.Fatalf("clearing the schedule not reported: %v", got)
	}
}

func TestCompiledExprs(t *testing.T) {
	for _, in := range []*string{nil, pstr("")} {
		if got, err := compiledFilterExpr(in, false); got != nil || err != nil {
			t.Fatalf("filter(%v) = %v, %v; want nil, nil", in, got, err)
		}
		if got, err := compiledTransformJs(in); got != nil || err != nil {
			t.Fatalf("transform(%v) = %v, %v; want nil, nil", in, got, err)
		}
	}
	f := pstr(`payload.n > 1`)
	if got, err := compiledFilterExpr(f, false); err != nil || got != f {
		t.Fatalf("valid filter = %v, %v", got, err)
	}
	if _, err := compiledFilterExpr(pstr("payload.n >"), false); err == nil {
		t.Fatal("broken filter accepted")
	}
	j := pstr("function transform(p) { return p; }")
	if got, err := compiledTransformJs(j); err != nil || got != j {
		t.Fatalf("valid js = %v, %v", got, err)
	}
	if _, err := compiledTransformJs(pstr("function(")); err == nil {
		t.Fatal("broken js accepted")
	}
}

// Database failures that land AFTER an earlier statement succeeded: the real
// pool fails only statements containing the marker.
func TestConnectionHandlersMidFlowDatabaseFailure(t *testing.T) {
	clean := testPool(t)
	cq := store.New(clean)
	uid, oid := seedOrg(t, cq)
	dropEvents(t, clean, oid)
	src, dst := newSource(t, cq, oid), seedDest(t, cq, oid, "https://sink.example.test/h")
	conn := seedConn(t, cq, src, dst)
	cid := store.GoUUID(conn.ID).String()
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr(t)})
	t.Cleanup(func() { _ = rdb.Close() })
	prefix := "connfail-" + uuid.NewString()
	dq := dqueue.NewClient(rdb).WithPrefix(prefix)
	t.Cleanup(func() {
		if keys, err := rdb.Keys(context.Background(), prefix+":*").Result(); err == nil && len(keys) > 0 {
			rdb.Del(context.Background(), keys...)
		}
	})
	route := func(q *store.Queries) http.Handler {
		return newRouter(q, func(r chi.Router, h Handlers) {
			h.Queue, h.BodyStore = dq, ingest.NewPostgresBodyStore(q)
			connRoutes(r, h)
		})
	}
	noTraffic := func() {
		t.Helper()
		mustEq(t, "events", rowCount(t, clean, `SELECT count(*) FROM events WHERE connection_id=$1`, conn.ID), 0)
		mustEq(t, "requests", rowCount(t, clean, `SELECT count(*) FROM requests WHERE source_id=$1`, src.ID), 0)
		items, _, err := dq.Items(context.Background(), "pending", oid.String(), 50)
		if err != nil || len(items) != 0 {
			t.Fatalf("queue = %v, %v; want empty", items, err)
		}
	}

	// Ownership lookup succeeds, the stats query fails.
	r := route(failingQueries(t, "e.is_test = false"))
	wantErr(t, do(t, r, http.MethodGet, "/api/connections/"+cid+"/stats", uid, oid, nil), 500, "stats")

	r = route(failingQueries(t, "insert into requests"))
	wantErr(t, do(t, r, http.MethodPost, "/api/connections/"+cid+"/test", uid, oid, nil), 500, "create request")
	noTraffic()

	r = route(failingQueries(t, "insert into events"))
	wantErr(t, do(t, r, http.MethodPost, "/api/connections/"+cid+"/test", uid, oid, nil), 500, "create event")
	mustEq(t, "events", rowCount(t, clean, `SELECT count(*) FROM events WHERE connection_id=$1`, conn.ID), 0)
	items, _, err := dq.Items(context.Background(), "pending", oid.String(), 50)
	if err != nil || len(items) != 0 {
		t.Fatalf("queue = %v, %v; want empty", items, err)
	}
	mustEq(t, "audit", auditCount(t, clean, oid, "connection.test", conn.ID.Bytes), 0)
}
