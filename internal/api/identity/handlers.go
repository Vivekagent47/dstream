// Package identity implements the /api handlers for who-you-are concerns:
// magic-link auth, orgs, membership, invites, API keys, and the audit log.
// Routes are declared centrally in the parent api package (router.go).
package identity

import (
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/store"
)

// Handlers carries the dependencies the identity endpoints need. Wired from
// api.Deps in api.Mount.
type Handlers struct {
	Log     *slog.Logger
	Queries *store.Queries
	// Pool exposes the underlying pgxpool so handlers can begin
	// transactions (notably the magic-link bootstrap, which wraps
	// user-create + invite-apply + workspace-mint in one atomic op).
	Pool   *pgxpool.Pool
	Redis  *redis.Client
	Signer *auth.SessionSigner
	// PublicBaseURL is the externally-visible scheme://host[:port] for the
	// service. Used to render invite links (and similar) into emails / logs.
	PublicBaseURL string
	// DevMode gates dev-only conveniences (notably: logging plaintext
	// magic-link and invite tokens to the server log). Production must
	// leave this off, since the server log is then an audit-bypass vector.
	DevMode bool
}
