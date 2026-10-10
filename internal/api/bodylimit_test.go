package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestMaxBodyBytes guards the OOM fix: the middleware must cap a request body so
// a handler can't buffer an unbounded amount (e.g. a multi-GB POST to an
// unauthenticated JSON route). Under the cap reads clean; over it, the read fails.
func TestMaxBodyBytes(t *testing.T) {
	const limit = 1 << 10 // 1 KiB
	var readErr error
	h := maxBodyBytes(limit)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, readErr = io.ReadAll(r.Body)
	}))

	readErr = nil
	under := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("a", limit-1)))
	h.ServeHTTP(httptest.NewRecorder(), under)
	if readErr != nil {
		t.Fatalf("under-limit body errored: %v", readErr)
	}

	readErr = nil
	over := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("a", limit+1)))
	h.ServeHTTP(httptest.NewRecorder(), over)
	if readErr == nil {
		t.Fatal("over-limit body was not rejected by MaxBytesReader")
	}
}
