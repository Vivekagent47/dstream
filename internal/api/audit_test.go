package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/store"
)

// --- DB-gated harness ---

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

func seedUserAndOrg(t *testing.T, q *store.Queries) (userID, orgID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	u, err := q.CreateUser(ctx, store.CreateUserParams{
		Email: "test+" + uuid.NewString() + "@example.test",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	o, err := q.CreateOrganization(ctx, store.CreateOrganizationParams{
		Name: "T",
		Slug: "t-" + uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := q.AddOrgMember(ctx, store.AddOrgMemberParams{
		OrgID:  o.ID,
		UserID: u.ID,
		Role:   "owner",
	}); err != nil {
		t.Fatalf("add member: %v", err)
	}
	return store.GoUUID(u.ID), store.GoUUID(o.ID)
}

// insertAudit is a small helper that bypasses the package's audit.Log so the
// test can fabricate rows with specific action/target_type/actor values.
func insertAudit(t *testing.T, q *store.Queries, orgID, userID uuid.UUID, action, targetType string) {
	t.Helper()
	if err := q.InsertAuditLog(context.Background(), store.InsertAuditLogParams{
		OrgID:       store.UUID(orgID),
		ActorUserID: store.UUID(userID),
		Action:      action,
		TargetType:  targetType,
		Metadata:    []byte(`{}`),
	}); err != nil {
		t.Fatalf("insert audit: %v", err)
	}
}

// newTestRouter returns a chi router with /api mounted using a fresh signer.
func newTestRouter(q *store.Queries) (*chi.Mux, *auth.SessionSigner) {
	r := chi.NewRouter()
	s := &auth.SessionSigner{Secret: []byte("test-secret-do-not-use-in-prod")}
	d := Deps{Queries: q, Signer: s}
	Mount(r, d)
	return r, s
}

// newTestRouterPool is newTestRouter plus a wired Pool, for handlers that open
// a transaction (e.g. AcceptInvite -> ConsumeOrgInvite).
func newTestRouterPool(pool *pgxpool.Pool, q *store.Queries) (*chi.Mux, *auth.SessionSigner) {
	r := chi.NewRouter()
	s := &auth.SessionSigner{Secret: []byte("test-secret-do-not-use-in-prod")}
	d := Deps{Queries: q, Signer: s, Pool: pool}
	Mount(r, d)
	return r, s
}

// requestWithSession builds an *http.Request with a valid session cookie for
// (userID, orgID).
func requestWithSession(t *testing.T, s *auth.SessionSigner, method, path string, userID, orgID uuid.UUID) *http.Request {
	t.Helper()
	w := httptest.NewRecorder()
	s.Issue(w, userID, orgID, 0)
	res := w.Result()
	defer res.Body.Close()
	r := httptest.NewRequest(method, path, nil)
	for _, c := range res.Cookies() {
		if c.Name == auth.SessionCookieName {
			r.AddCookie(c)
		}
	}
	return r
}

func TestListAudit_ScopedToActiveOrg(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)

	uidA, oidA := seedUserAndOrg(t, q)
	_, oidB := seedUserAndOrg(t, q)

	insertAudit(t, q, oidA, uidA, "source.create", "source")
	insertAudit(t, q, oidA, uidA, "source.delete", "source")
	insertAudit(t, q, oidB, uidA, "source.create", "source") // different org

	router, signer := newTestRouter(q)
	req := requestWithSession(t, signer, http.MethodGet, "/api/audit", uidA, oidA)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Entries []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Entries) != 2 {
		t.Fatalf("entries: got %d, want 2 (orgA only): %v", len(resp.Entries), resp.Entries)
	}
	for _, e := range resp.Entries {
		target := e["target"].(map[string]any)
		if target["type"] != "source" {
			t.Errorf("unexpected target_type: %v", target["type"])
		}
		actor := e["actor"].(map[string]any)
		if actor["type"] != "user" {
			t.Errorf("expected actor.type=user; got %v", actor["type"])
		}
	}
}

func TestListAudit_APIKeyForbidden(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	_, oid := seedUserAndOrg(t, q)

	full, prefix, hash, err := auth.NewAPIKey()
	if err != nil {
		t.Fatalf("new key: %v", err)
	}
	if _, err := q.CreateAPIKey(context.Background(), store.CreateAPIKeyParams{
		OrgID:   store.UUID(oid),
		Name:    "t",
		Prefix:  prefix,
		KeyHash: hash,
		Role:    string(auth.RoleAdmin),
	}); err != nil {
		t.Fatalf("create key: %v", err)
	}

	router, _ := newTestRouter(q)
	req := httptest.NewRequest(http.MethodGet, "/api/audit", nil)
	req.Header.Set("Authorization", "Bearer "+full)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d (want 403); body=%s", rec.Code, rec.Body.String())
	}
}

func TestListAudit_FilterByAction(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedUserAndOrg(t, q)

	insertAudit(t, q, oid, uid, "source.create", "source")
	insertAudit(t, q, oid, uid, "source.delete", "source")
	insertAudit(t, q, oid, uid, "destination.create", "destination")

	router, signer := newTestRouter(q)
	req := requestWithSession(t, signer, http.MethodGet, "/api/audit?action=source.create", uid, oid)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Entries []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Entries) != 1 || resp.Entries[0]["action"] != "source.create" {
		t.Fatalf("expected single source.create; got %v", resp.Entries)
	}
}

func TestListAudit_Pagination(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedUserAndOrg(t, q)

	for i := 0; i < 5; i++ {
		insertAudit(t, q, oid, uid, "source.create", "source")
	}

	router, signer := newTestRouter(q)

	// First page (limit=2).
	req := requestWithSession(t, signer, http.MethodGet, "/api/audit?limit=2", uid, oid)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("p1 status: got %d body=%s", rec.Code, rec.Body.String())
	}
	var p1 struct {
		Entries      []map[string]any `json:"entries"`
		NextBeforeID *int64           `json:"next_before_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p1); err != nil {
		t.Fatalf("decode p1: %v", err)
	}
	if len(p1.Entries) != 2 {
		t.Fatalf("p1 entries: got %d want 2", len(p1.Entries))
	}
	if p1.NextBeforeID == nil {
		t.Fatal("p1 next_before_id missing")
	}

	// Second page using the cursor.
	urlP2 := "/api/audit?limit=2&before_id=" + strconv.FormatInt(*p1.NextBeforeID, 10)
	req = requestWithSession(t, signer, http.MethodGet, urlP2, uid, oid)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("p2 status: got %d body=%s", rec.Code, rec.Body.String())
	}
	var p2 struct {
		Entries      []map[string]any `json:"entries"`
		NextBeforeID *int64           `json:"next_before_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p2); err != nil {
		t.Fatalf("decode p2: %v", err)
	}
	if len(p2.Entries) != 2 {
		t.Fatalf("p2 entries: got %d want 2", len(p2.Entries))
	}
	// IDs in p1 must be strictly greater than IDs in p2.
	p1Min := int64Of(p1.Entries[len(p1.Entries)-1]["id"])
	p2Max := int64Of(p2.Entries[0]["id"])
	if p2Max >= p1Min {
		t.Fatalf("pagination overlap: p1 min=%d, p2 max=%d", p1Min, p2Max)
	}
}

// int64Of pulls a JSON-decoded number (float64) into int64. We use this
// instead of round-tripping through encoding/json with a custom type because
// the test only cares about ordering, not exact representation.
func int64Of(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case int64:
		return x
	default:
		return 0
	}
}

// =============================================================================
// Audit: filters, pagination, actor shaping, cross-org isolation.
// =============================================================================

// auditPage is the GET /api/audit envelope.
type auditPage struct {
	Entries      []map[string]any `json:"entries"`
	NextBeforeID *int64           `json:"next_before_id"`
}

func decodeAudit(t *testing.T, rec *httptest.ResponseRecorder) auditPage {
	t.Helper()
	wantStatus(t, rec, http.StatusOK)
	var p auditPage
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode audit: %v; body=%s", err, rec.Body.String())
	}
	if p.Entries == nil {
		t.Fatalf("entries must be [] not null; body=%s", rec.Body.String())
	}
	return p
}

func auditActions(p auditPage) []string {
	var out []string
	for _, e := range p.Entries {
		out = append(out, e["action"].(string))
	}
	return out
}

func sameSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	m := map[string]int{}
	for _, g := range got {
		m[g]++
	}
	for _, w := range want {
		m[w]--
	}
	for _, n := range m {
		if n != 0 {
			return false
		}
	}
	return true
}

func (e *idEnv) auditFull(p store.InsertAuditLogParams) {
	e.t.Helper()
	if p.Metadata == nil {
		p.Metadata = []byte(`{}`)
	}
	if err := e.q.InsertAuditLog(context.Background(), p); err != nil {
		e.t.Fatalf("insert audit: %v", err)
	}
}

func TestServeAudit_FiltersCombineAndNeverCrossOrgs(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	alice, org := e.seedOrg()
	bob := e.seedRole(org, "member")
	tag := uuid.NewString()[:8]
	for _, r := range []struct {
		actor      uuid.UUID
		action, tt string
	}{
		{alice, "a.create." + tag, "source"},
		{alice, "a.delete." + tag, "source"},
		{bob, "a.create." + tag, "destination"},
	} {
		insertAudit(t, e.q, org, r.actor, r.action, r.tt)
	}
	// A different org, same actions, must never surface here.
	_, other := e.seedOrg()
	insertAudit(t, e.q, other, alice, "a.create."+tag, "source")

	get := func(query string) auditPage {
		return decodeAudit(t, e.do(http.MethodGet, "/api/audit"+query, alice, org, nil))
	}
	if got := get(""); len(got.Entries) != 3 {
		t.Fatalf("unfiltered: got %d want 3 (this org only)", len(got.Entries))
	}
	cases := []struct {
		query string
		want  int
	}{
		{"?action=a.create." + tag, 2},
		{"?target_type=source", 2},
		{"?actor_user_id=" + bob.String(), 1},
		{"?action=a.create." + tag + "&target_type=source", 1},
		{"?action=a.create." + tag + "&actor_user_id=" + alice.String() + "&target_type=source", 1},
		{"?action=a.delete." + tag + "&target_type=destination", 0},
		{"?action=nope." + tag, 0},
	}
	for _, c := range cases {
		if got := get(c.query); len(got.Entries) != c.want {
			t.Errorf("%s: got %d entries want %d (%v)", c.query, len(got.Entries), c.want, auditActions(got))
		}
	}
}

func TestServeAudit_BadQueryParamsAreRefused(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	owner, org := e.seedOrg()
	insertAudit(t, e.q, org, owner, "x.y", "source")
	for _, c := range []struct{ query, msg string }{
		{"?before_id=abc", "invalid before_id"},
		{"?before_id=-1", "invalid before_id"},
		{"?actor_user_id=not-a-uuid", "invalid actor_user_id"},
	} {
		wantErr(t, e.do(http.MethodGet, "/api/audit"+c.query, owner, org, nil), 400, c.msg)
	}
}

func TestServeAudit_LimitAndCursor(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	owner, org := e.seedOrg()
	for i := 0; i < 51; i++ {
		insertAudit(t, e.q, org, owner, "bulk."+strconv.Itoa(i), "source")
	}
	get := func(query string) auditPage {
		return decodeAudit(t, e.do(http.MethodGet, "/api/audit"+query, owner, org, nil))
	}

	// An unusable limit falls back to the default of 50, never to "all".
	for _, q := range []string{"", "?limit=0", "?limit=-3", "?limit=201", "?limit=abc"} {
		p := get(q)
		if len(p.Entries) != 50 || p.NextBeforeID == nil {
			t.Errorf("%q: got %d entries cursor=%v want 50 + cursor", q, len(p.Entries), p.NextBeforeID)
		}
	}
	// 200 is accepted and, with 51 rows, is not a full page: no cursor.
	if p := get("?limit=200"); len(p.Entries) != 51 || p.NextBeforeID != nil {
		t.Errorf("limit=200: got %d entries cursor=%v want 51, none", len(p.Entries), p.NextBeforeID)
	}
	// Walking the cursor visits every row exactly once, newest first.
	seen := map[string]bool{}
	var last int64 = 1 << 62
	for q := "?limit=20"; ; {
		p := get(q)
		for _, en := range p.Entries {
			id := int64Of(en["id"])
			if id >= last {
				t.Fatalf("ids not strictly descending: %d after %d", id, last)
			}
			last = id
			a := en["action"].(string)
			if seen[a] {
				t.Fatalf("row %s served twice", a)
			}
			seen[a] = true
		}
		if p.NextBeforeID == nil {
			break
		}
		q = "?limit=20&before_id=" + strconv.FormatInt(*p.NextBeforeID, 10)
	}
	if len(seen) != 51 {
		t.Fatalf("cursor walk saw %d rows want 51", len(seen))
	}
	// before_id=0 is a valid cursor that precedes every row.
	if p := get("?before_id=0"); len(p.Entries) != 0 || p.NextBeforeID != nil {
		t.Fatalf("before_id=0: %v", p.Entries)
	}
}

func TestListAuditForOrg_MembershipGatesAndDiffersFromActiveOrg(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	both, orgA := e.seedOrg()
	_, orgB := e.seedOrg()
	addMember(t, e.q, orgB, both, "member")
	stranger := seedUser(t, e.q)
	tag := uuid.NewString()[:8]
	insertAudit(t, e.q, orgA, both, "only.a."+tag, "source")
	insertAudit(t, e.q, orgB, both, "only.b."+tag, "source")

	// One session (active org A) reads each org through its own endpoint.
	if got := auditActions(decodeAudit(t, e.do(http.MethodGet, "/api/audit", both, orgA, nil))); !sameSet(got, []string{"only.a." + tag}) {
		t.Fatalf("/api/audit: %v", got)
	}
	if got := auditActions(decodeAudit(t, e.do(http.MethodGet, orgPath(orgB, "/audit"), both, orgA, nil))); !sameSet(got, []string{"only.b." + tag}) {
		t.Fatalf("/api/orgs/B/audit: %v", got)
	}

	// Not a member: refused, and the body carries no rows. Active org is
	// irrelevant, and a before_id aimed at the other org's rows changes nothing.
	for _, q := range []string{"", "?before_id=9223372036854775807", "?action=only.a." + tag} {
		rec := e.do(http.MethodGet, orgPath(orgA, "/audit")+q, stranger, orgA, nil)
		wantErr(t, rec, 403, "not a member of this org")
		if strings.Contains(rec.Body.String(), "only.a.") {
			t.Fatalf("refusal leaked audit rows: %s", rec.Body.String())
		}
	}
	// ...even with the stranger's active org pointing at an org they do belong to.
	_, strangerOrg := e.seedOrg()
	addMember(t, e.q, strangerOrg, stranger, "owner")
	wantErr(t, e.do(http.MethodGet, orgPath(orgA, "/audit"), stranger, strangerOrg, nil), 403, "not a member of this org")

	wantErr(t, e.do(http.MethodGet, "/api/orgs/nope/audit", both, orgA, nil), 400, "invalid org_id")
	key := newKeyAt(t, e.q, orgA, "admin")
	wantErr(t, e.withKey(key, http.MethodGet, orgPath(orgA, "/audit"), nil), 403, "session required")
}

func TestServeAudit_StoreFailure500(t *testing.T) {
	e := newIDEnv(t, failOn("from audit_logs a"), nil)
	owner, org := e.seedOrg()
	wantErr(t, e.do(http.MethodGet, "/api/audit", owner, org, nil), 500, "list audit")
	wantErr(t, e.do(http.MethodGet, orgPath(org, "/audit"), owner, org, nil), 500, "list audit")
}

func TestHydrateAuditRows_ActorShapes(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	owner, org := e.seedOrg()
	ctx := context.Background()
	name := "Alice A"
	if _, err := e.q.UpdateUserName(ctx, store.UpdateUserNameParams{ID: store.UUID(owner), Name: &name}); err != nil {
		t.Fatalf("name: %v", err)
	}
	var ownerEmail string
	if err := e.pool.QueryRow(ctx, `SELECT email FROM users WHERE id=$1`, owner).Scan(&ownerEmail); err != nil {
		t.Fatalf("email: %v", err)
	}
	keyID, _ := e.mintKey(owner, org, map[string]any{"name": "robot"})
	kid := uuid.MustParse(keyID)
	target := uuid.New()

	e.auditFull(store.InsertAuditLogParams{
		OrgID: store.UUID(org), ActorUserID: store.UUID(owner), Action: "u.act", TargetType: "source",
		TargetID: store.UUID(target), Metadata: []byte(`{"k":"v"}`),
	})
	e.auditFull(store.InsertAuditLogParams{
		OrgID: store.UUID(org), ActorApiKeyID: store.UUID(kid), Action: "k.act", TargetType: "source",
	})

	p := decodeAudit(t, e.do(http.MethodGet, "/api/audit?action=u.act", owner, org, nil))
	if len(p.Entries) != 1 {
		t.Fatalf("u.act: %v", p.Entries)
	}
	en := p.Entries[0]
	actor := en["actor"].(map[string]any)
	if actor["type"] != "user" || actor["user_id"] != owner.String() || actor["email"] != ownerEmail || actor["name"] != name {
		t.Fatalf("user actor: %v", actor)
	}
	if tg := en["target"].(map[string]any); tg["type"] != "source" || tg["id"] != target.String() {
		t.Fatalf("target: %v", tg)
	}
	if md := en["metadata"].(map[string]any); md["k"] != "v" {
		t.Fatalf("metadata: %v", md)
	}

	p = decodeAudit(t, e.do(http.MethodGet, "/api/audit?action=k.act", owner, org, nil))
	if len(p.Entries) != 1 {
		t.Fatalf("k.act: %v", p.Entries)
	}
	actor = p.Entries[0]["actor"].(map[string]any)
	if actor["type"] != "api_key" || actor["api_key_id"] != keyID || actor["name"] != "robot" {
		t.Fatalf("api key actor: %v", actor)
	}
	if _, ok := actor["user_id"]; ok {
		t.Fatalf("api key actor carries a user: %v", actor)
	}
	// No target id was recorded: the key is absent, not null.
	if tg := p.Entries[0]["target"].(map[string]any); tg["type"] != "source" || tg["id"] != nil {
		t.Fatalf("target without id: %v", tg)
	}
}

// audit_logs_check forbids an audit row with no actor, and ON DELETE SET NULL
// would trip it, so a user (or key) with audit rows cannot be deleted: there is
// no "deleted actor" row to read. The one way the live email join comes back
// empty is a user whose email is the empty string — users.email is NOT NULL
// but has no non-empty check — and then the snapshot taken at write time has to
// stand in.
func TestHydrateAuditRows_FallsBackToEmailSnapshot(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	_, org := e.seedOrg()
	reader, _ := e.seedOrg()
	addMember(t, e.q, org, reader, "member")
	ctx := context.Background()

	// users.email is UNIQUE, so only one blank-email user can exist. A leftover
	// can only come from a run of this test that died before cleanup; remove
	// just the rows carrying this test's marker, and the blank user only if
	// nothing else references it.
	const marker = "hydrate-fb."
	_, _ = e.pool.Exec(ctx, `DELETE FROM audit_logs WHERE action LIKE $1 AND actor_user_id IN (SELECT id FROM users WHERE email='')`, marker+"%")
	_, _ = e.pool.Exec(ctx, `DELETE FROM users WHERE email='' AND NOT EXISTS (SELECT 1 FROM audit_logs WHERE actor_user_id=users.id)`)
	u, err := e.q.CreateUser(ctx, store.CreateUserParams{Email: ""})
	if err != nil {
		t.Fatalf("create blank-email user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = e.pool.Exec(ctx, `DELETE FROM audit_logs WHERE actor_user_id=$1`, u.ID)
		_, _ = e.pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, u.ID)
	})
	ghost := store.GoUUID(u.ID)
	snap := "snapshot-" + uuid.NewString() + "@example.test"
	e.auditFull(store.InsertAuditLogParams{
		OrgID: store.UUID(org), ActorUserID: u.ID, ActorEmailSnapshot: &snap, Action: marker + "snap", TargetType: "source",
	})
	e.auditFull(store.InsertAuditLogParams{
		OrgID: store.UUID(org), ActorUserID: u.ID, Action: marker + "nosnap", TargetType: "source",
	})

	p := decodeAudit(t, e.do(http.MethodGet, "/api/audit?actor_user_id="+ghost.String(), reader, org, nil))
	byAction := map[string]map[string]any{}
	for _, en := range p.Entries {
		byAction[en["action"].(string)] = en["actor"].(map[string]any)
	}
	if a := byAction[marker+"snap"]; a["type"] != "user" || a["email"] != snap {
		t.Fatalf("snapshot fallback: %v", a)
	}
	if a := byAction[marker+"nosnap"]; a["type"] != "user" || a["user_id"] != ghost.String() {
		t.Fatalf("no snapshot: %v", a)
	} else if _, ok := a["email"]; ok {
		t.Fatalf("no live email and no snapshot must omit email: %v", a)
	}
}
