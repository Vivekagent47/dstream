package deliver

import (
	"net/netip"
	"testing"
)

func TestIsPublicIP(t *testing.T) {
	cases := []struct {
		ip     string
		public bool
	}{
		{"1.1.1.1", true},
		{"8.8.8.8", true},
		{"2606:4700:4700::1111", true},
		{"127.0.0.1", false},         // loopback
		{"::1", false},               // loopback v6
		{"169.254.169.254", false},   // cloud metadata (link-local)
		{"10.0.0.5", false},          // RFC1918
		{"172.16.4.2", false},        // RFC1918
		{"192.168.1.1", false},       // RFC1918
		{"0.0.0.0", false},           // unspecified
		{"fe80::1", false},           // link-local v6
		{"fc00::1", false},           // ULA
		{"224.0.0.1", false},         // multicast
		{"::ffff:127.0.0.1", false},  // v4-mapped loopback must not slip through
		{"::ffff:10.0.0.1", false},   // v4-mapped private
		{"100.64.0.1", false},        // CGNAT (RFC 6598) low edge
		{"100.127.255.254", false},   // CGNAT (RFC 6598) high edge
		{"::ffff:100.64.0.1", false}, // v4-mapped CGNAT must not slip through
		{"198.18.0.1", false},        // benchmarking (RFC 2544) low edge
		{"198.19.255.254", false},    // benchmarking (RFC 2544) high edge
		{"100.63.255.255", true},     // just below CGNAT — still public
		{"100.128.0.1", true},        // just above CGNAT — still public
		{"198.17.255.255", true},     // just below benchmarking — still public
		{"198.20.0.1", true},         // just above benchmarking — still public
	}
	for _, c := range cases {
		ip, err := netip.ParseAddr(c.ip)
		if err != nil {
			t.Fatalf("parse %q: %v", c.ip, err)
		}
		if got := isPublicIP(ip); got != c.public {
			t.Errorf("isPublicIP(%s) = %v, want %v", c.ip, got, c.public)
		}
	}
}

func TestValidateDestinationURL(t *testing.T) {
	ok := []string{"http://example.com", "https://hooks.example.com/x?y=1", "http://1.2.3.4:9000"}
	for _, u := range ok {
		if err := ValidateDestinationURL(u); err != nil {
			t.Errorf("ValidateDestinationURL(%q) unexpected error: %v", u, err)
		}
	}
	bad := []string{"file:///etc/passwd", "gopher://x", "ftp://h/x", "://nohost", "https://", "not a url", ""}
	for _, u := range bad {
		if err := ValidateDestinationURL(u); err == nil {
			t.Errorf("ValidateDestinationURL(%q) = nil, want error", u)
		}
	}
}

func TestForwardableHeader(t *testing.T) {
	blocked := []string{"Authorization", "authorization", "Cookie", "Host", "Content-Length", "X-Forwarded-For", "Connection", "Transfer-Encoding"}
	for _, h := range blocked {
		if forwardableHeader(h) {
			t.Errorf("forwardableHeader(%q) = true, want false (sensitive/hop-by-hop)", h)
		}
	}
	allowed := []string{"Content-Type", "X-Stripe-Signature", "X-GitHub-Event", "User-Agent", "Accept"}
	for _, h := range allowed {
		if !forwardableHeader(h) {
			t.Errorf("forwardableHeader(%q) = false, want true", h)
		}
	}
}
