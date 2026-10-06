package deliver

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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
		{"::", false},                // unspecified v6
		{"fd00::1", false},           // ULA (fc00::/7 upper half)
		{"fe80::a:b", false},         // link-local v6
		{"ff02::1", false},           // link-local multicast v6
		{"ff0e::1", false},           // global multicast v6
		{"239.255.255.250", false},   // multicast v4
		{"169.254.0.1", false},       // link-local v4 low edge
		{"172.31.255.255", false},    // RFC1918 high edge
		{"172.15.255.255", true},     // just below 172.16/12
		{"172.32.0.1", true},         // just above 172.16/12
		{"127.255.255.254", false},   // whole 127/8 is loopback
		{"2001:4860:4860::8888", true},
		// Documents CURRENT behaviour, not contract: isPublicIP treats the
		// broadcast address, and all of 240.0.0.0/4 (reserved), as public.
		{"255.255.255.255", true},
		{"240.0.0.1", true},
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

func TestIsSelfHost(t *testing.T) {
	self := []string{"dstream.example.com"}
	if !IsSelfHost("https://dstream.example.com/e/tok", self) {
		t.Fatal("own host must be self")
	}
	if IsSelfHost("https://customer.test/hook", self) {
		t.Fatal("external host must not be self")
	}
}

func TestIncomingHops(t *testing.T) {
	if got := incomingHops(map[string][]string{"Dstream-Webhook-Hops": {"2"}}); got != 2 {
		t.Fatalf("got %d want 2", got)
	}
	if got := incomingHops(map[string][]string{}); got != 0 {
		t.Fatalf("absent must be 0, got %d", got)
	}
}

func TestIsPublicIP_InvalidAddr(t *testing.T) {
	if isPublicIP(netip.Addr{}) {
		t.Fatal("the zero Addr must never be treated as public")
	}
}

func TestDialControl(t *testing.T) {
	guard := dialControl(false)
	for _, c := range []struct{ addr, wantErr string }{
		{"nonsense", "bad dial address"},
		{"example.com:80", "unparseable dial ip"},
		{"10.1.2.3:80", "non-public address 10.1.2.3"},
		{"[::1]:80", "non-public address ::1"},
		{"169.254.169.254:80", "non-public address 169.254.169.254"},
		{"[::ffff:127.0.0.1]:80", "non-public address ::ffff:127.0.0.1"},
	} {
		err := guard("tcp", c.addr, nil)
		if err == nil || !strings.Contains(err.Error(), "ssrf-guard") || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("dialControl(%q) = %v, want ssrf-guard error containing %q", c.addr, err, c.wantErr)
		}
	}
	if err := guard("tcp", "1.1.1.1:443", nil); err != nil {
		t.Errorf("public address refused: %v", err)
	}
	if err := guard("tcp", "[2606:4700:4700::1111]:443", nil); err != nil {
		t.Errorf("public v6 address refused: %v", err)
	}
	// Opt-out lets everything through, even addresses the guard cannot parse.
	open := dialControl(true)
	for _, a := range []string{"127.0.0.1:80", "169.254.169.254:80", "nonsense"} {
		if err := open("tcp", a, nil); err != nil {
			t.Errorf("allowPrivate must not refuse %q: %v", a, err)
		}
	}
}

func countingServer(h http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	var n atomic.Int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		if h != nil {
			h(w, r)
		}
	})), &n
}

// TestSafeClient_DialTimeRefusal is the security boundary: the refusal happens
// at dial, after name resolution, so a hostname that resolves to a private
// address (the DNS-rebinding shape) is refused and nothing is ever connected.
func TestSafeClient_DialTimeRefusal(t *testing.T) {
	srv, hits := countingServer(nil)
	defer srv.Close()
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	cl := newSafeHTTPClient(5*time.Second, false)

	for _, target := range []string{
		srv.URL,                              // literal 127.0.0.1
		"http://localhost:" + itoa(port),     // hostname -> loopback, resolved at dial
		"http://169.254.169.254/latest/meta", // cloud metadata
		"http://[::1]:" + itoa(port),
	} {
		resp, err := cl.Get(target)
		if err == nil {
			resp.Body.Close()
			t.Fatalf("GET %s succeeded; SSRF guard did not refuse", target)
		}
		if !strings.Contains(err.Error(), "ssrf-guard: refusing to connect to non-public address") {
			t.Errorf("GET %s: error %q is not the ssrf-guard refusal", target, err)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("destination was reached %d times despite the guard", n)
	}
}

func TestSafeClient_AllowPrivateOptOut(t *testing.T) {
	srv, hits := countingServer(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "hi") })
	defer srv.Close()
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	for _, mk := range []func(time.Duration, bool) *http.Client{newSafeHTTPClient, NewSafeHTTPClient} {
		cl := mk(5*time.Second, true)
		if cl.Timeout != 5*time.Second {
			t.Errorf("client timeout = %v, want 5s", cl.Timeout)
		}
		resp, err := cl.Get("http://localhost:" + itoa(port))
		if err != nil {
			t.Fatalf("allowPrivate client must reach loopback: %v", err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(b) != "hi" || resp.StatusCode != 200 {
			t.Errorf("got %d %q", resp.StatusCode, b)
		}
	}
	if hits.Load() != 2 {
		t.Errorf("hits = %d, want 2", hits.Load())
	}
	// And the exported constructor is guarded by default.
	if _, err := NewSafeHTTPClient(time.Second, false).Get(srv.URL); err == nil ||
		!strings.Contains(err.Error(), "ssrf-guard") {
		t.Errorf("NewSafeHTTPClient(false) did not refuse loopback: %v", err)
	}
}

func TestSafeClient_Redirects(t *testing.T) {
	cl := newSafeHTTPClient(5*time.Second, true)

	// Endless self-redirect: stops at 10 hops, having made exactly 10 requests.
	loop, loopHits := countingServer(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Path, http.StatusFound)
	})
	defer loop.Close()
	_, err := cl.Get(loop.URL + "/x")
	if err == nil || !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Fatalf("redirect loop err = %v", err)
	}
	if n := loopHits.Load(); n != 10 {
		t.Errorf("redirect loop made %d requests, want 10", n)
	}

	// A redirect to a non-http(s) scheme is refused by CheckRedirect.
	bad, _ := countingServer(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "ftp://example.com/x")
		w.WriteHeader(http.StatusFound)
	})
	defer bad.Close()
	_, err = cl.Get(bad.URL)
	if err == nil || !strings.Contains(err.Error(), `refusing redirect to scheme "ftp"`) {
		t.Fatalf("scheme redirect err = %v", err)
	}

	// A normal same-scheme redirect is followed.
	ok, _ := countingServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/final" {
			_, _ = io.WriteString(w, "arrived")
			return
		}
		http.Redirect(w, r, "/final", http.StatusFound)
	})
	defer ok.Close()
	resp, err := cl.Get(ok.URL)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "arrived" {
		t.Errorf("redirect body = %q", b)
	}
}

func TestSafeClient_Timeout(t *testing.T) {
	release := make(chan struct{})
	srv, _ := countingServer(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	defer srv.Close()
	defer close(release)
	start := time.Now()
	_, err := newSafeHTTPClient(100*time.Millisecond, true).Get(srv.URL)
	var ne net.Error
	if err == nil || !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("want a timeout error, got %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("timeout took %v", time.Since(start))
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestValidateDestinationURL_Messages(t *testing.T) {
	for raw, want := range map[string]string{
		"ftp://h/x":      `url scheme must be http or https, got "ftp"`,
		"https://":       "url must have a host",
		"http://[::1":    "invalid url",
		"  gopher://x  ": `got "gopher"`,
	} {
		err := ValidateDestinationURL(raw)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ValidateDestinationURL(%q) = %v, want %q", raw, err, want)
		}
	}
	// Surrounding whitespace is tolerated by the validator.
	if err := ValidateDestinationURL("  https://ok.example/x \n"); err != nil {
		t.Errorf("trimmed url rejected: %v", err)
	}
}

func TestIsSelfHost_Cases(t *testing.T) {
	self := []string{"dstream.example.com", "localhost"}
	for raw, want := range map[string]bool{
		"https://DSTREAM.example.com/e/x":      true, // host is case-insensitive
		"http://localhost:8080/x":              true, // port ignored
		"  https://dstream.example.com  ":      true, // trimmed
		"https://dstream.example.com.evil.io/": false,
		"https://evil.io/dstream.example.com":  false,
		"http://[::1":                          false, // unparseable
		"":                                     false,
	} {
		if got := IsSelfHost(raw, self); got != want {
			t.Errorf("IsSelfHost(%q) = %v, want %v", raw, got, want)
		}
	}
	if IsSelfHost("https://dstream.example.com", nil) {
		t.Error("no self hosts configured must match nothing")
	}
}

func TestIncomingHops_Cases(t *testing.T) {
	for name, c := range map[string]struct {
		h    map[string][]string
		want int
	}{
		"nil map":          {nil, 0},
		"lowercase key":    {map[string][]string{"dstream-webhook-hops": {"4"}}, 4},
		"mixed case key":   {map[string][]string{"DSTREAM-WEBHOOK-HOPS": {"1"}}, 1},
		"malformed":        {map[string][]string{"Dstream-Webhook-Hops": {"abc"}}, 0},
		"empty string":     {map[string][]string{"Dstream-Webhook-Hops": {""}}, 0},
		"zero":             {map[string][]string{"Dstream-Webhook-Hops": {"0"}}, 0},
		"negative":         {map[string][]string{"Dstream-Webhook-Hops": {"-3"}}, 0},
		"float":            {map[string][]string{"Dstream-Webhook-Hops": {"2.5"}}, 0},
		"empty value list": {map[string][]string{"Dstream-Webhook-Hops": {}}, 0},
		"first value wins": {map[string][]string{"Dstream-Webhook-Hops": {"3", "9"}}, 3},
		"over any limit":   {map[string][]string{"Dstream-Webhook-Hops": {"1000000"}}, 1000000},
		"unrelated header": {map[string][]string{"X-Hops": {"7"}}, 0},
	} {
		if got := incomingHops(c.h); got != c.want {
			t.Errorf("%s: incomingHops = %d, want %d", name, got, c.want)
		}
	}
}

func TestForwardableHeader_All(t *testing.T) {
	for k := range sensitiveHeaders {
		if forwardableHeader(k) || forwardableHeader(strings.ToLower(k)) || forwardableHeader(strings.ToUpper(k)) {
			t.Errorf("%q must be blocked in every casing", k)
		}
	}
	for _, k := range []string{"Dstream-Webhook-Hops", "proxy-authorization", "set-cookie", "X-Real-IP", "Te"} {
		if forwardableHeader(k) {
			t.Errorf("%q must be blocked", k)
		}
	}
	if !forwardableHeader("X-Forwarded-Custom") || !forwardableHeader("Dstream-Event-Id") {
		t.Error("non-listed headers must pass")
	}
}
