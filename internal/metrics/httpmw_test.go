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

	resp, err := http.Get(srv.URL + "/api/events/abc123")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// Label is the ROUTE PATTERN, not the raw path (no "abc123").
	got := testutil.ToFloat64(httpRequests.WithLabelValues("/api/events/{id}", "GET", "200"))
	if got != 1 {
		t.Errorf("got %v want 1 for pattern label", got)
	}
}
