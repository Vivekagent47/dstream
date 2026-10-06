package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

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

// =============================================================================
// Extended coverage: tenancy and privilege boundaries of the member routes.
// Harness (idEnv, stmtTracer, wantErr, ...) lives in orgs_test.go.
// =============================================================================

func memberPath(org, user uuid.UUID) string { return orgPath(org, "/members/"+user.String()) }

// --- GET /api/orgs/{org_id}/members ---

func TestListMembers_ScopedToOrgAndRefusals(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	owner, org := e.seedOrg()
	member := e.seedRole(org, "member")
	stranger, strangerOrg := e.seedOrg()

	rec := e.do(http.MethodGet, orgPath(org, "/members"), member, org, nil)
	wantStatus(t, rec, http.StatusOK)
	var rows []struct {
		UserID string `json:"user_id"`
		Role   string `json:"role"`
		Email  string `json:"email"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := map[string]string{}
	for _, r := range rows {
		if r.Email == "" {
			t.Errorf("member %s has no email in the listing", r.UserID)
		}
		got[r.UserID] = r.Role
	}
	if len(got) != 2 || got[owner.String()] != "owner" || got[member.String()] != "member" {
		t.Fatalf("listing: got %v want exactly {owner, member}", got)
	}
	if _, leaked := got[stranger.String()]; leaked {
		t.Fatal("listing leaked another org's member")
	}

	wantErr(t, e.do(http.MethodGet, "/api/orgs/zzz/members", owner, org, nil), http.StatusBadRequest, "invalid org_id")
	// Cross-org: the stranger holds a perfectly valid session, for their own org.
	wantErr(t, e.do(http.MethodGet, orgPath(org, "/members"), stranger, strangerOrg, nil), http.StatusForbidden, "not a member")
}

func TestListMembers_DBFailure_500(t *testing.T) {
	e := newIDEnv(t, failOn("join users u on u.id = m.user_id"), nil)
	owner, org := e.seedOrg()
	wantErr(t, e.do(http.MethodGet, orgPath(org, "/members"), owner, org, nil), http.StatusInternalServerError, "list members")
}

// --- PATCH /api/orgs/{org_id}/members/{user_id} ---

func TestPatchMember_Refusals_ChangeNothing(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	owner, org := e.seedOrg()
	admin := e.seedRole(org, "admin")
	member := e.seedRole(org, "member")
	other := e.seedRole(org, "member")
	stranger, strangerOrg := e.seedOrg()
	strangerMember := e.seedRole(strangerOrg, "member")
	before := e.snapshot(org)
	beforeStranger := e.snapshot(strangerOrg)
	role := func(r string) map[string]any { return map[string]any{"role": r} }

	for _, tc := range []struct {
		name string
		path string
		as   uuid.UUID
		asOr uuid.UUID
		body any
		code int
		msg  string
	}{
		{"bad org id", "/api/orgs/zzz/members/" + member.String(), admin, org, role("admin"), 400, "invalid org_id"},
		{"bad user id", orgPath(org, "/members/zzz"), admin, org, role("admin"), 400, "invalid user_id"},
		{"bad json", memberPath(org, member), admin, org, "{nope", 400, "invalid json"},
		{"unknown role", memberPath(org, member), admin, org, role("superuser"), 400, "invalid role"},
		{"empty role", memberPath(org, member), admin, org, role(""), 400, "invalid role"},
		{"role is case sensitive", memberPath(org, member), admin, org, role("Admin"), 400, "invalid role"},
		{"cross-org: stranger's owner", memberPath(org, member), stranger, strangerOrg, role("admin"), 403, "not a member"},
		{"member promotes a peer", memberPath(org, other), member, org, role("admin"), 403, "admin required"},
		{"member promotes self", memberPath(org, member), member, org, role("admin"), 403, "admin required"},
		{"member demotes the owner", memberPath(org, owner), member, org, role("member"), 403, "admin required"},
		{"admin promotes member to owner", memberPath(org, member), admin, org, role("owner"), 400, "use POST /transfer to promote to owner"},
		{"admin promotes self to owner", memberPath(org, admin), admin, org, role("owner"), 400, "use POST /transfer to promote to owner"},
		{"owner promotes member to owner", memberPath(org, member), owner, org, role("owner"), 400, "use POST /transfer to promote to owner"},
		{"admin demotes owner", memberPath(org, owner), admin, org, role("member"), 403, "owner required to demote an owner"},
		{"unknown target", memberPath(org, uuid.New()), admin, org, role("member"), 404, "member not found"},
		// The target exists, but in the stranger's org: not addressable from here.
		{"target belongs to another org", memberPath(org, strangerMember), admin, org, role("admin"), 404, "member not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantErr(t, e.do(http.MethodPatch, tc.path, tc.as, tc.asOr, tc.body), tc.code, tc.msg)
			e.sameSnapshot(org, before)
		})
	}
	e.sameSnapshot(org, before)
	e.sameSnapshot(strangerOrg, beforeStranger)
	if n := e.auditCount(org, "member.role_change"); n != 0 {
		t.Fatalf("refused patches wrote %d audit rows", n)
	}
}

func TestPatchMember_SameRole_NoOpNoAudit(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	owner, org := e.seedOrg()
	member := e.seedRole(org, "member")
	before := e.snapshot(org)
	wantStatus(t, e.do(http.MethodPatch, memberPath(org, member), owner, org, map[string]any{"role": "member"}), http.StatusNoContent)
	// An owner "promoting" themselves to owner is the same no-op.
	wantStatus(t, e.do(http.MethodPatch, memberPath(org, owner), owner, org, map[string]any{"role": "owner"}), http.StatusNoContent)
	e.sameSnapshot(org, before)
	if n := e.auditCount(org, "member.role_change"); n != 0 {
		t.Fatalf("no-op patches wrote %d audit rows", n)
	}
}

func TestPatchMember_DemoteMemberAndAudit(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	owner, org := e.seedOrg()
	admin := e.seedRole(org, "admin")
	wantStatus(t, e.do(http.MethodPatch, memberPath(org, admin), owner, org, map[string]any{"role": "member"}), http.StatusNoContent)
	if got := e.members(org)[admin]; got != "member" {
		t.Fatalf("role: got %q want member", got)
	}
	var meta string
	if err := e.pool.QueryRow(context.Background(),
		`SELECT metadata::text FROM audit_logs WHERE org_id=$1 AND action='member.role_change'`, org).Scan(&meta); err != nil {
		t.Fatalf("audit row: %v", err)
	}
	if !strings.Contains(meta, `"from": "admin"`) || !strings.Contains(meta, `"to": "member"`) {
		t.Fatalf("audit metadata %s should record admin -> member", meta)
	}
}

func TestPatchMember_MidFlowFailures(t *testing.T) {
	t.Run("target lookup fails", func(t *testing.T) {
		tr := &stmtTracer{} // target is only known after seeding; wired below
		e := newIDEnv(t, tr, nil)
		owner, org := e.seedOrg()
		target := e.seedRole(org, "member")
		tr.match = hasArg(target, "from org_members where org_id = $1 and user_id = $2")
		before := e.snapshot(org)
		wantErr(t, e.do(http.MethodPatch, memberPath(org, target), owner, org, map[string]any{"role": "admin"}),
			http.StatusInternalServerError, "get member")
		e.sameSnapshot(org, before)
	})
	t.Run("caller lookup fails", func(t *testing.T) {
		tr := &stmtTracer{}
		e := newIDEnv(t, tr, nil)
		owner, org := e.seedOrg()
		target := e.seedRole(org, "member")
		tr.match = hasArg(owner, "from org_members where org_id = $1 and user_id = $2")
		before := e.snapshot(org)
		wantErr(t, e.do(http.MethodPatch, memberPath(org, target), owner, org, map[string]any{"role": "admin"}),
			http.StatusForbidden, "not a member")
		e.sameSnapshot(org, before)
	})
	t.Run("role update fails", func(t *testing.T) {
		e := newIDEnv(t, failOn("update org_members set role = $3"), nil)
		owner, org := e.seedOrg()
		target := e.seedRole(org, "member")
		before := e.snapshot(org)
		wantErr(t, e.do(http.MethodPatch, memberPath(org, target), owner, org, map[string]any{"role": "admin"}),
			http.StatusInternalServerError, "update member role")
		e.sameSnapshot(org, before)
		if n := e.auditCount(org, "member.role_change"); n != 0 {
			t.Fatalf("a failed update wrote %d audit rows", n)
		}
	})
	t.Run("owner demotion fails", func(t *testing.T) {
		e := newIDEnv(t, failOn("update org_members m set role = $3"), nil)
		owner, org := e.seedOrg()
		second := e.seedRole(org, "owner")
		before := e.snapshot(org)
		wantErr(t, e.do(http.MethodPatch, memberPath(org, second), owner, org, map[string]any{"role": "admin"}),
			http.StatusInternalServerError, "demote owner")
		e.sameSnapshot(org, before)
	})
}

// --- DELETE /api/orgs/{org_id}/members/{user_id} ---

func TestRemoveMember_Refusals_ChangeNothing(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	owner, org := e.seedOrg()
	admin := e.seedRole(org, "admin")
	member := e.seedRole(org, "member")
	other := e.seedRole(org, "member")
	stranger, strangerOrg := e.seedOrg()
	strangerMember := e.seedRole(strangerOrg, "member")
	before := e.snapshot(org)
	beforeStranger := e.snapshot(strangerOrg)

	for _, tc := range []struct {
		name string
		path string
		as   uuid.UUID
		asOr uuid.UUID
		code int
		msg  string
	}{
		{"bad org id", "/api/orgs/zzz/members/" + member.String(), admin, org, 400, "invalid org_id"},
		{"bad user id", orgPath(org, "/members/zzz"), admin, org, 400, "invalid user_id"},
		{"cross-org: stranger's owner", memberPath(org, member), stranger, strangerOrg, 403, "not a member"},
		{"member removes a peer", memberPath(org, other), member, org, 403, "admin required"},
		{"member removes the owner", memberPath(org, owner), member, org, 403, "admin required"},
		{"admin removes the owner", memberPath(org, owner), admin, org, 403, "owner required to remove an owner"},
		{"unknown target", memberPath(org, uuid.New()), admin, org, 404, "member not found"},
		{"target belongs to another org", memberPath(org, strangerMember), admin, org, 404, "member not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantErr(t, e.do(http.MethodDelete, tc.path, tc.as, tc.asOr, nil), tc.code, tc.msg)
			e.sameSnapshot(org, before)
		})
	}
	e.sameSnapshot(org, before)
	e.sameSnapshot(strangerOrg, beforeStranger)
	if n := e.auditCount(org, "member.remove"); n != 0 {
		t.Fatalf("refused removals wrote %d audit rows", n)
	}
}

func TestRemoveMember_AdminRemovesAdmin_AuditsRemovedBySelfFalse(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	_, org := e.seedOrg()
	admin := e.seedRole(org, "admin")
	peer := e.seedRole(org, "admin")
	wantStatus(t, e.do(http.MethodDelete, memberPath(org, peer), admin, org, nil), http.StatusNoContent)
	if _, still := e.members(org)[peer]; still {
		t.Fatal("removed admin is still a member")
	}
	var meta string
	if err := e.pool.QueryRow(context.Background(),
		`SELECT metadata::text FROM audit_logs WHERE org_id=$1 AND action='member.remove'`, org).Scan(&meta); err != nil {
		t.Fatalf("audit row: %v", err)
	}
	if !strings.Contains(meta, `"removed_by_self": false`) {
		t.Fatalf("audit metadata %s should say removed_by_self=false", meta)
	}
}

func TestRemoveMember_SelfLeave_CookieRotation(t *testing.T) {
	t.Run("active org with another org: cookie moves", func(t *testing.T) {
		e := newIDEnv(t, nil, nil)
		_, left := e.seedOrg()
		_, stay := e.seedOrg()
		u := e.seedRole(left, "member")
		addMember(t, e.q, stay, u, "member")
		rec := e.do(http.MethodDelete, memberPath(left, u), u, left, nil)
		wantStatus(t, rec, http.StatusNoContent)
		if _, ok := e.members(left)[u]; ok {
			t.Fatal("user is still a member after leaving")
		}
		if got, o := e.cookieOrg(rec); got != u || o != stay {
			t.Fatalf("cookie: got user=%s org=%s want %s / %s", got, o, u, stay)
		}
	})
	t.Run("active org was the only one: cookie org nil", func(t *testing.T) {
		e := newIDEnv(t, nil, nil)
		_, org := e.seedOrg()
		u := e.seedRole(org, "member")
		rec := e.do(http.MethodDelete, memberPath(org, u), u, org, nil)
		wantStatus(t, rec, http.StatusNoContent)
		if _, o := e.cookieOrg(rec); o != uuid.Nil {
			t.Fatalf("cookie org: got %s want nil", o)
		}
	})
	t.Run("leaving a non-active org leaves the cookie alone", func(t *testing.T) {
		e := newIDEnv(t, nil, nil)
		_, left := e.seedOrg()
		_, active := e.seedOrg()
		u := e.seedRole(left, "member")
		addMember(t, e.q, active, u, "member")
		rec := e.do(http.MethodDelete, memberPath(left, u), u, active, nil)
		wantStatus(t, rec, http.StatusNoContent)
		if sessionCookie(rec) != nil {
			t.Fatal("cookie must not be re-issued when the active org is unchanged")
		}
	})
	t.Run("next-org lookup fails: cookie org nil", func(t *testing.T) {
		e := newIDEnv(t, failOn("order by m.created_at asc, m.org_id asc"), nil)
		_, left := e.seedOrg()
		_, stay := e.seedOrg()
		u := e.seedRole(left, "member")
		addMember(t, e.q, stay, u, "member")
		rec := e.do(http.MethodDelete, memberPath(left, u), u, left, nil)
		wantStatus(t, rec, http.StatusNoContent)
		if _, o := e.cookieOrg(rec); o != uuid.Nil {
			t.Fatalf("cookie org: got %s want nil when lookup fails", o)
		}
	})
}

func TestRemoveMember_MidFlowFailures(t *testing.T) {
	t.Run("target lookup fails", func(t *testing.T) {
		tr := &stmtTracer{}
		e := newIDEnv(t, tr, nil)
		owner, org := e.seedOrg()
		target := e.seedRole(org, "member")
		tr.match = hasArg(target, "from org_members where org_id = $1 and user_id = $2")
		before := e.snapshot(org)
		wantErr(t, e.do(http.MethodDelete, memberPath(org, target), owner, org, nil), http.StatusInternalServerError, "get member")
		e.sameSnapshot(org, before)
	})
	t.Run("caller lookup fails", func(t *testing.T) {
		tr := &stmtTracer{}
		e := newIDEnv(t, tr, nil)
		owner, org := e.seedOrg()
		target := e.seedRole(org, "member")
		tr.match = hasArg(owner, "from org_members where org_id = $1 and user_id = $2")
		before := e.snapshot(org)
		wantErr(t, e.do(http.MethodDelete, memberPath(org, target), owner, org, nil), http.StatusForbidden, "not a member")
		e.sameSnapshot(org, before)
	})
	t.Run("member delete fails", func(t *testing.T) {
		e := newIDEnv(t, failOn("delete from org_members where org_id = $1 and user_id = $2"), nil)
		owner, org := e.seedOrg()
		target := e.seedRole(org, "member")
		before := e.snapshot(org)
		wantErr(t, e.do(http.MethodDelete, memberPath(org, target), owner, org, nil), http.StatusInternalServerError, "delete member")
		e.sameSnapshot(org, before)
		if n := e.auditCount(org, "member.remove"); n != 0 {
			t.Fatalf("a failed delete wrote %d audit rows", n)
		}
	})
	t.Run("owner delete fails", func(t *testing.T) {
		e := newIDEnv(t, failOn("delete from org_members m where"), nil)
		owner, org := e.seedOrg()
		second := e.seedRole(org, "owner")
		before := e.snapshot(org)
		wantErr(t, e.do(http.MethodDelete, memberPath(org, second), owner, org, nil), http.StatusInternalServerError, "remove owner")
		e.sameSnapshot(org, before)
	})
}

// --- the last-owner guard, from every angle ---

// A sole owner cannot leave the org ownerless by demoting, removing or
// leaving; the only way out is a transfer, which preserves one owner.
func TestLastOwnerGuard_SoleOwner_EveryRoute(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	owner, org := e.seedOrg()
	admin := e.seedRole(org, "admin")
	before := e.snapshot(org)

	// Demote self (admin or member).
	for _, r := range []string{"admin", "member"} {
		wantErr(t, e.do(http.MethodPatch, memberPath(org, owner), owner, org, map[string]any{"role": r}),
			http.StatusConflict, "cannot demote last owner")
	}
	// Remove self.
	wantErr(t, e.do(http.MethodDelete, memberPath(org, owner), owner, org, nil), http.StatusConflict, "cannot remove last owner")
	// An admin cannot do it for them.
	wantErr(t, e.do(http.MethodPatch, memberPath(org, owner), admin, org, map[string]any{"role": "member"}),
		http.StatusForbidden, "owner required to demote an owner")
	wantErr(t, e.do(http.MethodDelete, memberPath(org, owner), admin, org, nil),
		http.StatusForbidden, "owner required to remove an owner")
	e.sameSnapshot(org, before)
	if e.owners(org) != 1 {
		t.Fatalf("owners: got %d want 1", e.owners(org))
	}

	// Transfer is the sanctioned exit, and it never leaves zero owners.
	wantStatus(t, e.do(http.MethodPost, orgPath(org, "/transfer"), owner, org,
		map[string]any{"to_user_id": admin.String()}), http.StatusNoContent)
	if e.owners(org) != 1 || e.members(org)[admin] != "owner" {
		t.Fatalf("after transfer: owners=%d roles=%v", e.owners(org), e.members(org))
	}
	// And the guard now protects the new owner.
	wantErr(t, e.do(http.MethodDelete, memberPath(org, admin), admin, org, nil), http.StatusConflict, "cannot remove last owner")
}

// With two owners either may leave, but the survivor is then the last owner.
func TestLastOwnerGuard_TwoOwners_OneMayLeave(t *testing.T) {
	t.Run("remove: one owner removes the other", func(t *testing.T) {
		e := newIDEnv(t, nil, nil)
		first, org := e.seedOrg()
		second := e.seedRole(org, "owner")
		wantStatus(t, e.do(http.MethodDelete, memberPath(org, second), first, org, nil), http.StatusNoContent)
		if _, still := e.members(org)[second]; still || e.owners(org) != 1 {
			t.Fatalf("members after removal: %v", e.members(org))
		}
		wantErr(t, e.do(http.MethodDelete, memberPath(org, first), first, org, nil), http.StatusConflict, "cannot remove last owner")
		if e.members(org)[first] != "owner" {
			t.Fatal("the surviving owner lost their row")
		}
	})
	t.Run("leave: an owner leaves, survivor cannot", func(t *testing.T) {
		e := newIDEnv(t, nil, nil)
		first, org := e.seedOrg()
		second := e.seedRole(org, "owner")
		wantStatus(t, e.do(http.MethodDelete, memberPath(org, first), first, org, nil), http.StatusNoContent)
		if got := e.members(org); len(got) != 1 || got[second] != "owner" {
			t.Fatalf("members: got %v want only the second owner", got)
		}
		wantErr(t, e.do(http.MethodDelete, memberPath(org, second), second, org, nil), http.StatusConflict, "cannot remove last owner")
	})
	t.Run("demote: one owner demotes the other, survivor cannot demote self", func(t *testing.T) {
		e := newIDEnv(t, nil, nil)
		first, org := e.seedOrg()
		second := e.seedRole(org, "owner")
		wantStatus(t, e.do(http.MethodPatch, memberPath(org, second), first, org, map[string]any{"role": "admin"}), http.StatusNoContent)
		if e.members(org)[second] != "admin" || e.owners(org) != 1 {
			t.Fatalf("roles: %v", e.members(org))
		}
		wantErr(t, e.do(http.MethodPatch, memberPath(org, first), first, org, map[string]any{"role": "member"}),
			http.StatusConflict, "cannot demote last owner")
		if e.members(org)[first] != "owner" {
			t.Fatal("the surviving owner was demoted")
		}
	})
}

// --- every privileged route, as a non-privileged caller ---

func TestMember_EveryPrivilegedRoute_Refused(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	owner, org := e.seedOrg()
	member := e.seedRole(org, "member")
	other := e.seedRole(org, "member")
	invite := stageInvite(t, e.q, org, owner, "pending+"+uuid.NewString()+"@example.test", "member")
	before := e.snapshot(org)

	for _, tc := range []struct {
		name         string
		method, path string
		body         any
		msg          string
	}{
		{"rename org", http.MethodPatch, orgPath(org, ""), map[string]any{"name": "Pwned"}, "admin required"},
		{"delete org", http.MethodDelete, orgPath(org, ""), nil, "owner required"},
		{"patch peer", http.MethodPatch, memberPath(org, other), map[string]any{"role": "admin"}, "admin required"},
		{"patch self up", http.MethodPatch, memberPath(org, member), map[string]any{"role": "admin"}, "admin required"},
		{"patch owner down", http.MethodPatch, memberPath(org, owner), map[string]any{"role": "member"}, "admin required"},
		{"remove peer", http.MethodDelete, memberPath(org, other), nil, "admin required"},
		{"remove owner", http.MethodDelete, memberPath(org, owner), nil, "admin required"},
		{"create invite", http.MethodPost, orgPath(org, "/invites"), map[string]any{"email": "x@y.test", "role": "member"}, "admin required"},
		{"delete invite", http.MethodDelete, orgPath(org, "/invites/"+invite.String()), nil, "admin required"},
		{"transfer", http.MethodPost, orgPath(org, "/transfer"), map[string]any{"to_user_id": other.String()}, "transfer not permitted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantErr(t, e.do(tc.method, tc.path, member, org, tc.body), http.StatusForbidden, tc.msg)
			e.sameSnapshot(org, before)
		})
	}
	e.sameSnapshot(org, before)
}

func TestAdmin_OwnerOnlyRoutes_Refused(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	owner, org := e.seedOrg()
	admin := e.seedRole(org, "admin")
	member := e.seedRole(org, "member")
	before := e.snapshot(org)

	for _, tc := range []struct {
		name         string
		method, path string
		body         any
		code         int
		msg          string
	}{
		{"delete org", http.MethodDelete, orgPath(org, ""), nil, 403, "owner required"},
		{"transfer to member", http.MethodPost, orgPath(org, "/transfer"), map[string]any{"to_user_id": member.String()}, 403, "transfer not permitted"},
		{"demote owner", http.MethodPatch, memberPath(org, owner), map[string]any{"role": "admin"}, 403, "owner required to demote an owner"},
		{"remove owner", http.MethodDelete, memberPath(org, owner), nil, 403, "owner required to remove an owner"},
		{"grant owner", http.MethodPatch, memberPath(org, member), map[string]any{"role": "owner"}, 400, "use POST /transfer to promote to owner"},
		{"self-grant owner", http.MethodPatch, memberPath(org, admin), map[string]any{"role": "owner"}, 400, "use POST /transfer to promote to owner"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantErr(t, e.do(tc.method, tc.path, admin, org, tc.body), tc.code, tc.msg)
			e.sameSnapshot(org, before)
		})
	}
	e.sameSnapshot(org, before)
	if e.owners(org) != 1 || e.members(org)[owner] != "owner" {
		t.Fatalf("ownership changed: %v", e.members(org))
	}
}
