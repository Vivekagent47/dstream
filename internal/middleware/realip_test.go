package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTrustedRealIP_EmptyIsNoop(t *testing.T) {
	mw, err := TrustedRealIP(nil)
	if err != nil {
		t.Fatal(err)
	}

	var seen string
	h := mw(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = r.RemoteAddr
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "203.0.113.5:1234"
	req.Header.Set("X-Forwarded-For", "8.8.8.8")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if seen != "203.0.113.5:1234" {
		t.Fatalf("expected unchanged RemoteAddr, got %q", seen)
	}
}

func TestTrustedRealIP_PeelsTrustedHop(t *testing.T) {
	mw, err := TrustedRealIP([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}

	var seen string
	h := mw(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = r.RemoteAddr
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.5:5555"
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.5")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if seen != "203.0.113.7" {
		t.Fatalf("expected real client 203.0.113.7, got %q", seen)
	}
}

func TestTrustedRealIP_UntrustedPeerKeepsRemoteAddr(t *testing.T) {
	mw, err := TrustedRealIP([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}

	var seen string
	h := mw(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = r.RemoteAddr
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "203.0.113.5:1234"
	req.Header.Set("X-Forwarded-For", "8.8.8.8")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if seen != "203.0.113.5:1234" {
		t.Fatalf("expected untrusted peer untouched, got %q", seen)
	}
}

func TestTrustedRealIP_BadCIDR(t *testing.T) {
	_, err := TrustedRealIP([]string{"not-an-ip"})
	if err == nil {
		t.Fatal("expected error on bad CIDR")
	}
}
