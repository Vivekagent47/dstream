package auth

import "net/http"

// RequireRole gates one route on a minimum org role. Mount it per-route with
// r.With(RequireRole(RoleAdmin)) for the privileged routes whose danger isn't
// implied by their HTTP method — secret reads, secret rotation, outbound
// publish, portal-access minting.
//
// It MUST mount below RequireOrg: for a session principal RequireOrg is what
// resolves the membership row and assigns Principal.Role (an API-key principal
// already carries its role from Authenticate), so a check above it reads the
// zero role for every logged-in user and denies them. A missing principal is
// treated as a denial rather than a 401 — these only mount below Authenticate,
// so the case is unreachable in production and failing closed is the safe
// reading of a mis-wiring.
func RequireRole(min Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := RequireMinRole(r.Context(), min); err != nil {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireAdminForURLChange gates a delivery-URL change at RoleAdmin, for
// handlers whose route is otherwise member-writable. It returns true when the
// caller may proceed and has already written the 403 when they may not.
//
// Call it only when the request actually changes the URL — a member editing a
// destination's name or an endpoint's retry policy is ordinary member work and
// stays allowed, which is why this is a handler-level check rather than a
// RequireRole mark on the route.
//
// Why a URL is privileged where the rest of the same PATCH is not: changing it
// silently redirects traffic that is already flowing, to a host the caller
// chooses. The SSRF and loop guards (deliver.ValidateDestinationURL,
// deliver.IsSelfHost) stop the request reaching dstream itself or a private
// address, but neither stops it reaching an attacker's public endpoint — and
// nobody is notified. A member who can do that can quietly exfiltrate every
// payload an org receives.
//
// Scope note, so the next reader knows what this does NOT cover: creating a
// destination or an endpoint is still member-level, so a member can add a new
// sink. That is a visible act — a new row in a list someone is looking at —
// where repointing an existing one is not, and widening the admin line to
// cover creation is a product decision, not a security patch. See PLAN.md §8.
func RequireAdminForURLChange(w http.ResponseWriter, r *http.Request) bool {
	if err := RequireMinRole(r.Context(), RoleAdmin); err != nil {
		http.Error(w, "changing a delivery URL requires the admin role", http.StatusForbidden)
		return false
	}
	return true
}

// AdminForDestructive gates every DELETE in the group it's mounted on at
// RoleAdmin and passes every other method through untouched.
//
// This is the fail-safe half of the permission matrix: a DELETE route added
// to the traffic plane in a year is gated the day it's written, with nobody
// needing to remember to opt in. The explicit RequireRole marks in the router
// cover the privileged routes this rule can't see.
func AdminForDestructive(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			if err := RequireMinRole(r.Context(), RoleAdmin); err != nil {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
