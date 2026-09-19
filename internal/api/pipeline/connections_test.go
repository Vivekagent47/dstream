package pipeline

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Vivekagent47/dstream/internal/store"
)

func connRoutes(r chi.Router, h Handlers) {
	r.Route("/connections", func(r chi.Router) {
		r.Post("/", h.CreateConnection)
		r.Patch("/{id}", h.PatchConnection)
	})
}

// Bad CEL / JS is rejected 400 before any DB work, so these need only an org
// session — no real source/destination/connection rows.
func TestConnectionBadFilterExpr(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	r := newRouter(q, connRoutes)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPost, "/api/connections", uid, oid, map[string]any{
		"source_id": uuid.NewString(), "destination_id": uuid.NewString(),
		"filter_expr": "payload.x ==",
	}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create bad filter: got %d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPatch, "/api/connections/"+uuid.NewString(), uid, oid, map[string]any{
		"filter_expr": "payload.x ==",
	}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("patch bad filter: got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestConnectionBadTransformJS(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	r := newRouter(q, connRoutes)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPost, "/api/connections", uid, oid, map[string]any{
		"source_id": uuid.NewString(), "destination_id": uuid.NewString(),
		"transform_js": "function(",
	}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create bad js: got %d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, http.MethodPatch, "/api/connections/"+uuid.NewString(), uid, oid, map[string]any{
		"transform_js": "function(",
	}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("patch bad js: got %d body=%s", rec.Code, rec.Body.String())
	}
}
