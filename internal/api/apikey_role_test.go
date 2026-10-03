package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/store"
)

// newKeyAt mints a real API key for orgID at the given role and returns the
// plaintext credential.
func newKeyAt(t *testing.T, q *store.Queries, orgID uuid.UUID, role string) string {
	t.Helper()
	full, prefix, hash, err := auth.NewAPIKey()
	if err != nil {
		t.Fatalf("new key: %v", err)
	}
	if _, err := q.CreateAPIKey(context.Background(), store.CreateAPIKeyParams{
		OrgID:   store.UUID(orgID),
		Name:    "t",
		Prefix:  prefix,
		KeyHash: hash,
		Role:    role,
	}); err != nil {
		t.Fatalf("create key: %v", err)
	}
	return full
}

func keyRequest(t *testing.T, router http.Handler, key, method, path string) int {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec.Code
}

func TestAPIKeyRole_MemberCannotDelete(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	_, orgID := seedUserAndOrg(t, q)
	router, _ := newTestRouterPool(pool, q)

	key := newKeyAt(t, q, orgID, string(auth.RoleMember))

	if code := keyRequest(t, router, key, http.MethodDelete, "/api/sources/"+sampleID); code != http.StatusForbidden {
		t.Errorf("member key DELETE: got %d want 403", code)
	}
	if code := keyRequest(t, router, key, http.MethodGet, "/api/sources"); code == http.StatusForbidden {
		t.Errorf("member key GET: got 403, want it allowed")
	}
}

func TestAPIKeyRole_AdminKeyUnchanged(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	_, orgID := seedUserAndOrg(t, q)
	router, _ := newTestRouterPool(pool, q)

	key := newKeyAt(t, q, orgID, string(auth.RoleAdmin))
	// Assert the exact 404 the handler returns for the unseeded sampleID, not
	// merely "not 403": a key that stopped authenticating at all would 401 and
	// satisfy "not 403" while failing the very guarantee this test pins.
	if code := keyRequest(t, router, key, http.MethodDelete, "/api/sources/"+sampleID); code != http.StatusNotFound {
		t.Errorf("admin key DELETE: got %d, want 404 from the handler", code)
	}
}

// A role the ladder doesn't know must not be storable at all: the CHECK
// constraint is the backstop under the Go-side ladder. That the ladder itself
// denies an unknown or empty role is pinned in internal/auth/rbac_test.go.
func TestAPIKeyRole_UnknownRoleRefusedByCheck(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	_, orgID := seedUserAndOrg(t, q)

	// One valid row in this test's freshly seeded org, purely as the subject
	// of the UPDATE below; its plaintext credential is never used.
	newKeyAt(t, q, orgID, string(auth.RoleAdmin))
	if _, err := pool.Exec(context.Background(),
		`UPDATE api_keys SET role = 'bogus' WHERE org_id = $1`, store.UUID(orgID)); err == nil {
		t.Fatal("CHECK must refuse an unknown role")
	}
}

// Identity routes resolve the caller by p.UserID, which is uuid.Nil for a key
// principal. That must stay a clean 403, not a 500, now that keys carry a role.
func TestAPIKeyRole_IdentityRoutesStillRefuseKeys(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	_, orgID := seedUserAndOrg(t, q)
	router, _ := newTestRouterPool(pool, q)

	key := newKeyAt(t, q, orgID, string(auth.RoleAdmin))
	code := keyRequest(t, router, key, http.MethodGet, "/api/orgs/"+orgID.String()+"/members")
	if code != http.StatusForbidden {
		t.Errorf("api key on identity route: got %d want 403", code)
	}
}

func TestCreateAPIKey_RoleValidation(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	userID, orgID := seedUserAndOrg(t, q) // seeded as owner
	router, signer := newTestRouterPool(pool, q)

	post := func(body map[string]any) (int, map[string]any) {
		req := requestWithSessionBody(t, signer, http.MethodPost, "/api/orgs/"+orgID.String()+"/api-keys", userID, orgID, body)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	if code, _ := post(map[string]any{"name": "owner-key", "role": "owner"}); code != http.StatusBadRequest {
		t.Errorf("role=owner: got %d want 400", code)
	}
	if code, _ := post(map[string]any{"name": "bogus-key", "role": "bogus"}); code != http.StatusBadRequest {
		t.Errorf("role=bogus: got %d want 400", code)
	}
	code, out := post(map[string]any{"name": "default-key"})
	if code != http.StatusCreated {
		t.Fatalf("no role: got %d want 201", code)
	}
	if out["role"] != "admin" {
		t.Errorf("default role: got %v want admin", out["role"])
	}

	// The response's "role" is the handler echoing its own body back, so it
	// proves nothing about what got persisted. Mint a member key through the
	// API and then authenticate with it: that walks handler -> store ->
	// Authenticate -> AdminForDestructive, and fails if params.Role were ever
	// hardcoded to admin.
	code, out = post(map[string]any{"name": "member-key", "role": "member"})
	if code != http.StatusCreated || out["role"] != "member" {
		t.Fatalf("role=member: got %d %v", code, out["role"])
	}
	minted, ok := out["key"].(string)
	if !ok {
		t.Fatalf("response has no string \"key\": %v", out)
	}
	if code = keyRequest(t, router, minted, http.MethodDelete, "/api/sources/"+sampleID); code != http.StatusForbidden {
		t.Errorf("minted member key DELETE: got %d want 403", code)
	}
}
