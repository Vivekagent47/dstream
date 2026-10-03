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
