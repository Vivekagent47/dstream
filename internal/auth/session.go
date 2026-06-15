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
	sessionCookieName = "dstream_session"
	sessionMaxAge     = 30 * 24 * time.Hour // 30d
)

var (
	ErrInvalidSession = errors.New("auth: invalid session")
	ErrExpiredSession = errors.New("auth: expired session")
)

// SessionSigner produces and verifies signed cookies containing a user_id +
// expiration. Single secret, HMAC-SHA256, stateless — no sessions table.
type SessionSigner struct {
	Secret []byte
}

// Issue writes a session cookie carrying the given user id valid for the
// default lifetime.
func (s *SessionSigner) Issue(w http.ResponseWriter, userID uuid.UUID) {
	exp := time.Now().Add(sessionMaxAge).Unix()
	val := s.encode(userID, exp)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    val,
		Path:     "/",
		Expires:  time.Unix(exp, 0),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		// Secure is set by the caller based on TLS.
	})
}

// Clear invalidates the cookie on the client (no server-side state to clear).
func (s *SessionSigner) Clear(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
	})
}

// Parse reads the session cookie from the request and returns the carried
// user id if signed correctly and not expired.
func (s *SessionSigner) Parse(r *http.Request) (uuid.UUID, error) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return uuid.Nil, ErrInvalidSession
	}
	return s.decode(c.Value)
}

func (s *SessionSigner) encode(userID uuid.UUID, exp int64) string {
	var payload [16 + 8]byte
	copy(payload[:16], userID[:])
	binary.BigEndian.PutUint64(payload[16:], uint64(exp))

	mac := hmac.New(sha256.New, s.Secret)
	mac.Write(payload[:])
	sig := mac.Sum(nil)

	out := make([]byte, len(payload)+len(sig))
	copy(out, payload[:])
	copy(out[len(payload):], sig)
	return base64.RawURLEncoding.EncodeToString(out)
}

func (s *SessionSigner) decode(raw string) (uuid.UUID, error) {
	buf, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return uuid.Nil, ErrInvalidSession
	}
	if len(buf) != 16+8+sha256.Size {
		return uuid.Nil, ErrInvalidSession
	}
	payload := buf[:24]
	sig := buf[24:]
	mac := hmac.New(sha256.New, s.Secret)
	mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return uuid.Nil, ErrInvalidSession
	}
	exp := int64(binary.BigEndian.Uint64(payload[16:]))
	if time.Now().Unix() > exp {
		return uuid.Nil, ErrExpiredSession
	}
	var id uuid.UUID
	copy(id[:], payload[:16])
	return id, nil
}
