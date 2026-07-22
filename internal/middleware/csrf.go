package middleware

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"github.com/Vivekagent47/dstream/internal/auth"
)

const (
	// CSRFCookieName carries a token readable by the SPA, which echoes it back
	// in CSRFHeaderName on state-changing requests. When a session is present
	// the token is an HMAC bound to the session cookie's value (see below), so a
	// double-submit match alone is not enough — the token must have been minted
	// for THIS session.
	CSRFCookieName = "dstream_csrf"
	CSRFHeaderName = "X-CSRF-Token"

	csrfCookieMaxAge = 30 * 24 * time.Hour
	csrfNonceBytes   = 16
)

// CSRF returns middleware enforcing a session-bound double-submit token.
//
// Plain double-submit (cookie value == header value) is defeated only by the
// attacker's inability to READ the cookie cross-origin. It still falls to an
// attacker who can WRITE a `dstream_csrf` cookie into the victim's browser (a
// compromised sibling subdomain setting a domain-scoped cookie, or any cookie
// injection): they then know the value and can forge the matching header. To
// close that, the token is bound to the session:
//
//	token = <nonce> "." base64(HMAC-SHA256(secret, sessionValue "." nonce))
//
// Validation recomputes the HMAC over the CURRENT session cookie value. An
// attacker who plants a csrf cookie cannot compute a token bound to the
// victim's session (the session cookie is HttpOnly + unreadable cross-site), so
// a planted value fails validation. `secret` is the session signer secret.
//
// Behaviour:
//  1. When a session is present, ensure the csrf cookie holds a token bound to
//     it (mint/refresh on mismatch — e.g. first request after login, or after a
//     re-login changed the session). This runs on every request incl. GET, so
//     the SPA's initial GETs refresh the cookie before it issues any mutation.
//  2. State-changing methods from a session-authed browser require a header
//     token that validates against the session.
//
// Exemptions: safe methods; `Authorization: Bearer` (API-key clients, immune by
// construction); requests with no session cookie (nothing to forge — downstream
// auth decides). `secure` gates the cookie's Secure attribute.
func CSRF(secure bool, secret []byte) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			session := sessionCookieValue(r)

			existing, _ := r.Cookie(CSRFCookieName)
			have := ""
			if existing != nil {
				have = existing.Value
			}

			// 1. Keep the SPA-visible token in sync with the session.
			switch {
			case session != "":
				// Bind (or rebind) to the current session if the cookie doesn't
				// already carry a valid bound token.
				if !validCSRFToken(have, session, secret) {
					tok, err := newBoundCSRFToken(session, secret)
					if err != nil {
						http.Error(w, "csrf init", http.StatusInternalServerError)
						return
					}
					setCSRFCookie(w, r, tok, secure)
					have = tok
				}
			case have == "":
				// No session yet (pre-login): a plain random token, so the SPA
				// has something to echo. Validation is skipped without a session.
				tok, err := newRandomToken()
				if err != nil {
					http.Error(w, "csrf init", http.StatusInternalServerError)
					return
				}
				setCSRFCookie(w, r, tok, secure)
				have = tok
			}

			// 2. Safe methods never mutate.
			switch r.Method {
			case http.MethodGet, http.MethodHead, http.MethodOptions:
				next.ServeHTTP(w, r)
				return
			}

			// 3. API-key clients can't be CSRF'd (attackers can't set the
			//    Authorization header cross-origin).
			if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
				next.ServeHTTP(w, r)
				return
			}

			// 4. No session → nothing to forge; let downstream auth decide.
			if session == "" {
				next.ServeHTTP(w, r)
				return
			}

			// 5. The header token must be bound to this session.
			if !validCSRFToken(r.Header.Get(CSRFHeaderName), session, secret) {
				http.Error(w, "csrf token mismatch", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// sessionCookieValue returns the raw session cookie value (the signed token
// string), or "" if absent. We bind to the value without verifying it here —
// the auth middleware verifies the session; an attacker who already knows a
// valid session value has won regardless, and binding to a bogus value is
// harmless (they still can't produce a matching token for the victim's session).
func sessionCookieValue(r *http.Request) string {
	c, err := r.Cookie(auth.SessionCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

func setCSRFCookie(w http.ResponseWriter, r *http.Request, tok string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookieName,
		Value:    tok,
		Path:     "/",
		MaxAge:   int(csrfCookieMaxAge / time.Second),
		HttpOnly: false, // intentional — JS must read it to echo in the header
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
	// Make the new token visible to downstream code on this same request.
	r.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: tok})
}

// newBoundCSRFToken mints "<nonce>.<hmac>" binding a fresh nonce to the session.
func newBoundCSRFToken(session string, secret []byte) (string, error) {
	nonce, err := newRandomToken()
	if err != nil {
		return "", err
	}
	return nonce + "." + csrfMAC(secret, session, nonce), nil
}

// validCSRFToken reports whether tok is a "<nonce>.<hmac>" bound to session.
func validCSRFToken(tok, session string, secret []byte) bool {
	if tok == "" {
		return false
	}
	nonce, mac, found := strings.Cut(tok, ".")
	if !found || nonce == "" || mac == "" {
		return false
	}
	expected := csrfMAC(secret, session, nonce)
	return subtle.ConstantTimeCompare([]byte(expected), []byte(mac)) == 1
}

func csrfMAC(secret []byte, session, nonce string) string {
	h := hmac.New(sha256.New, secret)
	h.Write([]byte(session))
	h.Write([]byte{'.'})
	h.Write([]byte(nonce))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

func newRandomToken() (string, error) {
	b := make([]byte, csrfNonceBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
