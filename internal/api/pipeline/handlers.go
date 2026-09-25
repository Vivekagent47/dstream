// Package pipeline implements the /api handlers for the traffic plane:
// sources, destinations, connections, and events. Routes are declared
// centrally in the parent api package (router.go).
package pipeline

import (
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/ingest"
	"github.com/Vivekagent47/dstream/internal/store"
)

// Handlers carries the dependencies the pipeline endpoints need. Wired from
// api.Deps in api.Mount.
type Handlers struct {
	Log     *slog.Logger
	Queries *store.Queries
	Queue   *dqueue.Client
	// BodyStore persists the synthetic payload for test-connection events, so
	// the delivery worker can read it back by body_ref (same path as ingest).
	BodyStore ingest.BodyStore
	// EvictSourceCache drops a source from the ingest in-process cache so
	// enable/disable and allowed-methods edits take effect immediately.
	// nil-safe: nil means no cache to evict.
	EvictSourceCache func(token string)
	// SelfHosts are dstream's own hostnames; a destination pointing at one is
	// rejected at create/patch (loop guard).
	SelfHosts []string
	// Replayer is the SSRF-guarded HTTP client for server-side bookmark
	// replay-to-URL (blocks loopback/private unless AllowPrivateDestinations).
	Replayer *http.Client
	// Pool begins transactions for multi-row writes (scenario step-replace).
	Pool *pgxpool.Pool
	// PublicBaseURL is the API's externally-visible scheme://host[:port], used
	// to build the source ingest URL server-side (it lives on the API host, not
	// the dashboard origin, so the client can't derive it from window.origin).
	PublicBaseURL string
}
