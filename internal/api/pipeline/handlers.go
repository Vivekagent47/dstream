// Package pipeline implements the /api handlers for the traffic plane:
// sources, destinations, connections, and events. Routes are declared
// centrally in the parent api package (router.go).
package pipeline

import (
	"log/slog"

	"github.com/Vivekagent47/dstream/internal/queue"
	"github.com/Vivekagent47/dstream/internal/store"
)

// Handlers carries the dependencies the pipeline endpoints need. Wired from
// api.Deps in api.Mount.
type Handlers struct {
	Log     *slog.Logger
	Queries *store.Queries
	Queue   *queue.Client
	// EvictSourceCache drops a source from the ingest in-process cache so
	// enable/disable and allowed-methods edits take effect immediately.
	// nil-safe: nil means no cache to evict.
	EvictSourceCache func(token string)
}
