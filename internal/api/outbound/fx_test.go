package outbound

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/store"
	"github.com/Vivekagent47/dstream/internal/webhook"
)

// tracerFn is a pgx QueryTracer driven by a function, so a test can fail one
// named statement (return a cancelled context) or run a side effect just before
// it. Every other statement runs normally against the real database.
type tracerFn func(ctx context.Context, sql string) context.Context

func (f tracerFn) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	return f(ctx, strings.ToLower(d.SQL))
}
func (tracerFn) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func cancelled(ctx context.Context) context.Context {
	c, cancel := context.WithCancel(ctx)
	cancel()
	return c
}

// failAll cancels every statement containing all of the lower-case markers.
func failAll(markers ...string) tracerFn {
	return func(ctx context.Context, sql string) context.Context {
		for _, m := range markers {
			if !strings.Contains(sql, m) {
				return ctx
			}
		}
		return cancelled(ctx)
	}
}

// failNth cancels only the nth (1-based) statement containing marker.
func failNth(marker string, n int) tracerFn {
	var mu sync.Mutex
	seen := 0
	return func(ctx context.Context, sql string) context.Context {
		if !strings.Contains(sql, marker) {
			return ctx
		}
		mu.Lock()
		defer mu.Unlock()
		seen++
		if seen == n {
			return cancelled(ctx)
		}
		return ctx
	}
}

// tracedQueries is a real pool whose statements pass through tr.
func tracedQueries(t *testing.T, tr tracerFn) *store.Queries {
	t.Helper()
	dsn := os.Getenv("DSTREAM_TEST_DB_URL")
	if dsn == "" {
		t.Skip("DSTREAM_TEST_DB_URL not set")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = 2
	cfg.ConnConfig.Tracer = tr
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return store.New(pool)
}

// NOTE: mountAll is a hand-copied router, so the 403 assertions here do not prove
// production wiring; that lives in internal/api/rbac_test.go:121-122 and
// router_matrix_test.go:272-290.
// mountAll mounts every outbound route as internal/api/router.go does. admin
// guards the routes the real router marks adminOnly.
func mountAll(r chi.Router, h Handlers, admin func(http.Handler) http.Handler) {
	r.Get("/operational-app", h.GetOperationalApp)
	r.Route("/applications", func(r chi.Router) {
		r.Get("/", h.ListApplications)
		r.Post("/", h.CreateApplication)
		r.Get("/{app_id}", h.GetApplication)
		r.Patch("/{app_id}", h.PatchApplication)
		r.Delete("/{app_id}", h.DeleteApplication)
		r.With(admin).Post("/{app_id}/portal-access", h.CreatePortalAccess)
		r.With(admin).Post("/{app_id}/portal-access/revoke", h.RevokePortalAccess)
		r.Route("/{app_id}/endpoints", func(r chi.Router) {
			r.Get("/", h.ListEndpoints)
			r.Post("/", h.CreateEndpoint)
			r.Get("/{id}", h.GetEndpoint)
			r.Patch("/{id}", h.PatchEndpoint)
			r.Delete("/{id}", h.DeleteEndpoint)
			r.With(admin).Get("/{id}/secret", h.GetEndpointSecret)
			r.With(admin).Post("/{id}/rotate-secret", h.RotateEndpointSecret)
			r.Post("/{id}/recover", h.RecoverEndpoint)
			r.Post("/{id}/test", h.TestEndpoint)
			r.Get("/{id}/attempts", h.ListEndpointAttempts)
		})
		r.Route("/{app_id}/messages", func(r chi.Router) {
			r.Get("/", h.ListMessages)
			r.With(admin).Post("/", h.CreateMessage)
			r.Get("/{id}", h.GetMessage)
			r.Get("/{id}/attempts", h.ListMessageAttempts)
			r.Get("/{id}/deliveries", h.ListMessageDeliveries)
			r.Post("/{id}/endpoints/{endpoint_id}/replay", h.ReplayDelivery)
		})
	})
	r.Route("/event-types", func(r chi.Router) {
		r.Get("/", h.ListEventTypes)
		r.Post("/", h.CreateEventType)
		r.Get("/{name}", h.GetEventType)
		r.Patch("/{name}", h.PatchEventType)
		r.Delete("/{name}", h.DeleteEventType)
	})
}

// mountPortal mirrors the /portal subtree of router.go: no app_id in any path,
// the token supplies it.
func mountPortal(r chi.Router, h Handlers) {
	r.Get("/app", h.GetApplication)
	r.Route("/endpoints", func(r chi.Router) {
		r.Get("/", h.ListEndpoints)
		r.Get("/{id}", h.GetEndpoint)
		r.Delete("/{id}", h.DeleteEndpoint)
		r.Get("/{id}/secret", h.GetEndpointSecret)
		r.Post("/{id}/rotate-secret", h.RotateEndpointSecret)
	})
	r.Route("/messages", func(r chi.Router) {
		r.Get("/", h.ListMessages)
		r.Get("/{id}", h.GetMessage)
	})
}

// fx is one org with an owner, two applications and a queue, behind the full
// router (session surface at /api, portal at /api/portal, and an unauthenticated
// copy at /bare for the "no principal" branch of every handler).
type fx struct {
	t    *testing.T
	pool *pgxpool.Pool
	q    *store.Queries // real queries, for fixtures and assertions
	rdb  *redis.Client
	dq   *dqueue.Client
	ps   *auth.PortalSigner
	logs *bytes.Buffer
	h    Handlers
	r    *chi.Mux

	uid, oid  uuid.UUID
	app, app2 store.Application
}

type fxOpt struct {
	queries *store.Queries // handler-side queries; nil = real
	mutate  func(*Handlers)
}

func newFx(t *testing.T, opts ...func(*fxOpt)) *fx {
	t.Helper()
	var o fxOpt
	for _, f := range opts {
		f(&o)
	}
	pool := testPool(t)
	rdb := testRedis(t)
	q := store.New(pool)
	prefix := "obfx-" + uuidNewShort()
	t.Cleanup(func() {
		ctx := context.Background()
		if keys, _ := rdb.Keys(ctx, prefix+":*").Result(); len(keys) > 0 {
			rdb.Del(ctx, keys...)
		}
	})
	f := &fx{
		t: t, pool: pool, q: q, rdb: rdb,
		dq:   dqueue.NewClient(rdb).WithPrefix(prefix),
		ps:   &auth.PortalSigner{Secret: []byte("test-secret-do-not-use-in-prod!!"), TTL: time.Hour},
		logs: &bytes.Buffer{},
	}
	f.uid, f.oid = seedOrg(t, q)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM message_deliveries WHERE org_id=$1`, f.oid)
	})
	hq := q
	if o.queries != nil {
		hq = o.queries
	}
	f.h = Handlers{
		Log: slog.New(slog.NewTextHandler(f.logs, nil)), Queries: hq, Queue: f.dq,
		Portal: f.ps, AppBaseURL: "https://app.example.test",
	}
	if o.mutate != nil {
		o.mutate(&f.h)
	}
	f.app = f.mkApp(f.oid, "A1")
	f.app2 = f.mkApp(f.oid, "A2")
	f.build()
	return f
}

func withQueries(q *store.Queries) func(*fxOpt) { return func(o *fxOpt) { o.queries = q } }
func withHandlers(m func(*Handlers)) func(*fxOpt) {
	return func(o *fxOpt) { o.mutate = m }
}

func (f *fx) build() {
	r := chi.NewRouter()
	admin := auth.RequireRole(auth.RoleAdmin)
	r.Route("/api", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(auth.Authenticate(f.h.Queries, sign(f.t)))
			r.Group(func(r chi.Router) {
				r.Use(auth.RequireOrg(f.h.Queries))
				mountAll(r, f.h, admin)
			})
		})
		r.Route("/portal", func(r chi.Router) {
			r.Use(auth.RequirePortal(f.h.Queries, f.ps))
			mountPortal(r, f.h)
		})
	})
	r.Route("/bare", func(r chi.Router) {
		mountAll(r, f.h, func(next http.Handler) http.Handler { return next })
	})
	f.r = r
}

func (f *fx) mkApp(org uuid.UUID, name string) store.Application {
	f.t.Helper()
	a, err := f.q.CreateApplication(context.Background(), store.CreateApplicationParams{
		OrgID: store.UUID(org), Name: name + "-" + uuidNewShort(), Metadata: []byte(`{}`),
	})
	if err != nil {
		f.t.Fatalf("seed app: %v", err)
	}
	return a
}

// mkEp seeds an endpoint straight into the store and returns the row.
func (f *fx) mkEp(app store.Application, url string, mod ...func(*store.CreateEndpointParams)) store.Endpoint {
	f.t.Helper()
	sec, err := webhook.GenerateSecret()
	if err != nil {
		f.t.Fatal(err)
	}
	p := store.CreateEndpointParams{AppID: app.ID, OrgID: app.OrgID, Url: url, Secret: sec, Headers: []byte("{}")}
	for _, m := range mod {
		m(&p)
	}
	ep, err := f.q.CreateEndpoint(context.Background(), p)
	if err != nil {
		f.t.Fatalf("seed endpoint: %v", err)
	}
	return ep
}

func (f *fx) mkMsg(app store.Application, eventType string) store.Message {
	f.t.Helper()
	m, err := f.q.CreateMessage(context.Background(), store.CreateMessageParams{
		AppID: app.ID, OrgID: app.OrgID, EventType: eventType, Payload: []byte(`{"n":1}`), PayloadHash: "h",
	})
	if err != nil {
		f.t.Fatalf("seed message: %v", err)
	}
	return m
}

func (f *fx) mkDel(msg store.Message, ep store.Endpoint) pgtype.UUID {
	f.t.Helper()
	d, err := f.q.CreateMessageDeliveriesBatch(context.Background(), store.CreateMessageDeliveriesBatchParams{
		MessageID: msg.ID, OrgID: msg.OrgID, EndpointIds: []pgtype.UUID{ep.ID},
	})
	if err != nil || len(d) != 1 {
		f.t.Fatalf("seed delivery: err=%v n=%d", err, len(d))
	}
	return d[0].ID
}

func (f *fx) mkEventType(org uuid.UUID, name string, schema []byte) store.EventType {
	f.t.Helper()
	et, err := f.q.CreateEventType(context.Background(), store.CreateEventTypeParams{
		OrgID: store.UUID(org), Name: name, Schema: schema,
	})
	if err != nil {
		f.t.Fatalf("seed event type: %v", err)
	}
	return et
}

// uniq returns a collision-free event type / uid name.
func uniq(prefix string) string { return prefix + "." + uuidNewShort() }

func (f *fx) as(uid, oid uuid.UUID, method, path string, body any) *httptest.ResponseRecorder {
	f.t.Helper()
	rec := httptest.NewRecorder()
	f.r.ServeHTTP(rec, sessionReq(f.t, sign(f.t), method, path, uid, oid, body))
	return rec
}

// own is a request as the org owner.
func (f *fx) own(method, path string, body any) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.as(f.uid, f.oid, method, path, body)
}

func (f *fx) bare(method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.r.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(`{}`)))
	return rec
}

func (f *fx) portalDo(method, path, token string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.r.ServeHTTP(rec, portalReq(method, path, token))
	return rec
}

func uid36(id pgtype.UUID) string { return store.GoUUID(id).String() }

func appPath(a store.Application) string { return "/api/applications/" + uid36(a.ID) }
func epPath(a store.Application, e store.Endpoint) string {
	return appPath(a) + "/endpoints/" + uid36(e.ID)
}

// want asserts the status and, for an error envelope, the exact message.
func want(t *testing.T, rec *httptest.ResponseRecorder, code int, msg string) {
	t.Helper()
	if rec.Code != code {
		t.Fatalf("status: got %d want %d, body=%s", rec.Code, code, rec.Body.String())
	}
	if msg == "" {
		return
	}
	var e map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e["error"] != msg {
		t.Fatalf("error message: got %q (err=%v) want %q", rec.Body.String(), err, msg)
	}
}

func obj(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode object: %v body=%s", err, rec.Body.String())
	}
	return m
}

func list(t *testing.T, rec *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	var m []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode list: %v body=%s", err, rec.Body.String())
	}
	return m
}

func (f *fx) epRow(app store.Application, id pgtype.UUID) store.Endpoint {
	f.t.Helper()
	e, err := f.q.GetEndpointForApp(context.Background(), store.GetEndpointForAppParams{ID: id, AppID: app.ID})
	if err != nil {
		f.t.Fatalf("load endpoint: %v", err)
	}
	return e
}

func (f *fx) epCount(app store.Application) int {
	f.t.Helper()
	rows, err := f.q.ListEndpointsByApp(context.Background(), store.ListEndpointsByAppParams{AppID: app.ID, Limit: 1000})
	if err != nil {
		f.t.Fatal(err)
	}
	return len(rows)
}

func (f *fx) msgCount(app store.Application) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM messages WHERE app_id=$1`, app.ID).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *fx) deliveries(msg store.Message) []store.ListDeliveriesForMessageRow {
	f.t.Helper()
	rows, err := f.q.ListDeliveriesForMessage(context.Background(), msg.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return rows
}

// closedQueue is a queue whose Redis client is closed: Enqueue always errors.
func closedQueue(t *testing.T) *dqueue.Client {
	t.Helper()
	rdb := testRedis(t)
	c := redis.NewClient(&redis.Options{Addr: rdb.Options().Addr})
	_ = c.Close()
	return dqueue.NewClient(c).WithPrefix("obclosed-" + uuidNewShort())
}

// httpDo sends a raw (non-JSON-marshalled) body as the org owner.
func httpDo(f *fx, method, path string, body *strings.Reader) *httptest.ResponseRecorder {
	f.t.Helper()
	req := sessionReq(f.t, sign(f.t), method, path, f.uid, f.oid, nil)
	req.Body = io.NopCloser(body)
	req.ContentLength = body.Size()
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.r.ServeHTTP(rec, req)
	return rec
}
