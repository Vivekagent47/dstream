package api

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/queue"
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
	Pool   *pgxpool.Pool
	Redis  *redis.Client
	Queue  *queue.Client
	Signer *auth.SessionSigner
	// PublicBaseURL is the externally-visible scheme://host[:port] for the
	// service. Used to render invite links (and similar) into emails / logs.
	PublicBaseURL string
	// DevMode gates dev-only conveniences (notably: logging plaintext
	// magic-link and invite tokens to the server log). Production must
	// leave this off, since the server log is then an audit-bypass vector.
	DevMode bool
}

// Mount wires the full /api router onto the parent. `extra` middleware is
// applied to every /api route (use it for cross-cutting concerns like CSRF).
func Mount(parent chi.Router, d Deps, extra ...func(http.Handler) http.Handler) {
	parent.Route("/api", func(r chi.Router) {
		for _, m := range extra {
			r.Use(m)
		}

		// Unauthenticated.
		r.Route("/auth", func(r chi.Router) {
			r.Post("/magic-link/request", d.requestMagicLink)
			r.Get("/magic-link/verify", d.verifyMagicLink)
			r.Post("/logout", d.logout)
		})

		// Invite peek/accept: peek is fully public (so a logged-out user
		// can see what they're being invited to before authenticating);
		// accept is authenticated separately inside the handler so it can
		// handle both session and post-magic-link flows.
		r.Get("/invites/{token}", d.peekInvite)
		r.Post("/invites/{token}/accept", d.acceptInvite)

		// Authenticated surface. Authenticate accepts either a session
		// cookie or an API key and attaches a Principal to ctx.
		r.Group(func(r chi.Router) {
			r.Use(auth.Authenticate(d.Queries, d.Signer))

			// User identity + org membership management. These do NOT
			// require an active org — a logged-in user with zero orgs
			// must still be able to list/create/select.
			r.Get("/me", d.me)
			r.Get("/orgs", d.listMyOrgs)
			r.Post("/orgs", d.createOrg)
			r.Post("/orgs/select", d.selectOrg)

			// Org-scoped admin operations (members, invites, api-keys,
			// audit, settings). RequireOrg is applied per-handler inside
			// the route group via the {org_id} path itself; the handler
			// verifies the caller's membership in that org.
			r.Route("/orgs/{org_id}", func(r chi.Router) {
				r.Get("/members", d.listMembers)
				r.Patch("/members/{user_id}", d.patchMember)
				r.Delete("/members/{user_id}", d.removeMember)
				r.Patch("/", d.updateOrg)
				r.Delete("/", d.deleteOrg)
				r.Post("/transfer", d.transferOwnership)

				r.Get("/invites", d.listInvites)
				r.Post("/invites", d.createInvite)
				r.Delete("/invites/{id}", d.deleteInvite)

				r.Get("/api-keys", d.listAPIKeys)
				r.Post("/api-keys", d.createAPIKey)
				r.Delete("/api-keys/{id}", d.revokeAPIKey)

				r.Get("/audit", d.listAuditForOrg)
			})

			// Tenant-scoped traffic plane. RequireOrg gates these: an
			// API-key principal always has an OrgID set; a session
			// principal must have selected an active org.
			r.Group(func(r chi.Router) {
				r.Use(auth.RequireOrg(d.Queries))

				r.Get("/audit", d.listAudit)

				r.Route("/sources", func(r chi.Router) {
					r.Get("/", d.listSources)
					r.Post("/", d.createSource)
					r.Get("/{id}", d.getSource)
					r.Delete("/{id}", d.deleteSource)
				})
				r.Route("/destinations", func(r chi.Router) {
					r.Get("/", d.listDestinations)
					r.Post("/", d.createDestination)
					r.Get("/{id}", d.getDestination)
					r.Patch("/{id}", d.patchDestination)
					r.Delete("/{id}", d.deleteDestination)
				})
				r.Route("/connections", func(r chi.Router) {
					r.Get("/", d.listConnections)
					r.Post("/", d.createConnection)
					r.Get("/{id}", d.getConnection)
					r.Patch("/{id}", d.patchConnection)
					r.Delete("/{id}", d.deleteConnection)
				})
				r.Route("/events", func(r chi.Router) {
					r.Get("/", d.listEvents)
					r.Get("/{id}", d.getEvent)
					r.Post("/{id}/retry", d.retryEvent)
				})

				// CLI control-plane lookups + WS tunnel registration.
				r.Get("/cli/sources", d.cliListSources)
				r.Get("/cli/connect", d.cliConnect)
			})
		})
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func httpErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
