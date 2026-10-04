package outbound

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Vivekagent47/dstream/internal/store"
)

// --- Repointing an endpoint requires admin ---
//
// The twin of TestMemberCannotRepointDestination in internal/api. Both go
// through auth.RequireAdminForURLChange, but each handler decides for itself
// whether the URL actually moved, and PatchEndpoint's version reads the stored
// row to find out — so it needs its own coverage.
//
// An endpoint is a live subscriber: whatever URL it holds receives every
// message the app publishes. A member who can move it receives them instead,
// and nothing in the product announces that.

func epPatchRoutes(r chi.Router, h Handlers) {
	r.Route("/applications", func(r chi.Router) {
		r.Post("/", h.CreateApplication)
		r.Route("/{app_id}/endpoints", func(r chi.Router) {
			r.Post("/", h.CreateEndpoint)
			r.Get("/{id}", h.GetEndpoint)
			r.Patch("/{id}", h.PatchEndpoint)
		})
	})
}

// seedMember adds a second user to an existing org at the member role and
// returns their id, so a test can act as someone who is not the owner.
func seedMember(t *testing.T, q *store.Queries, orgID uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	name := "Member"
	u, err := q.CreateUser(ctx, store.CreateUserParams{
		Email: "member-" + uuid.NewString() + "@example.test",
		Name:  &name,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := q.AddOrgMember(ctx, store.AddOrgMemberParams{
		OrgID: store.UUID(orgID), UserID: u.ID, Role: "member",
	}); err != nil {
		t.Fatalf("add member: %v", err)
	}
	return store.GoUUID(u.ID)
}

// seedAppWithEndpoint creates an application and one endpoint through the API
// as the owner, and returns their ids plus the endpoint's URL.
func seedAppWithEndpoint(t *testing.T, r *chi.Mux, uid, oid uuid.UUID, url string) (appID, epID string) {
	t.Helper()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPost, "/api/applications", uid, oid,
		map[string]any{"name": "App " + uuid.NewString()[:8]}))
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("create app: got %d body=%s", rec.Code, rec.Body.String())
	}
	var app map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &app)
	appID, _ = app["id"].(string)

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPost, "/api/applications/"+appID+"/endpoints", uid, oid,
		map[string]any{"url": url}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create endpoint: got %d body=%s", rec.Code, rec.Body.String())
	}
	var ep map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &ep)
	epID, _ = ep["id"].(string)
	return appID, epID
}

func TestMemberCannotRepointEndpoint(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	member := seedMember(t, q, oid)
	r := newRouter(q, nil, sign(t), epPatchRoutes)
	appID, epID := seedAppWithEndpoint(t, r, uid, oid, "https://original.example.test/hook")

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPatch,
		"/api/applications/"+appID+"/endpoints/"+epID, member, oid,
		map[string]any{"url": "https://attacker.example.test/collect"}))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member repoint: got %d want 403; body=%s", rec.Code, rec.Body.String())
	}

	// The refusal must also mean the write never landed.
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodGet,
		"/api/applications/"+appID+"/endpoints/"+epID, uid, oid, nil))
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["url"] != "https://original.example.test/hook" {
		t.Errorf("endpoint url changed despite the 403: %v", got["url"])
	}
}

// TestMemberCanEditEndpointWithoutMovingURL is why PatchEndpoint reads the
// stored row instead of gating on the field being present: the endpoint edit
// form sends `url` on every save, so a member changing only the description
// resends the same URL and must not be refused.
func TestMemberCanEditEndpointWithoutMovingURL(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	member := seedMember(t, q, oid)
	r := newRouter(q, nil, sign(t), epPatchRoutes)
	appID, epID := seedAppWithEndpoint(t, r, uid, oid, "https://original.example.test/hook")

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPatch,
		"/api/applications/"+appID+"/endpoints/"+epID, member, oid,
		map[string]any{
			"description": "renamed by a member",
			"url":         "https://original.example.test/hook", // resent unchanged
		}))
	if rec.Code != http.StatusOK {
		t.Fatalf("member edit with unchanged url: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
}
