package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/config"
	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/store"
)

// newTestRouterRedis is the createInvite-friendly sibling of newTestRouter:
// it wires a real Redis client so the rate limiter can run. Gated on
// DSTREAM_TEST_REDIS_ADDR or DSTREAM_REDIS_ADDR; tests that need it call this directly.
func newTestRouterRedis(t *testing.T, q *store.Queries) (*chi.Mux, *auth.SessionSigner, *redis.Client) {
	t.Helper()
	// Same fallback as ssoRedis: the rest of the repo spells the variable
	// DSTREAM_REDIS_ADDR, and a CreateInvite suite that silently skips is worse
	// than none.
	rdb := ssoRedis(t)
	r := chi.NewRouter()
	s := &auth.SessionSigner{Secret: []byte("test-secret-do-not-use-in-prod")}
	d := Deps{Queries: q, Signer: s, Redis: rdb, PublicBaseURL: "http://test.local"}
	Mount(r, d)
	return r, s, rdb
}

// --- GET /api/orgs/{org_id}/invites ---

func TestListInvites_MemberCanRead(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedUserAndOrg(t, q)

	// Stage an invite directly so we don't need redis.
	stageInvite(t, q, oid, uid, "x+"+uuid.NewString()+"@example.test", "member")

	router, signer := newTestRouter(q)
	req := requestWithSession(t, signer, http.MethodGet, "/api/orgs/"+oid.String()+"/invites", uid, oid)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", rec.Code, rec.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows: got %d want 1", len(rows))
	}
}

func TestListInvites_NonMember_403(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	_, oid := seedUserAndOrg(t, q)
	stranger := seedUser(t, q)

	router, signer := newTestRouter(q)
	req := requestWithSession(t, signer, http.MethodGet, "/api/orgs/"+oid.String()+"/invites", stranger, oid)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// --- POST /api/orgs/{org_id}/invites ---

func TestCreateInvite_Admin_202(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedUserAndOrg(t, q)

	router, signer, rdb := newTestRouterRedis(t, q)
	// Flush any prior rate-limit counters from previous runs that share this
	// redis instance. We only flush the keys this test will touch.
	ctx := context.Background()
	rdb.Del(ctx, "invite:inviter:"+uid.String())

	email := "newinvitee+" + uuid.NewString() + "@example.test"
	req := requestWithSessionBody(t, signer, http.MethodPost,
		"/api/orgs/"+oid.String()+"/invites", uid, oid,
		map[string]any{"email": email, "role": "member"})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status: got %d body=%s", rec.Code, rec.Body.String())
	}
	// Invite row should exist.
	rows, err := q.ListOrgInvitesByOrg(ctx, store.UUID(oid))
	if err != nil {
		t.Fatalf("list invites: %v", err)
	}
	found := false
	for _, row := range rows {
		if row.Email == email {
			found = true
			if row.Role != "member" {
				t.Errorf("role: got %q want member", row.Role)
			}
		}
	}
	if !found {
		t.Errorf("expected invite for %s", email)
	}
}

func TestCreateInvite_MemberRole_403(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	_, oid := seedUserAndOrg(t, q)
	member := seedUser(t, q)
	addMember(t, q, oid, member, "member")

	router, signer, _ := newTestRouterRedis(t, q)
	req := requestWithSessionBody(t, signer, http.MethodPost,
		"/api/orgs/"+oid.String()+"/invites", member, oid,
		map[string]any{"email": "x@y.test", "role": "member"})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestCreateInvite_InvalidRole_400(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedUserAndOrg(t, q)

	router, signer, _ := newTestRouterRedis(t, q)
	req := requestWithSessionBody(t, signer, http.MethodPost,
		"/api/orgs/"+oid.String()+"/invites", uid, oid,
		map[string]any{"email": "x@y.test", "role": "owner"})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestCreateInvite_AlreadyMember_409(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedUserAndOrg(t, q)
	existing := seedUser(t, q)
	addMember(t, q, oid, existing, "member")
	// Look up existing user's email for the invite address.
	u, err := q.GetUserByID(context.Background(), store.UUID(existing))
	if err != nil {
		t.Fatalf("get user: %v", err)
	}

	router, signer, rdb := newTestRouterRedis(t, q)
	rdb.Del(context.Background(), "invite:inviter:"+uid.String(), "invite:email:"+u.Email)

	req := requestWithSessionBody(t, signer, http.MethodPost,
		"/api/orgs/"+oid.String()+"/invites", uid, oid,
		map[string]any{"email": u.Email, "role": "member"})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d want 409; body=%s", rec.Code, rec.Body.String())
	}
}

// --- DELETE /api/orgs/{org_id}/invites/{id} ---

func TestDeleteInvite_Admin_204(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedUserAndOrg(t, q)
	inviteID := stageInvite(t, q, oid, uid, "x+"+uuid.NewString()+"@example.test", "member")

	router, signer := newTestRouter(q)
	req := requestWithSession(t, signer, http.MethodDelete,
		"/api/orgs/"+oid.String()+"/invites/"+inviteID.String(), uid, oid)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status: got %d want 204; body=%s", rec.Code, rec.Body.String())
	}
	rows, _ := q.ListOrgInvitesByOrg(context.Background(), store.UUID(oid))
	for _, r := range rows {
		if store.GoUUID(r.ID) == inviteID {
			t.Errorf("invite still present after delete")
		}
	}
}

func TestDeleteInvite_MemberCannot_403(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedUserAndOrg(t, q)
	member := seedUser(t, q)
	addMember(t, q, oid, member, "member")
	inviteID := stageInvite(t, q, oid, uid, "x+"+uuid.NewString()+"@example.test", "member")

	router, signer := newTestRouter(q)
	req := requestWithSession(t, signer, http.MethodDelete,
		"/api/orgs/"+oid.String()+"/invites/"+inviteID.String(), member, oid)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// --- GET /api/invites/{token} ---

func TestPeekInvite_PublicReturnsOrgInfo(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedUserAndOrg(t, q)
	email := "peek+" + uuid.NewString() + "@example.test"
	tok, err := auth.IssueOrgInvite(context.Background(), q, oid, uid, email, auth.RoleMember, time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	router, _ := newTestRouter(q)
	req := httptest.NewRequest(http.MethodGet, "/api/invites/"+tok, nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["email"] != email {
		t.Errorf("email: got %v want %s", resp["email"], email)
	}
	if resp["role"] != "member" {
		t.Errorf("role: got %v want member", resp["role"])
	}
	if resp["org_id"] != oid.String() {
		t.Errorf("org_id: got %v want %s", resp["org_id"], oid)
	}
}

func TestPeekInvite_Unknown_404(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)

	router, _ := newTestRouter(q)
	req := httptest.NewRequest(http.MethodGet, "/api/invites/garbage", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// --- POST /api/invites/{token}/accept ---

func TestAcceptInvite_SignedInMatchingEmail_AddsMember(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	ctx := context.Background()
	inviter, oid := seedUserAndOrg(t, q)

	// Invitee with matching email + their own org for the initial session.
	inviteeEmail := "accept+" + uuid.NewString() + "@example.test"
	u, err := q.CreateUser(ctx, store.CreateUserParams{Email: inviteeEmail})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	// Give them a personal org so they have a valid active_org_id.
	personalOrg, err := q.CreateOrganization(ctx, store.CreateOrganizationParams{
		Name: "Personal",
		Slug: "personal-" + uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("create personal: %v", err)
	}
	if err := q.AddOrgMember(ctx, store.AddOrgMemberParams{
		OrgID: personalOrg.ID, UserID: u.ID, Role: "owner",
	}); err != nil {
		t.Fatalf("personal owner: %v", err)
	}

	tok, err := auth.IssueOrgInvite(ctx, q, oid, inviter, inviteeEmail, auth.RoleMember, time.Hour)
	if err != nil {
		t.Fatalf("issue invite: %v", err)
	}

	router, signer := newTestRouterPool(pool, q)
	req := requestWithSession(t, signer, http.MethodPost,
		"/api/invites/"+tok+"/accept", store.GoUUID(u.ID), store.GoUUID(personalOrg.ID))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", rec.Code, rec.Body.String())
	}
	// Invitee should now be a member of the inviter's org.
	m, err := q.GetOrgMember(ctx, store.GetOrgMemberParams{
		OrgID: store.UUID(oid), UserID: u.ID,
	})
	if err != nil {
		t.Fatalf("get member: %v", err)
	}
	if m.Role != "member" {
		t.Errorf("role: got %q want member", m.Role)
	}
	// Response must include the org we joined.
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["org_id"] != oid.String() {
		t.Errorf("org_id: got %v want %s", resp["org_id"], oid)
	}
}

func TestAcceptInvite_SignedInWrongEmail_403(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	ctx := context.Background()
	inviter, oid := seedUserAndOrg(t, q)

	tok, err := auth.IssueOrgInvite(ctx, q, oid, inviter,
		"someone-else+"+uuid.NewString()+"@example.test", auth.RoleMember, time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	// Signed-in caller is a *different* user (the inviter, not the invitee).
	router, signer := newTestRouterPool(pool, q)
	req := requestWithSession(t, signer, http.MethodPost,
		"/api/invites/"+tok+"/accept", inviter, oid)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestAcceptInvite_SignedOut_RequiresLogin(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	ctx := context.Background()
	inviter, oid := seedUserAndOrg(t, q)

	email := "logged-out+" + uuid.NewString() + "@example.test"
	tok, err := auth.IssueOrgInvite(ctx, q, oid, inviter, email, auth.RoleMember, time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	router, _ := newTestRouterPool(pool, q)
	req := httptest.NewRequest(http.MethodPost, "/api/invites/"+tok+"/accept", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["requires_login"] != true {
		t.Errorf("expected requires_login=true; got %v", resp)
	}
}

func TestAcceptInvite_Unknown_404(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)

	router, _ := newTestRouterPool(pool, q)
	req := httptest.NewRequest(http.MethodPost, "/api/invites/no-such-token/accept", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// stageInvite inserts an invite row directly (no token redirection through
// IssueOrgInvite — useful for list/delete tests that don't care about the
// plaintext token). Returns the invite id.
func stageInvite(t *testing.T, q *store.Queries, orgID, inviterID uuid.UUID, email, role string) uuid.UUID {
	t.Helper()
	h := sha256.Sum256([]byte("stage-" + uuid.NewString()))
	row, err := q.CreateOrgInvite(context.Background(), store.CreateOrgInviteParams{
		OrgID:     store.UUID(orgID),
		Email:     email,
		Role:      role,
		TokenHash: h[:],
		InvitedBy: store.UUID(inviterID),
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	})
	if err != nil {
		t.Fatalf("stage invite: %v", err)
	}
	return store.GoUUID(row.ID)
}

// =============================================================================
// Extended coverage: the invite token lifecycle and its privilege boundaries.
// Harness (idEnv, stmtTracer, wantErr, ...) lives in orgs_test.go.
// =============================================================================

// withRedis wires a live Redis client into the router's Deps.
func withRedis(t *testing.T) func(*Deps) {
	rdb := ssoRedis(t)
	return func(d *Deps) { d.Redis = rdb }
}

// deadRedis is a client whose every command fails fast.
func deadRedis(t *testing.T) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 200 * time.Millisecond})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func uniqEmail(tag string) string { return tag + "+" + uuid.NewString() + "@example.test" }

// invitesFor returns the org's invite rows for one email: id, role, accepted.
func (e *idEnv) invitesFor(org uuid.UUID, email string) (n int, role string, accepted bool) {
	e.t.Helper()
	rows, err := e.pool.Query(context.Background(),
		`SELECT role, accepted_at IS NOT NULL FROM org_invites WHERE org_id=$1 AND email=$2`, org, email)
	if err != nil {
		e.t.Fatalf("invites for: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		n++
		if err := rows.Scan(&role, &accepted); err != nil {
			e.t.Fatalf("scan: %v", err)
		}
	}
	return
}

func (e *idEnv) magicLinks(email string) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM magic_link_tokens WHERE email=$1`, email).Scan(&n); err != nil {
		e.t.Fatalf("magic links: %v", err)
	}
	return n
}

func (e *idEnv) userExists(email string) bool {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM users WHERE email=$1`, email).Scan(&n); err != nil {
		e.t.Fatalf("user exists: %v", err)
	}
	return n > 0
}

// invitee creates a user (no org) with the given email.
func (e *idEnv) invitee(email string) uuid.UUID {
	e.t.Helper()
	u, err := e.q.CreateUser(context.Background(), store.CreateUserParams{Email: email})
	if err != nil {
		e.t.Fatalf("create invitee: %v", err)
	}
	return store.GoUUID(u.ID)
}

func (e *idEnv) issue(org, by uuid.UUID, email string, role auth.Role, ttl time.Duration) string {
	e.t.Helper()
	tok, err := auth.IssueOrgInvite(context.Background(), e.q, org, by, email, role, ttl)
	if err != nil {
		e.t.Fatalf("issue invite: %v", err)
	}
	return tok
}

func acceptPath(tok string) string { return "/api/invites/" + tok + "/accept" }

// --- GET /api/orgs/{org_id}/invites ---

func TestListInvites_ShapeAndRefusals(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	owner, org := e.seedOrg()
	member := e.seedRole(org, "member")
	stranger, strangerOrg := e.seedOrg()
	email := uniqEmail("listed")
	tok := e.issue(org, owner, email, auth.RoleMember, time.Hour)
	// Another org's invite must never show up here.
	foreign := uniqEmail("foreign")
	e.issue(strangerOrg, stranger, foreign, auth.RoleMember, time.Hour)
	// An accepted invite shows its accepted_at.
	invitee := e.invitee(uniqEmail("acc"))
	var inviteeEmail string
	_ = e.pool.QueryRow(context.Background(), `SELECT email FROM users WHERE id=$1`, invitee).Scan(&inviteeEmail)
	accTok := e.issue(org, owner, inviteeEmail, auth.RoleMember, time.Hour)
	wantStatus(t, e.do(http.MethodPost, acceptPath(accTok), invitee, uuid.New(), nil), http.StatusOK)

	rec := e.do(http.MethodGet, orgPath(org, "/invites"), member, org, nil)
	wantStatus(t, rec, http.StatusOK)
	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byEmail := map[string]map[string]any{}
	for _, r := range rows {
		byEmail[r["email"].(string)] = r
		for _, forbidden := range []string{"token_hash", "token", "invited_by"} {
			if _, ok := r[forbidden]; ok {
				t.Errorf("listing exposes %q: %v", forbidden, r)
			}
		}
	}
	if _, leaked := byEmail[foreign]; leaked {
		t.Fatal("listing leaked another org's invite")
	}
	pending, ok := byEmail[email]
	if !ok || pending["role"] != "member" || pending["invited_by_email"] == nil || pending["accepted_at"] != nil {
		t.Fatalf("pending invite row: %v", pending)
	}
	if acc, ok := byEmail[inviteeEmail]; !ok || acc["accepted_at"] == nil {
		t.Fatalf("accepted invite should carry accepted_at: %v", acc)
	}
	// The token is only in the invitee's inbox, never in a response.
	if strings.Contains(rec.Body.String(), tok) {
		t.Fatal("listing response contains a live invite token")
	}

	wantErr(t, e.do(http.MethodGet, "/api/orgs/zzz/invites", owner, org, nil), http.StatusBadRequest, "invalid org_id")
	wantErr(t, e.do(http.MethodGet, orgPath(org, "/invites"), stranger, strangerOrg, nil), http.StatusForbidden, "not a member")
}

func TestListInvites_DBFailure_500(t *testing.T) {
	e := newIDEnv(t, failOn("invited_by_email"), nil)
	owner, org := e.seedOrg()
	wantErr(t, e.do(http.MethodGet, orgPath(org, "/invites"), owner, org, nil), http.StatusInternalServerError, "list invites")
}

// --- POST /api/orgs/{org_id}/invites ---

func TestCreateInvite_Refusals_CreateNothing(t *testing.T) {
	e := newIDEnv(t, nil, withRedis(t))
	owner, org := e.seedOrg()
	admin := e.seedRole(org, "admin")
	member := e.seedRole(org, "member")
	stranger, strangerOrg := e.seedOrg()
	before := e.snapshot(org)
	ok := func() string { return uniqEmail("ok") }

	for _, tc := range []struct {
		name string
		path string
		as   uuid.UUID
		asOr uuid.UUID
		body any
		code int
		msg  string
	}{
		{"bad org id", "/api/orgs/zzz/invites", owner, org, map[string]any{"email": ok(), "role": "member"}, 400, "invalid org_id"},
		{"cross-org: stranger's owner", orgPath(org, "/invites"), stranger, strangerOrg, map[string]any{"email": ok(), "role": "member"}, 403, "not a member"},
		{"member", orgPath(org, "/invites"), member, org, map[string]any{"email": ok(), "role": "member"}, 403, "admin required"},
		{"bad json", orgPath(org, "/invites"), admin, org, "{nope", 400, "invalid json"},
		{"empty email", orgPath(org, "/invites"), admin, org, map[string]any{"email": "  ", "role": "member"}, 400, "invalid email"},
		{"email without @", orgPath(org, "/invites"), admin, org, map[string]any{"email": "nobody", "role": "member"}, 400, "invalid email"},
		// Ownership is never grantable by invite, whoever asks: an admin
		// cannot mint a role above their own, and an owner must transfer.
		{"admin invites an owner", orgPath(org, "/invites"), admin, org, map[string]any{"email": ok(), "role": "owner"}, 400, "role must be admin or member"},
		{"owner invites an owner", orgPath(org, "/invites"), owner, org, map[string]any{"email": ok(), "role": "owner"}, 400, "role must be admin or member"},
		{"empty role", orgPath(org, "/invites"), admin, org, map[string]any{"email": ok(), "role": ""}, 400, "role must be admin or member"},
		{"unknown role", orgPath(org, "/invites"), admin, org, map[string]any{"email": ok(), "role": "superuser"}, 400, "role must be admin or member"},
		{"role is case sensitive", orgPath(org, "/invites"), admin, org, map[string]any{"email": ok(), "role": "Admin"}, 400, "role must be admin or member"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantErr(t, e.do(http.MethodPost, tc.path, tc.as, tc.asOr, tc.body), tc.code, tc.msg)
			e.sameSnapshot(org, before)
		})
	}
	e.sameSnapshot(org, before)
	if n := e.auditCount(org, "member.invite"); n != 0 {
		t.Fatalf("refused invites wrote %d audit rows", n)
	}
}

func TestCreateInvite_Admin_StoresLowercasedAdminInviteAuditsAndEnqueues(t *testing.T) {
	rdb := ssoRedis(t)
	pfx := "invtest-" + uuid.NewString()
	t.Cleanup(func() {
		if keys, _ := rdb.Keys(context.Background(), pfx+":*").Result(); len(keys) > 0 {
			rdb.Del(context.Background(), keys...)
		}
	})
	e := newIDEnv(t, nil, func(d *Deps) { d.Redis = rdb; d.Queue = dqueue.NewClient(rdb).WithPrefix(pfx) })
	_, org := e.seedOrg()
	admin := e.seedRole(org, "admin")
	lower := uniqEmail("mixed")
	// Equal to the inviter's own role is allowed; above it is not (see refusals).
	rec := e.do(http.MethodPost, orgPath(org, "/invites"), admin, org,
		map[string]any{"email": "  " + strings.ToUpper(lower[:1]) + lower[1:] + " ", "role": "admin"})
	wantStatus(t, rec, http.StatusAccepted)

	n, role, accepted := e.invitesFor(org, lower)
	if n != 1 || role != "admin" || accepted {
		t.Fatalf("invite row: n=%d role=%q accepted=%v want 1/admin/false", n, role, accepted)
	}
	if c := e.auditCount(org, "member.invite"); c != 1 {
		t.Errorf("member.invite audit rows: got %d want 1", c)
	}
	keys, err := rdb.Keys(context.Background(), pfx+":*").Result()
	if err != nil || len(keys) == 0 {
		t.Errorf("no email task was enqueued under %s (err=%v)", pfx, err)
	}
}

func TestCreateInvite_ExistingUserOfAnotherOrg_Invitable(t *testing.T) {
	e := newIDEnv(t, nil, withRedis(t))
	owner, org := e.seedOrg()
	email := uniqEmail("elsewhere")
	other := e.invitee(email)
	_, otherOrg := e.seedOrg()
	addMember(t, e.q, otherOrg, other, "owner")

	wantStatus(t, e.do(http.MethodPost, orgPath(org, "/invites"), owner, org, map[string]any{"email": email, "role": "member"}), http.StatusAccepted)
	if n, _, _ := e.invitesFor(org, email); n != 1 {
		t.Fatalf("invite rows: got %d want 1", n)
	}
	if _, member := e.members(org)[other]; member {
		t.Fatal("an invite must not add the user before they accept")
	}
}

func TestCreateInvite_PendingDuplicate_409_AlreadyMember_409(t *testing.T) {
	e := newIDEnv(t, nil, withRedis(t))
	owner, org := e.seedOrg()
	email := uniqEmail("dup")
	body := map[string]any{"email": email, "role": "member"}
	wantStatus(t, e.do(http.MethodPost, orgPath(org, "/invites"), owner, org, body), http.StatusAccepted)
	// The partial unique index on (org_id, email) WHERE accepted_at IS NULL is
	// a real constraint: the second insert raises SQLSTATE 23505.
	wantErr(t, e.do(http.MethodPost, orgPath(org, "/invites"), owner, org,
		map[string]any{"email": email, "role": "admin"}), http.StatusConflict, "pending invite exists")
	if n, role, _ := e.invitesFor(org, email); n != 1 || role != "member" {
		t.Fatalf("invite rows: n=%d role=%q want the original single member invite", n, role)
	}

	existing := uniqEmail("already")
	u := e.invitee(existing)
	addMember(t, e.q, org, u, "member")
	wantErr(t, e.do(http.MethodPost, orgPath(org, "/invites"), owner, org,
		map[string]any{"email": existing, "role": "admin"}), http.StatusConflict, "already a member")
	if n, _, _ := e.invitesFor(org, existing); n != 0 {
		t.Fatalf("an invite was stored for an existing member: %d", n)
	}
	if e.members(org)[u] != "member" {
		t.Fatal("the existing member's role changed")
	}
}

func TestCreateInvite_RateLimits(t *testing.T) {
	t.Run("per invitee email", func(t *testing.T) {
		e := newIDEnv(t, nil, withRedis(t))
		owner, org := e.seedOrg()
		email := uniqEmail("flood")
		var last *httptest.ResponseRecorder
		for i := 0; i < 11; i++ {
			last = e.do(http.MethodPost, orgPath(org, "/invites"), owner, org, map[string]any{"email": email, "role": "member"})
			if i < 10 && last.Code == http.StatusTooManyRequests {
				t.Fatalf("request %d limited too early", i+1)
			}
		}
		wantErr(t, last, http.StatusTooManyRequests, "rate limited")
		if ra, err := strconv.Atoi(last.Header().Get("Retry-After")); err != nil || ra < 1 {
			t.Fatalf("Retry-After: got %q want a positive integer of seconds", last.Header().Get("Retry-After"))
		}
	})
	t.Run("per inviter", func(t *testing.T) {
		e := newIDEnv(t, nil, withRedis(t))
		owner, org := e.seedOrg()
		var last *httptest.ResponseRecorder
		for i := 0; i < 31; i++ {
			last = e.do(http.MethodPost, orgPath(org, "/invites"), owner, org, map[string]any{"email": uniqEmail("spray"), "role": "member"})
			if i < 30 && last.Code != http.StatusAccepted {
				t.Fatalf("request %d: got %d want 202; body=%s", i+1, last.Code, last.Body.String())
			}
		}
		wantErr(t, last, http.StatusTooManyRequests, "rate limited")
		if last.Header().Get("Retry-After") == "" {
			t.Fatal("429 without Retry-After")
		}
	})
	t.Run("limiter down fails closed", func(t *testing.T) {
		e := newIDEnv(t, nil, func(d *Deps) { d.Redis = deadRedis(t) })
		owner, org := e.seedOrg()
		email := uniqEmail("down")
		wantErr(t, e.do(http.MethodPost, orgPath(org, "/invites"), owner, org, map[string]any{"email": email, "role": "member"}),
			http.StatusServiceUnavailable, "rate limiter unavailable")
		if n, _, _ := e.invitesFor(org, email); n != 0 {
			t.Fatalf("an invite was stored while the limiter was down: %d", n)
		}
	})
}

func TestCreateInvite_MidFlow(t *testing.T) {
	t.Run("insert fails", func(t *testing.T) {
		e := newIDEnv(t, failOn("insert into org_invites"), withRedis(t))
		owner, org := e.seedOrg()
		email := uniqEmail("fail")
		wantErr(t, e.do(http.MethodPost, orgPath(org, "/invites"), owner, org, map[string]any{"email": email, "role": "member"}),
			http.StatusInternalServerError, "issue invite")
		if n, _, _ := e.invitesFor(org, email); n != 0 {
			t.Fatalf("invite rows after a failed insert: %d", n)
		}
		if c := e.auditCount(org, "member.invite"); c != 0 {
			t.Fatalf("a failed invite wrote %d audit rows", c)
		}
	})
	t.Run("mail enqueue fails: invite still stands", func(t *testing.T) {
		dq := dqueue.NewClient(deadRedis(t))
		e := newIDEnv(t, nil, func(d *Deps) { d.Redis = ssoRedis(t); d.Queue = dq })
		owner, org := e.seedOrg()
		email := uniqEmail("nomail")
		wantStatus(t, e.do(http.MethodPost, orgPath(org, "/invites"), owner, org, map[string]any{"email": email, "role": "member"}), http.StatusAccepted)
		if n, _, _ := e.invitesFor(org, email); n != 1 {
			t.Fatalf("invite rows: got %d want 1", n)
		}
	})
}

// --- DELETE /api/orgs/{org_id}/invites/{id} ---

func TestDeleteInvite_Refusals_Isolation_AndRevokesToken(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	owner, org := e.seedOrg()
	admin := e.seedRole(org, "admin")
	stranger, strangerOrg := e.seedOrg()
	email := uniqEmail("revoke")
	tok := e.issue(org, owner, email, auth.RoleMember, time.Hour)
	var id uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `SELECT id FROM org_invites WHERE org_id=$1 AND email=$2`, org, email).Scan(&id); err != nil {
		t.Fatal(err)
	}
	before := e.snapshot(org)

	wantErr(t, e.do(http.MethodDelete, "/api/orgs/zzz/invites/"+id.String(), owner, org, nil), http.StatusBadRequest, "invalid org_id")
	wantErr(t, e.do(http.MethodDelete, orgPath(org, "/invites/zzz"), owner, org, nil), http.StatusBadRequest, "invalid invite id")
	// Cross-org, the victim org in the path: refused outright.
	wantErr(t, e.do(http.MethodDelete, orgPath(org, "/invites/"+id.String()), stranger, strangerOrg, nil), http.StatusForbidden, "not a member")
	// Cross-org, the attacker's own org in the path with the victim's invite id:
	// the delete is scoped by org_id, so it matches nothing.
	// Documents CURRENT behaviour, not contract: a delete that matched no row
	// still answers 204 success (and audits invite.revoke for the stranger's org).
	wantStatus(t, e.do(http.MethodDelete, orgPath(strangerOrg, "/invites/"+id.String()), stranger, strangerOrg, nil), http.StatusNoContent)
	e.sameSnapshot(org, before)
	if n := e.auditCount(org, "invite.revoke"); n != 0 {
		t.Fatalf("refused/foreign deletes wrote %d audit rows against the victim org", n)
	}
	if n := e.auditCount(strangerOrg, "invite.revoke"); n != 1 {
		t.Fatalf("stranger org invite.revoke audit rows: got %d want 1 (current behaviour: a no-op delete is audited)", n)
	}
	if n, _, _ := e.invitesFor(strangerOrg, email); n != 0 {
		t.Fatalf("the stranger's org gained an invite: %d", n)
	}
	wantStatus(t, e.anon(http.MethodGet, "/api/invites/"+tok), http.StatusOK) // still redeemable

	// A real revoke: the row is gone and the token dies with it.
	wantStatus(t, e.do(http.MethodDelete, orgPath(org, "/invites/"+id.String()), admin, org, nil), http.StatusNoContent)
	if n, _, _ := e.invitesFor(org, email); n != 0 {
		t.Fatalf("invite survived revoke: %d", n)
	}
	if n := e.auditCount(org, "invite.revoke"); n != 1 {
		t.Fatalf("invite.revoke audit rows: got %d want 1", n)
	}
	wantErr(t, e.anon(http.MethodGet, "/api/invites/"+tok), http.StatusNotFound, "invalid invite")
	// Revoked token, matching signed-in user: nothing to accept.
	u := e.invitee(email)
	wantErr(t, e.do(http.MethodPost, acceptPath(tok), u, uuid.New(), nil), http.StatusNotFound, "invalid invite")
	if _, member := e.members(org)[u]; member {
		t.Fatal("a revoked invite added a member")
	}
}

func TestDeleteInvite_DBFailure_500(t *testing.T) {
	e := newIDEnv(t, failOn("delete from org_invites"), nil)
	owner, org := e.seedOrg()
	email := uniqEmail("keep")
	id := stageInvite(t, e.q, org, owner, email, "member")
	wantErr(t, e.do(http.MethodDelete, orgPath(org, "/invites/"+id.String()), owner, org, nil), http.StatusInternalServerError, "delete invite")
	if n, _, _ := e.invitesFor(org, email); n != 1 {
		t.Fatalf("invite rows: got %d want it untouched", n)
	}
	if n := e.auditCount(org, "invite.revoke"); n != 0 {
		t.Fatalf("a failed delete wrote %d audit rows", n)
	}
}

// --- GET /api/invites/{token} ---

func TestPeekInvite_NotRedeemable_404(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	owner, org := e.seedOrg()

	expired := e.issue(org, owner, uniqEmail("old"), auth.RoleMember, -time.Hour)
	wantErr(t, e.anon(http.MethodGet, "/api/invites/"+expired), http.StatusNotFound, "invalid invite")

	email := uniqEmail("used")
	used := e.issue(org, owner, email, auth.RoleMember, time.Hour)
	u := e.invitee(email)
	wantStatus(t, e.do(http.MethodPost, acceptPath(used), u, uuid.New(), nil), http.StatusOK)
	wantErr(t, e.anon(http.MethodGet, "/api/invites/"+used), http.StatusNotFound, "invalid invite")
}

func TestPeekInvite_ShowsOnlyWhatTheRecipientNeeds(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	owner, org := e.seedOrg()
	email := uniqEmail("peek")
	tok := e.issue(org, owner, email, auth.RoleAdmin, time.Hour)
	rec := e.anon(http.MethodGet, "/api/invites/"+tok)
	wantStatus(t, rec, http.StatusOK)
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	name, _ := e.orgName(org)
	if got["org_name"] != name || got["role"] != "admin" || got["email"] != email || got["expires_at"] == nil {
		t.Fatalf("peek body: %v", got)
	}
	if len(got) != 5 {
		t.Fatalf("peek exposes extra fields: %v", got)
	}
}

func TestPeekInvite_LookupFailure_404(t *testing.T) {
	e := newIDEnv(t, failOn("for update of i"), nil)
	owner, org := e.seedOrg()
	tok := e.issue(org, owner, uniqEmail("db"), auth.RoleMember, time.Hour)
	wantErr(t, e.anon(http.MethodGet, "/api/invites/"+tok), http.StatusNotFound, "invalid invite")
}

func TestPeekInvite_RateLimit(t *testing.T) {
	e := newIDEnv(t, nil, withRedis(t))
	addr := uniqueClientAddr()
	var last *httptest.ResponseRecorder
	for i := 0; i < 61; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/invites/probe-"+uuid.NewString(), nil)
		req.RemoteAddr = addr
		last = httptest.NewRecorder()
		e.h.ServeHTTP(last, req)
		if i < 60 && last.Code != http.StatusNotFound {
			t.Fatalf("probe %d: got %d want 404", i+1, last.Code)
		}
	}
	wantErr(t, last, http.StatusTooManyRequests, "too many requests")
	if last.Header().Get("Retry-After") == "" {
		t.Fatal("429 without Retry-After")
	}
}

// Peek fails OPEN when Redis is down: a public, read-only probe must not turn
// a Redis outage into a 5xx for a legitimate recipient.
func TestPeekInvite_RedisDown_FailsOpen(t *testing.T) {
	e := newIDEnv(t, nil, func(d *Deps) { d.Redis = deadRedis(t) })
	owner, org := e.seedOrg()
	tok := e.issue(org, owner, uniqEmail("open"), auth.RoleMember, time.Hour)
	wantStatus(t, e.anon(http.MethodGet, "/api/invites/"+tok), http.StatusOK)
}

// chi routes an empty {token} segment to Accept but not to Peek: the first is
// answered by the handler's own guard, the second never reaches a handler.
func TestInvites_EmptyTokenSegment(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	wantErr(t, e.anon(http.MethodPost, "/api/invites//accept"), http.StatusBadRequest, "missing token")
	if rec := e.anon(http.MethodGet, "/api/invites/"); rec.Code != http.StatusNotFound {
		t.Errorf("GET /api/invites/: got %d want 404 from the router", rec.Code)
	}
}

// --- POST /api/invites/{token}/accept ---

func TestAcceptInvite_SameTokenTwice_SecondRefused(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	owner, org := e.seedOrg()
	email := uniqEmail("twice")
	u := e.invitee(email)
	tok := e.issue(org, owner, email, auth.RoleAdmin, time.Hour)

	rec := e.do(http.MethodPost, acceptPath(tok), u, uuid.New(), nil)
	wantStatus(t, rec, http.StatusOK)
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["org_id"] != org.String() || body["role"] != "admin" {
		t.Fatalf("accept body: %v", body)
	}
	if gu, go_ := e.cookieOrg(rec); gu != u || go_ != org {
		t.Fatalf("cookie: got user=%s org=%s want %s / %s", gu, go_, u, org)
	}
	if e.members(org)[u] != "admin" {
		t.Fatalf("role: got %q want admin", e.members(org)[u])
	}
	if n, _, accepted := e.invitesFor(org, email); n != 1 || !accepted {
		t.Fatalf("invite: n=%d accepted=%v want 1/true", n, accepted)
	}
	if c := e.auditCount(org, "member.accept_invite"); c != 1 {
		t.Fatalf("member.accept_invite audit rows: got %d want 1", c)
	}

	// Demote the new member, then replay the same token: it must not restore
	// the role, re-add them, or write a second audit row.
	wantStatus(t, e.do(http.MethodPatch, memberPath(org, u), owner, org, map[string]any{"role": "member"}), http.StatusNoContent)
	rec = e.do(http.MethodPost, acceptPath(tok), u, org, nil)
	wantErr(t, rec, http.StatusNotFound, "invalid invite")
	if sessionCookie(rec) != nil {
		t.Fatal("a refused replay must not issue a cookie")
	}
	if e.members(org)[u] != "member" {
		t.Fatalf("replay changed the role: %q", e.members(org)[u])
	}
	if c := e.auditCount(org, "member.accept_invite"); c != 1 {
		t.Fatalf("replay wrote audit rows: %d", c)
	}
	// Removed and replayed: still no way back in.
	wantStatus(t, e.do(http.MethodDelete, memberPath(org, u), owner, org, nil), http.StatusNoContent)
	wantErr(t, e.do(http.MethodPost, acceptPath(tok), u, uuid.New(), nil), http.StatusNotFound, "invalid invite")
	if _, member := e.members(org)[u]; member {
		t.Fatal("a replayed token re-added a removed member")
	}
}

func TestAcceptInvite_Expired_404_NoMembership(t *testing.T) {
	e := newIDEnv(t, nil, withRedis(t))
	owner, org := e.seedOrg()
	email := uniqEmail("late")
	u := e.invitee(email)
	tok := e.issue(org, owner, email, auth.RoleMember, -time.Minute)

	wantErr(t, e.do(http.MethodPost, acceptPath(tok), u, uuid.New(), nil), http.StatusNotFound, "invalid invite")
	// Signed out: an expired token must not even mint a sign-in link.
	wantErr(t, e.anon(http.MethodPost, acceptPath(tok)), http.StatusNotFound, "invalid invite")
	if _, member := e.members(org)[u]; member {
		t.Fatal("an expired invite added a member")
	}
	if n, _, accepted := e.invitesFor(org, email); n != 1 || accepted {
		t.Fatalf("invite: n=%d accepted=%v want untouched", n, accepted)
	}
	if n := e.magicLinks(email); n != 0 {
		t.Fatalf("expired token minted %d magic links", n)
	}
}

func TestAcceptInvite_WrongAccount_403_NothingConsumed(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	owner, org := e.seedOrg()
	email := uniqEmail("target")
	tok := e.issue(org, owner, email, auth.RoleMember, time.Hour)
	other := e.invitee(uniqEmail("someone-else"))

	rec := e.do(http.MethodPost, acceptPath(tok), other, uuid.New(), nil)
	wantErr(t, rec, http.StatusForbidden, "invite addressed to different email")
	if sessionCookie(rec) != nil {
		t.Fatal("a refused accept must not issue a cookie")
	}
	if _, member := e.members(org)[other]; member {
		t.Fatal("the wrong account joined the org")
	}
	if n, _, accepted := e.invitesFor(org, email); n != 1 || accepted {
		t.Fatalf("invite: n=%d accepted=%v want still pending", n, accepted)
	}
	// ...and it is still redeemable by the right person.
	u := e.invitee(email)
	wantStatus(t, e.do(http.MethodPost, acceptPath(tok), u, uuid.New(), nil), http.StatusOK)
	if e.members(org)[u] != "member" {
		t.Fatal("the intended invitee could not redeem the token")
	}
}

// A session whose epoch was revoked (logout-all) is refused rather than
// allowed to consume the invite.
// Documents CURRENT behaviour, not contract: the refusal is right but the
// message is wrong, a revoked-epoch session is told "different email".
func TestAcceptInvite_RevokedSession_403(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	owner, org := e.seedOrg()
	email := uniqEmail("revoked")
	u := e.invitee(email)
	tok := e.issue(org, owner, email, auth.RoleMember, time.Hour)
	if err := e.q.BumpUserSessionEpoch(context.Background(), store.UUID(u)); err != nil {
		t.Fatal(err)
	}
	wantErr(t, e.do(http.MethodPost, acceptPath(tok), u, uuid.New(), nil), http.StatusForbidden, "invite addressed to different email")
	if _, member := e.members(org)[u]; member {
		t.Fatal("a revoked session joined the org")
	}
}

func TestAcceptInvite_EmailMatchIsCaseInsensitive(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	owner, org := e.seedOrg()
	lower := uniqEmail("case")
	u := e.invitee(strings.ToUpper(lower[:1]) + lower[1:]) // stored with a capital
	tok := e.issue(org, owner, lower, auth.RoleMember, time.Hour)
	wantStatus(t, e.do(http.MethodPost, acceptPath(tok), u, uuid.New(), nil), http.StatusOK)
	if e.members(org)[u] != "member" {
		t.Fatal("a case-differing email did not match its own invite")
	}
}

// Accepting an invite never changes the role of someone already in the org:
// a re-issued higher invite is not a back door to privilege, and a lower one
// is not a way to demote.
func TestAcceptInvite_AlreadyMember_RoleUnchanged(t *testing.T) {
	for _, tc := range []struct {
		have string
		want auth.Role
	}{
		{"member", auth.RoleAdmin},
		{"admin", auth.RoleMember},
		{"owner", auth.RoleMember},
	} {
		t.Run(tc.have+" accepts "+string(tc.want), func(t *testing.T) {
			e := newIDEnv(t, nil, nil)
			inviter, org := e.seedOrg()
			email := uniqEmail("already")
			u := e.invitee(email)
			addMember(t, e.q, org, u, tc.have)
			// Seed with a pending invite, as a direct insert, since the API
			// would refuse an invite for an existing member.
			tok := e.issue(org, inviter, email, tc.want, time.Hour)
			rec := e.do(http.MethodPost, acceptPath(tok), u, org, nil)
			wantStatus(t, rec, http.StatusOK)
			if got := e.members(org)[u]; got != tc.have {
				t.Fatalf("role: got %q want unchanged %q", got, tc.have)
			}
			if _, _, accepted := e.invitesFor(org, email); !accepted {
				t.Fatal("the invite should be consumed")
			}
			// Single use, even though it changed nothing.
			wantErr(t, e.do(http.MethodPost, acceptPath(tok), u, org, nil), http.StatusNotFound, "invalid invite")
		})
	}
}

func TestAcceptInvite_SignedOut_ThenSignsIn_JoinsOrg(t *testing.T) {
	e := newIDEnv(t, nil, withRedis(t))
	owner, org := e.seedOrg()
	email := uniqEmail("newcomer")
	tok := e.issue(org, owner, email, auth.RoleAdmin, time.Hour)

	// Signed out: no consumption, only a sign-in link for the invited address.
	rec := e.anon(http.MethodPost, acceptPath(tok))
	wantStatus(t, rec, http.StatusOK)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["requires_login"] != true {
		t.Fatalf("body: %v", body)
	}
	if sessionCookie(rec) != nil {
		t.Fatal("a signed-out accept must not issue a session")
	}
	if n := e.magicLinks(email); n != 1 {
		t.Fatalf("magic links minted: got %d want 1", n)
	}
	if e.userExists(email) {
		t.Fatal("an unauthenticated accept created an account")
	}
	if n, _, accepted := e.invitesFor(org, email); n != 1 || accepted {
		t.Fatalf("invite: n=%d accepted=%v want still pending", n, accepted)
	}

	// The invitee signs in with the link we minted for them.
	link, err := auth.IssueMagicLink(context.Background(), e.q, email, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/auth/magic-link/verify", strings.NewReader(`{"token":"`+link+`"}`))
	req.Header.Set("Content-Type", "application/json")
	vrec := httptest.NewRecorder()
	e.h.ServeHTTP(vrec, req)
	wantStatus(t, vrec, http.StatusNoContent)

	u, err := e.q.GetUserByEmail(context.Background(), email)
	if err != nil {
		t.Fatalf("user after sign-in: %v", err)
	}
	uid := store.GoUUID(u.ID)
	if e.members(org)[uid] != "admin" {
		t.Fatalf("role after sign-in: got %q want admin", e.members(org)[uid])
	}
	if n, _, accepted := e.invitesFor(org, email); n != 1 || !accepted {
		t.Fatalf("invite: n=%d accepted=%v want consumed by sign-in", n, accepted)
	}
	// The consumed token cannot be replayed by anyone.
	wantErr(t, e.anon(http.MethodGet, "/api/invites/"+tok), http.StatusNotFound, "invalid invite")
	wantErr(t, e.do(http.MethodPost, acceptPath(tok), uid, org, nil), http.StatusNotFound, "invalid invite")
}

func TestAcceptInvite_SessionForUnknownUser_FallsBackToSignIn(t *testing.T) {
	e := newIDEnv(t, nil, withRedis(t))
	owner, org := e.seedOrg()
	email := uniqEmail("ghost")
	tok := e.issue(org, owner, email, auth.RoleMember, time.Hour)
	// A correctly signed cookie for a user row that does not exist.
	rec := e.do(http.MethodPost, acceptPath(tok), uuid.New(), uuid.New(), nil)
	wantStatus(t, rec, http.StatusOK)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["requires_login"] != true {
		t.Fatalf("body: %v", body)
	}
	if got := e.members(org); len(got) != 1 {
		t.Fatalf("members: %v want only the owner", got)
	}
}

func TestAcceptInvite_SignedOut_RateLimits(t *testing.T) {
	t.Run("per invitee email", func(t *testing.T) {
		e := newIDEnv(t, nil, withRedis(t))
		owner, org := e.seedOrg()
		email := uniqEmail("bomb")
		tok := e.issue(org, owner, email, auth.RoleMember, time.Hour)
		var last *httptest.ResponseRecorder
		for i := 0; i < 6; i++ {
			last = e.anon(http.MethodPost, acceptPath(tok))
			if i < 5 {
				wantStatus(t, last, http.StatusOK)
			}
		}
		wantErr(t, last, http.StatusTooManyRequests, "too many requests")
		if last.Header().Get("Retry-After") == "" {
			t.Fatal("429 without Retry-After")
		}
		if n := e.magicLinks(email); n != 5 {
			t.Fatalf("magic links: got %d want the 5 inside the budget", n)
		}
	})
	t.Run("limiter down fails closed", func(t *testing.T) {
		e := newIDEnv(t, nil, func(d *Deps) { d.Redis = deadRedis(t) })
		owner, org := e.seedOrg()
		email := uniqEmail("down")
		tok := e.issue(org, owner, email, auth.RoleMember, time.Hour)
		wantErr(t, e.anon(http.MethodPost, acceptPath(tok)), http.StatusServiceUnavailable, "rate limiter unavailable")
		if n := e.magicLinks(email); n != 0 {
			t.Fatalf("magic links minted with the limiter down: %d", n)
		}
	})
}

// Under SSO enforcement a signed-out accept would mint a sign-in link and so
// route around the IdP; it is refused. A signed-in invitee is unaffected.
func TestAcceptInvite_SSOEnforced(t *testing.T) {
	e := newIDEnv(t, nil, func(d *Deps) { d.OIDC = config.OIDCConfig{Enforce: true} })
	owner, org := e.seedOrg()
	email := uniqEmail("sso")
	tok := e.issue(org, owner, email, auth.RoleMember, time.Hour)

	wantErr(t, e.anon(http.MethodPost, acceptPath(tok)), http.StatusForbidden, "magic-link sign-in is disabled; use single sign-on")
	if n := e.magicLinks(email); n != 0 {
		t.Fatalf("enforced accept minted %d magic links", n)
	}
	u := e.invitee(email)
	wantStatus(t, e.do(http.MethodPost, acceptPath(tok), u, uuid.New(), nil), http.StatusOK)
	if e.members(org)[u] != "member" {
		t.Fatal("a signed-in invitee must still be able to accept under enforcement")
	}
}

func TestAcceptInvite_MidFlow(t *testing.T) {
	t.Run("lookup fails", func(t *testing.T) {
		e := newIDEnv(t, failOn("for update of i"), nil)
		owner, org := e.seedOrg()
		email := uniqEmail("lk")
		u := e.invitee(email)
		tok := e.issue(org, owner, email, auth.RoleMember, time.Hour)
		wantErr(t, e.do(http.MethodPost, acceptPath(tok), u, uuid.New(), nil), http.StatusNotFound, "invalid invite")
		if _, member := e.members(org)[u]; member {
			t.Fatal("a failed lookup added a member")
		}
	})
	t.Run("consume fails: rolled back, still redeemable", func(t *testing.T) {
		tr := failOn("update org_invites set accepted_at")
		e := newIDEnv(t, tr, nil)
		owner, org := e.seedOrg()
		email := uniqEmail("tx")
		u := e.invitee(email)
		tok := e.issue(org, owner, email, auth.RoleMember, time.Hour)
		rec := e.do(http.MethodPost, acceptPath(tok), u, uuid.New(), nil)
		wantErr(t, rec, http.StatusInternalServerError, "consume invite")
		if sessionCookie(rec) != nil {
			t.Fatal("a failed accept must not issue a cookie")
		}
		// The membership insert ran inside the same transaction and must have
		// been rolled back with it.
		if _, member := e.members(org)[u]; member {
			t.Fatal("membership survived a failed consume (transaction leaked)")
		}
		if n, _, accepted := e.invitesFor(org, email); n != 1 || accepted {
			t.Fatalf("invite: n=%d accepted=%v want still pending", n, accepted)
		}
		if c := e.auditCount(org, "member.accept_invite"); c != 0 {
			t.Fatalf("a failed accept wrote %d audit rows", c)
		}
	})
	t.Run("token consumed by a concurrent accept after the lookup", func(t *testing.T) {
		var pool *pgxpool.Pool
		var email string
		tr := &stmtTracer{
			match: has("for update of i"), nth: 1,
			after: func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if _, err := pool.Exec(ctx, `UPDATE org_invites SET accepted_at=now() WHERE email=$1`, email); err != nil {
					t.Errorf("competing accept: %v", err)
				}
			},
		}
		e := newIDEnv(t, tr, nil)
		pool = e.pool
		owner, org := e.seedOrg()
		email = uniqEmail("race")
		u := e.invitee(email)
		tok := e.issue(org, owner, email, auth.RoleMember, time.Hour)
		wantErr(t, e.do(http.MethodPost, acceptPath(tok), u, uuid.New(), nil), http.StatusNotFound, "invalid invite")
		if _, member := e.members(org)[u]; member {
			t.Fatal("a token consumed by someone else still added this caller")
		}
	})
	t.Run("magic link insert fails", func(t *testing.T) {
		e := newIDEnv(t, failOn("insert into magic_link_tokens"), nil)
		owner, org := e.seedOrg()
		email := uniqEmail("ml")
		tok := e.issue(org, owner, email, auth.RoleMember, time.Hour)
		wantErr(t, e.anon(http.MethodPost, acceptPath(tok)), http.StatusInternalServerError, "magic link")
		if n := e.magicLinks(email); n != 0 {
			t.Fatalf("magic links: %d", n)
		}
	})
	t.Run("magic link mail enqueue fails: sign-in link still minted", func(t *testing.T) {
		dq := dqueue.NewClient(deadRedis(t))
		e := newIDEnv(t, nil, func(d *Deps) { d.Queue = dq })
		owner, org := e.seedOrg()
		email := uniqEmail("mq")
		tok := e.issue(org, owner, email, auth.RoleMember, time.Hour)
		wantStatus(t, e.anon(http.MethodPost, acceptPath(tok)), http.StatusOK)
		if n := e.magicLinks(email); n != 1 {
			t.Fatalf("magic links: got %d want 1", n)
		}
	})
}
