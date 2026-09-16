package outbound

import (
	"encoding/json"
	"math"
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

// TestRateLimitBoundPreventsOverflow documents why rate_limit is capped at
// maxRateLimit: int32PtrFromInt narrows with int32(), so an unbounded value
// above int32 max wraps NEGATIVE and the send gate (*x > 0) silently reads
// false → the endpoint becomes unlimited. The bound keeps every accepted value
// within the safe positive range. No DB needed.
func TestRateLimitBoundPreventsOverflow(t *testing.T) {
	if maxRateLimit >= math.MaxInt32 {
		t.Fatalf("maxRateLimit %d must stay below int32 max to avoid wrap", maxRateLimit)
	}
	max := maxRateLimit
	if p := int32PtrFromInt(&max); p == nil || *p != int32(maxRateLimit) || *p <= 0 {
		t.Fatalf("in-bound value must narrow to a positive int32, got %v", p)
	}
	// The wrap the bound prevents: a value past int32 max narrows negative.
	over := math.MaxInt32 + 1
	if p := int32PtrFromInt(&over); p == nil || *p > 0 {
		t.Fatalf("value above int32 max must wrap non-positive (this is what the cap blocks), got %v", p)
	}
}
