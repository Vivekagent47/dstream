package auth

import (
	"context"
	"errors"
)

var (
	// ErrInsufficientRole is returned by RequireMinRole when the principal's
	// role ranks below the requested minimum. Handlers map this to 403.
	ErrInsufficientRole = errors.New("auth: insufficient role")

	// ErrSessionRequired is returned by RequireSession when the principal
	// authenticated via API key on a session-only route. Handlers map this
	// to 403.
	ErrSessionRequired = errors.New("auth: session required")
)

// RequireMinRole returns nil if the principal's role is at or above min.
// Used by handlers to gate org-management actions that aren't covered by a
// blanket middleware (e.g. "admins can invite, members cannot"). The
// per-route inline call sites make the permissions matrix searchable in
// the source tree.
func RequireMinRole(ctx context.Context, min Role) error {
	p, err := FromContext(ctx)
	if err != nil {
		return err
	}
	if p.Role.LessThan(min) {
		return ErrInsufficientRole
	}
	return nil
}

// RequireSession returns nil if the principal authenticated via session
// cookie. Used to reject API keys on org-management endpoints — those are
// human-only.
func RequireSession(ctx context.Context) error {
	p, err := FromContext(ctx)
	if err != nil {
		return err
	}
	if p.Source != SourceSession {
		return ErrSessionRequired
	}
	return nil
}
