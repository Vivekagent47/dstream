package outbound

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/store"
)

func TestCreateEndpointRejectsSelfURL(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	h := Handlers{Log: discardLog(), Queries: q, SelfHosts: []string{"self.example.test"}}
	r := chi.NewRouter()
	r.Route("/api", func(r chi.Router) {
		r.Use(auth.Authenticate(q, sign(t)))
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireOrg(q))
			r.Route("/applications", func(r chi.Router) {
				r.Post("/", h.CreateApplication)
				r.Route("/{app_id}/endpoints", func(r chi.Router) { r.Post("/", h.CreateEndpoint) })
			})
		})
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPost, "/api/applications", uid, oid, map[string]any{"name": "A"}))
	var app map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &app)

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPost, "/api/applications/"+app["id"].(string)+"/endpoints", uid, oid,
		map[string]any{"url": "https://self.example.test/hook"}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("self-url must be rejected: got %d body=%s", rec.Code, rec.Body.String())
	}
}
