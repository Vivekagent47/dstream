package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
