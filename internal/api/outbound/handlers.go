package outbound

import (
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Vivekagent47/dstream/internal/api/httpx"
	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/store"
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
	Queue   *dqueue.Client
	// SelfHosts are dstream's own hostnames; an endpoint pointing at one is
	// rejected at create/patch (loop guard).
	SelfHosts []string
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
	}
}
