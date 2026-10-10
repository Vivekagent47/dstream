package outbound

import (
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Vivekagent47/dstream/internal/api/httpx"
	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/store"
	"github.com/Vivekagent47/dstream/internal/usage"
)

// isUniqueViolation detects Postgres unique_violation (SQLSTATE 23505)
// via errors.As against *pgconn.PgError.
func isUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

// Handlers serves the outbound (Svix-style) webhook API. Every route is
// org-scoped via the Principal in ctx.
type Handlers struct {
	Log     *slog.Logger
	Queries *store.Queries
	// Pool begins transactions (publish wraps message-create + fan-out atomically
	// so a message can never persist with zero deliveries).
	Pool  *pgxpool.Pool
	Queue *dqueue.Client
	// SelfHosts are dstream's own hostnames; an endpoint pointing at one is
	// rejected at create/patch (loop guard).
	SelfHosts []string
	// SecretGrace is how long a rotated endpoint's previous secret stays valid.
	SecretGrace time.Duration
	// Portal signs App Portal tokens; AppBaseURL builds the portal link.
	Portal     *auth.PortalSigner
	AppBaseURL string
	// Quota meters publishes against the org's messages plan and refuses at the
	// hard ceiling. nil = no quota enforcement (*usage.Gate is nil-safe).
	Quota *usage.Gate
}

func applicationView(a store.Application) map[string]any {
	return map[string]any{
		"id":         store.GoUUID(a.ID).String(),
		"org_id":     store.GoUUID(a.OrgID).String(),
		"uid":        httpx.DerefString(a.Uid),
		"name":       a.Name,
		"metadata":   httpx.RawJSONOrEmpty(a.Metadata),
		"created_at": a.CreatedAt.Time,
		"updated_at": a.UpdatedAt.Time,

		"is_operational": a.IsOperational,
	}
}
