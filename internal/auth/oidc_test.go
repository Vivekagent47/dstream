package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// --- scope handling ---

// Without the `openid` scope an IdP runs a plain OAuth2 authorization and
// returns no id_token, which surfaces as an opaque failure at a user's first
// login. Config validation accepts any scope list, so the guarantee lives at
// the point of use.
func TestWithOpenIDScope(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []string
		want []string
	}{
		{"empty gets the full default set", nil, []string{"openid", "email", "profile"}},
		{"openid prepended when missing", []string{"email", "profile"}, []string{"openid", "email", "profile"}},
		{"not duplicated when already listed", []string{"openid", "groups"}, []string{"openid", "groups"}},
		{"preserved mid-list", []string{"email", "openid"}, []string{"email", "openid"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := withOpenIDScope(tc.in)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

// --- a fake IdP, served over httptest (no network) ---

// fakeIdP serves discovery, JWKS and the token endpoint so the real
// OIDCAuthenticator can be exercised end to end against a locally signed ID
// token. It exists because "email_verified absent" is only representable at
// the claim-decoding boundary: by the time the handler sees an Identity, an
// absent claim has already collapsed to false.
type fakeIdP struct {
	srv    *httptest.Server
	key    *rsa.PrivateKey
	claims map[string]any // claims to mint into the next id_token
	// rogueKey, when set, signs the next id_token instead of key. It is never
	// published in the JWKS, which is how the signature check is tested.
	rogueKey *rsa.PrivateKey
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	f := &fakeIdP{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                                f.srv.URL,
			"authorization_endpoint":                f.srv.URL + "/authorize",
			"token_endpoint":                        f.srv.URL + "/token",
			"jwks_uri":                              f.srv.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"keys": []any{map[string]any{
			"kty": "RSA",
			"alg": "RS256",
			"use": "sig",
			"kid": "test-key",
			"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		body := map[string]any{"access_token": "at", "token_type": "Bearer"}
		if f.claims != nil {
			body["id_token"] = f.signIDToken(t, f.claims)
		}
		writeJSON(w, body)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// signIDToken mints an RS256 JWT over the given claims, filling in the
// standard ones the verifier requires.
func (f *fakeIdP) signIDToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	full := map[string]any{
		"iss": f.srv.URL,
		"aud": "test-client",
		"sub": "subject-1",
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(),
	}
	for k, v := range claims {
		full[k] = v
	}
	seg := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal jwt segment: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	signing := seg(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "test-key"}) + "." + seg(full)
	key := f.key
	if f.rogueKey != nil {
		key = f.rogueKey
	}
	sig, err := signRS256(key, signing)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signing + "." + sig
}

// signRS256 produces the JWS signature over "<header>.<payload>".
func signRS256(key *rsa.PrivateKey, signingInput string) (string, error) {
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(sig), nil
}

func (f *fakeIdP) authenticator(t *testing.T, scopes []string) *OIDCAuthenticator {
	t.Helper()
	a, err := NewOIDCAuthenticator(context.Background(), f.srv.URL,
		"test-client", "test-secret", "http://api.test/api/auth/sso/callback", scopes)
	if err != nil {
		t.Fatalf("new authenticator: %v", err)
	}
	return a
}

// --- NewOIDCAuthenticator / AuthCodeURL ---

func TestNewOIDCAuthenticator_AuthCodeURLCarriesStateNonceAndOpenID(t *testing.T) {
	idp := newFakeIdP(t)
	// Exactly the config that silently produced no id_token before the scope
	// guarantee moved to the point of use.
	a := idp.authenticator(t, []string{"email", "profile"})

	u, err := url.Parse(a.AuthCodeURL("the-state", "the-nonce"))
	if err != nil {
		t.Fatalf("parse auth url: %v", err)
	}
	if got := u.Query().Get("state"); got != "the-state" {
		t.Errorf("state: got %q", got)
	}
	if got := u.Query().Get("nonce"); got != "the-nonce" {
		t.Errorf("nonce: got %q", got)
	}
	scopes := strings.Fields(u.Query().Get("scope"))
	if len(scopes) == 0 || scopes[0] != oidc.ScopeOpenID {
		t.Fatalf("scope %q must request openid, or the IdP returns no id_token", u.Query().Get("scope"))
	}
	if got := u.Query().Get("redirect_uri"); got != "http://api.test/api/auth/sso/callback" {
		t.Errorf("redirect_uri: got %q", got)
	}
}

func TestNewOIDCAuthenticator_BadIssuerFailsBoot(t *testing.T) {
	// A discovery failure must surface at startup, not at every login.
	if _, err := NewOIDCAuthenticator(context.Background(),
		"http://127.0.0.1:1/not-an-issuer", "c", "s", "http://api.test/cb", nil); err == nil {
		t.Fatal("expected discovery against a dead issuer to fail")
	}
}

// A *refused* connection (the test above) fails fast on its own. The dangerous
// shape is an issuer that accepts the TCP connection and never answers — a
// corporate IdP behind a WAF, or an overloaded proxy. go-oidc's discovery falls
// back to http.DefaultClient, which has no timeout, so the caller's context
// deadline is the only thing that can end that wait.
//
// cmd/dstream/server.go runs discovery BEFORE ListenAndServe, so an unbounded
// wait there leaves nothing bound at all — no /healthz, no ingest, no dashboard
// — turning an IdP incident into a total outage. That is why the boot passes a
// 10s context; this pins the half that can silently regress: that discovery
// actually honours it.
func TestNewOIDCAuthenticator_SilentIssuerHonoursContextDeadline(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return // listener closed by cleanup
			}
			defer c.Close() // held open, never written to
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	errc := make(chan error, 1)
	go func() {
		_, err := NewOIDCAuthenticator(ctx, "http://"+ln.Addr().String(),
			"c", "s", "http://api.test/cb", nil)
		errc <- err
	}()

	// Generous relative to the 500ms deadline: this asserts "bounded", not a
	// latency figure, so it cannot flake on a loaded machine.
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("discovery against a silent issuer somehow succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("discovery against a silent issuer did not return within 10s — the caller's deadline is not honoured, so a black-holed IdP hangs the boot")
	}
}

// --- Exchange ---

func TestExchange_VerifiedIdentity(t *testing.T) {
	idp := newFakeIdP(t)
	idp.claims = map[string]any{
		"email":          "sso@example.test",
		"email_verified": true,
		"name":           "SSO User",
		"nonce":          "n-1",
	}
	id, err := idp.authenticator(t, nil).Exchange(context.Background(), "code")
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if id.Email != "sso@example.test" || !id.EmailVerified || id.Nonce != "n-1" ||
		id.Subject != "subject-1" || id.Name != "SSO User" {
		t.Fatalf("identity: %+v", id)
	}
}

// An absent email_verified claim must decode to false, not to the Go zero
// value of a trusted flag by accident. This is the half of the email-takeover
// guard that cannot be expressed at the handler seam.
func TestExchange_EmailVerifiedAbsentIsFalse(t *testing.T) {
	idp := newFakeIdP(t)
	idp.claims = map[string]any{"email": "sso@example.test", "nonce": "n-1"}
	id, err := idp.authenticator(t, nil).Exchange(context.Background(), "code")
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if id.EmailVerified {
		t.Fatal("absent email_verified decoded as verified")
	}
}

func TestExchange_EmailVerifiedFalseStaysFalse(t *testing.T) {
	idp := newFakeIdP(t)
	idp.claims = map[string]any{"email": "sso@example.test", "email_verified": false}
	id, err := idp.authenticator(t, nil).Exchange(context.Background(), "code")
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if id.EmailVerified {
		t.Fatal("email_verified:false decoded as verified")
	}
}

// A token response with no id_token at all is an error, never an empty
// Identity that a caller might mistake for an anonymous success.
func TestExchange_NoIDToken(t *testing.T) {
	idp := newFakeIdP(t) // claims nil => token endpoint omits id_token
	_, err := idp.authenticator(t, nil).Exchange(context.Background(), "code")
	if err == nil {
		t.Fatal("expected an error when the response carries no id_token")
	}
	if !strings.Contains(err.Error(), "no id_token") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Signature verification is the reason this dependency exists (spec §3.1):
// an ID token is a bearer assertion of identity, so an unverified signature
// means anyone who can reach the callback can mint their own.
//
// No other test covers it. iss/aud/exp/nbf are all checked independently of
// the signature, so a token whose claims are perfect but whose signing key
// was never published must still be refused — this test is the only thing
// that fails if signature checking is switched off.
func TestExchange_UnpublishedSigningKeyRejected(t *testing.T) {
	idp := newFakeIdP(t)
	rogue, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rogue key: %v", err)
	}
	idp.rogueKey = rogue // not in the JWKS
	// Claims that would otherwise sail through every other check.
	idp.claims = map[string]any{
		"email": "attacker@example.test", "email_verified": true, "nonce": "n-1",
	}
	if _, err := idp.authenticator(t, nil).Exchange(context.Background(), "code"); err == nil {
		t.Fatal("an id token signed by an unpublished key was accepted")
	}
}

// Wrong audience: a token minted for a different client must not verify.
func TestExchange_WrongAudienceRejected(t *testing.T) {
	idp := newFakeIdP(t)
	idp.claims = map[string]any{"aud": "someone-else", "email": "x@example.test"}
	_, err := idp.authenticator(t, nil).Exchange(context.Background(), "code")
	if err == nil {
		t.Fatal("expected verification to reject a token for another audience")
	}
}

// The exchange error must not carry the authorization code: it is logged, and
// a code is auth-bypass material.
func TestExchange_ErrorOmitsTheCode(t *testing.T) {
	idp := newFakeIdP(t)
	a := idp.authenticator(t, nil)
	idp.srv.Close() // token endpoint now unreachable
	_, err := a.Exchange(context.Background(), "super-secret-code")
	if err == nil {
		t.Fatal("expected the exchange to fail")
	}
	if strings.Contains(err.Error(), "super-secret-code") {
		t.Fatalf("error leaked the authorization code: %v", err)
	}
}
