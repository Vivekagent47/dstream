package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/store"
)

// sampleID is a syntactically valid UUID for path interpolation. No row with
// this id exists: the middleware runs before the handler, so a 404 from the
// handler still proves the request was authorized.
const sampleID = "00000000-0000-4000-8000-000000000001"

// rbacCase is one cell of the permission matrix.
//
// pattern is the chi route pattern, used by the coverage assertion to prove
// every mounted traffic-plane route appears here. path is the concrete URL
// the request is sent to.
type rbacCase struct {
	method  string
	pattern string
	path    string
	min     auth.Role // lowest role allowed to reach the handler
}

func p(pattern string) string {
	s := strings.ReplaceAll(pattern, "{id}", sampleID)
	s = strings.ReplaceAll(s, "{app_id}", sampleID)
	s = strings.ReplaceAll(s, "{endpoint_id}", sampleID)
	return strings.ReplaceAll(s, "{name}", "user.created")
}

func c(method, pattern string, min auth.Role) rbacCase {
	return rbacCase{method: method, pattern: pattern, path: p(pattern), min: min}
}

// rbacMatrix is the permission matrix for the tenant traffic plane. It is the
// executable copy of the table in the spec; router.go is the declarative one.
var rbacMatrix = []rbacCase{
	c(http.MethodGet, "/api/audit", auth.RoleMember),
	c(http.MethodGet, "/api/usage", auth.RoleMember),
	c(http.MethodGet, "/api/usage/history", auth.RoleMember),
	c(http.MethodPost, "/api/filter-preview", auth.RoleMember),
	c(http.MethodPost, "/api/transform-preview", auth.RoleMember),

	c(http.MethodGet, "/api/sources", auth.RoleMember),
	c(http.MethodPost, "/api/sources", auth.RoleMember),
	c(http.MethodGet, "/api/sources/{id}", auth.RoleMember),
	c(http.MethodGet, "/api/sources/{id}/metrics", auth.RoleMember),
	c(http.MethodPatch, "/api/sources/{id}", auth.RoleMember),
	c(http.MethodDelete, "/api/sources/{id}", auth.RoleAdmin),

	c(http.MethodGet, "/api/destinations", auth.RoleMember),
	c(http.MethodPost, "/api/destinations", auth.RoleMember),
	c(http.MethodGet, "/api/destinations/{id}", auth.RoleMember),
	c(http.MethodGet, "/api/destinations/{id}/metrics", auth.RoleMember),
	c(http.MethodPatch, "/api/destinations/{id}", auth.RoleMember),
	c(http.MethodDelete, "/api/destinations/{id}", auth.RoleAdmin),

	c(http.MethodGet, "/api/connections/stats", auth.RoleMember),
	c(http.MethodGet, "/api/connections", auth.RoleMember),
	c(http.MethodPost, "/api/connections", auth.RoleMember),
	c(http.MethodGet, "/api/connections/{id}", auth.RoleMember),
	c(http.MethodGet, "/api/connections/{id}/stats", auth.RoleMember),
	c(http.MethodPost, "/api/connections/{id}/test", auth.RoleMember),
	c(http.MethodPatch, "/api/connections/{id}", auth.RoleMember),
	c(http.MethodDelete, "/api/connections/{id}", auth.RoleAdmin),

	c(http.MethodGet, "/api/events", auth.RoleMember),
	c(http.MethodGet, "/api/events/histogram", auth.RoleMember),
	c(http.MethodGet, "/api/events/{id}", auth.RoleMember),
	c(http.MethodPost, "/api/events/{id}/retry", auth.RoleMember),

	c(http.MethodGet, "/api/bookmarks", auth.RoleMember),
	c(http.MethodPost, "/api/bookmarks", auth.RoleMember),
	c(http.MethodPost, "/api/bookmarks/import", auth.RoleMember),
	c(http.MethodGet, "/api/bookmarks/{id}", auth.RoleMember),
	c(http.MethodPatch, "/api/bookmarks/{id}", auth.RoleMember),
	c(http.MethodDelete, "/api/bookmarks/{id}", auth.RoleAdmin),
	c(http.MethodPost, "/api/bookmarks/{id}/replay", auth.RoleMember),
	c(http.MethodPost, "/api/bookmarks/{id}/replay-to", auth.RoleMember),
	c(http.MethodGet, "/api/bookmarks/{id}/export", auth.RoleMember),

	c(http.MethodGet, "/api/capture-rules", auth.RoleMember),
	c(http.MethodPost, "/api/capture-rules", auth.RoleMember),
	c(http.MethodGet, "/api/capture-rules/{id}", auth.RoleMember),
	c(http.MethodPatch, "/api/capture-rules/{id}", auth.RoleMember),
	c(http.MethodDelete, "/api/capture-rules/{id}", auth.RoleAdmin),

	c(http.MethodGet, "/api/scenarios", auth.RoleMember),
	c(http.MethodPost, "/api/scenarios", auth.RoleMember),
	c(http.MethodGet, "/api/scenarios/{id}", auth.RoleMember),
	c(http.MethodPatch, "/api/scenarios/{id}", auth.RoleMember),
	c(http.MethodDelete, "/api/scenarios/{id}", auth.RoleAdmin),
	c(http.MethodPost, "/api/scenarios/{id}/replay-to", auth.RoleMember),

	c(http.MethodGet, "/api/operational-app", auth.RoleMember),

	c(http.MethodGet, "/api/applications", auth.RoleMember),
	c(http.MethodPost, "/api/applications", auth.RoleMember),
	c(http.MethodGet, "/api/applications/{app_id}", auth.RoleMember),
	c(http.MethodPatch, "/api/applications/{app_id}", auth.RoleMember),
	c(http.MethodDelete, "/api/applications/{app_id}", auth.RoleAdmin),
	c(http.MethodPost, "/api/applications/{app_id}/portal-access", auth.RoleAdmin),
	c(http.MethodPost, "/api/applications/{app_id}/portal-access/revoke", auth.RoleAdmin),

	c(http.MethodGet, "/api/applications/{app_id}/endpoints", auth.RoleMember),
	c(http.MethodPost, "/api/applications/{app_id}/endpoints", auth.RoleMember),
	c(http.MethodGet, "/api/applications/{app_id}/endpoints/{id}", auth.RoleMember),
	c(http.MethodPatch, "/api/applications/{app_id}/endpoints/{id}", auth.RoleMember),
	c(http.MethodDelete, "/api/applications/{app_id}/endpoints/{id}", auth.RoleAdmin),
	c(http.MethodGet, "/api/applications/{app_id}/endpoints/{id}/secret", auth.RoleAdmin),
	c(http.MethodPost, "/api/applications/{app_id}/endpoints/{id}/rotate-secret", auth.RoleAdmin),
	c(http.MethodPost, "/api/applications/{app_id}/endpoints/{id}/recover", auth.RoleMember),
	c(http.MethodPost, "/api/applications/{app_id}/endpoints/{id}/test", auth.RoleMember),
	c(http.MethodGet, "/api/applications/{app_id}/endpoints/{id}/attempts", auth.RoleMember),

	c(http.MethodGet, "/api/applications/{app_id}/messages", auth.RoleMember),
	c(http.MethodPost, "/api/applications/{app_id}/messages", auth.RoleAdmin),
	c(http.MethodGet, "/api/applications/{app_id}/messages/{id}", auth.RoleMember),
	c(http.MethodGet, "/api/applications/{app_id}/messages/{id}/attempts", auth.RoleMember),
	c(http.MethodGet, "/api/applications/{app_id}/messages/{id}/deliveries", auth.RoleMember),
	c(http.MethodPost, "/api/applications/{app_id}/messages/{id}/endpoints/{endpoint_id}/replay", auth.RoleMember),

	c(http.MethodGet, "/api/event-types", auth.RoleMember),
	c(http.MethodPost, "/api/event-types", auth.RoleMember),
	c(http.MethodGet, "/api/event-types/{name}", auth.RoleMember),
	c(http.MethodPatch, "/api/event-types/{name}", auth.RoleMember),
	c(http.MethodDelete, "/api/event-types/{name}", auth.RoleAdmin),

	c(http.MethodGet, "/api/cli/sources", auth.RoleMember),
	c(http.MethodGet, "/api/cli/connect", auth.RoleMember),
}

// seedMemberAt adds a fresh user to orgID at the given role.
func seedMemberAt(t *testing.T, q *store.Queries, orgID uuid.UUID, role auth.Role) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	u, err := q.CreateUser(ctx, store.CreateUserParams{
		Email: "rbac+" + uuid.NewString() + "@example.test",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := q.AddOrgMember(ctx, store.AddOrgMemberParams{
		OrgID:  store.UUID(orgID),
		UserID: u.ID,
		Role:   string(role),
	}); err != nil {
		t.Fatalf("add member: %v", err)
	}
	return store.GoUUID(u.ID)
}

func TestRBACMatrix(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	_, orgID := seedUserAndOrg(t, q)

	users := map[auth.Role]uuid.UUID{
		auth.RoleMember: seedMemberAt(t, q, orgID, auth.RoleMember),
		auth.RoleAdmin:  seedMemberAt(t, q, orgID, auth.RoleAdmin),
		auth.RoleOwner:  seedMemberAt(t, q, orgID, auth.RoleOwner),
	}

	router, signer := newTestRouterPool(pool, q)

	for _, tc := range rbacMatrix {
		for _, role := range []auth.Role{auth.RoleMember, auth.RoleAdmin, auth.RoleOwner} {
			name := tc.method + " " + tc.pattern + " as " + string(role)
			t.Run(name, func(t *testing.T) {
				// Precondition: the pattern must actually be mounted. Without
				// this a renamed route would 404 and read as "not 403".
				if !router.Match(chi.NewRouteContext(), tc.method, tc.path) {
					t.Fatalf("no route mounted for %s %s", tc.method, tc.path)
				}
				req := requestWithSession(t, signer, tc.method, tc.path, users[role], orgID)
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, req)

				denied := rec.Code == http.StatusForbidden
				wantDenied := role.LessThan(tc.min)
				if denied != wantDenied {
					t.Errorf("got status %d (denied=%v), want denied=%v; body=%s",
						rec.Code, denied, wantDenied, rec.Body.String())
				}
			})
		}
	}
}

// TestRBACMatrixCoversEveryRoute is the anti-rot assertion: every route
// mounted on the traffic plane must appear in rbacMatrix. A new route added
// without a matrix entry fails here, which is the whole reason the matrix is
// a table rather than scattered handler checks.
func TestRBACMatrixCoversEveryRoute(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	router, _ := newTestRouterPool(pool, q)

	// Surfaces with their own permission models, excluded deliberately:
	// unauthenticated auth/invite routes, the user-level routes that work
	// without an active org, org administration (gated inline per handler),
	// and the App Portal (customer-scoped token).
	excluded := []string{"/api/auth/", "/api/invites", "/api/me", "/api/orgs", "/api/portal/"}

	inMatrix := map[string]bool{}
	for _, tc := range rbacMatrix {
		inMatrix[tc.method+" "+normalizeRoute(tc.pattern)] = true
	}

	var missing []string
	err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		route = normalizeRoute(route)
		if !strings.HasPrefix(route, "/api/") {
			return nil
		}
		for _, ex := range excluded {
			// Match on a path boundary, not a bare substring: "/api/me"
			// is a prefix of "/api/messages", and a bare HasPrefix would
			// drop such a future route from this assertion silently. The
			// TrimSuffix keeps the deliberate "/api/auth/" spelling
			// working, which is what holds /api/audit in scope.
			if route == ex || strings.HasPrefix(route, strings.TrimSuffix(ex, "/")+"/") {
				return nil
			}
		}
		if !inMatrix[method+" "+route] {
			missing = append(missing, method+" "+route)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(missing) > 0 {
		t.Fatalf("routes missing from rbacMatrix (add them, don't delete this test):\n  %s",
			strings.Join(missing, "\n  "))
	}
}

// TestPortalUnaffectedByRBAC pins the boundary: a portal token still deletes
// its own endpoints. The org-side middlewares must not reach /api/portal/*.
//
// This mints a real token rather than asserting on RequirePortal's own
// refusal. A tokenless request is rejected with 401 before any RBAC
// middleware mounted *inside* the portal group would ever run, so the
// tokenless version cannot see that mis-mount at all; only a request that
// clears RequirePortal proves the gate isn't there.
func TestPortalUnaffectedByRBAC(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	ctx := context.Background()

	_, orgID := seedUserAndOrg(t, q)
	app, err := q.CreateApplication(ctx, store.CreateApplicationParams{
		OrgID: store.UUID(orgID), Name: "Portal RBAC", Metadata: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	portal := &auth.PortalSigner{Secret: []byte("test-secret-do-not-use-in-prod!!"), TTL: time.Hour}
	router := chi.NewRouter()
	Mount(router, Deps{
		Queries: q,
		Pool:    pool,
		Signer:  &auth.SessionSigner{Secret: []byte("test-secret-do-not-use-in-prod")},
		Portal:  portal,
	})
	tok, _ := portal.Mint(store.GoUUID(app.ID), orgID, 0)

	// A portal token carries no org role, so any org-side gate would refuse
	// it with 403. Assert the exact 404 the handler returns for an unseeded
	// endpoint rather than merely "not 403": every RequirePortal rejection
	// is 401 or 500, so a token that stopped being accepted would satisfy
	// "not 403" and let this test pass while proving nothing.
	for _, tc := range []struct{ method, path string }{
		{http.MethodDelete, "/api/portal/endpoints/" + sampleID},
		{http.MethodPost, "/api/portal/endpoints/" + sampleID + "/rotate-secret"},
		{http.MethodGet, "/api/portal/endpoints/" + sampleID + "/secret"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("portal %s %s: want 404 from handler, got %d %s",
				tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}

// normalizeRoute puts a chi.Walk route and a matrix pattern into the same
// spelling. chi renders an index route declared as Route("/sources") +
// Get("/") as "/api/sources/", and a subrouter mount as "/api/sources/*";
// the matrix writes both as "/api/sources".
func normalizeRoute(route string) string {
	route = strings.TrimSuffix(route, "/*")
	if route == "/" {
		return route
	}
	return strings.TrimSuffix(route, "/")
}
