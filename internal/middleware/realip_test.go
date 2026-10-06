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

// TestTrustedRealIP_PeerAndHeaderShapes pins the rewrite for the awkward inputs
// the happy-path tests skip: a trusted peer with no port, a peer that is not an
// IP at all, a malformed or empty X-Forwarded-For, a single-IP trust entry, and
// a chain of only trusted hops.
func TestTrustedRealIP_PeerAndHeaderShapes(t *testing.T) {
	cases := []struct {
		name    string
		trusted []string
		peer    string
		xff     string
		want    string
	}{
		{"bare trusted peer (no port) is honoured", []string{"10.0.0.0/8"}, "10.0.0.5", "203.0.113.7, 10.0.0.5", "203.0.113.7"},
		{"single-IP trust entry is honoured", []string{"10.0.0.5"}, "10.0.0.5:80", "203.0.113.7", "203.0.113.7"},
		{"non-IP peer is untrusted, header ignored", []string{"10.0.0.0/8"}, "not-an-ip", "203.0.113.7", "not-an-ip"},
		{"malformed XFF from trusted peer keeps the peer", []string{"10.0.0.0/8"}, "10.0.0.5:80", "garbage", "10.0.0.5:80"},
		{"missing XFF from trusted peer keeps the peer", []string{"10.0.0.0/8"}, "10.0.0.5:80", "", "10.0.0.5:80"},
		{"all-trusted chain yields no client, peer kept", []string{"10.0.0.0/8"}, "10.0.0.5:80", "10.0.0.9, 10.0.0.5", "10.0.0.5:80"},
		{"spoofed leftmost entry is not the client", []string{"10.0.0.0/8"}, "10.0.0.5:80", "6.6.6.6, 203.0.113.7, 10.0.0.5", "203.0.113.7"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mw, err := TrustedRealIP(tc.trusted)
			if err != nil {
				t.Fatal(err)
			}
			var seen string
			h := mw(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen = r.RemoteAddr }))
			req := httptest.NewRequest("GET", "/", nil)
			req.RemoteAddr = tc.peer
			if tc.xff != "" {
				req.Header.Set("X-Forwarded-For", tc.xff)
			}
			h.ServeHTTP(httptest.NewRecorder(), req)
			if seen != tc.want {
				t.Fatalf("RemoteAddr = %q, want %q", seen, tc.want)
			}
		})
	}
}
