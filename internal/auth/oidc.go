package auth

import (
	"context"
	"fmt"
	"slices"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Identity is the verified subset of an ID token that dstream acts on.
// Nothing else from the token is retained: dstream's identity model keys on
// the verified email, so carrying more would invite depending on it.
type Identity struct {
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
	// Nonce as asserted by the ID token. The caller compares it against the
	// nonce it issued; a mismatch means the token was not minted for this
	// login attempt.
	Nonce string
}

// Authenticator is the narrow seam the SSO handlers depend on. The real
// implementation wraps go-oidc; tests substitute a fake, so the callback
// handler is testable without a live IdP or a signing key.
type Authenticator interface {
	// AuthCodeURL returns the IdP URL to redirect the browser to.
	AuthCodeURL(state, nonce string) string
	// Exchange swaps a callback code for a verified Identity. It returns an
	// error if the ID token is absent, unverifiable, or fails any standard
	// claim check.
	Exchange(ctx context.Context, code string) (Identity, error)
}

// OIDCAuthenticator is the go-oidc-backed Authenticator.
type OIDCAuthenticator struct {
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
}

// withOpenIDScope guarantees the `openid` scope is requested. Without it an
// IdP runs a plain OAuth2 authorization and returns **no id_token** — which
// surfaces as an opaque "response carried no id_token" at a user's first
// login. Config validation deliberately accepts any scope list
// (DSTREAM_OIDC_SCOPES=email,profile is legal), so the guarantee has to live
// here, at the point of use, rather than in one config path.
func withOpenIDScope(scopes []string) []string {
	if len(scopes) == 0 {
		return []string{oidc.ScopeOpenID, "email", "profile"}
	}
	if slices.Contains(scopes, oidc.ScopeOpenID) {
		return scopes
	}
	return append([]string{oidc.ScopeOpenID}, scopes...)
}

// NewOIDCAuthenticator performs discovery against the issuer. It is called at
// server startup, so a wrong issuer fails the boot rather than every login.
func NewOIDCAuthenticator(ctx context.Context, issuer, clientID, clientSecret, redirectURL string, scopes []string) (*OIDCAuthenticator, error) {
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery for %q: %w", issuer, err)
	}
	return &OIDCAuthenticator{
		oauth: oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RedirectURL:  redirectURL,
			Endpoint:     provider.Endpoint(),
			Scopes:       withOpenIDScope(scopes),
		},
		verifier: provider.Verifier(&oidc.Config{ClientID: clientID}),
	}, nil
}

func (a *OIDCAuthenticator) AuthCodeURL(state, nonce string) string {
	return a.oauth.AuthCodeURL(state, oidc.Nonce(nonce))
}

func (a *OIDCAuthenticator) Exchange(ctx context.Context, code string) (Identity, error) {
	tok, err := a.oauth.Exchange(ctx, code)
	if err != nil {
		// Deliberately not wrapped with the code or any token material: this
		// error is logged, and an authorization code is auth-bypass material.
		return Identity{}, fmt.Errorf("oidc: code exchange failed")
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok || raw == "" {
		return Identity{}, fmt.Errorf("oidc: response carried no id_token")
	}
	idt, err := a.verifier.Verify(ctx, raw)
	if err != nil {
		return Identity{}, fmt.Errorf("oidc: id token verification failed: %w", err)
	}
	var claims struct {
		Email         string `json:"email"`
		EmailVerified *bool  `json:"email_verified"`
		Name          string `json:"name"`
	}
	if err := idt.Claims(&claims); err != nil {
		return Identity{}, fmt.Errorf("oidc: decode claims: %w", err)
	}
	return Identity{
		Subject: idt.Subject,
		Email:   claims.Email,
		// The pointer only exists so a missing claim cannot decode as a
		// trusted flag. Absent and false are then deliberately collapsed:
		// both are refused by the caller, with a message distinct from a
		// generic auth failure, and nothing downstream acts on the
		// difference — so carrying it would promise a signal no log line
		// actually emits. TestExchange_EmailVerifiedAbsentIsFalse pins this.
		EmailVerified: claims.EmailVerified != nil && *claims.EmailVerified,
		Name:          claims.Name,
		Nonce:         idt.Nonce,
	}, nil
}
