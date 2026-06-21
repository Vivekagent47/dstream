package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/store"
)

// newTestRouterRedis is the createInvite-friendly sibling of newTestRouter:
// it wires a real Redis client so the rate limiter can run. Gated on
// DSTREAM_TEST_REDIS_ADDR; tests that need it call this directly.
func newTestRouterRedis(t *testing.T, q *store.Queries) (*chi.Mux, *auth.SessionSigner, *redis.Client) {
	t.Helper()
	addr := os.Getenv("DSTREAM_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("DSTREAM_TEST_REDIS_ADDR not set")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = rdb.Close() })
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

	router, signer := newTestRouter(q)
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
	router, signer := newTestRouter(q)
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

	router, _ := newTestRouter(q)
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

	router, _ := newTestRouter(q)
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
