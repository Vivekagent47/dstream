package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/store"
)

var testSigner = &auth.SessionSigner{Secret: []byte("admin-test-secret-do-not-use-0000")}

// failTracer makes every statement whose SQL contains marker fail with a real
// driver error (its context is cancelled before it is sent), so one handler
// branch can be provoked without touching the others or the auth lookup.
type failTracer struct{ marker string }

func (f failTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if f.marker != "" && strings.Contains(strings.ToLower(d.SQL), strings.ToLower(f.marker)) {
		c, cancel := context.WithCancel(ctx)
		cancel()
		return c
	}
	return ctx
}
func (failTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// env is the real stack behind /admin: Postgres, Redis under a prefix unique
// to the test, and the real Mount() with SuperAdminOnly.
type env struct {
	d    Deps
	pool *pgxpool.Pool
	rdb  *redis.Client
	q    *dqueue.Client
	pfx  string
	h    http.Handler
}

func newEnv(t *testing.T) *env { return newEnvFailing(t, "") }

// newEnvFailing is newEnv with every statement containing failSQL failing.
func newEnvFailing(t *testing.T, failSQL string) *env {
	t.Helper()
	dsn := os.Getenv("DSTREAM_TEST_DB_URL")
	if dsn == "" {
		t.Skip("DSTREAM_TEST_DB_URL not set")
	}
	addr := os.Getenv("DSTREAM_REDIS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:6379"
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = 2
	cfg.ConnConfig.Tracer = failTracer{marker: failSQL}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skip("no redis")
	}
	pfx := "admtest-" + uuid.NewString()
	t.Cleanup(func() {
		if keys, _ := rdb.Keys(context.Background(), pfx+":*").Result(); len(keys) > 0 {
			rdb.Del(context.Background(), keys...)
		}
		_ = rdb.Close()
	})
	q := dqueue.NewClient(rdb).WithPrefix(pfx)
	e := &env{pool: pool, rdb: rdb, q: q, pfx: pfx}
	e.d = Deps{
		Log: discardLogger(), Queries: store.New(pool), Redis: rdb, Signer: testSigner,
		Queue: q, Pool: pool, Version: "v-test-" + uuid.NewString(),
	}
	r := chi.NewRouter()
	Mount(r, e.d)
	e.h = r
	return e
}

func (e *env) seedUser(t *testing.T, superAdmin bool) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	email := "adm+" + uuid.NewString() + "@example.test"
	u, err := e.d.Queries.CreateUser(ctx, store.CreateUserParams{Email: email})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if superAdmin {
		if err := e.d.Queries.PromoteUserToSuperAdmin(ctx, email); err != nil {
			t.Fatalf("promote: %v", err)
		}
	}
	t.Cleanup(func() { _, _ = e.pool.Exec(context.Background(), "DELETE FROM users WHERE id = $1", u.ID) })
	return store.GoUUID(u.ID)
}

func (e *env) seedOrg(t *testing.T, name string) store.CreateOrganizationRow {
	t.Helper()
	o, err := e.d.Queries.CreateOrganization(context.Background(), store.CreateOrganizationParams{
		Name: name, Slug: "adm-" + uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	t.Cleanup(func() { _ = e.d.Queries.DeleteOrganization(context.Background(), o.ID) })
	return o
}

// seedKey mints a real API key for org at role and returns the credential.
func (e *env) seedKey(t *testing.T, org store.CreateOrganizationRow, role string) string {
	t.Helper()
	full, prefix, hash, err := auth.NewAPIKey()
	if err != nil {
		t.Fatalf("new key: %v", err)
	}
	if _, err := e.d.Queries.CreateAPIKey(context.Background(), store.CreateAPIKeyParams{
		OrgID: org.ID, Name: "k", Prefix: prefix, KeyHash: hash, Role: role,
	}); err != nil {
		t.Fatalf("create key: %v", err)
	}
	return full
}

// principal is how a request authenticates: a session for user, an API key,
// or nothing.
type principal struct {
	user   uuid.UUID
	apiKey string
	cookie string // raw cookie value, for a forged session
}

func (e *env) do(t *testing.T, p principal, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rd *bytes.Reader
	switch b := body.(type) {
	case nil:
		rd = bytes.NewReader(nil)
	case string:
		rd = bytes.NewReader([]byte(b))
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rd = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, rd)
	switch {
	case p.apiKey != "":
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	case p.cookie != "":
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: p.cookie})
	case p.user != uuid.Nil:
		w := httptest.NewRecorder()
		testSigner.Issue(w, p.user, uuid.Nil, 0)
		for _, c := range w.Result().Cookies() {
			req.AddCookie(c)
		}
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func mustStatus(t *testing.T, rec *httptest.ResponseRecorder, code int, msg string) {
	t.Helper()
	if rec.Code != code {
		t.Fatalf("status: got %d want %d; body=%q", rec.Code, code, rec.Body.String())
	}
	if msg != "" && strings.TrimSpace(rec.Body.String()) != msg {
		t.Fatalf("message: got %q want %q", strings.TrimSpace(rec.Body.String()), msg)
	}
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

func (e *env) lane(t *testing.T, lane, org string) []dqueue.Item {
	t.Helper()
	items, _, err := e.q.Items(context.Background(), lane, org, 200)
	if err != nil {
		t.Fatalf("items %s: %v", lane, err)
	}
	return items
}

// enqueue puts one payload for org on the pending list and returns its id.
func (e *env) enqueue(t *testing.T, org uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := e.q.Enqueue(context.Background(), dqueue.Payload{EventID: id, OrgID: org, EnqueuedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return id
}

// pick leases the next item and returns its raw member (now in processing).
func (e *env) pick(t *testing.T) string {
	t.Helper()
	raw, _, ok, err := e.q.FairPick(context.Background(), 60000)
	if err != nil || !ok {
		t.Fatalf("fairpick ok=%v err=%v", ok, err)
	}
	return raw
}

func (e *env) dead(t *testing.T, org uuid.UUID) string {
	t.Helper()
	id := e.enqueue(t, org)
	raw := e.pick(t) // tokened lease handle: DeadLetter needs it to clear processing
	if err := e.q.DeadLetter(context.Background(), raw); err != nil {
		t.Fatalf("dead letter: %v", err)
	}
	// The dead lane holds the plain payload; that is the admin-facing identity.
	for _, it := range e.lane(t, "dead", "") {
		if it.EventID == id {
			return it.Raw
		}
	}
	t.Fatalf("dead item not found")
	return ""
}

func (e *env) scheduled(t *testing.T, org uuid.UUID) string {
	t.Helper()
	p := dqueue.Payload{EventID: uuid.New(), OrgID: org, Attempt: 2, EnqueuedAt: time.Now().UnixMilli()}
	if err := e.q.Schedule(context.Background(), p, time.Now().Add(time.Hour).UnixMilli()); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	items := e.lane(t, "scheduled", "")
	for _, it := range items {
		if it.EventID == p.EventID {
			return it.Raw
		}
	}
	t.Fatalf("scheduled item not found")
	return ""
}

// seedTraffic builds source -> connection -> destination in org plus one
// request carrying n events, of which the first failed are status 'failed'.
func (e *env) seedTraffic(t *testing.T, org store.CreateOrganizationRow, n, failed int) (srcName, dstName string, dstID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	qs := e.d.Queries
	srcName, dstName = "src-"+uuid.NewString(), "dst-"+uuid.NewString()
	src, err := qs.CreateSource(ctx, store.CreateSourceParams{
		OrgID: org.ID, Name: srcName, Type: "generic", IngestToken: "tok-" + uuid.NewString(), SigningConfig: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	url := "http://127.0.0.1:1/x"
	dst, err := qs.CreateDestination(ctx, store.CreateDestinationParams{
		OrgID: org.ID, Name: dstName, Type: "http", Url: &url, AuthConfig: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("destination: %v", err)
	}
	conn, err := qs.CreateConnection(ctx, store.CreateConnectionParams{SourceID: src.ID, DestinationID: dst.ID, Enabled: true})
	if err != nil {
		t.Fatalf("connection: %v", err)
	}
	reqID := uuid.New()
	ct := "application/json"
	if _, err := qs.CreateRequest(ctx, store.CreateRequestParams{
		ID: store.UUID(reqID), SourceID: src.ID, HTTPMethod: "POST", HTTPPath: "/e/x", Headers: []byte("{}"),
		BodyHash: "h-" + uuid.NewString(), BodyRef: "pg:" + reqID.String(), ContentType: &ct,
	}); err != nil {
		t.Fatalf("request: %v", err)
	}
	conns := make([]pgtype.UUID, n)
	for i := range conns {
		conns[i] = conn.ID
	}
	evs, err := qs.CreateEventsBatch(ctx, store.CreateEventsBatchParams{
		RequestID: store.UUID(reqID), OrgID: org.ID, ConnectionIds: conns,
	})
	if err != nil || len(evs) != n {
		t.Fatalf("events: n=%d err=%v", len(evs), err)
	}
	for _, ev := range evs[:failed] {
		if _, err := e.pool.Exec(ctx, "UPDATE events SET status='failed' WHERE id=$1", ev.ID); err != nil {
			t.Fatalf("fail event: %v", err)
		}
	}
	return srcName, dstName, store.GoUUID(dst.ID)
}

func (e *env) eventStatus(t *testing.T, id uuid.UUID) string {
	t.Helper()
	var s string
	if err := e.pool.QueryRow(context.Background(), "SELECT status FROM events WHERE id=$1", id).Scan(&s); err != nil {
		t.Fatalf("event status: %v", err)
	}
	return s
}

// ---------------------------------------------------------------------------
// The property /admin rests on: only a super-admin SESSION gets in.
// ---------------------------------------------------------------------------

type adminRoute struct {
	method, path string
	body         any
}

func (e *env) allRoutes(orgID, rawDead, rawSched string) []adminRoute {
	return []adminRoute{
		{http.MethodGet, "/admin/queues", nil},
		{http.MethodGet, "/admin/queues/items?lane=dead", nil},
		{http.MethodGet, "/admin/queues/orgs", nil},
		{http.MethodPost, "/admin/queues/dead/requeue", map[string]string{"raw": rawDead}},
		{http.MethodPost, "/admin/queues/scheduled/promote", map[string]string{"raw": rawSched}},
		{http.MethodPost, "/admin/queues/dead/drain", nil},
		{http.MethodGet, "/admin/usage", nil},
		{http.MethodGet, "/admin/plans", nil},
		{http.MethodGet, "/admin/overview", nil},
		{http.MethodGet, "/admin/orgs", nil},
		{http.MethodGet, "/admin/destinations/hot", nil},
		{http.MethodGet, "/admin/system", nil},
		{http.MethodPatch, "/admin/orgs/" + orgID + "/plan", map[string]string{"plan": "enterprise"}},
	}
}

func TestAdminRoutes_RefuseEveryoneButSuperAdminSession(t *testing.T) {
	e := newEnv(t)
	org := e.seedOrg(t, "Refuse Org")
	orgID := store.GoUUID(org.ID)
	rawDead := e.dead(t, orgID)
	rawSched := e.scheduled(t, orgID)
	member := e.seedUser(t, false)
	adminKey := e.seedKey(t, org, string(auth.RoleAdmin))
	memberKey := e.seedKey(t, org, string(auth.RoleMember))

	// Positive control: both keys are genuine, i.e. the real API-key middleware
	// accepts them. Only then does a 401 from /admin mean "valid key, refused".
	ctl := chi.NewRouter()
	ctl.Use(auth.Authenticate(e.d.Queries, testSigner))
	ctl.Get("/ok", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	for name, key := range map[string]string{"admin": adminKey, "member": memberKey} {
		req := httptest.NewRequest(http.MethodGet, "/ok", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		ctl.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s key is not accepted by Authenticate (%d): the refusal below would prove nothing", name, rec.Code)
		}
	}

	cases := []struct {
		name string
		p    principal
		code int
		msg  string
	}{
		{"no credentials", principal{}, http.StatusUnauthorized, "unauthorized"},
		{"admin-role api key", principal{apiKey: adminKey}, http.StatusUnauthorized, "unauthorized"},
		{"member-role api key", principal{apiKey: memberKey}, http.StatusUnauthorized, "unauthorized"},
		{"forged session cookie", principal{cookie: "not-a-session"}, http.StatusUnauthorized, "unauthorized"},
		{"non-super-admin session", principal{user: member}, http.StatusForbidden, "forbidden"},
	}
	for _, rt := range e.allRoutes(orgID.String(), rawDead, rawSched) {
		for _, c := range cases {
			t.Run(rt.method+" "+rt.path+" / "+c.name, func(t *testing.T) {
				mustStatus(t, e.do(t, c.p, rt.method, rt.path, rt.body), c.code, c.msg)
			})
		}
	}

	// Nothing moved: the refused mutations did not run.
	if got := e.lane(t, "dead", ""); len(got) != 1 || got[0].Raw != rawDead {
		t.Errorf("dead lane changed by a refused request: %+v", got)
	}
	if got := e.lane(t, "scheduled", ""); len(got) != 1 || got[0].Raw != rawSched {
		t.Errorf("scheduled lane changed by a refused request: %+v", got)
	}
	if got := e.lane(t, "pending", orgID.String()); len(got) != 0 {
		t.Errorf("pending lane gained items from a refused request: %+v", got)
	}
	q, err := e.d.Queries.GetOrgQuota(context.Background(), org.ID)
	if err != nil {
		t.Fatalf("get quota: %v", err)
	}
	if q.Plan == "enterprise" {
		t.Errorf("plan changed by a refused PATCH")
	}
}

func TestAdminRoutes_SuperAdminSessionReachesEveryRoute(t *testing.T) {
	e := newEnv(t)
	org := e.seedOrg(t, "Reach Org")
	orgID := store.GoUUID(org.ID)
	rawDead := e.dead(t, orgID)
	e.dead(t, orgID) // second dead item, for drain to remove
	rawSched := e.scheduled(t, orgID)
	su := principal{user: e.seedUser(t, true)}

	for _, rt := range e.allRoutes(orgID.String(), rawDead, rawSched) {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			rec := e.do(t, su, rt.method, rt.path, rt.body)
			mustStatus(t, rec, http.StatusOK, "")
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("content-type: %q", ct)
			}
			switch rt.path {
			case "/admin/queues":
				if s := decode[dqueue.Stats](t, rec); s.Dead != 2 || s.Scheduled != 1 {
					t.Errorf("stats: %+v", s)
				}
			case "/admin/queues/items?lane=dead":
				if _, ok := decode[map[string]any](t, rec)["items"]; !ok {
					t.Errorf("no items key: %s", rec.Body.String())
				}
			case "/admin/queues/orgs", "/admin/usage", "/admin/orgs", "/admin/destinations/hot":
				if decode[[]any](t, rec) == nil {
					t.Errorf("want a JSON array, got %s", rec.Body.String())
				}
			case "/admin/overview":
				if _, ok := decode[map[string]any](t, rec)["top_sources"]; !ok {
					t.Errorf("no top_sources key: %s", rec.Body.String())
				}
			case "/admin/system":
				if _, ok := decode[map[string]any](t, rec)["redis_info"]; !ok {
					t.Errorf("no redis_info key: %s", rec.Body.String())
				}
			case "/admin/queues/dead/requeue":
				if !decode[map[string]bool](t, rec)["requeued"] {
					t.Errorf("requeued false")
				}
				if got := e.lane(t, "pending", orgID.String()); len(got) != 1 || got[0].Attempt != 0 {
					t.Errorf("pending after requeue: %+v", got)
				}
			case "/admin/queues/scheduled/promote":
				if !decode[map[string]bool](t, rec)["promoted"] {
					t.Errorf("promoted false")
				}
				if got := e.lane(t, "scheduled", ""); len(got) != 0 {
					t.Errorf("scheduled not emptied: %+v", got)
				}
			case "/admin/queues/dead/drain":
				if n := decode[map[string]int64](t, rec)["drained"]; n != 1 {
					t.Errorf("drained %d want 1 (the other dead item was requeued)", n)
				}
			case "/admin/plans":
				if plans := decode[[]planView](t, rec); len(plans) != 4 || plans[3].Plan != "custom" {
					t.Errorf("plans: %+v", plans)
				}
			case "/admin/orgs/" + orgID.String() + "/plan":
				if decode[map[string]any](t, rec)["plan"] != "enterprise" {
					t.Errorf("plan body: %s", rec.Body.String())
				}
				got, _ := e.d.Queries.GetOrgQuota(context.Background(), org.ID)
				if got.Plan != "enterprise" {
					t.Errorf("stored plan %q", got.Plan)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Queue endpoints.
// ---------------------------------------------------------------------------

func TestQueues_EmptyQueue(t *testing.T) {
	e := newEnv(t)
	su := principal{user: e.seedUser(t, true)}

	rec := e.do(t, su, http.MethodGet, "/admin/queues", nil)
	mustStatus(t, rec, http.StatusOK, "")
	raw := decode[map[string]any](t, rec)
	for _, k := range []string{"pending", "scheduled", "processing", "dead"} {
		if v, ok := raw[k]; !ok || v != float64(0) {
			t.Errorf("stats[%s]=%v (present=%v) want 0", k, v, ok)
		}
	}
	// dqueue.Stats guarantees a non-nil TopOrgs, so an empty queue is [].
	if top, ok := raw["top_orgs"].([]any); !ok || len(top) != 0 {
		t.Errorf("top_orgs=%#v want an empty JSON array", raw["top_orgs"])
	}
	for _, lane := range []string{"dead", "scheduled", "processing"} {
		rec = e.do(t, su, http.MethodGet, "/admin/queues/items?lane="+lane, nil)
		mustStatus(t, rec, http.StatusOK, "")
		got := decode[map[string]any](t, rec)
		if items, ok := got["items"].([]any); !ok || len(items) != 0 || got["truncated"] != false {
			t.Errorf("%s: %v", lane, got)
		}
	}
	rec = e.do(t, su, http.MethodGet, "/admin/queues/items?lane=pending&org="+uuid.NewString(), nil)
	mustStatus(t, rec, http.StatusOK, "")
	if got := decode[map[string]any](t, rec); got["truncated"] != false {
		t.Errorf("pending: %v", got)
	} else if items, ok := got["items"].([]any); !ok || len(items) != 0 {
		t.Errorf("pending items: %v", got)
	}
	rec = e.do(t, su, http.MethodGet, "/admin/queues/orgs", nil)
	mustStatus(t, rec, http.StatusOK, "")
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("orgs on an empty queue: %q", rec.Body.String())
	}
	rec = e.do(t, su, http.MethodPost, "/admin/queues/dead/drain", nil)
	mustStatus(t, rec, http.StatusOK, "")
	if n := decode[map[string]int64](t, rec)["drained"]; n != 0 {
		t.Errorf("drained %d from an empty dead lane", n)
	}
}

func TestQueues_PopulatedStatsItemsAndOrgs(t *testing.T) {
	e := newEnv(t)
	su := principal{user: e.seedUser(t, true)}
	orgA, orgB := e.seedOrg(t, "Queue Org A"), e.seedOrg(t, "Queue Org B")
	a, b := store.GoUUID(orgA.ID), store.GoUUID(orgB.ID)
	ghost := uuid.New() // pending depth for an org the database has never heard of

	deadRaw := e.dead(t, ghost)
	procID := e.enqueue(t, ghost)
	e.pick(t)           // lease only; Items reports the plain member, compared below
	e.enqueue(t, ghost) // and one still pending
	schedRaw := e.scheduled(t, ghost)
	for i := 0; i < 3; i++ {
		e.enqueue(t, a)
	}
	e.enqueue(t, b)
	e.enqueue(t, b)
	// A pending key whose org segment is not a UUID: the name lookup is skipped.
	if err := e.rdb.RPush(context.Background(), e.pfx+":pending:not-a-uuid", "x").Err(); err != nil {
		t.Fatalf("rpush: %v", err)
	}

	rec := e.do(t, su, http.MethodGet, "/admin/queues", nil)
	mustStatus(t, rec, http.StatusOK, "")
	s := decode[dqueue.Stats](t, rec)
	if s.Pending != 7 || s.Scheduled != 1 || s.Processing != 1 || s.Dead != 1 {
		t.Errorf("stats: %+v", s)
	}
	if len(s.TopOrgs) != 4 || s.TopOrgs[0].OrgID != a.String() || s.TopOrgs[0].Pending != 3 || s.TopOrgs[1].OrgID != b.String() {
		t.Errorf("top orgs: %+v", s.TopOrgs)
	}

	itemsFor := func(q string) (items []dqueue.Item, truncated bool) {
		rec := e.do(t, su, http.MethodGet, "/admin/queues/items?"+q, nil)
		mustStatus(t, rec, http.StatusOK, "")
		got := decode[struct {
			Items     []dqueue.Item `json:"items"`
			Truncated bool          `json:"truncated"`
		}](t, rec)
		return got.Items, got.Truncated
	}
	if it, tr := itemsFor("lane=dead"); len(it) != 1 || it[0].Raw != deadRaw || it[0].OrgID != ghost || tr {
		t.Errorf("dead items: %+v truncated=%v", it, tr)
	}
	if it, _ := itemsFor("lane=processing"); len(it) != 1 || it[0].EventID != procID {
		t.Errorf("processing items: %+v", it)
	}
	if it, _ := itemsFor("lane=scheduled"); len(it) != 1 || it[0].Raw != schedRaw || it[0].Attempt != 2 {
		t.Errorf("scheduled items: %+v", it)
	}
	if it, tr := itemsFor("lane=pending&org=" + a.String()); len(it) != 3 || tr {
		t.Errorf("pending items: %d truncated=%v", len(it), tr)
	}
	if it, tr := itemsFor("lane=pending&org=" + a.String() + "&limit=2"); len(it) != 2 || !tr {
		t.Errorf("limit=2: %d truncated=%v want 2/true", len(it), tr)
	}
	// An unparsable limit falls back to the default rather than failing.
	if it, tr := itemsFor("lane=pending&org=" + a.String() + "&limit=lots"); len(it) != 3 || tr {
		t.Errorf("limit=lots: %d truncated=%v want 3/false", len(it), tr)
	}

	rec = e.do(t, su, http.MethodGet, "/admin/queues/orgs", nil)
	mustStatus(t, rec, http.StatusOK, "")
	rows := decode[[]struct {
		OrgID   string `json:"org_id"`
		OrgName string `json:"org_name"`
		Pending int64  `json:"pending"`
	}](t, rec)
	byOrg := map[string]string{}
	pending := map[string]int64{}
	for i, r := range rows {
		byOrg[r.OrgID], pending[r.OrgID] = r.OrgName, r.Pending
		if i > 0 && r.Pending > rows[i-1].Pending {
			t.Errorf("rows not sorted by pending desc: %+v", rows)
		}
	}
	want := map[string]string{a.String(): "Queue Org A", b.String(): "Queue Org B", ghost.String(): "", "not-a-uuid": ""}
	if len(rows) != len(want) {
		t.Fatalf("rows: %+v", rows)
	}
	for id, name := range want {
		if got, ok := byOrg[id]; !ok || got != name {
			t.Errorf("org %s: name %q (present=%v) want %q", id, got, ok, name)
		}
	}
	if pending[a.String()] != 3 || pending[b.String()] != 2 {
		t.Errorf("pending: %v", pending)
	}
}

func TestQueueItems_RejectsBadQuery(t *testing.T) {
	e := newEnv(t)
	su := principal{user: e.seedUser(t, true)}
	mustStatus(t, e.do(t, su, http.MethodGet, "/admin/queues/items", nil), http.StatusBadRequest, "unknown lane")
	mustStatus(t, e.do(t, su, http.MethodGet, "/admin/queues/items?lane=bogus", nil), http.StatusBadRequest, "unknown lane")
	mustStatus(t, e.do(t, su, http.MethodGet, "/admin/queues/items?lane=pending", nil), http.StatusBadRequest, "pending lane requires org")
}

func TestRequeueDead_FlipsEventRowAndMovesItem(t *testing.T) {
	e := newEnv(t)
	su := principal{user: e.seedUser(t, true)}
	org := e.seedOrg(t, "Requeue Org")
	orgID := store.GoUUID(org.ID)
	_, _, _ = e.seedTraffic(t, org, 1, 1)
	var evID uuid.UUID
	if err := e.pool.QueryRow(context.Background(), "SELECT id FROM events WHERE org_id=$1", org.ID).Scan(&evID); err != nil {
		t.Fatalf("event: %v", err)
	}
	if got := e.eventStatus(t, evID); got != "failed" {
		t.Fatalf("seed status %q", got)
	}
	raw := marshalPayload(t, dqueue.Payload{EventID: evID, OrgID: orgID, Attempt: 5, EnqueuedAt: 1})
	if _, err := e.rdb.RPush(context.Background(), e.pfx+":dead", raw).Result(); err != nil {
		t.Fatalf("seed dead: %v", err)
	}

	rec := e.do(t, su, http.MethodPost, "/admin/queues/dead/requeue", map[string]string{"raw": raw})
	mustStatus(t, rec, http.StatusOK, "")
	if !decode[map[string]bool](t, rec)["requeued"] {
		t.Fatalf("requeued false: %s", rec.Body.String())
	}
	if got := e.eventStatus(t, evID); got != "queued" {
		t.Errorf("event status after requeue: %q want queued", got)
	}
	if got := e.lane(t, "dead", ""); len(got) != 0 {
		t.Errorf("dead lane not emptied: %+v", got)
	}
	if got := e.lane(t, "pending", orgID.String()); len(got) != 1 || got[0].EventID != evID || got[0].Attempt != 0 {
		t.Errorf("pending after requeue: %+v (attempt must reset to 0)", got)
	}

	// Same raw again: it is no longer dead, so nothing moves and nothing is claimed.
	rec = e.do(t, su, http.MethodPost, "/admin/queues/dead/requeue", map[string]string{"raw": raw})
	mustStatus(t, rec, http.StatusOK, "")
	if decode[map[string]bool](t, rec)["requeued"] {
		t.Errorf("requeued true for an item that was not dead")
	}
	if got := e.lane(t, "pending", orgID.String()); len(got) != 1 {
		t.Errorf("a no-op requeue enqueued a duplicate: %+v", got)
	}
}

func marshalPayload(t *testing.T, p dqueue.Payload) string {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return string(b)
}

func TestQueueMutations_RejectBadBodies(t *testing.T) {
	e := newEnv(t)
	su := principal{user: e.seedUser(t, true)}
	org := store.GoUUID(e.seedOrg(t, "Bad Body Org").ID)
	deadRaw := e.dead(t, org)
	schedRaw := e.scheduled(t, org)

	for _, path := range []string{"/admin/queues/dead/requeue", "/admin/queues/scheduled/promote"} {
		mustStatus(t, e.do(t, su, http.MethodPost, path, "{not json"), http.StatusBadRequest, "raw required")
		mustStatus(t, e.do(t, su, http.MethodPost, path, `{}`), http.StatusBadRequest, "raw required")
		mustStatus(t, e.do(t, su, http.MethodPost, path, `{"raw":""}`), http.StatusBadRequest, "raw required")
	}
	// Non-JSON raw reaches the queue, which cannot decode it.
	mustStatus(t, e.do(t, su, http.MethodPost, "/admin/queues/dead/requeue", map[string]string{"raw": "garbage"}), http.StatusInternalServerError, "requeue")
	mustStatus(t, e.do(t, su, http.MethodPost, "/admin/queues/scheduled/promote", map[string]string{"raw": "garbage"}), http.StatusInternalServerError, "promote")

	if got := e.lane(t, "dead", ""); len(got) != 1 || got[0].Raw != deadRaw {
		t.Errorf("dead lane changed: %+v", got)
	}
	if got := e.lane(t, "scheduled", ""); len(got) != 1 || got[0].Raw != schedRaw {
		t.Errorf("scheduled lane changed: %+v", got)
	}
	if got := e.lane(t, "pending", org.String()); len(got) != 0 {
		t.Errorf("pending gained items: %+v", got)
	}
}

func TestPromoteScheduled_MovesItemToPending(t *testing.T) {
	e := newEnv(t)
	su := principal{user: e.seedUser(t, true)}
	org := uuid.New()
	raw := e.scheduled(t, org)

	rec := e.do(t, su, http.MethodPost, "/admin/queues/scheduled/promote", map[string]string{"raw": raw})
	mustStatus(t, rec, http.StatusOK, "")
	if !decode[map[string]bool](t, rec)["promoted"] {
		t.Fatalf("promoted false")
	}
	if got := e.lane(t, "scheduled", ""); len(got) != 0 {
		t.Errorf("scheduled not emptied: %+v", got)
	}
	if got := e.lane(t, "pending", org.String()); len(got) != 1 || got[0].Raw != raw || got[0].Attempt != 2 {
		t.Errorf("pending: %+v (attempt must be unchanged)", got)
	}

	// Already promoted: reported as not promoted, nothing duplicated.
	rec = e.do(t, su, http.MethodPost, "/admin/queues/scheduled/promote", map[string]string{"raw": raw})
	mustStatus(t, rec, http.StatusOK, "")
	if decode[map[string]bool](t, rec)["promoted"] {
		t.Errorf("promoted true for an item that was not scheduled")
	}
	if got := e.lane(t, "pending", org.String()); len(got) != 1 {
		t.Errorf("duplicate after no-op promote: %+v", got)
	}
}

func TestDrainDead_RemovesEveryDeadItemOnly(t *testing.T) {
	e := newEnv(t)
	su := principal{user: e.seedUser(t, true)}
	org := uuid.New()
	e.dead(t, org)
	e.dead(t, org)
	e.dead(t, org)
	e.enqueue(t, org)
	e.scheduled(t, org)

	rec := e.do(t, su, http.MethodPost, "/admin/queues/dead/drain", nil)
	mustStatus(t, rec, http.StatusOK, "")
	if n := decode[map[string]int64](t, rec)["drained"]; n != 3 {
		t.Errorf("drained %d want 3", n)
	}
	if got := e.lane(t, "dead", ""); len(got) != 0 {
		t.Errorf("dead not emptied: %+v", got)
	}
	if len(e.lane(t, "pending", org.String())) != 1 || len(e.lane(t, "scheduled", "")) != 1 {
		t.Errorf("drain touched a lane other than dead")
	}
}

// With Redis gone every queue endpoint reports its own 500 and changes nothing.
func TestQueueEndpoints_RedisFailureIs500(t *testing.T) {
	e := newEnv(t)
	su := principal{user: e.seedUser(t, true)}
	raw := marshalPayload(t, dqueue.Payload{EventID: uuid.New(), OrgID: uuid.New()})
	// Close the client the handlers use, after seeding is done (Cleanup uses the
	// same client, so redial for it).
	if err := e.rdb.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	cases := []struct {
		method, path string
		body         any
		msg          string
	}{
		{http.MethodGet, "/admin/queues", nil, "queues"},
		{http.MethodGet, "/admin/queues/items?lane=dead", nil, "items"},
		{http.MethodGet, "/admin/queues/orgs", nil, "orgs"},
		{http.MethodPost, "/admin/queues/dead/requeue", map[string]string{"raw": raw}, "requeue"},
		{http.MethodPost, "/admin/queues/scheduled/promote", map[string]string{"raw": raw}, "promote"},
		{http.MethodPost, "/admin/queues/dead/drain", nil, "drain"},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			mustStatus(t, e.do(t, su, c.method, c.path, c.body), http.StatusInternalServerError, c.msg)
		})
	}
}

// ---------------------------------------------------------------------------
// Database-backed views.
// ---------------------------------------------------------------------------

func TestUsage_OrgWithNoRollupRowsReadsZero(t *testing.T) {
	e := newEnv(t)
	su := principal{user: e.seedUser(t, true)}
	org := e.seedOrg(t, "Quiet Org")

	rec := e.do(t, su, http.MethodGet, "/admin/usage", nil)
	mustStatus(t, rec, http.StatusOK, "")
	rows := decode[[]map[string]any](t, rec)
	var found map[string]any
	for _, r := range rows {
		if r["org_id"] == store.GoUUID(org.ID).String() {
			found = r
		}
	}
	if found == nil {
		t.Fatalf("org missing from %d rows", len(rows))
	}
	if found["org_name"] != "Quiet Org" || found["period"] != "month" || found["partial"] != true {
		t.Errorf("row: %v", found)
	}
	u, _ := found["usage"].(map[string]any)
	for _, m := range []string{"requests", "events", "messages", "attempts"} {
		if u[m] != float64(0) {
			t.Errorf("usage[%s]=%v want 0", m, u[m])
		}
	}
}

func TestUsage_FailedRollupLookupSkipsOrgNotPage(t *testing.T) {
	e := newEnvFailing(t, "-- name: getUsageForPeriod ")
	su := principal{user: e.seedUser(t, true)}
	e.seedOrg(t, "Skipped Org")
	rec := e.do(t, su, http.MethodGet, "/admin/usage", nil)
	mustStatus(t, rec, http.StatusOK, "")
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("every lookup fails, so no org row may be emitted: %q", rec.Body.String())
	}
}

func TestOrgsList_ContainsSeededOrg(t *testing.T) {
	e := newEnv(t)
	su := principal{user: e.seedUser(t, true)}
	org := e.seedOrg(t, "Listed Org")

	rec := e.do(t, su, http.MethodGet, "/admin/orgs", nil)
	mustStatus(t, rec, http.StatusOK, "")
	rows := decode[[]map[string]any](t, rec)
	if len(rows) == 0 || len(rows) > 200 {
		t.Fatalf("rows: %d", len(rows))
	}
	for _, r := range rows {
		if r["id"] == store.GoUUID(org.ID).String() {
			if r["name"] != "Listed Org" || r["slug"] != org.Slug {
				t.Errorf("row: %v", r)
			}
			ts, err := time.Parse(time.RFC3339Nano, r["created_at"].(string))
			if err != nil || time.Since(ts) > time.Hour {
				t.Errorf("created_at %v: %v", r["created_at"], err)
			}
			return
		}
	}
	t.Errorf("seeded org not listed (newest-first, 200 cap)")
}

type overviewBody struct {
	Organizations int64   `json:"organizations"`
	Users         int64   `json:"users"`
	Events24h     int64   `json:"events_24h"`
	PerMin        float64 `json:"events_per_min"`
	TopSources    []struct {
		Name   string `json:"source_name"`
		ID     string `json:"source_id"`
		Events int64  `json:"events"`
	} `json:"top_sources"`
}

func TestOverview_CountsSeededTraffic(t *testing.T) {
	e := newEnv(t)
	su := principal{user: e.seedUser(t, true)}

	rec := e.do(t, su, http.MethodGet, "/admin/overview", nil)
	mustStatus(t, rec, http.StatusOK, "")
	base := decode[overviewBody](t, rec)
	// Seed one more event than the current leader (orphans from killed runs
	// included) so the ranking has no tie, floor 40.
	n := 40
	if len(base.TopSources) > 0 && int(base.TopSources[0].Events)+1 > n {
		n = int(base.TopSources[0].Events) + 1
	}
	org := e.seedOrg(t, "Overview Org")
	srcName, _, _ := e.seedTraffic(t, org, n, 0)

	rec = e.do(t, su, http.MethodGet, "/admin/overview", nil)
	mustStatus(t, rec, http.StatusOK, "")
	got := decode[overviewBody](t, rec)
	// Other packages only add rows concurrently, so these are lower bounds.
	if got.Organizations < base.Organizations+1 {
		t.Errorf("organizations %d -> %d, want a rise of at least 1", base.Organizations, got.Organizations)
	}
	if got.Events24h < base.Events24h+int64(n) {
		t.Errorf("events_24h %d -> %d, want a rise of at least %d", base.Events24h, got.Events24h, n)
	}
	if want := float64(got.Events24h) / 1440.0; got.PerMin != want {
		t.Errorf("events_per_min=%v want %v", got.PerMin, want)
	}
	if len(got.TopSources) == 0 || len(got.TopSources) > 5 {
		t.Fatalf("top_sources: %+v", got.TopSources)
	}
	for i := 1; i < len(got.TopSources); i++ {
		if got.TopSources[i].Events > got.TopSources[i-1].Events {
			t.Errorf("top_sources not sorted by events desc")
		}
	}
	top := got.TopSources[0]
	if top.Name != srcName || top.Events != int64(n) {
		t.Errorf("top source %q with %d events, want %q with %d", top.Name, top.Events, srcName, n)
	}
	if _, err := uuid.Parse(top.ID); err != nil {
		t.Errorf("source_id %q", top.ID)
	}
}

func TestHotDestinations_ReportsFailureRate(t *testing.T) {
	e := newEnv(t)
	su := principal{user: e.seedUser(t, true)}
	type hotRow struct {
		ID     string  `json:"destination_id"`
		Name   string  `json:"destination_name"`
		Total  int64   `json:"total"`
		Failed int64   `json:"failed"`
		Rate   float64 `json:"failure_rate"`
	}
	rec := e.do(t, su, http.MethodGet, "/admin/destinations/hot", nil)
	mustStatus(t, rec, http.StatusOK, "")
	// One more failure than the current worst (orphans included), floor 30,
	// so the destination ranks first with no tie.
	failed := 30
	if cur := decode[[]hotRow](t, rec); len(cur) > 0 && int(cur[0].Failed)+1 > failed {
		failed = int(cur[0].Failed) + 1
	}
	total := failed + 10
	org := e.seedOrg(t, "Hot Org")
	_, dstName, dstID := e.seedTraffic(t, org, total, failed)

	rec = e.do(t, su, http.MethodGet, "/admin/destinations/hot", nil)
	mustStatus(t, rec, http.StatusOK, "")
	rows := decode[[]hotRow](t, rec)
	if len(rows) == 0 || len(rows) > 20 {
		t.Fatalf("rows: %d, cap is 20", len(rows))
	}
	r := rows[0]
	if r.ID != dstID.String() || r.Name != dstName || r.Total != int64(total) || r.Failed != int64(failed) ||
		r.Rate != float64(failed)/float64(total) {
		t.Errorf("first row %+v, want %s %q total=%d failed=%d", r, dstID, dstName, total, failed)
	}
}

func TestSystem_ReportsVersionPoolAndRedis(t *testing.T) {
	e := newEnv(t)
	su := principal{user: e.seedUser(t, true)}
	rec := e.do(t, su, http.MethodGet, "/admin/system", nil)
	mustStatus(t, rec, http.StatusOK, "")
	got := decode[struct {
		Version  string `json:"version"`
		Postgres struct {
			Max int32 `json:"max_conns"`
			Tot int32 `json:"total_conns"`
		} `json:"postgres"`
		Redis string `json:"redis_info"`
		Queue string `json:"queue_deliveries_name"`
	}](t, rec)
	if got.Version != e.d.Version || got.Queue != "deliveries" {
		t.Errorf("body: %+v", got)
	}
	if got.Postgres.Max != 2 || got.Postgres.Tot < 1 {
		t.Errorf("pool stats: %+v", got.Postgres)
	}
	if !strings.Contains(got.Redis, "redis_version") {
		t.Errorf("redis_info lacks the server section: %.80q", got.Redis)
	}
}

func TestSystem_RedisDownStillAnswersWithTheError(t *testing.T) {
	e := newEnv(t)
	su := principal{user: e.seedUser(t, true)}
	if err := e.rdb.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	rec := e.do(t, su, http.MethodGet, "/admin/system", nil)
	mustStatus(t, rec, http.StatusOK, "")
	if info := decode[map[string]any](t, rec)["redis_info"].(string); !strings.Contains(info, "closed") {
		t.Errorf("redis_info should carry the redis error, got %q", info)
	}
}

// Each database failure inside a handler is its own 500 with its own message.
func TestAdminHandlers_DatabaseFailureIs500(t *testing.T) {
	cases := []struct {
		name, failSQL, method, path string
		body                        any
		msg                         string
	}{
		{"overview count orgs", "-- name: countOrganizations ", http.MethodGet, "/admin/overview", nil, "overview"},
		{"overview count users", "-- name: countUsers ", http.MethodGet, "/admin/overview", nil, "overview"},
		{"overview events since", "-- name: adminEventsSince ", http.MethodGet, "/admin/overview", nil, "overview"},
		{"overview top sources", "-- name: adminTopSources ", http.MethodGet, "/admin/overview", nil, "overview"},
		{"orgs list", "-- name: listAllOrganizations ", http.MethodGet, "/admin/orgs", nil, "orgs"},
		{"usage quotas", "-- name: listOrgQuotas ", http.MethodGet, "/admin/usage", nil, "usage"},
		{"hot destinations", "-- name: hotDestinations ", http.MethodGet, "/admin/destinations/hot", nil, "hot destinations"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnvFailing(t, c.failSQL)
			su := principal{user: e.seedUser(t, true)}
			mustStatus(t, e.do(t, su, c.method, c.path, c.body), http.StatusInternalServerError, c.msg)
		})
	}
}

func TestRequeueDead_EventRowUpdateFailureDoesNotUndoRequeue(t *testing.T) {
	e := newEnvFailing(t, "-- name: markEventQueued ")
	su := principal{user: e.seedUser(t, true)}
	org := e.seedOrg(t, "Requeue Fail Org")
	e.seedTraffic(t, org, 1, 1)
	var evID uuid.UUID
	if err := e.pool.QueryRow(context.Background(), "SELECT id FROM events WHERE org_id=$1", org.ID).Scan(&evID); err != nil {
		t.Fatalf("event: %v", err)
	}
	raw := marshalPayload(t, dqueue.Payload{EventID: evID, OrgID: store.GoUUID(org.ID), Attempt: 3, EnqueuedAt: 1})
	if err := e.rdb.RPush(context.Background(), e.pfx+":dead", raw).Err(); err != nil {
		t.Fatalf("seed dead: %v", err)
	}
	rec := e.do(t, su, http.MethodPost, "/admin/queues/dead/requeue", map[string]string{"raw": raw})
	mustStatus(t, rec, http.StatusOK, "")
	if !decode[map[string]bool](t, rec)["requeued"] {
		t.Errorf("the requeue is best-effort on the DB row; the item must still move")
	}
	if got := e.lane(t, "pending", store.GoUUID(org.ID).String()); len(got) != 1 || got[0].EventID != evID {
		t.Errorf("pending: %+v", got)
	}
	if got := e.eventStatus(t, evID); got != "failed" {
		t.Errorf("event status %q: the blocked UPDATE must have left it failed", got)
	}
}

// ---------------------------------------------------------------------------
// PATCH /admin/orgs/{org_id}/plan, through the real middleware.
// ---------------------------------------------------------------------------

func TestPatchPlan_RefusalsLeaveTheOrgUntouched(t *testing.T) {
	e := newEnv(t)
	su := principal{user: e.seedUser(t, true)}
	org := e.seedOrg(t, "Patch Org")
	before, err := e.d.Queries.GetOrgQuota(context.Background(), org.ID)
	if err != nil {
		t.Fatalf("get quota: %v", err)
	}
	path := "/admin/orgs/" + store.GoUUID(org.ID).String() + "/plan"

	mustStatus(t, e.do(t, su, http.MethodPatch, "/admin/orgs/not-a-uuid/plan", map[string]string{"plan": "pro"}),
		http.StatusBadRequest, "invalid org_id")
	mustStatus(t, e.do(t, su, http.MethodPatch, path, "{broken"), http.StatusBadRequest, "invalid json")
	mustStatus(t, e.do(t, su, http.MethodPatch, path, map[string]string{"plan": "platinum"}),
		http.StatusBadRequest, "plan must be one of free|pro|enterprise|custom")
	mustStatus(t, e.do(t, su, http.MethodPatch, "/admin/orgs/"+uuid.NewString()+"/plan", map[string]string{"plan": "pro"}),
		http.StatusNotFound, "org not found")

	after, err := e.d.Queries.GetOrgQuota(context.Background(), org.ID)
	if err != nil {
		t.Fatalf("get quota: %v", err)
	}
	if after != before {
		t.Errorf("quota changed by refused PATCHes: %+v -> %+v", before, after)
	}
}

func TestPatchPlan_DatabaseFailuresAre500(t *testing.T) {
	for _, c := range []struct{ name, failSQL string }{
		{"read quota", "-- name: getOrgQuota "},
		{"write quota", "-- name: updateOrgQuota "},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnvFailing(t, c.failSQL)
			su := principal{user: e.seedUser(t, true)}
			org := e.seedOrg(t, "Patch Fail Org")
			rec := e.do(t, su, http.MethodPatch, "/admin/orgs/"+store.GoUUID(org.ID).String()+"/plan", map[string]string{"plan": "pro"})
			mustStatus(t, rec, http.StatusInternalServerError, "patch plan")
			var plan string
			if err := e.pool.QueryRow(context.Background(), "SELECT plan FROM organizations WHERE id=$1", org.ID).Scan(&plan); err != nil {
				t.Fatalf("plan: %v", err)
			}
			if plan == "pro" {
				t.Errorf("plan moved to pro despite the 500")
			}
		})
	}
}

// A partial custom PATCH changes only the fields it names: here the two
// message limits, leaving the event limits and period as stored.
func TestPatchPlan_CustomMessageLimitsAloneKeepTheRest(t *testing.T) {
	e := newEnv(t)
	su := principal{user: e.seedUser(t, true)}
	org := e.seedOrg(t, "Custom Org")
	ctx := context.Background()
	if _, err := e.d.Queries.UpdateOrgQuota(ctx, store.UpdateOrgQuotaParams{
		ID: org.ID, Plan: "custom", QuotaPeriod: "day",
		QuotaEventsSoft: 10, QuotaEventsHard: 20, QuotaMessagesSoft: 1, QuotaMessagesHard: 2,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rec := e.do(t, su, http.MethodPatch, "/admin/orgs/"+store.GoUUID(org.ID).String()+"/plan",
		map[string]int64{"quota_messages_soft": 30, "quota_messages_hard": 40})
	mustStatus(t, rec, http.StatusOK, "")
	got, err := e.d.Queries.GetOrgQuota(ctx, org.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Plan != "custom" || got.QuotaPeriod != "day" || got.QuotaEventsSoft != 10 || got.QuotaEventsHard != 20 ||
		got.QuotaMessagesSoft != 30 || got.QuotaMessagesHard != 40 {
		t.Errorf("stored: %+v", got)
	}
}
