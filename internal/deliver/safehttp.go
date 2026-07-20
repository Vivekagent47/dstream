package deliver

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/propagation"
)

// newSafeHTTPClient returns an http.Client for outbound webhook delivery whose
// dialer refuses to connect to non-public IP addresses unless allowPrivate is
// set (self-hosters delivering to private ranges opt in explicitly).
//
// The guard runs at DIAL time — after DNS resolution — so it also defeats
// DNS-rebinding: a hostname that resolves to a public IP during a create-time
// pre-check but to 169.254.169.254 (cloud metadata) at delivery time is still
// rejected here. CheckRedirect re-validates the scheme on every hop; the dial
// guard covers redirected hosts automatically because each hop dials afresh.
func newSafeHTTPClient(timeout time.Duration, allowPrivate bool) *http.Client {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			if allowPrivate {
				return nil
			}
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return fmt.Errorf("ssrf-guard: bad dial address %q: %w", address, err)
			}
			ip, err := netip.ParseAddr(host)
			if err != nil {
				return fmt.Errorf("ssrf-guard: unparseable dial ip %q", host)
			}
			if !isPublicIP(ip) {
				return fmt.Errorf("ssrf-guard: refusing to connect to non-public address %s", ip)
			}
			return nil
		},
	}
	base := &http.Transport{
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{
		Timeout: timeout,
		// Trace the outbound call locally, but inject NO propagation headers: a
		// no-op propagator stops dstream's internal traceparent/tracestate (and
		// baggage) from leaking to customer-controlled destination URLs. The span
		// is still recorded on our side.
		Transport: otelhttp.NewTransport(base,
			otelhttp.WithPropagators(propagation.NewCompositeTextMapPropagator())),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("stopped after 10 redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("ssrf-guard: refusing redirect to scheme %q", req.URL.Scheme)
			}
			return nil
		},
	}
}

// isPublicIP reports whether ip is a globally-routable unicast address safe to
// deliver to. Rejects loopback, private (RFC1918 + ULA fc00::/7), link-local
// (incl. 169.254.169.254 cloud metadata and fe80::/10), multicast, and the
// unspecified address.
func isPublicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return false
	}
	return true
}

// ValidateDestinationURL rejects destination URLs that are structurally unsafe
// to deliver to — anything that isn't a well-formed http(s) URL with a host.
// This is the create/patch-time fast check; the dial-time guard in
// newSafeHTTPClient is the real security boundary (it catches IPs and rebinding).
func ValidateDestinationURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("url scheme must be http or https, got %q", u.Scheme)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("url must have a host")
	}
	return nil
}

// sensitiveHeaders are stripped from inbound requests before they are forwarded
// to a destination: credentials that were meant for dstream (not the
// destination) plus hop-by-hop headers that must not be proxied. Keys are in
// http.CanonicalHeaderKey form for direct map/compare use.
var sensitiveHeaders = map[string]struct{}{
	"Authorization":       {},
	"Proxy-Authorization": {},
	"Cookie":              {},
	"Set-Cookie":          {},
	"Host":                {},
	"Content-Length":      {}, // set by the http client from the body
	"Connection":          {},
	"Proxy-Connection":    {},
	"Keep-Alive":          {},
	"Transfer-Encoding":   {},
	"Te":                  {},
	"Trailer":             {},
	"Upgrade":             {},
	"X-Forwarded-For":     {},
	"X-Forwarded-Host":    {},
	"X-Forwarded-Proto":   {},
	"X-Real-Ip":           {},
}

// forwardableHeader reports whether an inbound header key may be forwarded to a
// destination. The comparison is case-insensitive via canonicalization.
func forwardableHeader(key string) bool {
	_, blocked := sensitiveHeaders[http.CanonicalHeaderKey(key)]
	return !blocked
}
