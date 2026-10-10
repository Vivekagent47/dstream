package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	apicli "github.com/Vivekagent47/dstream/internal/api/cli"
	"github.com/Vivekagent47/dstream/internal/api/identity"
	"github.com/Vivekagent47/dstream/internal/api/outbound"
	"github.com/Vivekagent47/dstream/internal/api/pipeline"
	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/config"
	"github.com/Vivekagent47/dstream/internal/deliver"
	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/ingest"
	"github.com/Vivekagent47/dstream/internal/store"
	"github.com/Vivekagent47/dstream/internal/usage"
)

// maxAPIBodyBytes is the hard ceiling on any /api request body. Above the largest
// legitimate body (bookmark import, 12 MiB); routes needing a tighter cap set
// their own MaxBytesReader, which takes effect first.
const maxAPIBodyBytes = 16 << 20

// maxBodyBytes wraps every request body in a MaxBytesReader so a single request
// can't buffer unbounded memory (OOM). A nil body (GET) is left alone.
func maxBodyBytes(n int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, n)
			}
			next.ServeHTTP(w, r)
		})
	}
}

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
	// PublicBaseURL is the externally-visible scheme://host[:port] for the API
	// (ingest URLs, CLI WebSocket Origin pin).
	PublicBaseURL string
	// AppBaseURL is the frontend/SPA origin used to build user-facing links in
	// emails (magic-link verify, invite). May differ from PublicBaseURL.
	AppBaseURL string
	// EvictSourceCache drops a source from the ingest in-process cache so
	// enable/disable and allowed-methods edits take effect immediately.
	// nil-safe: nil means no cache to evict.
	EvictSourceCache func(token string)
	// SelfHosts are dstream's own hostnames; a webhook endpoint/destination
	// pointing at one is rejected at create/patch (loop guard).
	SelfHosts []string
	// AllowPrivateDestinations lets the bookmark replay-to client reach
	// loopback/private targets (self-host only; default false).
	AllowPrivateDestinations bool
	// SecretGrace is how long a rotated endpoint's previous signing secret
	// stays valid after a rotate.
	SecretGrace time.Duration
	// Portal signs App Portal tokens for the mint/revoke endpoints.
	Portal *auth.PortalSigner
	// Authenticator is the OIDC seam for the SSO routes. nil when SSO is not
	// configured, in which case those routes 404.
	Authenticator auth.Authenticator
	// OIDC carries the SSO provisioning defaults and the enforce flag.
	OIDC config.OIDCConfig
	// Quota enforces per-org usage limits on publish. Shared with the ingest
	// handler so both paths read one cached snapshot of the limits. nil = no
	// enforcement.
	Quota *usage.Gate
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
		Queue:         d.Queue,
		Signer:        d.Signer,
		AppBaseURL:    d.AppBaseURL,
		Authenticator: d.Authenticator,
		OIDC:          d.OIDC,

		EvictSourceCache: d.EvictSourceCache,
	}
	pl := pipeline.Handlers{
		Log:              d.Log,
		Queries:          d.Queries,
		Queue:            d.Queue,
		BodyStore:        d.BodyStore,
		EvictSourceCache: d.EvictSourceCache,
		SelfHosts:        d.SelfHosts,
		Replayer:         deliver.NewSafeHTTPClient(30*time.Second, d.AllowPrivateDestinations),
		Pool:             d.Pool,
		PublicBaseURL:    d.PublicBaseURL,
	}
	cli := apicli.Handlers{
		Log:           d.Log,
		Queries:       d.Queries,
		Redis:         d.Redis,
		PublicBaseURL: d.PublicBaseURL,
	}
	ob := outbound.Handlers{
		Log:         d.Log,
		Queries:     d.Queries,
		Pool:        d.Pool,
		Queue:       d.Queue,
		SelfHosts:   d.SelfHosts,
		SecretGrace: d.SecretGrace,
		Portal:      d.Portal,
		AppBaseURL:  d.AppBaseURL,
		Quota:       d.Quota,
	}

	parent.Route("/api", func(r chi.Router) {
		for _, m := range extra {
			r.Use(m)
		}
		// Cap every request body. Without this an unauthenticated route that decodes
		// JSON (e.g. /auth/magic-link/request) buffers whatever the client sends,
		// so a multi-GB POST OOMs the process. 16 MiB clears the largest legitimate
		// body (bookmark import, 12 MiB); routes that need a tighter limit (publish
		// 5 MiB) still wrap their own MaxBytesReader, which wins.
		r.Use(maxBodyBytes(maxAPIBodyBytes))

		// Unauthenticated.
		r.Route("/auth", func(r chi.Router) {
			r.Post("/magic-link/request", id.RequestMagicLink)
			// POST (not GET) so a session-establishing request can't be
			// triggered cross-site via <img>/navigation — the JSON body forces a
			// CORS preflight, blocking login-CSRF / session fixation.
			r.Post("/magic-link/verify", id.VerifyMagicLink)
			r.Post("/logout", id.Logout)

			// Which login methods the login page should render. No secret:
			// a deployment's use of SSO is visible from its login page.
			r.Get("/methods", id.AuthMethods)

			// SSO. These must be GET — an IdP redirects the browser back by
			// navigation, which cannot be a POST — so the login-CSRF /
			// session-fixation problem that made /magic-link/verify POST-only
			// has to be closed a different way.
			//
			// What closes it is the dstream_sso_state cookie: /sso/start sets
			// it alongside the Redis state row, and the callback refuses any
			// state the browser cannot also present in that cookie. The nonce
			// and the single-use (GETDEL) state are NOT sufficient on their
			// own — /sso/start is unauthenticated, so an attacker can mint a
			// valid state and a genuine ID token for his own account, then
			// phish the victim into a top-level GET of the callback. An
			// attacker cannot set a cookie in the victim's browser for this
			// origin, absent control of a sibling subdomain, which is what
			// makes the binding the load-bearing part. Do not remove it.
			//
			// Named residual: that caveat is real here. A Domain=example.com
			// cookie set from any compromised sibling subdomain IS sent to
			// this origin and r.Cookie returns the first match, and
			// dstream_sso_state is not __Host- prefixed (that requires
			// Secure, which is false in local HTTP dev). Unlike
			// dstream_session (HMAC) and dstream_csrf (bound to the session
			// value), this cookie's entire security property is that its
			// value cannot be injected — so it is the one cookie in the repo
			// where a sibling-subdomain write is directly exploitable. Same
			// caveat as internal/middleware/csrf.go's cookie-tossing note.
			// See ssoStateCookieName in identity/sso.go.
			//
			// Both routes stay mounted when SSO is unconfigured and 404 from
			// the handler, so the route table does not change shape with
			// configuration.
			r.Get("/sso/start", id.StartSSO)
			r.Get("/sso/callback", id.CallbackSSO)
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
			r.Patch("/me", id.PatchMe)
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
				// DELETE ⇒ admin, for every route in this group including
				// ones added later. RequireOrg must stay above: it is what
				// resolves a session principal's membership row into
				// Principal.Role (an API key carries its role from
				// Authenticate already).
				r.Use(auth.AdminForDestructive)

				// adminOnly marks the privileged routes whose danger isn't
				// implied by their method: secret material, outbound publish,
				// and portal-access minting.
				adminOnly := auth.RequireRole(auth.RoleAdmin)

				r.Get("/audit", id.ListAudit)

				// Current-period usage + history for the active org. Read-only
				// for every role: quotas are granted by the platform operator
				// at PATCH /admin/orgs/{org_id}/plan, not by the tenant.
				r.Get("/usage", id.GetUsage)
				r.Get("/usage/history", id.GetUsageHistory)

				// Filter/transform dev-time preview (stateless pipeline funcs).
				r.Post("/filter-preview", pipeline.FilterPreview)
				r.Post("/transform-preview", pipeline.TransformPreview)

				r.Route("/sources", func(r chi.Router) {
					r.Get("/", pl.ListSources)
					r.Post("/", pl.CreateSource)
					r.Get("/{id}", pl.GetSource)
					r.Get("/{id}/metrics", pl.SourceMetrics)
					r.Patch("/{id}", pl.PatchSource)
					r.Delete("/{id}", pl.DeleteSource)
				})
				r.Route("/destinations", func(r chi.Router) {
					r.Get("/", pl.ListDestinations)
					r.Post("/", pl.CreateDestination)
					r.Get("/{id}", pl.GetDestination)
					r.Get("/{id}/metrics", pl.DestinationMetrics)
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
				r.Route("/bookmarks", func(r chi.Router) {
					r.Get("/", pl.ListBookmarks)
					r.Post("/", pl.CreateBookmark)
					r.Post("/import", pl.ImportBookmark)
					r.Get("/{id}", pl.GetBookmark)
					r.Patch("/{id}", pl.PatchBookmark)
					r.Delete("/{id}", pl.DeleteBookmark)
					r.Post("/{id}/replay", pl.ReplayBookmark)
					r.Post("/{id}/replay-to", pl.ReplayBookmarkTo)
					r.Get("/{id}/export", pl.ExportBookmark)
				})
				r.Route("/capture-rules", func(r chi.Router) {
					r.Get("/", pl.ListCaptureRules)
					r.Post("/", pl.CreateCaptureRule)
					r.Get("/{id}", pl.GetCaptureRule)
					r.Patch("/{id}", pl.PatchCaptureRule)
					r.Delete("/{id}", pl.DeleteCaptureRule)
				})
				r.Route("/scenarios", func(r chi.Router) {
					r.Get("/", pl.ListScenarios)
					r.Post("/", pl.CreateScenario)
					r.Get("/{id}", pl.GetScenario)
					r.Patch("/{id}", pl.PatchScenario)
					r.Delete("/{id}", pl.DeleteScenario)
					r.Post("/{id}/replay-to", pl.ReplayScenarioTo)
				})

				r.Get("/operational-app", ob.GetOperationalApp)

				r.Route("/applications", func(r chi.Router) {
					r.Get("/", ob.ListApplications)
					r.Post("/", ob.CreateApplication)
					r.Get("/{app_id}", ob.GetApplication)
					r.Patch("/{app_id}", ob.PatchApplication)
					r.Delete("/{app_id}", ob.DeleteApplication)
					r.With(adminOnly).Post("/{app_id}/portal-access", ob.CreatePortalAccess)
					r.With(adminOnly).Post("/{app_id}/portal-access/revoke", ob.RevokePortalAccess)

					r.Route("/{app_id}/endpoints", func(r chi.Router) {
						r.Get("/", ob.ListEndpoints)
						r.Post("/", ob.CreateEndpoint)
						r.Get("/{id}", ob.GetEndpoint)
						r.Patch("/{id}", ob.PatchEndpoint)
						r.Delete("/{id}", ob.DeleteEndpoint)
						r.With(adminOnly).Get("/{id}/secret", ob.GetEndpointSecret)
						r.With(adminOnly).Post("/{id}/rotate-secret", ob.RotateEndpointSecret)
						r.Post("/{id}/recover", ob.RecoverEndpoint)
						r.Post("/{id}/test", ob.TestEndpoint)
						r.Get("/{id}/attempts", ob.ListEndpointAttempts)
					})

					r.Route("/{app_id}/messages", func(r chi.Router) {
						r.Get("/", ob.ListMessages)
						r.With(adminOnly).Post("/", ob.CreateMessage)
						r.Get("/{id}", ob.GetMessage)
						r.Get("/{id}/attempts", ob.ListMessageAttempts)
						r.Get("/{id}/deliveries", ob.ListMessageDeliveries)
						r.Post("/{id}/endpoints/{endpoint_id}/replay", ob.ReplayDelivery)
					})
				})

				r.Route("/event-types", func(r chi.Router) {
					r.Get("/", ob.ListEventTypes)
					r.Post("/", ob.CreateEventType)
					r.Get("/{name}", ob.GetEventType)
					r.Patch("/{name}", ob.PatchEventType)
					r.Delete("/{name}", ob.DeleteEventType)
				})

				// CLI control-plane lookups + WS tunnel registration.
				r.Get("/cli/sources", cli.ListSources)
				r.Get("/cli/connect", cli.Connect)
			})
		})

		// App Portal surface. Authed ONLY by a Bearer portal token
		// (RequirePortal); no session, no CSRF, no org gate. The token pins
		// the app_id, injected into the route context, so these reuse the
		// outbound handlers verbatim scoped to one application. Only the
		// routes below exist here — publish + app/event-type CRUD are absent.
		// NOTE: portal route paths MUST NOT contain an {app_id} segment — the
		// app_id is injected from the token; a path segment would let chi's
		// last-wins URLParam override the token's app. Keep paths app_id-free.
		r.Route("/portal", func(r chi.Router) {
			r.Use(auth.RequirePortal(d.Queries, d.Portal))
			r.Get("/app", ob.GetApplication)
			// Same stateless preview funcs as the session surface; no app
			// scoping needed (they only compile/eval the request body).
			r.Post("/filter-preview", pipeline.FilterPreview)
			r.Post("/transform-preview", pipeline.TransformPreview)
			r.Route("/endpoints", func(r chi.Router) {
				r.Get("/", ob.ListEndpoints)
				r.Post("/", ob.CreateEndpoint)
				r.Get("/{id}", ob.GetEndpoint)
				r.Patch("/{id}", ob.PatchEndpoint)
				r.Delete("/{id}", ob.DeleteEndpoint)
				r.Get("/{id}/secret", ob.GetEndpointSecret)
				r.Post("/{id}/rotate-secret", ob.RotateEndpointSecret)
				r.Post("/{id}/recover", ob.RecoverEndpoint)
				r.Post("/{id}/test", ob.TestEndpoint)
				r.Get("/{id}/attempts", ob.ListEndpointAttempts)
			})
			r.Route("/messages", func(r chi.Router) {
				r.Get("/", ob.ListMessages)
				r.Get("/{id}", ob.GetMessage)
				r.Get("/{id}/attempts", ob.ListMessageAttempts)
				r.Get("/{id}/deliveries", ob.ListMessageDeliveries)
				r.Post("/{id}/endpoints/{endpoint_id}/replay", ob.ReplayDelivery)
			})
			r.Get("/event-types", ob.ListEventTypes)
		})
	})
}
