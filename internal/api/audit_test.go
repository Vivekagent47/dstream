package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/store"
)

// --- DB-gated harness ---

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

func seedUserAndOrg(t *testing.T, q *store.Queries) (userID, orgID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	u, err := q.CreateUser(ctx, store.CreateUserParams{
		Email: "test+" + uuid.NewString() + "@example.test",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	o, err := q.CreateOrganization(ctx, store.CreateOrganizationParams{
		Name: "T",
		Slug: "t-" + uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := q.AddOrgMember(ctx, store.AddOrgMemberParams{
		OrgID:  o.ID,
		UserID: u.ID,
		Role:   "owner",
	}); err != nil {
		t.Fatalf("add member: %v", err)
	}
	return store.GoUUID(u.ID), store.GoUUID(o.ID)
}

// insertAudit is a small helper that bypasses the package's audit.Log so the
// test can fabricate rows with specific action/target_type/actor values.
func insertAudit(t *testing.T, q *store.Queries, orgID, userID uuid.UUID, action, targetType string) {
	t.Helper()
	if err := q.InsertAuditLog(context.Background(), store.InsertAuditLogParams{
		OrgID:       store.UUID(orgID),
		ActorUserID: store.UUID(userID),
		Action:      action,
		TargetType:  targetType,
		Metadata:    []byte(`{}`),
	}); err != nil {
		t.Fatalf("insert audit: %v", err)
	}
}

// newTestRouter returns a chi router with /api mounted using a fresh signer.
func newTestRouter(q *store.Queries) (*chi.Mux, *auth.SessionSigner) {
	r := chi.NewRouter()
	s := &auth.SessionSigner{Secret: []byte("test-secret-do-not-use-in-prod")}
	d := Deps{Queries: q, Signer: s}
	Mount(r, d)
	return r, s
}

// requestWithSession builds an *http.Request with a valid session cookie for
// (userID, orgID).
func requestWithSession(t *testing.T, s *auth.SessionSigner, method, path string, userID, orgID uuid.UUID) *http.Request {
	t.Helper()
	w := httptest.NewRecorder()
	s.Issue(w, userID, orgID)
	res := w.Result()
	defer res.Body.Close()
	r := httptest.NewRequest(method, path, nil)
	for _, c := range res.Cookies() {
		if c.Name == auth.SessionCookieName {
			r.AddCookie(c)
		}
	}
	return r
}

func TestListAudit_ScopedToActiveOrg(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)

	uidA, oidA := seedUserAndOrg(t, q)
	_, oidB := seedUserAndOrg(t, q)

	insertAudit(t, q, oidA, uidA, "source.create", "source")
	insertAudit(t, q, oidA, uidA, "source.delete", "source")
	insertAudit(t, q, oidB, uidA, "source.create", "source") // different org

	router, signer := newTestRouter(q)
	req := requestWithSession(t, signer, http.MethodGet, "/api/audit", uidA, oidA)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Entries []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Entries) != 2 {
		t.Fatalf("entries: got %d, want 2 (orgA only): %v", len(resp.Entries), resp.Entries)
	}
	for _, e := range resp.Entries {
		target := e["target"].(map[string]any)
		if target["type"] != "source" {
			t.Errorf("unexpected target_type: %v", target["type"])
		}
		actor := e["actor"].(map[string]any)
		if actor["type"] != "user" {
			t.Errorf("expected actor.type=user; got %v", actor["type"])
		}
	}
}

func TestListAudit_APIKeyForbidden(t *testing.T) {
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
	req := httptest.NewRequest(http.MethodGet, "/api/audit", nil)
	req.Header.Set("Authorization", "Bearer "+full)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d (want 403); body=%s", rec.Code, rec.Body.String())
	}
}

func TestListAudit_FilterByAction(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedUserAndOrg(t, q)

	insertAudit(t, q, oid, uid, "source.create", "source")
	insertAudit(t, q, oid, uid, "source.delete", "source")
	insertAudit(t, q, oid, uid, "destination.create", "destination")

	router, signer := newTestRouter(q)
	req := requestWithSession(t, signer, http.MethodGet, "/api/audit?action=source.create", uid, oid)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Entries []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Entries) != 1 || resp.Entries[0]["action"] != "source.create" {
		t.Fatalf("expected single source.create; got %v", resp.Entries)
	}
}

func TestListAudit_Pagination(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedUserAndOrg(t, q)

	for i := 0; i < 5; i++ {
		insertAudit(t, q, oid, uid, "source.create", "source")
	}

	router, signer := newTestRouter(q)

	// First page (limit=2).
	req := requestWithSession(t, signer, http.MethodGet, "/api/audit?limit=2", uid, oid)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("p1 status: got %d body=%s", rec.Code, rec.Body.String())
	}
	var p1 struct {
		Entries      []map[string]any `json:"entries"`
		NextBeforeID *int64           `json:"next_before_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p1); err != nil {
		t.Fatalf("decode p1: %v", err)
	}
	if len(p1.Entries) != 2 {
		t.Fatalf("p1 entries: got %d want 2", len(p1.Entries))
	}
	if p1.NextBeforeID == nil {
		t.Fatal("p1 next_before_id missing")
	}

	// Second page using the cursor.
	urlP2 := "/api/audit?limit=2&before_id=" + strconv.FormatInt(*p1.NextBeforeID, 10)
	req = requestWithSession(t, signer, http.MethodGet, urlP2, uid, oid)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("p2 status: got %d body=%s", rec.Code, rec.Body.String())
	}
	var p2 struct {
		Entries      []map[string]any `json:"entries"`
		NextBeforeID *int64           `json:"next_before_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p2); err != nil {
		t.Fatalf("decode p2: %v", err)
	}
	if len(p2.Entries) != 2 {
		t.Fatalf("p2 entries: got %d want 2", len(p2.Entries))
	}
	// IDs in p1 must be strictly greater than IDs in p2.
	p1Min := int64Of(p1.Entries[len(p1.Entries)-1]["id"])
	p2Max := int64Of(p2.Entries[0]["id"])
	if p2Max >= p1Min {
		t.Fatalf("pagination overlap: p1 min=%d, p2 max=%d", p1Min, p2Max)
	}
}

// int64Of pulls a JSON-decoded number (float64) into int64. We use this
// instead of round-tripping through encoding/json with a custom type because
// the test only cares about ordering, not exact representation.
func int64Of(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case int64:
		return x
	default:
		return 0
	}
}
