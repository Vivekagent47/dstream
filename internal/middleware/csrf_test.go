package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Vivekagent47/dstream/internal/auth"
)

func csrfChain() http.Handler {
	return CSRF(false)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
}

func TestCSRF_SetsCookieOnFirstGet(t *testing.T) {
	rec := httptest.NewRecorder()
	csrfChain().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	cookies := rec.Result().Cookies()
	found := false
	for _, c := range cookies {
		if c.Name == CSRFCookieName && c.Value != "" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected CSRF cookie to be set")
	}
}

func TestCSRF_SafeMethodsBypass(t *testing.T) {
	for _, m := range []string{"GET", "HEAD", "OPTIONS"} {
		req := httptest.NewRequest(m, "/", nil)
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "fake"})
		// No CSRF header — should still pass on safe methods.
		rec := httptest.NewRecorder()
		csrfChain().ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s: got %d", m, rec.Code)
		}
	}
}

func TestCSRF_BearerAuthBypass(t *testing.T) {
	req := httptest.NewRequest("POST", "/", strings.NewReader("{}"))
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "fake"})
	req.Header.Set("Authorization", "Bearer dsk_anything")
	rec := httptest.NewRecorder()
	csrfChain().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("bearer bypass failed: %d", rec.Code)
	}
}

func TestCSRF_NoSessionBypass(t *testing.T) {
	// POST without session cookie → CSRF doesn't apply (no session to forge).
	req := httptest.NewRequest("POST", "/", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	csrfChain().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("no-session POST should pass: %d", rec.Code)
	}
}

func TestCSRF_SessionPostMissingHeader_Forbidden(t *testing.T) {
	req := httptest.NewRequest("POST", "/", strings.NewReader("{}"))
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "session"})
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: "csrf-token-here"})
	// No X-CSRF-Token header.
	rec := httptest.NewRecorder()
	csrfChain().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

func TestCSRF_SessionPostMatchingHeader_OK(t *testing.T) {
	req := httptest.NewRequest("POST", "/", strings.NewReader("{}"))
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "session"})
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: "csrf-token-here"})
	req.Header.Set(CSRFHeaderName, "csrf-token-here")
	rec := httptest.NewRecorder()
	csrfChain().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestCSRF_SessionPostMismatchedHeader_Forbidden(t *testing.T) {
	req := httptest.NewRequest("POST", "/", strings.NewReader("{}"))
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "session"})
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: "csrf-token-here"})
	req.Header.Set(CSRFHeaderName, "different-token")
	rec := httptest.NewRecorder()
	csrfChain().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}
