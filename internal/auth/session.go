package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
)

const (
	// SessionCookieName is the cookie carrying the signed HMAC session.
	// Exported so other middleware (e.g. CSRF) can detect session-authed
	// requests without importing the full auth package surface.
	SessionCookieName = "dstream_session"
	sessionMaxAge     = 30 * 24 * time.Hour // 30d
)

var (
	ErrInvalidSession = errors.New("auth: invalid session")
	ErrExpiredSession = errors.New("auth: expired session")
)

// SessionSigner produces and verifies signed cookies containing a user_id,
// active_org_id, and expiration. Single secret, HMAC-SHA256, stateless — no
// sessions table.
//
// Secure must be set to true in production (TLS) so the cookie is never sent
// over plaintext. In development without TLS, leave it false.
type SessionSigner struct {
	Secret []byte
	Secure bool
}

// Issue writes a session cookie carrying the given user id and active org
// id, valid for the default lifetime. activeOrgID may be uuid.Nil for a
// user that has no memberships yet (defensive — bootstrap should have
// created one).
func (s *SessionSigner) Issue(w http.ResponseWriter, userID, activeOrgID uuid.UUID) {
	exp := time.Now().Add(sessionMaxAge).Unix()
	val := s.encode(userID, activeOrgID, exp)
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    val,
		Path:     "/",
		Expires:  time.Unix(exp, 0),
		HttpOnly: true,
		Secure:   s.Secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// Clear invalidates the cookie on the client (no server-side state to clear).
func (s *SessionSigner) Clear(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.Secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// Parse reads the session cookie from the request and returns the carried
// (user_id, active_org_id) if signed correctly and not expired.
func (s *SessionSigner) Parse(r *http.Request) (uuid.UUID, uuid.UUID, error) {
	c, err := r.Cookie(SessionCookieName)
	if err != nil {
		return uuid.Nil, uuid.Nil, ErrInvalidSession
	}
	return s.decode(c.Value)
}

func (s *SessionSigner) encode(userID, orgID uuid.UUID, exp int64) string {
	var payload [16 + 16 + 8]byte
	copy(payload[:16], userID[:])
	copy(payload[16:32], orgID[:])
	binary.BigEndian.PutUint64(payload[32:], uint64(exp))

	mac := hmac.New(sha256.New, s.Secret)
	mac.Write(payload[:])
	sig := mac.Sum(nil)

	out := make([]byte, len(payload)+len(sig))
	copy(out, payload[:])
	copy(out[len(payload):], sig)
	return base64.RawURLEncoding.EncodeToString(out)
}

func (s *SessionSigner) decode(raw string) (uuid.UUID, uuid.UUID, error) {
	buf, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return uuid.Nil, uuid.Nil, ErrInvalidSession
	}
	if len(buf) != 16+16+8+sha256.Size {
		return uuid.Nil, uuid.Nil, ErrInvalidSession
	}
	payload := buf[:40]
	sig := buf[40:]
	mac := hmac.New(sha256.New, s.Secret)
	mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return uuid.Nil, uuid.Nil, ErrInvalidSession
	}
	exp := int64(binary.BigEndian.Uint64(payload[32:]))
	if time.Now().Unix() > exp {
		return uuid.Nil, uuid.Nil, ErrExpiredSession
	}
	var u, o uuid.UUID
	copy(u[:], payload[:16])
	copy(o[:], payload[16:32])
	return u, o, nil
}
