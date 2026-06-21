package middleware

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"github.com/Vivekagent47/dstream/internal/auth"
)

const (
	// CSRFCookieName carries a random token readable by the SPA. The SPA
	// echoes it back in CSRFHeaderName on state-changing requests; the
	// double-submit comparison defeats CSRF because an attacker on a
	// different origin can't read the cookie to forge the header.
	CSRFCookieName = "dstream_csrf"
	CSRFHeaderName = "X-CSRF-Token"

	csrfCookieMaxAge = 30 * 24 * time.Hour
	csrfTokenBytes   = 32
)

// CSRF returns middleware that:
//
//  1. Sets a `dstream_csrf` cookie on every request that lacks one (so the
//     SPA can read it once and forward in headers).
//  2. On state-changing methods (POST/PUT/PATCH/DELETE) from a session-authed
//     browser, requires header `X-CSRF-Token` to match the cookie.
//
// Exemptions:
//
//   - Safe methods (GET/HEAD/OPTIONS).
//   - Requests carrying `Authorization: Bearer ...` (machine clients using
//     API keys are immune by construction — attackers can't set headers
//     cross-origin).
//   - Requests without a session cookie (anonymous or pre-login).
//
// `secure` should be true in production so the cookie is TLS-only.
func CSRF(secure bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 1. Ensure cookie exists. Mint a fresh token if missing or empty.
			existing, _ := r.Cookie(CSRFCookieName)
			if existing == nil || existing.Value == "" {
				tok, err := newCSRFToken()
				if err != nil {
					http.Error(w, "csrf init", http.StatusInternalServerError)
					return
				}
				http.SetCookie(w, &http.Cookie{
					Name:     CSRFCookieName,
					Value:    tok,
					Path:     "/",
					MaxAge:   int(csrfCookieMaxAge / time.Second),
					HttpOnly: false, // intentional — JS must read it
					Secure:   secure,
					SameSite: http.SameSiteLaxMode,
				})
				// Make the new token visible to downstream code on this same
				// request, so SetCookie + immediate Cookie() read still works
				// for handlers that mint the session on this request.
				r.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: tok})
				existing = &http.Cookie{Value: tok}
			}

			// 2. Skip checks on safe methods.
			switch r.Method {
			case http.MethodGet, http.MethodHead, http.MethodOptions:
				next.ServeHTTP(w, r)
				return
			}

			// 3. API-key auth bypasses CSRF (machine clients).
			if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
				next.ServeHTTP(w, r)
				return
			}

			// 4. No session cookie means no session to forge — let the
			//    downstream auth middleware decide (401 vs allow).
			if _, err := r.Cookie(auth.SessionCookieName); err != nil {
				next.ServeHTTP(w, r)
				return
			}

			// 5. Double-submit check.
			header := r.Header.Get(CSRFHeaderName)
			if header == "" ||
				subtle.ConstantTimeCompare([]byte(existing.Value), []byte(header)) != 1 {
				http.Error(w, "csrf token mismatch", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func newCSRFToken() (string, error) {
	b := make([]byte, csrfTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
