package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/store"
)

// seedUser creates a fresh user; convenience for tests that need a second
// user without an org.
func seedUser(t *testing.T, q *store.Queries) uuid.UUID {
	t.Helper()
	u, err := q.CreateUser(context.Background(), store.CreateUserParams{
		Email: "test+" + uuid.NewString() + "@example.test",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return store.GoUUID(u.ID)
}

// addMember inserts an org_members row with the given role.
func addMember(t *testing.T, q *store.Queries, orgID, userID uuid.UUID, role string) {
	t.Helper()
	if err := q.AddOrgMember(context.Background(), store.AddOrgMemberParams{
		OrgID:  store.UUID(orgID),
		UserID: store.UUID(userID),
		Role:   role,
	}); err != nil {
		t.Fatalf("add member: %v", err)
	}
}

func requestWithSessionBody(
	t *testing.T, s *auth.SessionSigner,
	method, path string, userID, orgID uuid.UUID, body any,
) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	w := httptest.NewRecorder()
	s.Issue(w, userID, orgID, 0)
	res := w.Result()
	defer res.Body.Close()
	r := httptest.NewRequest(method, path, &buf)
	r.Header.Set("Content-Type", "application/json")
	for _, c := range res.Cookies() {
		if c.Name == auth.SessionCookieName {
			r.AddCookie(c)
		}
	}
	return r
}

// --- /api/me ---

func TestMe_Session_ReturnsUserAndOrgs(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedUserAndOrg(t, q)

	router, signer := newTestRouter(q)
	req := requestWithSession(t, signer, http.MethodGet, "/api/me", uid, oid)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := resp["user"]; !ok {
		t.Errorf("user missing: %v", resp)
	}
	orgs, ok := resp["orgs"].([]any)
	if !ok || len(orgs) != 1 {
		t.Errorf("expected one org, got: %v", resp["orgs"])
	}
	if resp["active_org_id"] != oid.String() {
		t.Errorf("active_org_id: got %v want %s", resp["active_org_id"], oid)
	}
}

func TestMe_APIKey_ReturnsOrgIDOnly(t *testing.T) {
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
		Role:    "admin",
	}); err != nil {
		t.Fatalf("create key: %v", err)
	}

	router, _ := newTestRouter(q)
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.Header.Set("Authorization", "Bearer "+full)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := resp["user"]; ok {
		t.Errorf("api key principal must not return user: %v", resp)
	}
	api, ok := resp["api_key"].(map[string]any)
	if !ok || api["org_id"] != oid.String() {
		t.Errorf("api_key missing/wrong: %v", resp)
	}
}

// --- /api/orgs/select ---

func TestSelectOrg_ValidMembership_ReissuesCookie(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oidA := seedUserAndOrg(t, q)
	// Second org user is also a member of.
	_, oidB := seedUserAndOrg(t, q)
	addMember(t, q, oidB, uid, "member")

	router, signer := newTestRouter(q)
	req := requestWithSessionBody(t, signer, http.MethodPost, "/api/orgs/select", uid, oidA, map[string]any{
		"org_id": oidB.String(),
	})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", rec.Code, rec.Body.String())
	}
	// New session cookie must carry oidB now.
	cookieFound := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionCookieName {
			cookieFound = true
		}
	}
	if !cookieFound {
		t.Errorf("expected session cookie re-issued")
	}
}

func TestSelectOrg_NonMember_Returns403(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oidA := seedUserAndOrg(t, q)
	_, oidB := seedUserAndOrg(t, q) // uid is NOT a member of oidB

	router, signer := newTestRouter(q)
	req := requestWithSessionBody(t, signer, http.MethodPost, "/api/orgs/select", uid, oidA, map[string]any{
		"org_id": oidB.String(),
	})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d (want 403); body=%s", rec.Code, rec.Body.String())
	}
}

// --- POST /api/orgs ---

func TestCreateOrg_AddsCreatorAsOwner(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedUserAndOrg(t, q)

	router, signer := newTestRouter(q)
	req := requestWithSessionBody(t, signer, http.MethodPost, "/api/orgs", uid, oid, map[string]any{
		"name": "Acme",
	})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d body=%s", rec.Code, rec.Body.String())
	}
	var org store.Organization
	if err := json.Unmarshal(rec.Body.Bytes(), &org); err != nil {
		t.Fatalf("decode: %v", err)
	}
	m, err := q.GetOrgMember(context.Background(), store.GetOrgMemberParams{
		OrgID:  org.ID,
		UserID: store.UUID(uid),
	})
	if err != nil {
		t.Fatalf("get owner: %v", err)
	}
	if m.Role != "owner" {
		t.Errorf("role: got %q want owner", m.Role)
	}
	if org.Name != "Acme" {
		t.Errorf("name: got %q", org.Name)
	}
}

// --- PATCH /api/orgs/{org_id} ---

func TestUpdateOrg_AdminRename_OK(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedUserAndOrg(t, q) // uid is owner

	router, signer := newTestRouter(q)
	req := requestWithSessionBody(t, signer, http.MethodPatch, "/api/orgs/"+oid.String(), uid, oid, map[string]any{
		"name": "Renamed",
	})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", rec.Code, rec.Body.String())
	}
	var org store.Organization
	if err := json.Unmarshal(rec.Body.Bytes(), &org); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if org.Name != "Renamed" {
		t.Errorf("name: got %q", org.Name)
	}
}

func TestUpdateOrg_MemberCannot_403(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	_, oid := seedUserAndOrg(t, q)
	memberID := seedUser(t, q)
	addMember(t, q, oid, memberID, "member")

	router, signer := newTestRouter(q)
	req := requestWithSessionBody(t, signer, http.MethodPatch, "/api/orgs/"+oid.String(), memberID, oid, map[string]any{
		"name": "x",
	})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d (want 403); body=%s", rec.Code, rec.Body.String())
	}
}

// --- DELETE /api/orgs/{org_id} ---

func TestDeleteOrg_OwnerOnly(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	owner, oid := seedUserAndOrg(t, q)
	adminID := seedUser(t, q)
	addMember(t, q, oid, adminID, "admin")

	router, signer := newTestRouter(q)

	// admin → 403
	req := requestWithSession(t, signer, http.MethodDelete, "/api/orgs/"+oid.String(), adminID, oid)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("admin delete: got %d want 403; body=%s", rec.Code, rec.Body.String())
	}

	// owner → 204 + cascade
	req = requestWithSession(t, signer, http.MethodDelete, "/api/orgs/"+oid.String(), owner, oid)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("owner delete: got %d want 204; body=%s", rec.Code, rec.Body.String())
	}
	if _, err := q.GetOrganizationBySlug(context.Background(), "irrelevant"); err == nil {
		// guard against false positive; just ensure GetOrgMember after delete fails
		_ = err
	}
	if _, err := q.GetOrgMember(context.Background(), store.GetOrgMemberParams{
		OrgID:  store.UUID(oid),
		UserID: store.UUID(owner),
	}); err == nil {
		t.Errorf("expected org_members cascade after org delete")
	}
}

// --- POST /api/orgs/{org_id}/transfer ---

func TestTransferOwnership_NonMember_403(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	owner, oid := seedUserAndOrg(t, q)
	stranger := seedUser(t, q)

	router, signer := newTestRouter(q)
	req := requestWithSessionBody(t, signer, http.MethodPost, "/api/orgs/"+oid.String()+"/transfer", owner, oid, map[string]any{
		"to_user_id": stranger.String(),
	})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	// The handler deliberately returns a generic 403 for both "target isn't a
	// member" and "caller isn't owner", to avoid leaking which guard tripped.
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestTransferOwnership_Owner_DemotesSelf_PromotesTarget(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	owner, oid := seedUserAndOrg(t, q)
	target := seedUser(t, q)
	addMember(t, q, oid, target, "admin")

	router, signer := newTestRouter(q)
	req := requestWithSessionBody(t, signer, http.MethodPost, "/api/orgs/"+oid.String()+"/transfer", owner, oid, map[string]any{
		"to_user_id": target.String(),
	})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status: got %d want 204; body=%s", rec.Code, rec.Body.String())
	}

	cm, _ := q.GetOrgMember(context.Background(), store.GetOrgMemberParams{
		OrgID: store.UUID(oid), UserID: store.UUID(owner),
	})
	tm, _ := q.GetOrgMember(context.Background(), store.GetOrgMemberParams{
		OrgID: store.UUID(oid), UserID: store.UUID(target),
	})
	if cm.Role != "admin" {
		t.Errorf("caller role: got %q want admin", cm.Role)
	}
	if tm.Role != "owner" {
		t.Errorf("target role: got %q want owner", tm.Role)
	}
}

func TestTransferOwnership_NonOwner_403(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	_, oid := seedUserAndOrg(t, q)
	admin := seedUser(t, q)
	addMember(t, q, oid, admin, "admin")
	target := seedUser(t, q)
	addMember(t, q, oid, target, "member")

	router, signer := newTestRouter(q)
	req := requestWithSessionBody(t, signer, http.MethodPost, "/api/orgs/"+oid.String()+"/transfer", admin, oid, map[string]any{
		"to_user_id": target.String(),
	})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// =============================================================================
// Shared harness for the identity org / member / invite suites.
//
// Every request goes through the real Mount, the real Authenticate middleware
// and a real signed session cookie. Failures are injected one named statement
// at a time with a pgx QueryTracer, so a handler's 500 branch runs against the
// real database rather than a fake store.
// =============================================================================

// normSQL lowercases a statement and collapses whitespace, so a marker does
// not depend on sqlc's indentation.
func normSQL(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

// stmtTracer acts on statements whose normalised SQL (and args) match. By
// default it cancels the statement's context so it fails on the real pool;
// with after set it lets the statement run and calls after once it has
// finished, which lets a test interleave a competing write at an exact point.
type stmtTracer struct {
	match func(sql string, args []any) bool
	nth   int // act only on the nth match (1-based); 0 = every match
	after func()
	n     atomic.Int32
}

type hookKey struct{}

func (s *stmtTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if !s.match(normSQL(d.SQL), d.Args) {
		return ctx
	}
	k := int(s.n.Add(1))
	if s.nth != 0 && k != s.nth {
		return ctx
	}
	if s.after != nil {
		return context.WithValue(ctx, hookKey{}, true)
	}
	c, cancel := context.WithCancel(ctx)
	cancel()
	return c
}

func (s *stmtTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if s.after != nil && ctx.Value(hookKey{}) != nil {
		s.after()
	}
}

// has matches statements containing every fragment.
func has(frags ...string) func(string, []any) bool {
	return func(sql string, _ []any) bool {
		for _, f := range frags {
			if !strings.Contains(sql, f) {
				return false
			}
		}
		return true
	}
}

// hasArg is has plus: one of the statement's args is the given uuid.
func hasArg(u uuid.UUID, frags ...string) func(string, []any) bool {
	base := has(frags...)
	return func(sql string, args []any) bool {
		if !base(sql, args) {
			return false
		}
		for _, a := range args {
			if p, ok := a.(pgtype.UUID); ok && p.Valid && uuid.UUID(p.Bytes) == u {
				return true
			}
		}
		return false
	}
}

// failOn is a tracer failing every statement containing frags.
func failOn(frags ...string) *stmtTracer { return &stmtTracer{match: has(frags...)} }

// idEnv is the real stack behind the identity routes. pool/q are untraced and
// are what assertions read; the router runs on its own (possibly traced) pool.
type idEnv struct {
	t      *testing.T
	pool   *pgxpool.Pool
	q      *store.Queries
	h      http.Handler
	signer *auth.SessionSigner
}

func newIDEnv(t *testing.T, tr pgx.QueryTracer, mod func(*Deps)) *idEnv {
	t.Helper()
	pool := testPool(t) // skips without DSTREAM_TEST_DB_URL
	cfg, err := pgxpool.ParseConfig(os.Getenv("DSTREAM_TEST_DB_URL"))
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = 4
	if tr != nil {
		cfg.ConnConfig.Tracer = tr
	}
	routed, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(routed.Close)
	signer := &auth.SessionSigner{Secret: []byte("test-secret-do-not-use-in-prod")}
	d := Deps{
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Queries: store.New(routed),
		Pool: routed, Signer: signer, AppBaseURL: "http://app.test",
	}
	if mod != nil {
		mod(&d)
	}
	r := chi.NewRouter()
	Mount(r, d)
	return &idEnv{t: t, pool: pool, q: store.New(pool), h: r, signer: signer}
}

// do sends a request as (uid, org) from a client address of its own, so the
// per-IP rate budgets never leak between requests or tests.
func (e *idEnv) do(method, path string, uid, org uuid.UUID, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	req := requestWithSessionBody(e.t, e.signer, method, path, uid, org, body)
	req.RemoteAddr = uniqueClientAddr()
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

// anon sends an unauthenticated request.
func (e *idEnv) anon(method, path string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = uniqueClientAddr()
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

// wantErr asserts the status and the exact {"error": msg} envelope.
func wantErr(t *testing.T, rec *httptest.ResponseRecorder, code int, msg string) {
	t.Helper()
	if rec.Code != code {
		t.Fatalf("status: got %d want %d; body=%s", rec.Code, code, rec.Body.String())
	}
	var env map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v; body=%s", err, rec.Body.String())
	}
	if env["error"] != msg {
		t.Fatalf("error: got %q want %q", env["error"], msg)
	}
}

func wantStatus(t *testing.T, rec *httptest.ResponseRecorder, code int) {
	t.Helper()
	if rec.Code != code {
		t.Fatalf("status: got %d want %d; body=%s", rec.Code, code, rec.Body.String())
	}
}

// cookieOrg returns the active org carried by the session cookie a response
// set, failing if there is none.
func (e *idEnv) cookieOrg(rec *httptest.ResponseRecorder) (user, org uuid.UUID) {
	e.t.Helper()
	c := sessionCookie(rec)
	if c == nil {
		e.t.Fatalf("no session cookie set; status=%d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(c)
	u, o, _, err := e.signer.Parse(req)
	if err != nil {
		e.t.Fatalf("parse cookie: %v", err)
	}
	return u, o
}

// members returns user_id -> role for the org.
func (e *idEnv) members(org uuid.UUID) map[uuid.UUID]string {
	e.t.Helper()
	rows, err := e.pool.Query(context.Background(), `SELECT user_id, role FROM org_members WHERE org_id=$1`, org)
	if err != nil {
		e.t.Fatalf("members: %v", err)
	}
	defer rows.Close()
	out := map[uuid.UUID]string{}
	for rows.Next() {
		var u uuid.UUID
		var r string
		if err := rows.Scan(&u, &r); err != nil {
			e.t.Fatalf("scan: %v", err)
		}
		out[u] = r
	}
	return out
}

func (e *idEnv) owners(org uuid.UUID) int {
	n := 0
	for _, r := range e.members(org) {
		if r == "owner" {
			n++
		}
	}
	return n
}

func (e *idEnv) orgName(org uuid.UUID) (string, bool) {
	e.t.Helper()
	var n string
	err := e.pool.QueryRow(context.Background(), `SELECT name FROM organizations WHERE id=$1`, org).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false
	}
	if err != nil {
		e.t.Fatalf("org name: %v", err)
	}
	return n, true
}

func (e *idEnv) auditCount(org uuid.UUID, action string) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_logs WHERE org_id=$1 AND action=$2`, org, action).Scan(&n); err != nil {
		e.t.Fatalf("audit count: %v", err)
	}
	return n
}

// snapshot is a stable rendering of everything the privileged routes can
// change in one org: its name, its membership and its invites.
func (e *idEnv) snapshot(org uuid.UUID) string {
	e.t.Helper()
	name, _ := e.orgName(org)
	var parts []string
	for u, r := range e.members(org) {
		parts = append(parts, "m:"+u.String()+"="+r)
	}
	rows, err := e.pool.Query(context.Background(),
		`SELECT id, email, role, accepted_at IS NOT NULL FROM org_invites WHERE org_id=$1`, org)
	if err != nil {
		e.t.Fatalf("snapshot invites: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var email, role string
		var acc bool
		if err := rows.Scan(&id, &email, &role, &acc); err != nil {
			e.t.Fatalf("scan: %v", err)
		}
		parts = append(parts, fmt.Sprintf("i:%s/%s/%s/%v", id, email, role, acc))
	}
	sort.Strings(parts)
	return name + "|" + strings.Join(parts, ",")
}

// sameSnapshot fails if the org changed since before was taken.
func (e *idEnv) sameSnapshot(org uuid.UUID, before string) {
	e.t.Helper()
	if after := e.snapshot(org); after != before {
		e.t.Fatalf("org %s changed on a refused request:\nbefore %s\nafter  %s", org, before, after)
	}
}

// seedOrg creates an org owned by a fresh user. Both ids are unique.
func (e *idEnv) seedOrg() (owner, org uuid.UUID) { return seedUserAndOrg(e.t, e.q) }

// seedRole adds a fresh user to org at role.
func (e *idEnv) seedRole(org uuid.UUID, role string) uuid.UUID {
	u := seedUser(e.t, e.q)
	addMember(e.t, e.q, org, u, role)
	return u
}

func orgPath(org uuid.UUID, suffix string) string { return "/api/orgs/" + org.String() + suffix }

// --- every session-only route refuses an API key ---

func TestIdentityRoutes_APIKeyRefused_SessionRequired(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	owner, org := e.seedOrg()
	member := e.seedRole(org, "member")
	_ = owner
	key := newKeyAt(t, e.q, org, "admin")
	before := e.snapshot(org)

	inv := uuid.NewString()
	routes := []struct{ method, path string }{
		{http.MethodGet, "/api/orgs"},
		{http.MethodPost, "/api/orgs"},
		{http.MethodPost, "/api/orgs/select"},
		{http.MethodPatch, orgPath(org, "")},
		{http.MethodDelete, orgPath(org, "")},
		{http.MethodPost, orgPath(org, "/transfer")},
		{http.MethodGet, orgPath(org, "/members")},
		{http.MethodPatch, orgPath(org, "/members/"+member.String())},
		{http.MethodDelete, orgPath(org, "/members/"+member.String())},
		{http.MethodGet, orgPath(org, "/invites")},
		{http.MethodPost, orgPath(org, "/invites")},
		{http.MethodDelete, orgPath(org, "/invites/"+inv)},
	}
	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			req := httptest.NewRequest(rt.method, rt.path, strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			e.h.ServeHTTP(rec, req)
			wantErr(t, rec, http.StatusForbidden, "session required")
		})
	}
	e.sameSnapshot(org, before)
}

// --- GET /api/orgs ---

func TestListMyOrgs_OnlyCallersOrgsWithRoles(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	uid, orgA := e.seedOrg()
	_, orgB := e.seedOrg()
	addMember(t, e.q, orgB, uid, "member")
	_, orgC := e.seedOrg() // the caller is NOT in this one

	rec := e.do(http.MethodGet, "/api/orgs", uid, orgA, nil)
	wantStatus(t, rec, http.StatusOK)
	var rows []struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
		Role string `json:"role"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := map[string]string{}
	for _, r := range rows {
		if r.Slug == "" {
			t.Errorf("org %s has no slug in the response", r.ID)
		}
		got[r.ID] = r.Role
	}
	if got[orgA.String()] != "owner" || got[orgB.String()] != "member" {
		t.Fatalf("roles: got %v want %s=owner %s=member", got, orgA, orgB)
	}
	if _, leaked := got[orgC.String()]; leaked {
		t.Fatalf("response leaked an org the caller is not in: %s", orgC)
	}
	if len(rows) != 2 {
		t.Fatalf("rows: got %d want exactly the caller's 2 orgs", len(rows))
	}
}

func TestListMyOrgs_DBFailure_500(t *testing.T) {
	e := newIDEnv(t, failOn("join org_members m on m.org_id = o.id"), nil)
	uid, org := e.seedOrg()
	wantErr(t, e.do(http.MethodGet, "/api/orgs", uid, org, nil), http.StatusInternalServerError, "list orgs")
}

// --- POST /api/orgs ---

func TestCreateOrg_BadInput_400(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	uid, org := e.seedOrg()
	for _, tc := range []struct {
		name string
		body any
		msg  string
	}{
		{"not json", "{nope", "invalid json"},
		{"empty name", map[string]any{"name": ""}, "name required"},
		{"blank name", map[string]any{"name": " \t "}, "name required"},
		{"missing name", map[string]any{}, "name required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantErr(t, e.do(http.MethodPost, "/api/orgs", uid, org, tc.body), http.StatusBadRequest, tc.msg)
		})
	}
}

func TestCreateOrg_StoresOrgOwnerSeedAndAudit(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	uid, org := e.seedOrg()
	name := "  Acme " + uuid.NewString() + "  "

	rec := e.do(http.MethodPost, "/api/orgs", uid, org, map[string]any{"name": name})
	wantStatus(t, rec, http.StatusCreated)
	var created struct {
		ID, Name, Slug string
	}
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	created.ID, _ = raw["id"].(string)
	created.Name, _ = raw["name"].(string)
	created.Slug, _ = raw["slug"].(string)
	newOrg := uuid.MustParse(created.ID)

	if created.Name != strings.TrimSpace(name) {
		t.Errorf("name: got %q want trimmed %q", created.Name, strings.TrimSpace(name))
	}
	if stored, ok := e.orgName(newOrg); !ok || stored != created.Name {
		t.Errorf("stored name: got %q ok=%v want %q", stored, ok, created.Name)
	}
	if got := e.members(newOrg); len(got) != 1 || got[uid] != "owner" {
		t.Errorf("members: got %v want only the creator as owner", got)
	}
	var ops int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM applications WHERE org_id=$1 AND is_operational`, newOrg).Scan(&ops); err != nil {
		t.Fatalf("op app: %v", err)
	}
	if ops != 1 {
		t.Errorf("operational app rows: got %d want 1", ops)
	}
	if n := e.auditCount(newOrg, "org.create"); n != 1 {
		t.Errorf("org.create audit rows: got %d want 1", n)
	}
}

// Two orgs with the same display name must both be created, with distinct
// slugs: the random suffix is what keeps a name collision from being a slug
// collision.
func TestCreateOrg_SameNameTwice_DistinctSlugs(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	uid, org := e.seedOrg()
	name := "Twin " + uuid.NewString()
	slugs := map[string]bool{}
	for i := 0; i < 2; i++ {
		rec := e.do(http.MethodPost, "/api/orgs", uid, org, map[string]any{"name": name})
		wantStatus(t, rec, http.StatusCreated)
		var raw map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &raw)
		slugs[raw["slug"].(string)] = true
	}
	if len(slugs) != 2 {
		t.Fatalf("slugs: got %v want two distinct", slugs)
	}
	var n int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM organizations WHERE name=$1`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("orgs named %q: got %d want 2", name, n)
	}
}

// slugifyName is unexported in internal/api/identity; the slug is observable
// in the CreateOrg response, so the table drives it through the route.
func TestCreateOrg_SlugifyName_Table(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	uid, org := e.seedOrg()
	for _, tc := range []struct{ name, prefix string }{
		{"Acme Corp", "acme-corp"},
		{"  Hello,   World!!  ", "hello-world"},
		{"A--B__C", "a-b-c"},
		{"a/b\\c.d", "a-b-c-d"},
		{"UPPER lower 123", "upper-lower-123"},
		{"Ünïcode Café", "n-code-caf"}, // non-ASCII letters are separators, not letters
		{"日本語", "org"},                 // nothing slug-safe survives -> fallback
		{"!!! ???", "org"},
		{"---lead and trail---", "lead-and-trail"},
		{"x", "x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := e.do(http.MethodPost, "/api/orgs", uid, org, map[string]any{"name": tc.name})
			wantStatus(t, rec, http.StatusCreated)
			var raw map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &raw)
			slug, _ := raw["slug"].(string)
			if !regexp.MustCompile("^" + regexp.QuoteMeta(tc.prefix) + "-[0-9a-f]{12}$").MatchString(slug) {
				t.Fatalf("slug: got %q want %s-<12 hex>", slug, tc.prefix)
			}
			var stored string
			if err := e.pool.QueryRow(context.Background(),
				`SELECT slug FROM organizations WHERE id=$1`, raw["id"].(string)).Scan(&stored); err != nil || stored != slug {
				t.Fatalf("stored slug: got %q err=%v want %q", stored, err, slug)
			}
		})
	}
}

func TestCreateOrg_MidFlowFailures(t *testing.T) {
	// Documents CURRENT behaviour, not contract. CreateOrg runs without a
	// transaction, so a failure after the org insert strands the org row.
	for _, tc := range []struct {
		name  string
		frags []string
		code  int
		msg   string
		// wantOrg: the org row survives the failed request; wantMembers: how
		// many members it is left with.
		wantOrg     bool
		wantMembers int
	}{
		// Documents CURRENT behaviour, not contract: a database outage on the
		// org insert is reported to the client as a bad request (400), not a 5xx.
		// A real slug collision cannot be provoked by input (48-bit random
		// suffix), so the statement is failed directly.
		{"create org", []string{"insert into organizations"}, http.StatusBadRequest, "create org", false, 0},
		// Documents CURRENT behaviour, not contract: the org is stranded with
		// no members, so it cannot be selected, administered, invited into or
		// deleted through the API (each needs a membership).
		{"add owner", []string{"insert into org_members"}, http.StatusInternalServerError, "add owner", true, 0},
		// Documents CURRENT behaviour, not contract: the org is left half
		// provisioned: it has its owner but no operational app.
		{"seed operational app", []string{"insert into applications", "is_operational"}, http.StatusInternalServerError, "provision org", true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newIDEnv(t, failOn(tc.frags...), nil)
			uid, org := e.seedOrg()
			name := "Doomed " + uuid.NewString()
			t.Cleanup(func() {
				_, _ = e.pool.Exec(context.Background(), `DELETE FROM organizations WHERE name=$1`, name)
			})
			wantErr(t, e.do(http.MethodPost, "/api/orgs", uid, org, map[string]any{"name": name}), tc.code, tc.msg)

			var id uuid.UUID
			err := e.pool.QueryRow(context.Background(), `SELECT id FROM organizations WHERE name=$1`, name).Scan(&id)
			if !tc.wantOrg {
				if !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("a failed create should leave no org row: id=%s err=%v", id, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected the stranded org row to exist: %v", err)
			}
			if got := len(e.members(id)); got != tc.wantMembers {
				t.Fatalf("stranded org members: got %d want %d", got, tc.wantMembers)
			}
			if tc.name == "seed operational app" {
				var ops int
				_ = e.pool.QueryRow(context.Background(),
					`SELECT count(*) FROM applications WHERE org_id=$1 AND is_operational`, id).Scan(&ops)
				if ops != 0 {
					t.Fatalf("operational apps: got %d want 0", ops)
				}
			}
		})
	}
}

// --- POST /api/orgs/select ---

func TestSelectOrg_BadInput_400(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	uid, org := e.seedOrg()
	for _, tc := range []struct {
		name string
		body any
		msg  string
	}{
		{"not json", "{nope", "invalid json"},
		{"not a uuid", map[string]any{"org_id": "zzz"}, "invalid org_id"},
		{"missing", map[string]any{}, "invalid org_id"},
		{"nil uuid", map[string]any{"org_id": uuid.Nil.String()}, "invalid org_id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := e.do(http.MethodPost, "/api/orgs/select", uid, org, tc.body)
			wantErr(t, rec, http.StatusBadRequest, tc.msg)
			if sessionCookie(rec) != nil {
				t.Fatal("a refused select must not re-issue the cookie")
			}
		})
	}
}

func TestSelectOrg_NonMember_403_NoCookie(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	uid, mine := e.seedOrg()
	_, theirs := e.seedOrg()
	rec := e.do(http.MethodPost, "/api/orgs/select", uid, mine, map[string]any{"org_id": theirs.String()})
	wantErr(t, rec, http.StatusForbidden, "not a member of that org")
	if sessionCookie(rec) != nil {
		t.Fatal("a refused select must not re-issue the cookie")
	}
}

func TestSelectOrg_Member_CookieCarriesNewOrgAndUser(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	uid, first := e.seedOrg()
	_, second := e.seedOrg()
	addMember(t, e.q, second, uid, "member")
	rec := e.do(http.MethodPost, "/api/orgs/select", uid, first, map[string]any{"org_id": second.String()})
	wantStatus(t, rec, http.StatusOK)
	gotUser, gotOrg := e.cookieOrg(rec)
	if gotUser != uid || gotOrg != second {
		t.Fatalf("cookie: got user=%s org=%s want %s / %s", gotUser, gotOrg, uid, second)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["active_org_id"] != second.String() {
		t.Fatalf("active_org_id: got %q want %s", body["active_org_id"], second)
	}
}

func TestSelectOrg_LookupFailure_500(t *testing.T) {
	e := newIDEnv(t, failOn("from org_members where org_id = $1 and user_id = $2"), nil)
	uid, org := e.seedOrg()
	rec := e.do(http.MethodPost, "/api/orgs/select", uid, org, map[string]any{"org_id": org.String()})
	wantErr(t, rec, http.StatusInternalServerError, "membership lookup")
	if sessionCookie(rec) != nil {
		t.Fatal("a failed lookup must not re-issue the cookie")
	}
}

// --- PATCH /api/orgs/{org_id} ---

func TestUpdateOrg_Refusals_ChangeNothing(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	owner, org := e.seedOrg()
	admin := e.seedRole(org, "admin")
	member := e.seedRole(org, "member")
	stranger, strangerOrg := e.seedOrg()
	before := e.snapshot(org)
	beforeStranger := e.snapshot(strangerOrg)
	rename := map[string]any{"name": "Hijacked"}

	for _, tc := range []struct {
		name string
		path string
		as   uuid.UUID
		asOr uuid.UUID
		body any
		code int
		msg  string
	}{
		{"bad org id", "/api/orgs/zzz", owner, org, rename, 400, "invalid org_id"},
		{"cross-org: another org's owner", orgPath(org, ""), stranger, strangerOrg, rename, 403, "not a member"},
		{"member", orgPath(org, ""), member, org, rename, 403, "admin required"},
		{"bad json", orgPath(org, ""), admin, org, "{nope", 400, "invalid json"},
		{"nothing to update", orgPath(org, ""), admin, org, map[string]any{}, 400, "nothing to update"},
		{"blank name", orgPath(org, ""), admin, org, map[string]any{"name": "  "}, 400, "name required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantErr(t, e.do(http.MethodPatch, tc.path, tc.as, tc.asOr, tc.body), tc.code, tc.msg)
			e.sameSnapshot(org, before)
		})
	}
	e.sameSnapshot(org, before)
	e.sameSnapshot(strangerOrg, beforeStranger)
	if n := e.auditCount(org, "org.update"); n != 0 {
		t.Fatalf("refused renames wrote %d audit rows", n)
	}
}

func TestUpdateOrg_Rename_TrimsStoresAndAudits(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	owner, org := e.seedOrg()
	oldName, _ := e.orgName(org)
	newName := "Renamed " + uuid.NewString()

	rec := e.do(http.MethodPatch, orgPath(org, ""), owner, org, map[string]any{"name": "  " + newName + " "})
	wantStatus(t, rec, http.StatusOK)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["name"] != newName {
		t.Errorf("response name: got %v want %q", body["name"], newName)
	}
	if stored, _ := e.orgName(org); stored != newName {
		t.Errorf("stored name: got %q want %q", stored, newName)
	}
	var meta string
	if err := e.pool.QueryRow(context.Background(),
		`SELECT metadata::text FROM audit_logs WHERE org_id=$1 AND action='org.update'`, org).Scan(&meta); err != nil {
		t.Fatalf("audit row: %v", err)
	}
	if !strings.Contains(meta, oldName) || !strings.Contains(meta, newName) {
		t.Errorf("audit metadata %s should record from %q to %q", meta, oldName, newName)
	}
}

func TestUpdateOrg_SameName_NoWriteNoAudit(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	owner, org := e.seedOrg()
	name, _ := e.orgName(org)
	var before time.Time
	_ = e.pool.QueryRow(context.Background(), `SELECT updated_at FROM organizations WHERE id=$1`, org).Scan(&before)

	rec := e.do(http.MethodPatch, orgPath(org, ""), owner, org, map[string]any{"name": " " + name + " "})
	wantStatus(t, rec, http.StatusOK)
	var after time.Time
	_ = e.pool.QueryRow(context.Background(), `SELECT updated_at FROM organizations WHERE id=$1`, org).Scan(&after)
	if !after.Equal(before) {
		t.Errorf("updated_at moved on a no-op rename: %v -> %v", before, after)
	}
	if n := e.auditCount(org, "org.update"); n != 0 {
		t.Errorf("no-op rename wrote %d audit rows", n)
	}
}

func TestUpdateOrg_MidFlow(t *testing.T) {
	t.Run("get org fails", func(t *testing.T) {
		e := newIDEnv(t, failOn("select id, name, slug, created_at, updated_at from organizations where id = $1"), nil)
		owner, org := e.seedOrg()
		before := e.snapshot(org)
		wantErr(t, e.do(http.MethodPatch, orgPath(org, ""), owner, org, map[string]any{"name": "X"}),
			http.StatusInternalServerError, "get org")
		e.sameSnapshot(org, before)
	})
	t.Run("update fails", func(t *testing.T) {
		e := newIDEnv(t, failOn("update organizations set name"), nil)
		owner, org := e.seedOrg()
		before := e.snapshot(org)
		wantErr(t, e.do(http.MethodPatch, orgPath(org, ""), owner, org, map[string]any{"name": "X"}),
			http.StatusInternalServerError, "update org")
		e.sameSnapshot(org, before)
	})
	t.Run("org deleted after the membership check", func(t *testing.T) {
		// Deleting the org the instant the caller's membership has been read
		// leaves the handler holding a valid caller and a missing org.
		var pool *pgxpool.Pool
		var org uuid.UUID
		tr := &stmtTracer{
			match: has("from org_members where org_id = $1 and user_id = $2"), nth: 1,
			after: func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if _, err := pool.Exec(ctx, `DELETE FROM organizations WHERE id=$1`, org); err != nil {
					t.Errorf("concurrent org delete: %v", err)
				}
			},
		}
		e := newIDEnv(t, tr, nil)
		pool = e.pool
		var owner uuid.UUID
		owner, org = e.seedOrg()
		wantErr(t, e.do(http.MethodPatch, orgPath(org, ""), owner, org, map[string]any{"name": "X"}),
			http.StatusNotFound, "org not found")
	})
}

// --- DELETE /api/orgs/{org_id} ---

func TestDeleteOrg_Refusals_ChangeNothing(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	owner, org := e.seedOrg()
	admin := e.seedRole(org, "admin")
	member := e.seedRole(org, "member")
	stranger, strangerOrg := e.seedOrg()
	before := e.snapshot(org)
	beforeStranger := e.snapshot(strangerOrg)

	wantErr(t, e.do(http.MethodDelete, "/api/orgs/zzz", owner, org, nil), http.StatusBadRequest, "invalid org_id")
	wantErr(t, e.do(http.MethodDelete, orgPath(org, ""), admin, org, nil), http.StatusForbidden, "owner required")
	wantErr(t, e.do(http.MethodDelete, orgPath(org, ""), member, org, nil), http.StatusForbidden, "owner required")
	// Another org's owner, with the victim org in the path.
	wantErr(t, e.do(http.MethodDelete, orgPath(org, ""), stranger, strangerOrg, nil), http.StatusForbidden, "not a member")
	e.sameSnapshot(org, before)
	e.sameSnapshot(strangerOrg, beforeStranger)
	if n := e.auditCount(org, "org.delete"); n != 0 {
		t.Fatalf("refused deletes wrote %d audit rows", n)
	}
}

func TestDeleteOrg_Owner_CascadesAndRotatesCookie(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	owner, doomed := e.seedOrg()
	_, keep := e.seedOrg()
	addMember(t, e.q, keep, owner, "member")
	other := e.seedRole(doomed, "member")
	stageInvite(t, e.q, doomed, owner, "pending+"+uuid.NewString()+"@example.test", "member")

	rec := e.do(http.MethodDelete, orgPath(doomed, ""), owner, doomed, nil)
	wantStatus(t, rec, http.StatusNoContent)

	if _, ok := e.orgName(doomed); ok {
		t.Fatal("org row survived the delete")
	}
	if got := e.members(doomed); len(got) != 0 {
		t.Fatalf("memberships survived the cascade: %v", got)
	}
	var invites int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM org_invites WHERE org_id=$1`, doomed).Scan(&invites)
	if invites != 0 {
		t.Fatalf("invites survived the cascade: %d", invites)
	}
	// The other org and the other member's account are untouched.
	if got := e.members(keep)[owner]; got != "member" {
		t.Fatalf("unrelated membership changed: %q", got)
	}
	if _, err := e.q.GetUserByID(context.Background(), store.UUID(other)); err != nil {
		t.Fatalf("a member's account must survive org deletion: %v", err)
	}
	// The active org was the deleted one, so the cookie moves to the survivor.
	if u, o := e.cookieOrg(rec); u != owner || o != keep {
		t.Fatalf("cookie: got user=%s org=%s want %s / %s", u, o, owner, keep)
	}
}

func TestDeleteOrg_LastOrg_CookieOrgNil(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	owner, org := e.seedOrg()
	rec := e.do(http.MethodDelete, orgPath(org, ""), owner, org, nil)
	wantStatus(t, rec, http.StatusNoContent)
	if _, ok := e.orgName(org); ok {
		t.Fatal("org row survived the delete")
	}
	if _, o := e.cookieOrg(rec); o != uuid.Nil {
		t.Fatalf("cookie org: got %s want nil (user now has no org)", o)
	}
}

func TestDeleteOrg_NotActiveOrg_CookieUntouched(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	owner, active := e.seedOrg()
	_, doomed := e.seedOrg()
	addMember(t, e.q, doomed, owner, "owner")
	rec := e.do(http.MethodDelete, orgPath(doomed, ""), owner, active, nil)
	wantStatus(t, rec, http.StatusNoContent)
	if _, ok := e.orgName(doomed); ok {
		t.Fatal("org row survived the delete")
	}
	if sessionCookie(rec) != nil {
		t.Fatal("deleting a non-active org must not touch the session cookie")
	}
	if _, ok := e.orgName(active); !ok {
		t.Fatal("the active org must survive")
	}
}

// Either of two owners may destroy the org; ownership count is irrelevant to
// delete, only the owner role is.
func TestDeleteOrg_TwoOwners_EitherMayDelete(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	first, org := e.seedOrg()
	_ = first
	second := e.seedRole(org, "owner")
	wantStatus(t, e.do(http.MethodDelete, orgPath(org, ""), second, org, nil), http.StatusNoContent)
	if _, ok := e.orgName(org); ok {
		t.Fatal("org row survived the delete")
	}
}

func TestDeleteOrg_MidFlow(t *testing.T) {
	t.Run("delete fails", func(t *testing.T) {
		e := newIDEnv(t, failOn("delete from organizations"), nil)
		owner, org := e.seedOrg()
		before := e.snapshot(org)
		wantErr(t, e.do(http.MethodDelete, orgPath(org, ""), owner, org, nil), http.StatusInternalServerError, "delete org")
		e.sameSnapshot(org, before)
	})
	t.Run("snapshot lookup fails: still deletes", func(t *testing.T) {
		e := newIDEnv(t, failOn("select id, name, slug, created_at, updated_at from organizations where id = $1"), nil)
		owner, org := e.seedOrg()
		wantStatus(t, e.do(http.MethodDelete, orgPath(org, ""), owner, org, nil), http.StatusNoContent)
		if _, ok := e.orgName(org); ok {
			t.Fatal("org row survived the delete")
		}
	})
	t.Run("next-org lookup fails: cookie org nil", func(t *testing.T) {
		e := newIDEnv(t, failOn("order by m.created_at asc, m.org_id asc"), nil)
		owner, doomed := e.seedOrg()
		_, keep := e.seedOrg()
		addMember(t, e.q, keep, owner, "member")
		rec := e.do(http.MethodDelete, orgPath(doomed, ""), owner, doomed, nil)
		wantStatus(t, rec, http.StatusNoContent)
		if _, o := e.cookieOrg(rec); o != uuid.Nil {
			t.Fatalf("cookie org: got %s want nil when the next-org lookup fails", o)
		}
	})
}

// --- POST /api/orgs/{org_id}/transfer ---

func TestTransferOwnership_Refusals_ChangeNothing(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	owner, org := e.seedOrg()
	admin := e.seedRole(org, "admin")
	member := e.seedRole(org, "member")
	stranger, strangerOrg := e.seedOrg()
	outsider := seedUser(t, e.q) // belongs to no org
	before := e.snapshot(org)
	beforeStranger := e.snapshot(strangerOrg)
	to := func(u uuid.UUID) map[string]any { return map[string]any{"to_user_id": u.String()} }

	for _, tc := range []struct {
		name string
		path string
		as   uuid.UUID
		asOr uuid.UUID
		body any
		code int
		msg  string
	}{
		{"bad org id", "/api/orgs/zzz/transfer", owner, org, to(member), 400, "invalid org_id"},
		{"bad json", orgPath(org, "/transfer"), owner, org, "{nope", 400, "invalid json"},
		{"bad target id", orgPath(org, "/transfer"), owner, org, map[string]any{"to_user_id": "zzz"}, 400, "invalid to_user_id"},
		{"to self", orgPath(org, "/transfer"), owner, org, to(owner), 400, "cannot transfer to self"},
		{"admin may not transfer", orgPath(org, "/transfer"), admin, org, to(member), 403, "transfer not permitted"},
		{"member may not transfer", orgPath(org, "/transfer"), member, org, to(admin), 403, "transfer not permitted"},
		{"target is not a member", orgPath(org, "/transfer"), owner, org, to(outsider), 403, "transfer not permitted"},
		{"target is another org's owner", orgPath(org, "/transfer"), owner, org, to(stranger), 403, "transfer not permitted"},
		{"cross-org: stranger's owner uses the victim org", orgPath(org, "/transfer"), stranger, strangerOrg, to(member), 403, "transfer not permitted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantErr(t, e.do(http.MethodPost, tc.path, tc.as, tc.asOr, tc.body), tc.code, tc.msg)
			e.sameSnapshot(org, before)
		})
	}
	e.sameSnapshot(org, before)
	e.sameSnapshot(strangerOrg, beforeStranger)
	if n := e.auditCount(org, "org.transfer"); n != 0 {
		t.Fatalf("refused transfers wrote %d audit rows", n)
	}
}

func TestTransferOwnership_PromotesTargetDemotesCallerAuditsOnce(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	owner, org := e.seedOrg()
	target := e.seedRole(org, "member")
	bystander := e.seedRole(org, "member")

	wantStatus(t, e.do(http.MethodPost, orgPath(org, "/transfer"), owner, org,
		map[string]any{"to_user_id": target.String()}), http.StatusNoContent)
	got := e.members(org)
	if got[target] != "owner" || got[owner] != "admin" || got[bystander] != "member" {
		t.Fatalf("roles after transfer: got %v", got)
	}
	if e.owners(org) != 1 {
		t.Fatalf("owners: got %d want exactly 1", e.owners(org))
	}
	if n := e.auditCount(org, "org.transfer"); n != 1 {
		t.Fatalf("org.transfer audit rows: got %d want 1", n)
	}
	// The previous owner is now an admin: the transfer cannot be repeated.
	wantErr(t, e.do(http.MethodPost, orgPath(org, "/transfer"), owner, org,
		map[string]any{"to_user_id": bystander.String()}), http.StatusForbidden, "transfer not permitted")
	if e.members(org)[bystander] != "member" {
		t.Fatal("a refused repeat transfer changed the bystander's role")
	}
}

// With two owners, one handing off to the other still leaves exactly one
// owner: the target stays owner and the caller steps down.
func TestTransferOwnership_BetweenTwoOwners_StillOneOwner(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	first, org := e.seedOrg()
	second := e.seedRole(org, "owner")
	wantStatus(t, e.do(http.MethodPost, orgPath(org, "/transfer"), first, org,
		map[string]any{"to_user_id": second.String()}), http.StatusNoContent)
	got := e.members(org)
	if got[second] != "owner" || got[first] != "admin" || e.owners(org) != 1 {
		t.Fatalf("roles: got %v owners=%d want second=owner first=admin, 1 owner", got, e.owners(org))
	}
}

func TestTransferOwnership_DBFailure_500(t *testing.T) {
	e := newIDEnv(t, failOn("update org_members m set role = case"), nil)
	owner, org := e.seedOrg()
	target := e.seedRole(org, "member")
	before := e.snapshot(org)
	wantErr(t, e.do(http.MethodPost, orgPath(org, "/transfer"), owner, org,
		map[string]any{"to_user_id": target.String()}), http.StatusInternalServerError, "transfer ownership")
	e.sameSnapshot(org, before)
}
