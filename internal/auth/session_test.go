package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

func newSigner(t *testing.T) *SessionSigner {
	t.Helper()
	return &SessionSigner{Secret: []byte("test-secret-do-not-use-in-prod")}
}

// readSetCookie pulls the session cookie from a recorder response and
// reconstructs a *http.Request carrying it, ready to feed back to Parse.
func readSetCookie(t *testing.T, w *httptest.ResponseRecorder) *http.Request {
	t.Helper()
	res := w.Result()
	defer res.Body.Close()
	var sc *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == SessionCookieName {
			sc = c
			break
		}
	}
	if sc == nil {
		t.Fatal("session cookie not set on response")
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(sc)
	return r
}

func TestSession_Roundtrip(t *testing.T) {
	s := newSigner(t)
	uid := uuid.New()
	oid := uuid.New()

	w := httptest.NewRecorder()
	s.Issue(w, uid, oid, 0)

	r := readSetCookie(t, w)
	gotU, gotO, _, err := s.Parse(r)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if gotU != uid {
		t.Errorf("user_id mismatch: got %s want %s", gotU, uid)
	}
	if gotO != oid {
		t.Errorf("org_id mismatch: got %s want %s", gotO, oid)
	}
}

func TestSession_Roundtrip_NilOrg(t *testing.T) {
	s := newSigner(t)
	uid := uuid.New()

	w := httptest.NewRecorder()
	s.Issue(w, uid, uuid.Nil, 0)

	r := readSetCookie(t, w)
	gotU, gotO, _, err := s.Parse(r)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if gotU != uid {
		t.Errorf("user_id mismatch: got %s want %s", gotU, uid)
	}
	if gotO != uuid.Nil {
		t.Errorf("org_id mismatch: got %s want uuid.Nil", gotO)
	}
}

func TestSession_EpochRoundtrip(t *testing.T) {
	s := newSigner(t)
	uid := uuid.New()
	oid := uuid.New()

	w := httptest.NewRecorder()
	s.Issue(w, uid, oid, 42)

	r := readSetCookie(t, w)
	gotU, gotO, gotEpoch, err := s.Parse(r)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if gotU != uid || gotO != oid {
		t.Errorf("id mismatch: got (%s,%s) want (%s,%s)", gotU, gotO, uid, oid)
	}
	if gotEpoch != 42 {
		t.Errorf("epoch mismatch: got %d want 42", gotEpoch)
	}
}

func TestSession_TamperFailsHMAC(t *testing.T) {
	s := newSigner(t)
	w := httptest.NewRecorder()
	s.Issue(w, uuid.New(), uuid.New(), 0)

	res := w.Result()
	defer res.Body.Close()
	var raw string
	for _, c := range res.Cookies() {
		if c.Name == SessionCookieName {
			raw = c.Value
			break
		}
	}
	if raw == "" {
		t.Fatal("cookie not set")
	}

	// Flip a byte in the user_id portion of the payload; HMAC must reject.
	buf, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	buf[0] ^= 0xff
	tampered := base64.RawURLEncoding.EncodeToString(buf)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: tampered})

	if _, _, _, err := s.Parse(r); err != ErrInvalidSession {
		t.Fatalf("Parse(tampered) = %v, want ErrInvalidSession", err)
	}
}

func TestSession_Expired(t *testing.T) {
	s := newSigner(t)
	uid := uuid.New()
	oid := uuid.New()

	// Forge a cookie with an exp in the past, signed correctly.
	val := encodeWithExp(s, uid, oid, time.Now().Add(-time.Hour).Unix())
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: val})

	if _, _, _, err := s.Parse(r); err != ErrExpiredSession {
		t.Fatalf("Parse(expired) = %v, want ErrExpiredSession", err)
	}
}

func TestSession_TruncatedFailsLength(t *testing.T) {
	s := newSigner(t)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "abc"})
	if _, _, _, err := s.Parse(r); err != ErrInvalidSession {
		t.Fatalf("Parse(short) = %v, want ErrInvalidSession", err)
	}
}

// encodeWithExp duplicates SessionSigner.encode with a caller-chosen exp so
// the expired-cookie test doesn't depend on time mocking. Epoch fixed at 0.
func encodeWithExp(s *SessionSigner, userID, orgID uuid.UUID, exp int64) string {
	var payload [sessionPayloadLen]byte
	copy(payload[:16], userID[:])
	copy(payload[16:32], orgID[:])
	binary.BigEndian.PutUint64(payload[32:40], uint64(exp))
	binary.BigEndian.PutUint64(payload[40:48], 0)

	mac := hmac.New(sha256.New, s.Secret)
	mac.Write(payload[:])
	sig := mac.Sum(nil)

	out := make([]byte, len(payload)+len(sig))
	copy(out, payload[:])
	copy(out[len(payload):], sig)
	return base64.RawURLEncoding.EncodeToString(out)
}
