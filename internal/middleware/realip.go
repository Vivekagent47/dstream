// Package middleware contains HTTP middleware for the dstream server.
package middleware

import (
	"fmt"
	"net"
	"net/http"

	"github.com/realclientip/realclientip-go"
)

// TrustedRealIP returns a middleware that rewrites r.RemoteAddr to the real
// client IP, extracted from X-Forwarded-For but only when both:
//
//  1. the immediate TCP peer is inside one of trustedProxies, and
//  2. the peeled-back hop chain yields a non-empty client IP.
//
// trustedProxies is a list of single IPs (e.g. "10.0.0.1") and/or CIDRs
// (e.g. "10.0.0.0/8") covering the reverse proxies and load balancers in
// front of dstream. The strategy walks XFF right-to-left, peels off trusted
// hops, and stops at the first untrusted hop — that is the real client.
//
// If trustedProxies is empty, or the immediate peer is not in the trust set,
// the middleware leaves r.RemoteAddr as the direct TCP peer. Naively trusting
// XFF lets any client forge their IP (see GHSA-3fxj-6jh8-hvhx) — this peer
// gate is the defense.
//
// Returns an error if any entry is invalid.
func TrustedRealIP(trustedProxies []string) (func(http.Handler) http.Handler, error) {
	if len(trustedProxies) == 0 {
		return func(next http.Handler) http.Handler { return next }, nil
	}

	ranges, err := realclientip.AddressesAndRangesToIPNets(trustedProxies...)
	if err != nil {
		return nil, fmt.Errorf("parse trusted proxies: %w", err)
	}

	strat, err := realclientip.NewRightmostTrustedRangeStrategy("X-Forwarded-For", ranges)
	if err != nil {
		return nil, fmt.Errorf("build realip strategy: %w", err)
	}

	peerTrusted := func(remoteAddr string) bool {
		host, _, splitErr := net.SplitHostPort(remoteAddr)
		if splitErr != nil {
			host = remoteAddr
		}
		ip := net.ParseIP(host)
		if ip == nil {
			return false
		}
		for i := range ranges {
			if ranges[i].Contains(ip) {
				return true
			}
		}
		return false
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if peerTrusted(r.RemoteAddr) {
				if ip := strat.ClientIP(r.Header, r.RemoteAddr); ip != "" {
					r.RemoteAddr = ip
				}
			}
			next.ServeHTTP(w, r)
		})
	}, nil
}
