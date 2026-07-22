package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Vivekagent47/dstream/internal/store"
)

// --- pure unit coverage for the handler helpers ---

func TestRequireMinRole_NoPrincipal(t *testing.T) {
	if err := RequireMinRole(context.Background(), RoleMember); err == nil {
		t.Fatal("RequireMinRole(empty) returned nil; want error")
	}
}

func TestRequireMinRole_Matrix(t *testing.T) {
	cases := []struct {
		role Role
		min  Role
		ok   bool
	}{
		{RoleMember, RoleMember, true},
		{RoleAdmin, RoleMember, true},
		{RoleOwner, RoleMember, true},

		{RoleMember, RoleAdmin, false},
		{RoleAdmin, RoleAdmin, true},
		{RoleOwner, RoleAdmin, true},

		{RoleMember, RoleOwner, false},
		{RoleAdmin, RoleOwner, false},
		{RoleOwner, RoleOwner, true},
	}
	for _, c := range cases {
		ctx := WithPrincipal(context.Background(), Principal{
			Source: SourceSession,
			Role:   c.role,
		})
		err := RequireMinRole(ctx, c.min)
		gotOK := err == nil
		if gotOK != c.ok {
			t.Errorf("RequireMinRole(role=%s, min=%s) err=%v; want ok=%v", c.role, c.min, err, c.ok)
		}
		if !gotOK && !errors.Is(err, ErrInsufficientRole) {
			t.Errorf("unexpected error type: %v (want ErrInsufficientRole)", err)
		}
	}
}

func TestRequireSession(t *testing.T) {
	// API key principal → ErrSessionRequired.
	ctx := WithPrincipal(context.Background(), Principal{Source: SourceAPIKey})
	if err := RequireSession(ctx); !errors.Is(err, ErrSessionRequired) {
		t.Errorf("api_key principal: got %v, want ErrSessionRequired", err)
	}
	// Session principal → nil.
	ctx = WithPrincipal(context.Background(), Principal{Source: SourceSession})
	if err := RequireSession(ctx); err != nil {
		t.Errorf("session principal: got %v, want nil", err)
	}
	// No principal → non-nil "no principal" error (not ErrSessionRequired).
	if err := RequireSession(context.Background()); err == nil {
		t.Fatal("RequireSession(empty) returned nil; want error")
	}
}

// --- DB-gated integration coverage ---

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

// seedUserAndOrg creates a fresh user, a fresh org, and inserts an
// org_members row with the given role. Returns ids for use in test asserts.
func seedUserAndOrg(t *testing.T, q *store.Queries, role Role) (userID, orgID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	email := "test+" + uuid.NewString() + "@example.test"
	u, err := q.CreateUser(ctx, store.CreateUserParams{Email: email})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	o, err := q.CreateOrganization(ctx, store.CreateOrganizationParams{
		Name: "Test " + email,
		Slug: "test-" + uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := q.AddOrgMember(ctx, store.AddOrgMemberParams{
		OrgID:  o.ID,
		UserID: u.ID,
		Role:   string(role),
	}); err != nil {
		t.Fatalf("add member: %v", err)
	}
	return store.GoUUID(u.ID), store.GoUUID(o.ID)
}

// nextHandler captures the principal passed downstream so tests can
// verify Authenticate/RequireOrg populated it correctly.
type captured struct {
	called bool
	p      Principal
}

func (c *captured) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := FromContext(r.Context())
		c.called = true
		if err == nil {
			c.p = p
		}
		w.WriteHeader(http.StatusOK)
	})
}

func TestAuthenticate_Session_PopulatesPrincipal(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedUserAndOrg(t, q, RoleMember)

	s := newSigner(t)
	w := httptest.NewRecorder()
	s.Issue(w, uid, oid, 0)
	r := readSetCookie(t, w)

	cap := &captured{}
	mw := Authenticate(q, s)(cap.handler())

	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if !cap.called {
		t.Fatal("next handler not invoked")
	}
	if cap.p.Source != SourceSession || cap.p.UserID != uid || cap.p.OrgID != oid {
		t.Errorf("principal mismatch: %+v", cap.p)
	}
	// Role is NOT populated by Authenticate alone.
	if cap.p.Role != "" {
		t.Errorf("Authenticate populated Role=%q; should be empty until RequireOrg", cap.p.Role)
	}
}

func TestAuthenticate_Session_UnknownUser_401(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	s := newSigner(t)

	// Sign a valid cookie for a user that does not exist. GetUserByID errors
	// (pgx.ErrNoRows), so the epoch (revocation) check can't run — Authenticate
	// must fail closed with 401, not serve the request (audit #4).
	w := httptest.NewRecorder()
	s.Issue(w, uuid.New(), uuid.New(), 0)
	r := readSetCookie(t, w)

	cap := &captured{}
	mw := Authenticate(q, s)(cap.handler())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, r)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401 (fail closed on unknown user)", rec.Code)
	}
	if cap.called {
		t.Fatal("next handler was invoked; Authenticate failed open on lookup error")
	}
}

func TestAuthenticate_NoCreds_401(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	s := newSigner(t)
	mw := Authenticate(q, s)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
}

func TestRequireOrg_Session_PopulatesRole(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedUserAndOrg(t, q, RoleAdmin)

	cap := &captured{}
	mw := RequireOrg(q)(cap.handler())

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = r.WithContext(WithPrincipal(r.Context(), Principal{
		Source: SourceSession,
		UserID: uid,
		OrgID:  oid,
	}))
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if cap.p.Role != RoleAdmin {
		t.Errorf("Role: got %q, want %q", cap.p.Role, RoleAdmin)
	}
}

func TestRequireOrg_NilOrg_409(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)

	mw := RequireOrg(q)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = r.WithContext(WithPrincipal(r.Context(), Principal{
		Source: SourceSession,
		UserID: uuid.New(),
		OrgID:  uuid.Nil,
	}))
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, r)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409", rec.Code)
	}
}

func TestRequireOrg_NonMember_403(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)

	// User belongs to orgA; we'll claim active_org_id = orgB (which exists
	// but has no membership for this user).
	uid, _ := seedUserAndOrg(t, q, RoleMember)
	// Second org with no membership for uid.
	otherOrg, err := q.CreateOrganization(context.Background(), store.CreateOrganizationParams{
		Name: "Other",
		Slug: "other-" + uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("create other org: %v", err)
	}
	otherOID := store.GoUUID(otherOrg.ID)

	mw := RequireOrg(q)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = r.WithContext(WithPrincipal(r.Context(), Principal{
		Source: SourceSession,
		UserID: uid,
		OrgID:  otherOID,
	}))
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403", rec.Code)
	}
}

func TestRequireOrg_APIKey_PassesThroughAsAdmin(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)

	cap := &captured{}
	mw := RequireOrg(q)(cap.handler())

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = r.WithContext(WithPrincipal(r.Context(), Principal{
		Source: SourceAPIKey,
		OrgID:  uuid.New(),
	}))
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if cap.p.Role != RoleAdmin {
		t.Errorf("Role: got %q, want %q (sentinel for api_key)", cap.p.Role, RoleAdmin)
	}
}

func TestAuthenticate_APIKey_PopulatesPrincipal(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)

	// Need an org for the api_key FK.
	o, err := q.CreateOrganization(context.Background(), store.CreateOrganizationParams{
		Name: "KeyOrg",
		Slug: "keyorg-" + uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}

	full, prefix, hash, err := NewAPIKey()
	if err != nil {
		t.Fatalf("new api key: %v", err)
	}
	row, err := q.CreateAPIKey(context.Background(), store.CreateAPIKeyParams{
		OrgID:   o.ID,
		Name:    "test",
		Prefix:  prefix,
		KeyHash: hash,
	})
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}

	s := newSigner(t)
	cap := &captured{}
	mw := Authenticate(q, s)(cap.handler())

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+full)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if cap.p.Source != SourceAPIKey {
		t.Errorf("Source: got %q, want api_key", cap.p.Source)
	}
	if cap.p.OrgID != store.GoUUID(o.ID) {
		t.Errorf("OrgID: got %s, want %s", cap.p.OrgID, store.GoUUID(o.ID))
	}
	if cap.p.APIKeyID != store.GoUUID(row.ID) {
		t.Errorf("APIKeyID: got %s, want %s", cap.p.APIKeyID, store.GoUUID(row.ID))
	}
}
