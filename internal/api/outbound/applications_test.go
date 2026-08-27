package outbound

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

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
