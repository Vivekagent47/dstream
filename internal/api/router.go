package api

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	apicli "github.com/Vivekagent47/dstream/internal/api/cli"
	"github.com/Vivekagent47/dstream/internal/api/identity"
	"github.com/Vivekagent47/dstream/internal/api/pipeline"
	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/ingest"
	"github.com/Vivekagent47/dstream/internal/store"
)

// Deps bundles everything an API handler might need so we can wire them via
// a single struct instead of an explosion of constructor arguments.
type Deps struct {
	Log     *slog.Logger
	Queries *store.Queries
	// Pool exposes the underlying pgxpool so handlers can begin
	// transactions (notably the magic-link bootstrap, which wraps
	// user-create + invite-apply + workspace-mint in one atomic op).
	Pool      *pgxpool.Pool
	Redis     *redis.Client
	Queue     *dqueue.Client
	BodyStore ingest.BodyStore
	Signer    *auth.SessionSigner
	// PublicBaseURL is the externally-visible scheme://host[:port] for the
	// service. Used to render invite links (and similar) into emails / logs.
	PublicBaseURL string
	// DevMode gates dev-only conveniences (notably: logging plaintext
	// magic-link and invite tokens to the server log). Production must
	// leave this off, since the server log is then an audit-bypass vector.
	DevMode bool
	// EvictSourceCache drops a source from the ingest in-process cache so
	// enable/disable and allowed-methods edits take effect immediately.
	// nil-safe: nil means no cache to evict.
	EvictSourceCache func(token string)
}

// Mount wires the full /api router onto the parent. `extra` middleware is
// applied to every /api route (use it for cross-cutting concerns like CSRF).
// Handlers live in subpackages (identity, pipeline, cli); every route is
// still declared here so the auth layering stays visible in one place.
func Mount(parent chi.Router, d Deps, extra ...func(http.Handler) http.Handler) {
	id := identity.Handlers{
		Log:           d.Log,
		Queries:       d.Queries,
		Pool:          d.Pool,
		Redis:         d.Redis,
		Signer:        d.Signer,
		PublicBaseURL: d.PublicBaseURL,
		DevMode:       d.DevMode,
	}
	pl := pipeline.Handlers{
		Log:              d.Log,
		Queries:          d.Queries,
		Queue:            d.Queue,
		BodyStore:        d.BodyStore,
		EvictSourceCache: d.EvictSourceCache,
	}
	cli := apicli.Handlers{
		Log:           d.Log,
		Queries:       d.Queries,
		Redis:         d.Redis,
		PublicBaseURL: d.PublicBaseURL,
	}

	parent.Route("/api", func(r chi.Router) {
		for _, m := range extra {
			r.Use(m)
		}

		// Unauthenticated.
		r.Route("/auth", func(r chi.Router) {
			r.Post("/magic-link/request", id.RequestMagicLink)
			// POST (not GET) so a session-establishing request can't be
			// triggered cross-site via <img>/navigation — the JSON body forces a
			// CORS preflight, blocking login-CSRF / session fixation.
			r.Post("/magic-link/verify", id.VerifyMagicLink)
			r.Post("/logout", id.Logout)
		})

		// Invite peek/accept: peek is fully public (so a logged-out user
		// can see what they're being invited to before authenticating);
		// accept is authenticated separately inside the handler so it can
		// handle both session and post-magic-link flows.
		r.Get("/invites/{token}", id.PeekInvite)
		r.Post("/invites/{token}/accept", id.AcceptInvite)

		// Authenticated surface. Authenticate accepts either a session
		// cookie or an API key and attaches a Principal to ctx.
		r.Group(func(r chi.Router) {
			r.Use(auth.Authenticate(d.Queries, d.Signer))

			// User identity + org membership management. These do NOT
			// require an active org — a logged-in user with zero orgs
			// must still be able to list/create/select.
			r.Get("/me", id.Me)
			r.Get("/orgs", id.ListMyOrgs)
			r.Post("/orgs", id.CreateOrg)
			r.Post("/orgs/select", id.SelectOrg)

			// Org-scoped admin operations (members, invites, api-keys,
			// audit, settings). RequireOrg is applied per-handler inside
			// the route group via the {org_id} path itself; the handler
			// verifies the caller's membership in that org.
			r.Route("/orgs/{org_id}", func(r chi.Router) {
				r.Get("/members", id.ListMembers)
				r.Patch("/members/{user_id}", id.PatchMember)
				r.Delete("/members/{user_id}", id.RemoveMember)
				r.Patch("/", id.UpdateOrg)
				r.Delete("/", id.DeleteOrg)
				r.Post("/transfer", id.TransferOwnership)

				r.Get("/invites", id.ListInvites)
				r.Post("/invites", id.CreateInvite)
				r.Delete("/invites/{id}", id.DeleteInvite)

				r.Get("/api-keys", id.ListAPIKeys)
				r.Post("/api-keys", id.CreateAPIKey)
				r.Delete("/api-keys/{id}", id.RevokeAPIKey)

				r.Get("/audit", id.ListAuditForOrg)
			})

			// Tenant-scoped traffic plane. RequireOrg gates these: an
			// API-key principal always has an OrgID set; a session
			// principal must have selected an active org.
			r.Group(func(r chi.Router) {
				r.Use(auth.RequireOrg(d.Queries))

				r.Get("/audit", id.ListAudit)

				r.Route("/sources", func(r chi.Router) {
					r.Get("/", pl.ListSources)
					r.Post("/", pl.CreateSource)
					r.Get("/{id}", pl.GetSource)
					r.Patch("/{id}", pl.PatchSource)
					r.Delete("/{id}", pl.DeleteSource)
				})
				r.Route("/destinations", func(r chi.Router) {
					r.Get("/", pl.ListDestinations)
					r.Post("/", pl.CreateDestination)
					r.Get("/{id}", pl.GetDestination)
					r.Patch("/{id}", pl.PatchDestination)
					r.Delete("/{id}", pl.DeleteDestination)
				})
				r.Route("/connections", func(r chi.Router) {
					r.Get("/stats", pl.AllConnectionStats)
					r.Get("/", pl.ListConnections)
					r.Post("/", pl.CreateConnection)
					r.Get("/{id}", pl.GetConnection)
					r.Get("/{id}/stats", pl.ConnectionStats)
					r.Post("/{id}/test", pl.TestConnection)
					r.Patch("/{id}", pl.PatchConnection)
					r.Delete("/{id}", pl.DeleteConnection)
				})
				r.Route("/events", func(r chi.Router) {
					r.Get("/", pl.ListEvents)
					r.Get("/histogram", pl.EventsHistogram)
					r.Get("/{id}", pl.GetEvent)
					r.Post("/{id}/retry", pl.RetryEvent)
				})

				// CLI control-plane lookups + WS tunnel registration.
				r.Get("/cli/sources", cli.ListSources)
				r.Get("/cli/connect", cli.Connect)
			})
		})
	})
}
