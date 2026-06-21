package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/store"
)

// --- GET /api/orgs/{org_id}/members ---

func TestListMembers_AnyMember_OK(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	_, oid := seedUserAndOrg(t, q)
	member := seedUser(t, q)
	addMember(t, q, oid, member, "member")

	router, signer := newTestRouter(q)
	req := requestWithSession(t, signer, http.MethodGet, "/api/orgs/"+oid.String()+"/members", member, oid)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", rec.Code, rec.Body.String())
	}
}

// --- PATCH /api/orgs/{org_id}/members/{user_id} ---

func TestPatchMember_AdminPromotesMember_OK(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	owner, oid := seedUserAndOrg(t, q)
	admin := seedUser(t, q)
	addMember(t, q, oid, admin, "admin")
	target := seedUser(t, q)
	addMember(t, q, oid, target, "member")

	_ = owner
	router, signer := newTestRouter(q)
	req := requestWithSessionBody(t, signer, http.MethodPatch,
		"/api/orgs/"+oid.String()+"/members/"+target.String(),
		admin, oid, map[string]any{"role": "admin"})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status: got %d body=%s", rec.Code, rec.Body.String())
	}
	tm, _ := q.GetOrgMember(context.Background(), store.GetOrgMemberParams{
		OrgID: store.UUID(oid), UserID: store.UUID(target),
	})
	if tm.Role != "admin" {
		t.Errorf("target role: got %q want admin", tm.Role)
	}
}

func TestPatchMember_MemberCannotPromote_403(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	_, oid := seedUserAndOrg(t, q)
	caller := seedUser(t, q)
	addMember(t, q, oid, caller, "member")
	target := seedUser(t, q)
	addMember(t, q, oid, target, "member")

	router, signer := newTestRouter(q)
	req := requestWithSessionBody(t, signer, http.MethodPatch,
		"/api/orgs/"+oid.String()+"/members/"+target.String(),
		caller, oid, map[string]any{"role": "admin"})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestPatchMember_LastOwnerDemoted_409(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	owner, oid := seedUserAndOrg(t, q) // owner is the only owner

	router, signer := newTestRouter(q)
	req := requestWithSessionBody(t, signer, http.MethodPatch,
		"/api/orgs/"+oid.String()+"/members/"+owner.String(),
		owner, oid, map[string]any{"role": "admin"})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d want 409; body=%s", rec.Code, rec.Body.String())
	}
}

func TestPatchMember_InvalidRole_400(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	owner, oid := seedUserAndOrg(t, q)
	target := seedUser(t, q)
	addMember(t, q, oid, target, "member")

	router, signer := newTestRouter(q)
	req := requestWithSessionBody(t, signer, http.MethodPatch,
		"/api/orgs/"+oid.String()+"/members/"+target.String(),
		owner, oid, map[string]any{"role": "god"})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// --- DELETE /api/orgs/{org_id}/members/{user_id} ---

func TestDeleteMember_AdminRemovesMember_OK(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	_, oid := seedUserAndOrg(t, q)
	admin := seedUser(t, q)
	addMember(t, q, oid, admin, "admin")
	target := seedUser(t, q)
	addMember(t, q, oid, target, "member")

	router, signer := newTestRouter(q)
	req := requestWithSession(t, signer, http.MethodDelete,
		"/api/orgs/"+oid.String()+"/members/"+target.String(), admin, oid)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status: got %d want 204; body=%s", rec.Code, rec.Body.String())
	}
	if _, err := q.GetOrgMember(context.Background(), store.GetOrgMemberParams{
		OrgID: store.UUID(oid), UserID: store.UUID(target),
	}); err == nil {
		t.Errorf("expected member row gone")
	}
}

func TestDeleteMember_LastOwnerRemoved_409(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	owner, oid := seedUserAndOrg(t, q)

	router, signer := newTestRouter(q)
	req := requestWithSession(t, signer, http.MethodDelete,
		"/api/orgs/"+oid.String()+"/members/"+owner.String(), owner, oid)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d want 409; body=%s", rec.Code, rec.Body.String())
	}
}

func TestDeleteMember_UserCanRemoveSelf_OK(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	_, oid := seedUserAndOrg(t, q)
	member := seedUser(t, q)
	addMember(t, q, oid, member, "member")

	router, signer := newTestRouter(q)
	req := requestWithSession(t, signer, http.MethodDelete,
		"/api/orgs/"+oid.String()+"/members/"+member.String(), member, oid)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status: got %d want 204; body=%s", rec.Code, rec.Body.String())
	}
	if _, err := q.GetOrgMember(context.Background(), store.GetOrgMemberParams{
		OrgID: store.UUID(oid), UserID: store.UUID(member),
	}); err == nil {
		t.Errorf("expected member row gone after self-remove")
	}
	// Cookie must be re-issued; we just check Set-Cookie present.
	cookieFound := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionCookieName {
			cookieFound = true
		}
	}
	if !cookieFound {
		t.Errorf("self-remove from active org: expected session cookie re-issued")
	}
}
