package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

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

func TestTransferOwnership_NonMember_400(t *testing.T) {
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
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400; body=%s", rec.Code, rec.Body.String())
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
