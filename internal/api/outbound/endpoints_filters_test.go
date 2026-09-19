package outbound

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Vivekagent47/dstream/internal/store"
)

func epFilterRoutes(r chi.Router, h Handlers) {
	r.Route("/applications", func(r chi.Router) {
		r.Post("/", h.CreateApplication)
		r.Route("/{app_id}/endpoints", func(r chi.Router) {
			r.Post("/", h.CreateEndpoint)
			r.Get("/{id}", h.GetEndpoint)
			r.Patch("/{id}", h.PatchEndpoint)
		})
	})
}

func mkAppForFilterTest(t *testing.T, r *chi.Mux, uid, oid uuid.UUID) string {
	t.Helper()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPost, "/api/applications", uid, oid, map[string]any{"name": "A"}))
	var app map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &app)
	return app["id"].(string)
}

func TestEndpointBadFilterExpr(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	r := newRouter(q, nil, sign(t), epFilterRoutes)
	appID := mkAppForFilterTest(t, r, uid, oid)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPost, "/api/applications/"+appID+"/endpoints", uid, oid, map[string]any{
		"url": "https://example.test/hook", "filter_expr": "payload.x ==",
	}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create bad filter: got %d body=%s", rec.Code, rec.Body.String())
	}

	// patch validated before DB update, so a random id still 400s on bad CEL
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPatch, "/api/applications/"+appID+"/endpoints/"+uuid.NewString(), uid, oid, map[string]any{
		"filter_expr": "payload.x ==",
	}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("patch bad filter: got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestEndpointBadTransformJS(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	r := newRouter(q, nil, sign(t), epFilterRoutes)
	appID := mkAppForFilterTest(t, r, uid, oid)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPost, "/api/applications/"+appID+"/endpoints", uid, oid, map[string]any{
		"url": "https://example.test/hook", "transform_js": "function(",
	}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create bad js: got %d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPatch, "/api/applications/"+appID+"/endpoints/"+uuid.NewString(), uid, oid, map[string]any{
		"transform_js": "function(",
	}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("patch bad js: got %d body=%s", rec.Code, rec.Body.String())
	}
}

// Empty string clears a stored filter to SQL NULL (disabled).
func TestEndpointEmptyFilterDisables(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	r := newRouter(q, nil, sign(t), epFilterRoutes)
	appID := mkAppForFilterTest(t, r, uid, oid)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPost, "/api/applications/"+appID+"/endpoints", uid, oid, map[string]any{
		"url": "https://example.test/hook", "filter_expr": `event_type == "x"`,
	}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d body=%s", rec.Code, rec.Body.String())
	}
	var ep map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &ep)
	epID := ep["id"].(string)
	if ep["filter_expr"] == nil {
		t.Fatalf("filter_expr should be set on create, got nil")
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPatch, "/api/applications/"+appID+"/endpoints/"+epID, uid, oid, map[string]any{
		"filter_expr": "",
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("patch clear: got %d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodGet, "/api/applications/"+appID+"/endpoints/"+epID, uid, oid, nil))
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["filter_expr"] != nil {
		t.Fatalf("filter_expr should be null after clear, got %v", got["filter_expr"])
	}
}
