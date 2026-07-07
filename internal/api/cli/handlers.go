// Package cli implements the /api/cli control plane: source lookups for the
// dstream CLI and the WebSocket tunnel that delivers events to local
// processes. Routes are declared centrally in the parent api package.
package cli

import (
	"log/slog"

	"github.com/redis/go-redis/v9"

	"github.com/Vivekagent47/dstream/internal/store"
)

// Handlers carries the dependencies the CLI endpoints need. Wired from
// api.Deps in api.Mount.
type Handlers struct {
	Log     *slog.Logger
	Queries *store.Queries
	Redis   *redis.Client
	// PublicBaseURL is the externally-visible scheme://host[:port] for the
	// service, echoed to the CLI for constructing ingest URLs.
	PublicBaseURL string
}
