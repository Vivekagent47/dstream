package auth

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func newPortalSigner(t *testing.T) *PortalSigner {
	t.Helper()
	return &PortalSigner{Secret: []byte("test-secret-do-not-use-in-prod!!"), TTL: time.Hour}
}

func TestPortalRoundTrip(t *testing.T) {
	s := newPortalSigner(t)
	app, org := uuid.New(), uuid.New()
	tok, exp := s.Mint(app, org, 3)
	if !exp.After(time.Now()) {
		t.Fatal("expiry not in the future")
	}
	gotApp, gotOrg, gotEpoch, err := s.Parse(tok)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if gotApp != app || gotOrg != org || gotEpoch != 3 {
		t.Fatalf("round-trip mismatch: %v %v %d", gotApp, gotOrg, gotEpoch)
	}
}

func TestPortalRejectsTamper(t *testing.T) {
	s := newPortalSigner(t)
	tok, _ := s.Mint(uuid.New(), uuid.New(), 0)
	b := []byte(tok)
	b[len(b)-1] ^= 0x01 // flip a signature bit
	if _, _, _, err := s.Parse(string(b)); err == nil {
		t.Fatal("expected tampered token to fail")
	}
}

func TestPortalRejectsExpired(t *testing.T) {
	s := &PortalSigner{Secret: []byte("test-secret-do-not-use-in-prod!!"), TTL: -time.Second}
	tok, _ := s.Mint(uuid.New(), uuid.New(), 0)
	if _, _, _, err := s.Parse(tok); err != ErrExpiredPortalToken {
		t.Fatalf("expected ErrExpiredPortalToken, got %v", err)
	}
}

func TestPortalRejectsSessionToken(t *testing.T) {
	// A SessionSigner value signed with the same secret must not verify as a
	// portal token (domain separation via the tag byte + length).
	secret := []byte("test-secret-do-not-use-in-prod!!")
	sess := &SessionSigner{Secret: secret}
	ps := &PortalSigner{Secret: secret, TTL: time.Hour}
	// Issue a session cookie value, then feed it to the portal parser.
	// Build the session value the same way Issue does, via encode:
	val := sess.encode(uuid.New(), uuid.New(), 0, time.Now().Add(time.Hour).Unix())
	if _, _, _, err := ps.Parse(val); err == nil {
		t.Fatal("session token must not verify as portal token")
	}
}
