package config

import (
	"os"
	"reflect"
	"testing"
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
