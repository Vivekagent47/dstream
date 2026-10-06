package metrics

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestHTTPMiddlewareRecordsRoutePattern(t *testing.T) {
	r := chi.NewRouter()
	r.Use(HTTPMiddleware)
	r.Get("/api/events/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(r)
	defer srv.Close()

	// The counters are process-global, so assert the delta (survives -count=N).
	before := testutil.ToFloat64(httpRequests.WithLabelValues("/api/events/{id}", "GET", "200"))
	resp, err := http.Get(srv.URL + "/api/events/abc123")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// Label is the ROUTE PATTERN, not the raw path (no "abc123").
	got := testutil.ToFloat64(httpRequests.WithLabelValues("/api/events/{id}", "GET", "200")) - before
	if got != 1 {
		t.Errorf("got %v want 1 for pattern label", got)
	}
}

// A handler that never writes still answers 200 on the wire; the metric must
// say 200, not 0.
func TestHTTPMiddlewareImplicit200(t *testing.T) {
	r := chi.NewRouter()
	r.Use(HTTPMiddleware)
	r.Get("/implicit200/{id}", func(http.ResponseWriter, *http.Request) {})

	srv := httptest.NewServer(r)
	defer srv.Close()
	before := testutil.ToFloat64(httpRequests.WithLabelValues("/implicit200/{id}", "GET", "200"))
	resp, err := http.Get(srv.URL + "/implicit200/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("wire status = %d, want 200", resp.StatusCode)
	}
	if got := testutil.ToFloat64(httpRequests.WithLabelValues("/implicit200/{id}", "GET", "200")) - before; got != 1 {
		t.Errorf("200 series = %v, want 1", got)
	}
	if got := testutil.ToFloat64(httpRequests.WithLabelValues("/implicit200/{id}", "GET", "0")); got != 0 {
		t.Errorf("status 0 series = %v, want none", got)
	}
}
