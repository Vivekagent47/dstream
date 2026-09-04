package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Vivekagent47/dstream/internal/store"
)

// seedPortalApp creates a fresh org (+owner) and an application in it, returning
// their ids. PortalEpoch starts at 0.
func seedPortalApp(t *testing.T, q *store.Queries) (appID, orgID uuid.UUID) {
	t.Helper()
	_, orgID = seedUserAndOrg(t, q, RoleOwner)
	app, err := q.CreateApplication(context.Background(), store.CreateApplicationParams{
		OrgID: store.UUID(orgID), Name: "Portal Test", Metadata: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	return store.GoUUID(app.ID), orgID
}

func TestRequirePortalHappyPath(t *testing.T) {
	q := store.New(testPool(t))
	appID, orgID := seedPortalApp(t, q)
	ps := newPortalSigner(t)
	tok, _ := ps.Mint(appID, orgID, 0)

	var gotApp, gotOrg string
	var gotSource Source
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotApp = chi.URLParam(r, "app_id")
		p, _ := FromContext(r.Context())
		gotOrg = p.OrgID.String()
		gotSource = p.Source
		w.WriteHeader(http.StatusOK)
	})

	r := chi.NewRouter()
	r.Handle("/x", RequirePortal(q, ps)(final))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	if gotApp != appID.String() {
		t.Fatalf("app_id not injected: got %q want %q", gotApp, appID.String())
	}
	if gotOrg != orgID.String() {
		t.Fatalf("principal OrgID: got %q want %q", gotOrg, orgID.String())
	}
	if gotSource != SourcePortal {
		t.Fatalf("principal Source: got %q want %q", gotSource, SourcePortal)
	}
}

func TestRequirePortalRejectsRevoked(t *testing.T) {
	q := store.New(testPool(t))
	appID, orgID := seedPortalApp(t, q)
	ps := newPortalSigner(t)
	tok, _ := ps.Mint(appID, orgID, 0)

	// Bump epoch so the token's epoch (0) no longer matches.
	if err := q.BumpApplicationPortalEpoch(context.Background(), store.BumpApplicationPortalEpochParams{
		ID: store.UUID(appID), OrgID: store.UUID(orgID),
	}); err != nil {
		t.Fatalf("bump epoch: %v", err)
	}

	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	r := chi.NewRouter()
	r.Handle("/x", RequirePortal(q, ps)(final))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token: got %d want 401", rec.Code)
	}
}

func TestRequirePortalRejectsGarbage(t *testing.T) {
	q := store.New(testPool(t))
	ps := newPortalSigner(t)

	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	r := chi.NewRouter()
	r.Handle("/x", RequirePortal(q, ps)(final))

	for _, hdr := range []string{"", "Bearer not-a-token", "Bearer dsk_abc_def"} {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		if hdr != "" {
			req.Header.Set("Authorization", hdr)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("hdr %q: got %d want 401", hdr, rec.Code)
		}
	}
}
