package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Vivekagent47/dstream/internal/auth"
)

var csrfTestSecret = []byte("test-csrf-secret-at-least-32-bytes-long")

func csrfChain() http.Handler {
	return CSRF(false, csrfTestSecret)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
	found := false
	for _, c := range rec.Result().Cookies() {
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
	rec := httptest.NewRecorder()
	csrfChain().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

func TestCSRF_SessionPostBoundToken_OK(t *testing.T) {
	tok, err := newBoundCSRFToken("session", csrfTestSecret)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	req := httptest.NewRequest("POST", "/", strings.NewReader("{}"))
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "session"})
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: tok})
	req.Header.Set(CSRFHeaderName, tok)
	rec := httptest.NewRecorder()
	csrfChain().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

// TestCSRF_TokenBoundToOtherSession_Forbidden is the N7 protection: a token
// that is a valid HMAC but bound to a DIFFERENT session (e.g. one an attacker
// planted via a cookie they control) must not validate against the victim's
// session. Plain double-submit would have accepted this (cookie == header).
func TestCSRF_TokenBoundToOtherSession_Forbidden(t *testing.T) {
	other, err := newBoundCSRFToken("attacker-session", csrfTestSecret)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	req := httptest.NewRequest("POST", "/", strings.NewReader("{}"))
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "victim-session"})
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: other})
	req.Header.Set(CSRFHeaderName, other) // cookie == header, but bound to a different session
	rec := httptest.NewRecorder()
	csrfChain().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 (token bound to other session), got %d", rec.Code)
	}
}

func TestCSRF_GarbageHeader_Forbidden(t *testing.T) {
	req := httptest.NewRequest("POST", "/", strings.NewReader("{}"))
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "session"})
	req.Header.Set(CSRFHeaderName, "not-a-valid-token")
	rec := httptest.NewRecorder()
	csrfChain().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}
