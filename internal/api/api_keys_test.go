package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Vivekagent47/dstream/internal/store"
)

// --- POST /api/orgs/{org_id}/api-keys ---

func TestCreateAPIKey_Admin_ReturnsSecretOnce(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedUserAndOrg(t, q)

	router, signer := newTestRouter(q)
	req := requestWithSessionBody(t, signer, http.MethodPost,
		"/api/orgs/"+oid.String()+"/api-keys", uid, oid,
		map[string]any{"name": "ci-key"})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	key, ok := resp["key"].(string)
	if !ok || key == "" {
		t.Fatalf("expected plaintext key in response, got %v", resp)
	}
	if !strings.HasPrefix(key, "dsk_") {
		t.Errorf("key shape: got %q want dsk_ prefix", key)
	}
	if resp["prefix"] == nil || resp["prefix"] == "" {
		t.Errorf("expected prefix; got %v", resp)
	}
	if resp["id"] == nil {
		t.Errorf("expected id; got %v", resp)
	}
}

func TestCreateAPIKey_MemberCannot_403(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	_, oid := seedUserAndOrg(t, q)
	member := seedUser(t, q)
	addMember(t, q, oid, member, "member")

	router, signer := newTestRouter(q)
	req := requestWithSessionBody(t, signer, http.MethodPost,
		"/api/orgs/"+oid.String()+"/api-keys", member, oid,
		map[string]any{"name": "x"})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestCreateAPIKey_EmptyName_400(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedUserAndOrg(t, q)

	router, signer := newTestRouter(q)
	req := requestWithSessionBody(t, signer, http.MethodPost,
		"/api/orgs/"+oid.String()+"/api-keys", uid, oid,
		map[string]any{"name": "   "})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// --- GET /api/orgs/{org_id}/api-keys ---

func TestListAPIKeys_StripsHash(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedUserAndOrg(t, q)

	// Mint a key inline.
	router, signer := newTestRouter(q)
	req := requestWithSessionBody(t, signer, http.MethodPost,
		"/api/orgs/"+oid.String()+"/api-keys", uid, oid,
		map[string]any{"name": "test"})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status: got %d body=%s", rec.Code, rec.Body.String())
	}

	// Now list.
	req = requestWithSession(t, signer, http.MethodGet,
		"/api/orgs/"+oid.String()+"/api-keys", uid, oid)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status: got %d body=%s", rec.Code, rec.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows: got %d want 1", len(rows))
	}
	row := rows[0]
	if _, ok := row["key_hash"]; ok {
		t.Errorf("list response leaked key_hash: %v", row)
	}
	if _, ok := row["key"]; ok {
		t.Errorf("list response leaked plaintext key: %v", row)
	}
	if row["prefix"] == nil || row["name"] == nil || row["id"] == nil {
		t.Errorf("missing expected fields: %v", row)
	}
}

func TestListAPIKeys_AnyMember(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedUserAndOrg(t, q)
	member := seedUser(t, q)
	addMember(t, q, oid, member, "member")

	// Owner mints, member lists.
	router, signer := newTestRouter(q)
	req := requestWithSessionBody(t, signer, http.MethodPost,
		"/api/orgs/"+oid.String()+"/api-keys", uid, oid,
		map[string]any{"name": "shared"})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status: got %d body=%s", rec.Code, rec.Body.String())
	}

	req = requestWithSession(t, signer, http.MethodGet,
		"/api/orgs/"+oid.String()+"/api-keys", member, oid)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", rec.Code, rec.Body.String())
	}
}

// --- DELETE /api/orgs/{org_id}/api-keys/{id} ---

func TestRevokeAPIKey_Admin_MarksRevoked(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedUserAndOrg(t, q)

	router, signer := newTestRouter(q)
	req := requestWithSessionBody(t, signer, http.MethodPost,
		"/api/orgs/"+oid.String()+"/api-keys", uid, oid,
		map[string]any{"name": "to-revoke"})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status: got %d body=%s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	keyID, err := uuid.Parse(created["id"].(string))
	if err != nil {
		t.Fatalf("parse id: %v", err)
	}

	req = requestWithSession(t, signer, http.MethodDelete,
		"/api/orgs/"+oid.String()+"/api-keys/"+keyID.String(), uid, oid)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status: got %d want 204; body=%s", rec.Code, rec.Body.String())
	}

	// ListAPIKeysByOrg filters out revoked rows.
	rows, err := q.ListAPIKeysByOrg(context.Background(), store.UUID(oid))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, r := range rows {
		if store.GoUUID(r.ID) == keyID {
			t.Errorf("revoked key still listed: %v", r)
		}
	}
}

func TestRevokeAPIKey_MemberCannot_403(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedUserAndOrg(t, q)
	member := seedUser(t, q)
	addMember(t, q, oid, member, "member")

	// Owner mints.
	router, signer := newTestRouter(q)
	req := requestWithSessionBody(t, signer, http.MethodPost,
		"/api/orgs/"+oid.String()+"/api-keys", uid, oid,
		map[string]any{"name": "k"})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	keyID, err := uuid.Parse(created["id"].(string))
	if err != nil {
		t.Fatalf("parse id: %v", err)
	}

	// Member attempts to revoke.
	req = requestWithSession(t, signer, http.MethodDelete,
		"/api/orgs/"+oid.String()+"/api-keys/"+keyID.String(), member, oid)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// =============================================================================
// Key lifecycle and refusals. Harness (idEnv, failOn, wantErr, ...) lives in
// orgs_test.go.
// =============================================================================

// withKey sends a request authenticated by an API key.
func (e *idEnv) withKey(key, method, path string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			e.t.Fatalf("encode: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	req.RemoteAddr = uniqueClientAddr()
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

// keyRow is what the database holds for one key.
type keyRow struct {
	exists    bool
	name      string
	role      string
	prefix    string
	hash      []byte
	revoked   bool
	revokedAt time.Time
	expiresAt *time.Time
}

func (e *idEnv) keyRow(id string) keyRow {
	e.t.Helper()
	var r keyRow
	var rev *time.Time
	err := e.pool.QueryRow(context.Background(),
		`SELECT name, role, prefix, key_hash, revoked_at, expires_at FROM api_keys WHERE id=$1`, id).
		Scan(&r.name, &r.role, &r.prefix, &r.hash, &rev, &r.expiresAt)
	if err != nil {
		return keyRow{}
	}
	r.exists = true
	if rev != nil {
		r.revoked, r.revokedAt = true, *rev
	}
	return r
}

func (e *idEnv) keyCount(org uuid.UUID) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM api_keys WHERE org_id=$1`, org).Scan(&n); err != nil {
		e.t.Fatalf("key count: %v", err)
	}
	return n
}

// mintKey creates a key through the API as an admin and returns (id, secret).
func (e *idEnv) mintKey(admin, org uuid.UUID, body map[string]any) (id, secret string) {
	e.t.Helper()
	rec := e.do(http.MethodPost, orgPath(org, "/api-keys"), admin, org, body)
	wantStatus(e.t, rec, http.StatusCreated)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		e.t.Fatalf("decode: %v", err)
	}
	return out["id"].(string), out["key"].(string)
}

// authStatus is the status GET /api/me gives a bearer key: 200 while the key
// authenticates, 401 once it does not.
func (e *idEnv) authStatus(key string) int {
	e.t.Helper()
	return e.withKey(key, http.MethodGet, "/api/me", nil).Code
}

func TestAPIKey_Lifecycle_SecretOnceHashOnlyRevokeStopsAuth(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	owner, org := e.seedOrg()

	rec := e.do(http.MethodPost, orgPath(org, "/api-keys"), owner, org,
		map[string]any{"name": "  ci  ", "role": "member", "expires_in_days": 30})
	wantStatus(t, rec, http.StatusCreated)
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	id, secret := created["id"].(string), created["key"].(string)
	if created["name"] != "ci" || created["role"] != "member" {
		t.Fatalf("response: %v", created)
	}

	// The stored row holds a hash of the secret, never the secret itself.
	row := e.keyRow(id)
	if !row.exists || row.name != "ci" || row.role != "member" {
		t.Fatalf("stored row: %+v", row)
	}
	_, after, _ := strings.Cut(secret, "_"+row.prefix+"_")
	sum := sha256.Sum256([]byte(after))
	if after == "" || !bytes.Equal(row.hash, sum[:]) {
		t.Fatalf("key_hash is not sha256(secret)")
	}
	if bytes.Contains(row.hash, []byte(after)) || row.prefix != created["prefix"] {
		t.Fatalf("stored material leaks or mismatches the secret")
	}
	if row.expiresAt == nil || time.Until(*row.expiresAt) < 29*24*time.Hour || time.Until(*row.expiresAt) > 31*24*time.Hour {
		t.Fatalf("expires_at: %v want ~30d out", row.expiresAt)
	}
	if n := e.auditCount(org, "api_key.create"); n != 1 {
		t.Fatalf("api_key.create audit rows: %d want 1", n)
	}

	// List shows the key's metadata and nothing secret.
	rec = e.do(http.MethodGet, orgPath(org, "/api-keys"), owner, org, nil)
	wantStatus(t, rec, http.StatusOK)
	if strings.Contains(rec.Body.String(), after) || strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("list leaked the secret: %s", rec.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(rows) != 1 || rows[0]["id"] != id || rows[0]["role"] != "member" || rows[0]["prefix"] != row.prefix {
		t.Fatalf("list: %v", rows)
	}
	for _, banned := range []string{"key", "key_hash", "secret"} {
		if _, ok := rows[0][banned]; ok {
			t.Errorf("list exposes %q", banned)
		}
	}

	if got := e.authStatus(secret); got != http.StatusOK {
		t.Fatalf("fresh key: got %d want 200", got)
	}

	// Revoke: 204, row stamped, and the key is refused afterwards.
	wantStatus(t, e.do(http.MethodDelete, orgPath(org, "/api-keys/"+id), owner, org, nil), http.StatusNoContent)
	row = e.keyRow(id)
	if !row.revoked {
		t.Fatal("revoked_at not set")
	}
	if got := e.authStatus(secret); got != http.StatusUnauthorized {
		t.Fatalf("revoked key: got %d want 401", got)
	}
	if n := e.auditCount(org, "api_key.revoke"); n != 1 {
		t.Fatalf("api_key.revoke audit rows: %d want 1", n)
	}
	rec = e.do(http.MethodGet, orgPath(org, "/api-keys"), owner, org, nil)
	if strings.Contains(rec.Body.String(), id) {
		t.Fatalf("revoked key still listed: %s", rec.Body.String())
	}

	// Documents CURRENT behaviour, not contract: a second revoke answers 204,
	// leaves revoked_at at the original time, and still writes a second
	// api_key.revoke audit row for an event that happened once (the handler
	// audits unconditionally after the UPDATE).
	first := row.revokedAt
	wantStatus(t, e.do(http.MethodDelete, orgPath(org, "/api-keys/"+id), owner, org, nil), http.StatusNoContent)
	if again := e.keyRow(id); !again.revokedAt.Equal(first) {
		t.Fatalf("second revoke moved revoked_at: %v -> %v", first, again.revokedAt)
	}
	if n := e.auditCount(org, "api_key.revoke"); n != 2 {
		t.Fatalf("api_key.revoke audit rows after the repeat revoke: %d (current behaviour: 2, one duplicate)", n)
	}
	if got := e.authStatus(secret); got != http.StatusUnauthorized {
		t.Fatalf("key after double revoke: got %d want 401", got)
	}
}

func TestCreateAPIKey_NonPositiveExpiryIsNonExpiring(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	owner, org := e.seedOrg()
	for _, days := range []int{0, -5} {
		id, _ := e.mintKey(owner, org, map[string]any{"name": "forever", "expires_in_days": days})
		if row := e.keyRow(id); row.expiresAt != nil || row.role != "admin" {
			t.Fatalf("days=%d: expires_at=%v role=%q want NULL/admin", days, row.expiresAt, row.role)
		}
	}
}

func TestCreateAPIKey_Refusals(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	owner, org := e.seedOrg()
	_, otherOrg := e.seedOrg()
	member := e.seedRole(org, "member")
	stranger := seedUser(t, e.q)
	key := newKeyAt(t, e.q, org, "admin")

	path := orgPath(org, "/api-keys")
	cases := []struct {
		name string
		do   func() *httptest.ResponseRecorder
		code int
		msg  string
	}{
		{"api key principal", func() *httptest.ResponseRecorder {
			return e.withKey(key, http.MethodPost, path, map[string]any{"name": "k"})
		}, 403, "session required"},
		{"bad org id", func() *httptest.ResponseRecorder {
			return e.do(http.MethodPost, "/api/orgs/not-a-uuid/api-keys", owner, org, map[string]any{"name": "k"})
		}, 400, "invalid org_id"},
		{"not a member", func() *httptest.ResponseRecorder {
			return e.do(http.MethodPost, path, stranger, org, map[string]any{"name": "k"})
		}, 403, "not a member"},
		{"admin of another org", func() *httptest.ResponseRecorder {
			return e.do(http.MethodPost, orgPath(otherOrg, "/api-keys"), owner, org, map[string]any{"name": "k"})
		}, 403, "not a member"},
		{"member", func() *httptest.ResponseRecorder {
			return e.do(http.MethodPost, path, member, org, map[string]any{"name": "k"})
		}, 403, "admin required"},
		{"bad json", func() *httptest.ResponseRecorder {
			req := requestWithSessionBody(t, e.signer, http.MethodPost, path, owner, org, nil)
			req.Body = http.NoBody
			rec := httptest.NewRecorder()
			e.h.ServeHTTP(rec, req)
			return rec
		}, 400, "invalid json"},
		{"blank name", func() *httptest.ResponseRecorder {
			return e.do(http.MethodPost, path, owner, org, map[string]any{"name": " \t "})
		}, 400, "name required"},
		{"owner role", func() *httptest.ResponseRecorder {
			return e.do(http.MethodPost, path, owner, org, map[string]any{"name": "k", "role": "owner"})
		}, 400, "role must be admin or member"},
		{"unknown role", func() *httptest.ResponseRecorder {
			return e.do(http.MethodPost, path, owner, org, map[string]any{"name": "k", "role": "root"})
		}, 400, "role must be admin or member"},
	}
	// The one key above is the only row; every refusal must leave it alone.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantErr(t, tc.do(), tc.code, tc.msg)
			if n := e.keyCount(org) + e.keyCount(otherOrg); n != 1 {
				t.Fatalf("a refused create changed the key set: %d keys want 1", n)
			}
			if n := e.auditCount(org, "api_key.create"); n != 0 {
				t.Fatalf("refused create audited %d times", n)
			}
		})
	}
}

func TestListAPIKeys_Refusals(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	owner, org := e.seedOrg()
	_, otherOrg := e.seedOrg()
	key := newKeyAt(t, e.q, otherOrg, "admin")

	wantErr(t, e.withKey(key, http.MethodGet, orgPath(otherOrg, "/api-keys"), nil), 403, "session required")
	wantErr(t, e.do(http.MethodGet, "/api/orgs/nope/api-keys", owner, org, nil), 400, "invalid org_id")
	// Cross-org: org A's owner must not read org B's keys, nor learn their prefixes.
	rec := e.do(http.MethodGet, orgPath(otherOrg, "/api-keys"), owner, org, nil)
	wantErr(t, rec, 403, "not a member")
	if strings.Contains(rec.Body.String(), "dsk_") {
		t.Fatalf("refusal leaked key material: %s", rec.Body.String())
	}
}

func TestRevokeAPIKey_Refusals_LeaveKeyAuthenticating(t *testing.T) {
	e := newIDEnv(t, nil, nil)
	owner, org := e.seedOrg()
	member := e.seedRole(org, "member")
	otherOwner, otherOrg := e.seedOrg()
	stranger := seedUser(t, e.q)
	id, secret := e.mintKey(owner, org, map[string]any{"name": "target"})
	otherID, otherSecret := e.mintKey(otherOwner, otherOrg, map[string]any{"name": "other"})
	callerKey := newKeyAt(t, e.q, org, "admin")

	cases := []struct {
		name string
		do   func() *httptest.ResponseRecorder
		code int
		msg  string
	}{
		{"api key principal", func() *httptest.ResponseRecorder {
			return e.withKey(callerKey, http.MethodDelete, orgPath(org, "/api-keys/"+id), nil)
		}, 403, "session required"},
		{"bad org id", func() *httptest.ResponseRecorder {
			return e.do(http.MethodDelete, "/api/orgs/nope/api-keys/"+id, owner, org, nil)
		}, 400, "invalid org_id"},
		{"bad key id", func() *httptest.ResponseRecorder {
			return e.do(http.MethodDelete, orgPath(org, "/api-keys/nope"), owner, org, nil)
		}, 400, "invalid key id"},
		{"not a member", func() *httptest.ResponseRecorder {
			return e.do(http.MethodDelete, orgPath(org, "/api-keys/"+id), stranger, org, nil)
		}, 403, "not a member"},
		{"member", func() *httptest.ResponseRecorder {
			return e.do(http.MethodDelete, orgPath(org, "/api-keys/"+id), member, org, nil)
		}, 403, "admin required"},
		{"other org's admin", func() *httptest.ResponseRecorder {
			return e.do(http.MethodDelete, orgPath(org, "/api-keys/"+id), otherOwner, otherOrg, nil)
		}, 403, "not a member"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantErr(t, tc.do(), tc.code, tc.msg)
			if row := e.keyRow(id); row.revoked || e.authStatus(secret) != http.StatusOK {
				t.Fatal("a refused revoke killed the key")
			}
		})
	}

	// Documents CURRENT behaviour, not contract: org A's admin names org B's
	// key under org A's path. The response is 204, B's key is untouched (the
	// UPDATE is scoped by org_id and matches no row), yet org A's audit log
	// gains an api_key.revoke entry for a key that was not revoked.
	wantStatus(t, e.do(http.MethodDelete, orgPath(org, "/api-keys/"+otherID), owner, org, nil), http.StatusNoContent)
	if row := e.keyRow(otherID); row.revoked || e.authStatus(otherSecret) != http.StatusOK {
		t.Fatal("org A revoked org B's key")
	}
	if n := e.auditCount(org, "api_key.revoke"); n != 1 {
		t.Fatalf("org A api_key.revoke audit rows: %d (current behaviour: 1, a false entry)", n)
	}
	if n := e.auditCount(otherOrg, "api_key.revoke"); n != 0 {
		t.Fatalf("org B audit rows: %d want 0", n)
	}
}

func TestAPIKey_StoreFailures_500AndNoSideEffect(t *testing.T) {
	t.Run("list", func(t *testing.T) {
		e := newIDEnv(t, failOn("from api_keys where org_id"), nil)
		owner, org := e.seedOrg()
		wantErr(t, e.do(http.MethodGet, orgPath(org, "/api-keys"), owner, org, nil), 500, "list keys")
	})
	t.Run("create", func(t *testing.T) {
		e := newIDEnv(t, failOn("insert into api_keys"), nil)
		owner, org := e.seedOrg()
		wantErr(t, e.do(http.MethodPost, orgPath(org, "/api-keys"), owner, org, map[string]any{"name": "k"}), 500, "create key")
		if n := e.keyCount(org); n != 0 {
			t.Fatalf("failed create left %d keys", n)
		}
		if n := e.auditCount(org, "api_key.create"); n != 0 {
			t.Fatalf("failed create audited %d times", n)
		}
	})
	t.Run("revoke", func(t *testing.T) {
		e := newIDEnv(t, failOn("update api_keys set revoked_at"), nil)
		owner, org := e.seedOrg()
		id, secret := e.mintKey(owner, org, map[string]any{"name": "k"})
		wantErr(t, e.do(http.MethodDelete, orgPath(org, "/api-keys/"+id), owner, org, nil), 500, "revoke key")
		if e.keyRow(id).revoked || e.authStatus(secret) != http.StatusOK {
			t.Fatal("failed revoke still killed the key")
		}
		if n := e.auditCount(org, "api_key.revoke"); n != 0 {
			t.Fatalf("failed revoke audited %d times", n)
		}
	})
}
