package auth

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// Role is the org-membership role attached to a session principal.
//
// API-key principals don't carry a DB role; the middleware assigns them
// RoleAdmin as a sentinel so that role-gated handlers using
// RequireMinRole(...) accept them, while still allowing session-only
// handlers to reject API keys via RequireSession.
type Role string

const (
	RoleMember Role = "member"
	RoleAdmin  Role = "admin"
	RoleOwner  Role = "owner"
)

// roleOrder defines the partial order member < admin < owner. Unknown roles
// rank at 0, strictly below member, so RequireMinRole rejects them.
var roleOrder = map[Role]int{
	RoleMember: 1,
	RoleAdmin:  2,
	RoleOwner:  3,
}

// LessThan reports whether r ranks strictly below other.
func (r Role) LessThan(other Role) bool {
	return roleOrder[r] < roleOrder[other]
}

// Source distinguishes session-authed humans from API-key-authed machines.
// Handlers that must be human-only check Source explicitly via RequireSession.
type Source string

const (
	SourceSession Source = "session"
	SourceAPIKey  Source = "api_key"
)

// Principal is the authenticated identity for a request. Exactly one of
// UserID (session) or APIKeyID (API key) is populated; OrgID is populated
// for both once RequireOrg has resolved the active org.
//
// UserEmail and APIKeyName are denormalized snapshots captured at auth
// time so audit-row writes don't need a fresh GetUserByID / GetAPIKey
// round-trip per mutation. Both are best-effort: empty string is treated
// as "unknown" by the audit helper.
type Principal struct {
	Source     Source
	UserID     uuid.UUID
	OrgID      uuid.UUID
	APIKeyID   uuid.UUID
	Role       Role
	UserEmail  string
	APIKeyName string
}

type ctxKey int

const ctxKeyPrincipal ctxKey = iota

// WithPrincipal attaches p to ctx for downstream handlers to retrieve via
// FromContext.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKeyPrincipal, p)
}

// FromContext recovers the principal attached by an auth middleware.
// Returns a non-nil error if no principal is in ctx.
func FromContext(ctx context.Context) (Principal, error) {
	v, ok := ctx.Value(ctxKeyPrincipal).(Principal)
	if !ok {
		return Principal{}, errors.New("no principal in context")
	}
	return v, nil
}
