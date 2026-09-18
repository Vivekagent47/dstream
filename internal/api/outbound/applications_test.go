package outbound

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/Vivekagent47/dstream/internal/opevents"
	"github.com/Vivekagent47/dstream/internal/store"
)

func appRoutes(r chi.Router, h Handlers) {
	r.Route("/applications", func(r chi.Router) {
		r.Get("/", h.ListApplications)
		r.Post("/", h.CreateApplication)
		r.Get("/{app_id}", h.GetApplication)
		r.Patch("/{app_id}", h.PatchApplication)
		r.Delete("/{app_id}", h.DeleteApplication)
	})
}

func TestCreateApplication(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedOrg(t, q)
	r := newRouter(q, nil, sign(t), appRoutes)

	req := sessionReq(t, sign(t), http.MethodPost, "/api/applications", uid, oid,
		map[string]any{"name": "Acme", "uid": "cust_1"})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["name"] != "Acme" || got["uid"] != "cust_1" {
		t.Fatalf("unexpected body: %v", got)
	}
}

// A duplicate uid within an org must be a 409, not a 500.
func TestCreateApplicationDuplicateUID(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	r := newRouter(q, nil, sign(t), appRoutes)
	body := map[string]any{"name": "Acme", "uid": "dup_1"}

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPost, "/api/applications", uid, oid, body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d body=%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPost, "/api/applications", uid, oid, body))
	if rec.Code != http.StatusConflict {
		t.Fatalf("dup uid: got %d want 409 body=%s", rec.Code, rec.Body.String())
	}
}

// The per-org operational app must not be mutable/deletable via the generic
// /applications/{id} routes (it is managed only through /api/operational-app).
func TestOperationalAppNotModifiableViaGenericRoutes(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	r := newRouter(q, nil, sign(t), appRoutes)

	opID, err := opevents.SeedOperationalApp(context.Background(), q, oid)
	if err != nil {
		t.Fatalf("seed op app: %v", err)
	}
	base := "/api/applications/" + opID.String()

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPatch, base, uid, oid,
		map[string]any{"name": "hijacked"}))
	if rec.Code != http.StatusNotFound {
		t.Errorf("patch op app: got %d want 404", rec.Code)
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodDelete, base, uid, oid, nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("delete op app: got %d want 404", rec.Code)
	}
}
