package outbound

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/Vivekagent47/dstream/internal/store"
)

func epRoutes(r chi.Router, h Handlers) {
	r.Route("/applications", func(r chi.Router) {
		r.Post("/", h.CreateApplication)
		r.Route("/{app_id}/endpoints", func(r chi.Router) {
			r.Post("/", h.CreateEndpoint)
			r.Get("/{id}", h.GetEndpoint)
			r.Get("/{id}/secret", h.GetEndpointSecret)
		})
	})
}

func TestCreateEndpointReturnsSecretOnce(t *testing.T) {
	q := store.New(testPool(t))
	uid, oid := seedOrg(t, q)
	r := newRouter(q, nil, sign(t), epRoutes)

	// create app
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPost, "/api/applications", uid, oid, map[string]any{"name": "A"}))
	var app map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &app)
	appID := app["id"].(string)

	// create endpoint
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodPost, "/api/applications/"+appID+"/endpoints", uid, oid,
		map[string]any{"url": "https://example.test/hook", "filter_event_types": []string{"invoice.paid"}}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create ep: got %d body=%s", rec.Code, rec.Body.String())
	}
	var ep map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &ep)
	if s, _ := ep["secret"].(string); len(s) < 6 || s[:6] != "whsec_" {
		t.Fatalf("create must return secret, got %v", ep["secret"])
	}
	epID := ep["id"].(string)

	// GET must NOT include the secret
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodGet, "/api/applications/"+appID+"/endpoints/"+epID, uid, oid, nil))
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if _, present := got["secret"]; present {
		t.Fatalf("GET endpoint must omit secret")
	}

	// /secret reveals it
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, sessionReq(t, sign(t), http.MethodGet, "/api/applications/"+appID+"/endpoints/"+epID+"/secret", uid, oid, nil))
	var sec map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &sec)
	if s, _ := sec["secret"].(string); s[:6] != "whsec_" {
		t.Fatalf("/secret must reveal, got %v", sec)
	}
}
