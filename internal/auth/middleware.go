package auth

import (
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Vivekagent47/dstream/internal/store"
)

// Authenticate is the entry-point middleware. It accepts either an API key
// (Authorization: Bearer dsk_...) or a session cookie, and attaches a
// Principal to the request context.
//
// API-key requests carry Source=SourceAPIKey and OrgID resolved from the
// key row. Session requests carry Source=SourceSession; UserID is set, and
// OrgID is whatever the session cookie embeds (may be uuid.Nil for a user
// who has no memberships yet — RequireOrg will reject those for
// tenant-scoped routes).
//
// Role is NOT populated here; only RequireOrg looks up the membership row
// and assigns Role to session principals (API-key principals get
// RoleAdmin as a pass-through sentinel).
//
// Missing or invalid credentials → 401.
func Authenticate(q *store.Queries, s *SessionSigner) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// API key path.
			if raw := ExtractAPIKey(r.Header.Get("Authorization")); raw != "" {
				row, err := VerifyAPIKey(r.Context(), q, raw)
				if err == nil {
					p := Principal{
						Source:     SourceAPIKey,
						APIKeyID:   store.GoUUID(row.ID),
						OrgID:      store.GoUUID(row.OrgID),
						APIKeyName: row.Name,
					}
					next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
					return
				}
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			// Session path. Resolve the user's email NOW so downstream
			// audit.Log calls don't each fire a fresh GetUserByID — every
			// authenticated mutation pays for that lookup otherwise.
			if userID, orgID, epoch, err := s.Parse(r); err == nil {
				p := Principal{
					Source: SourceSession,
					UserID: userID,
					OrgID:  orgID, // may be uuid.Nil if user has no active org yet
				}
				if u, uerr := q.GetUserByID(r.Context(), store.UUID(userID)); uerr == nil {
					// Epoch mismatch means the session was revoked (logout-all /
					// disable) after this cookie was issued — reject it.
					if int64(u.SessionEpoch) != epoch {
						http.Error(w, "unauthorized", http.StatusUnauthorized)
						return
					}
					p.UserEmail = u.Email
					p.SessionEpoch = int64(u.SessionEpoch)
				}
				next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
				return
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		})
	}
}

// RequireOrg gates tenant-scoped routes. It assumes Authenticate has
// attached a Principal upstream. It:
//
//   - 401s if no principal is in context.
//   - 409s a session principal whose active_org_id is uuid.Nil (the SPA
//     must force the user to pick an org).
//   - For session principals, looks up org_members(org_id, user_id) and
//     assigns Principal.Role. Returns 403 if no membership row exists.
//   - For API-key principals, assigns RoleAdmin as a sentinel and passes
//     through (the API key implicitly authorizes traffic CRUD; admin-only
//     endpoints add RequireSession on top).
func RequireOrg(q *store.Queries) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, err := FromContext(r.Context())
			if err != nil {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if p.OrgID == (uuid.UUID{}) {
				http.Error(w, "no active org", http.StatusConflict)
				return
			}
			if p.Source == SourceAPIKey {
				p.Role = RoleAdmin
				next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
				return
			}
			m, err := q.GetOrgMember(r.Context(), store.GetOrgMemberParams{
				OrgID:  store.UUID(p.OrgID),
				UserID: store.UUID(p.UserID),
			})
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					http.Error(w, "not a member of active org", http.StatusForbidden)
					return
				}
				http.Error(w, "membership lookup failed", http.StatusInternalServerError)
				return
			}
			p.Role = Role(m.Role)
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
		})
	}
}

// SuperAdminOnly gates the /admin/* surface to users with is_super_admin.
// It performs its own cookie parse rather than chaining off Authenticate
// because the super-admin surface is session-only and the org dimension is
// irrelevant.
func SuperAdminOnly(q *store.Queries, s *SessionSigner) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			uid, _, epoch, err := s.Parse(r) // active_org_id unused for super-admin
			if err != nil {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			u, err := q.GetUserByID(r.Context(), store.UUID(uid))
			if err != nil || !u.IsSuperAdmin || int64(u.SessionEpoch) != epoch {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			p := Principal{Source: SourceSession, UserID: uid, SessionEpoch: int64(u.SessionEpoch)}
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
		})
	}
}
