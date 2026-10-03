package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// okHandler is the terminal handler under test: reaching it means the
// middleware allowed the request through. It records that in *called —
// httptest.ResponseRecorder ignores a second WriteHeader, so a middleware that
// denied *and* forgot to return would still show 403 while the handler ran.
// The deny cases assert !*called so that loss is caught.
func okHandler(called *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*called = true
		w.WriteHeader(http.StatusOK)
	})
}

func withRole(method string, role Role) *http.Request {
	req := httptest.NewRequest(method, "/", nil)
	return req.WithContext(WithPrincipal(context.Background(), Principal{Role: role}))
}

func TestRequireRole(t *testing.T) {
	cases := []struct {
		name string
		role Role
		min  Role
		want int
	}{
		{"member below admin", RoleMember, RoleAdmin, http.StatusForbidden},
		{"admin meets admin", RoleAdmin, RoleAdmin, http.StatusOK},
		{"owner above admin", RoleOwner, RoleAdmin, http.StatusOK},
		{"member meets member", RoleMember, RoleMember, http.StatusOK},
		{"unknown role denied", Role("bogus"), RoleMember, http.StatusForbidden},
		{"empty role denied", Role(""), RoleMember, http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			var called bool
			RequireRole(c.min)(okHandler(&called)).ServeHTTP(rec, withRole(http.MethodGet, c.role))
			if rec.Code != c.want {
				t.Errorf("role=%s min=%s: got %d want %d", c.role, c.min, rec.Code, c.want)
			}
			if c.want == http.StatusForbidden && called {
				t.Errorf("role=%s min=%s: denied but handler still ran", c.role, c.min)
			}
		})
	}
}

// A request with no principal must fail closed. In production these
// middlewares always mount below Authenticate, so this cannot happen — but
// a future mis-wiring must deny rather than pass through.
func TestRequireRole_NoPrincipal(t *testing.T) {
	rec := httptest.NewRecorder()
	var called bool
	RequireRole(RoleMember)(okHandler(&called)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusForbidden {
		t.Errorf("no principal: got %d want %d", rec.Code, http.StatusForbidden)
	}
	// Pin the gate's own body so a 403 from anywhere else can't pass for a hit.
	if rec.Body.String() != "forbidden\n" {
		t.Errorf("no principal body: got %q want %q", rec.Body.String(), "forbidden\n")
	}
	if called {
		t.Error("no principal: denied but handler still ran")
	}
}

func TestAdminForDestructive(t *testing.T) {
	cases := []struct {
		method string
		role   Role
		want   int
	}{
		{http.MethodDelete, RoleMember, http.StatusForbidden},
		{http.MethodDelete, RoleAdmin, http.StatusOK},
		{http.MethodDelete, RoleOwner, http.StatusOK},
		{http.MethodDelete, Role(""), http.StatusForbidden},
		{http.MethodGet, RoleMember, http.StatusOK},
		{http.MethodPost, RoleMember, http.StatusOK},
		{http.MethodPatch, RoleMember, http.StatusOK},
		{http.MethodPut, RoleMember, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.method+"/"+string(c.role), func(t *testing.T) {
			rec := httptest.NewRecorder()
			var called bool
			AdminForDestructive(okHandler(&called)).ServeHTTP(rec, withRole(c.method, c.role))
			if rec.Code != c.want {
				t.Errorf("%s role=%s: got %d want %d", c.method, c.role, rec.Code, c.want)
			}
			if c.want == http.StatusForbidden && called {
				t.Errorf("%s role=%s: denied but handler still ran", c.method, c.role)
			}
		})
	}
}

// The zero role is what a principal carries before RequireOrg resolves the
// membership row. Mounting AdminForDestructive above RequireOrg would
// therefore deny every user including owners; this pins that behavior so the
// ordering requirement is visible in the test suite, not just a comment.
func TestAdminForDestructive_NoPrincipalDenies(t *testing.T) {
	rec := httptest.NewRecorder()
	var called bool
	AdminForDestructive(okHandler(&called)).ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/", nil))
	if rec.Code != http.StatusForbidden {
		t.Errorf("no principal DELETE: got %d want %d", rec.Code, http.StatusForbidden)
	}
	if called {
		t.Error("no principal DELETE: denied but handler still ran")
	}
}
