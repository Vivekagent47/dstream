package config

import (
	"os"
	"reflect"
	"testing"
	"time"
)

func TestLoad_TrustedProxiesCSV(t *testing.T) {
	t.Setenv("DSTREAM_TRUSTED_PROXIES", "10.0.0.0/8,172.16.0.0/12,127.0.0.1")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"10.0.0.0/8", "172.16.0.0/12", "127.0.0.1"}
	if !reflect.DeepEqual(c.TrustedProxies, want) {
		t.Fatalf("got %#v want %#v", c.TrustedProxies, want)
	}
}

func TestLoad_TrustedProxiesEmpty(t *testing.T) {
	os.Unsetenv("DSTREAM_TRUSTED_PROXIES")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.TrustedProxies) != 0 {
		t.Fatalf("expected empty, got %#v", c.TrustedProxies)
	}
}

func TestLoad_SessionSecretBinding(t *testing.T) {
	want := "0123456789abcdef0123456789abcdef0123456789ab"
	t.Setenv("DSTREAM_SESSION_SECRET", want)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.SessionSecret != want {
		t.Fatalf("SessionSecret got %q want %q", c.SessionSecret, want)
	}
}

func TestLoad_CookieSecureBinding(t *testing.T) {
	t.Setenv("DSTREAM_COOKIE_SECURE", "true")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.CookieSecure {
		t.Fatalf("CookieSecure: want true")
	}
}

func TestLoad_SMTPHostBinding(t *testing.T) {
	t.Setenv("DSTREAM_SMTP_HOST", "smtp.example.com")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.SMTP.Host != "smtp.example.com" {
		t.Fatalf("SMTP.Host got %q", c.SMTP.Host)
	}
}

// Regression: tracing.otlp_endpoint must bind from env. viper's AutomaticEnv
// only reads env for keys with a registered default/BindEnv, so without the
// SetDefault for this key the value is silently dropped and the exporter falls
// back to localhost — breaking compose's http://jaeger:4318 override.
func TestLoad_TracingOTLPEndpointBinding(t *testing.T) {
	t.Setenv("DSTREAM_TRACING_OTLP_ENDPOINT", "http://jaeger:4318")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Tracing.OTLPEndpoint != "http://jaeger:4318" {
		t.Fatalf("OTLPEndpoint got %q want %q", c.Tracing.OTLPEndpoint, "http://jaeger:4318")
	}
}

// payload_retention must parse a duration from env (same viper gotcha as above:
// the "0s" default registers the key so AutomaticEnv actually reads it).
func TestLoad_PayloadRetentionBinding(t *testing.T) {
	t.Setenv("DSTREAM_PAYLOAD_RETENTION", "720h")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.PayloadRetention != 720*time.Hour {
		t.Fatalf("PayloadRetention got %v want 720h", c.PayloadRetention)
	}
}

// Unset => zero => keep payloads forever.
func TestLoad_PayloadRetentionDefaultZero(t *testing.T) {
	os.Unsetenv("DSTREAM_PAYLOAD_RETENTION")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.PayloadRetention != 0 {
		t.Fatalf("PayloadRetention default got %v want 0", c.PayloadRetention)
	}
}

// Boot-time validation exists because every one of these misconfigurations
// produces a confusing failure inside the auth path at a user's first login
// instead of a clear one at startup.
func TestValidateOIDC(t *testing.T) {
	cases := []struct {
		name    string
		oidc    OIDCConfig
		wantErr bool
	}{
		{"disabled is always valid", OIDCConfig{}, false},
		{"full config", OIDCConfig{Issuer: "https://idp.test", ClientID: "id", ClientSecret: "sec"}, false},
		{"issuer without client id", OIDCConfig{Issuer: "https://idp.test", ClientSecret: "sec"}, true},
		{"issuer without client secret", OIDCConfig{Issuer: "https://idp.test", ClientID: "id"}, true},
		// Enforcing SSO with no provider configured removes every way to log in.
		{"enforce without issuer", OIDCConfig{Enforce: true}, true},
		// owner is reserved for explicit promotion; an IdP must not mint owners.
		{"default role owner", OIDCConfig{Issuer: "https://idp.test", ClientID: "id", ClientSecret: "sec", DefaultRole: "owner"}, true},
		{"default role bogus", OIDCConfig{Issuer: "https://idp.test", ClientID: "id", ClientSecret: "sec", DefaultRole: "wat"}, true},
		{"default role admin", OIDCConfig{Issuer: "https://idp.test", ClientID: "id", ClientSecret: "sec", DefaultRole: "admin"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Config{OIDC: c.oidc}.ValidateOIDC()
			if (err != nil) != c.wantErr {
				t.Errorf("got err=%v, wantErr=%v", err, c.wantErr)
			}
		})
	}
}

func TestOIDCEnabled(t *testing.T) {
	if (OIDCConfig{}).Enabled() {
		t.Error("empty config must be disabled")
	}
	if !(OIDCConfig{Issuer: "https://idp.test"}).Enabled() {
		t.Error("a non-empty issuer must enable SSO")
	}
}

// Every DSTREAM_OIDC_* var must actually reach the struct. viper's AutomaticEnv
// only reads env for keys with a registered default/BindEnv (same gotcha as
// tracing.otlp_endpoint above) — a silently-empty client secret would make SSO
// fail at someone's first login with no clue why.
func TestLoad_OIDCBinding(t *testing.T) {
	t.Setenv("DSTREAM_OIDC_ISSUER", "https://idp.test/realms/dstream")
	t.Setenv("DSTREAM_OIDC_CLIENT_ID", "dstream-web")
	t.Setenv("DSTREAM_OIDC_CLIENT_SECRET", "s3cr3t")
	t.Setenv("DSTREAM_OIDC_SCOPES", "openid,email,profile,groups")
	t.Setenv("DSTREAM_OIDC_DEFAULT_ORG", "acme")
	t.Setenv("DSTREAM_OIDC_DEFAULT_ROLE", "admin")
	t.Setenv("DSTREAM_OIDC_ENFORCE", "true")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := OIDCConfig{
		Issuer:         "https://idp.test/realms/dstream",
		ClientID:       "dstream-web",
		ClientSecret:   "s3cr3t",
		Scopes:         []string{"openid", "email", "profile", "groups"},
		DefaultOrgSlug: "acme",
		DefaultRole:    "admin",
		Enforce:        true,
	}
	if !reflect.DeepEqual(c.OIDC, want) {
		t.Fatalf("OIDC env binding broken\n got %#v\nwant %#v", c.OIDC, want)
	}
}

// A deployment that sets none of the OIDC vars must behave exactly as before:
// SSO off, valid, and the defaults in place for when it is switched on.
func TestLoad_OIDCDefaultsDisabled(t *testing.T) {
	for _, k := range []string{
		"DSTREAM_OIDC_ISSUER", "DSTREAM_OIDC_CLIENT_ID", "DSTREAM_OIDC_CLIENT_SECRET",
		"DSTREAM_OIDC_SCOPES", "DSTREAM_OIDC_DEFAULT_ORG", "DSTREAM_OIDC_DEFAULT_ROLE",
		"DSTREAM_OIDC_ENFORCE",
	} {
		os.Unsetenv(k)
	}
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.OIDC.Enabled() {
		t.Fatalf("SSO must be off by default, got %#v", c.OIDC)
	}
	if err := c.ValidateOIDC(); err != nil {
		t.Fatalf("unconfigured deployment must boot: %v", err)
	}
	if !reflect.DeepEqual(c.OIDC.Scopes, []string{"openid", "email", "profile"}) {
		t.Fatalf("default scopes got %#v", c.OIDC.Scopes)
	}
	if c.OIDC.DefaultRole != "member" {
		t.Fatalf("default role got %q want member", c.OIDC.DefaultRole)
	}
}
