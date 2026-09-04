package auth

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/Vivekagent47/dstream/internal/store"
)

// RequirePortal authenticates an App Portal request by its Bearer portal
// token. On success it attaches a Principal{Source: SourcePortal, OrgID} and
// injects the token's app_id into the chi route context, so the outbound
// handlers (which read chi.URLParam("app_id") + principal.OrgID) run unchanged
// and are scoped to exactly the token's application. Any failure → 401.
func RequirePortal(q *store.Queries, s *PortalSigner) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := ExtractAPIKey(r.Header.Get("Authorization")) // strips "Bearer "
			if raw == "" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			appID, orgID, epoch, err := s.Parse(raw)
			if err != nil {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			app, err := q.GetApplicationForOrg(r.Context(), store.GetApplicationForOrgParams{
				ID: store.UUID(appID), OrgID: store.UUID(orgID),
			})
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				http.Error(w, "portal lookup failed", http.StatusInternalServerError)
				return
			}
			if app.PortalEpoch != epoch {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			// Inject app_id so downstream handlers' chi.URLParam("app_id") resolves.
			if rctx := chi.RouteContext(r.Context()); rctx != nil {
				rctx.URLParams.Add("app_id", appID.String())
			}
			p := Principal{Source: SourcePortal, OrgID: orgID}
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
		})
	}
}
