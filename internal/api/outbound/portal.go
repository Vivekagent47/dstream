package outbound

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/Vivekagent47/dstream/internal/api/httpx"
	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/store"
)

// CreatePortalAccess mints a short-lived, app-scoped portal link for the
// caller's application. Admin-authed (this route sits under Authenticate +
// RequireOrg). The token embeds the app's current portal_epoch.
func (d Handlers) CreatePortalAccess(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	app, ok := d.appForOrg(w, r, p.OrgID) // reads {app_id}, 404 if not this org
	if !ok {
		return
	}
	token, exp := d.Portal.Mint(store.GoUUID(app.ID), p.OrgID, app.PortalEpoch)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"url":        d.AppBaseURL + "/portal#token=" + token,
		"token":      token,
		"expires_at": exp,
	})
}

// RevokePortalAccess bumps the application's portal_epoch, invalidating every
// outstanding portal link for it.
func (d Handlers) RevokePortalAccess(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	app, ok := d.appForOrg(w, r, p.OrgID)
	if !ok {
		return
	}
	if err := d.Queries.BumpApplicationPortalEpoch(r.Context(), store.BumpApplicationPortalEpochParams{
		ID: app.ID, OrgID: store.UUID(p.OrgID),
	}); err != nil {
		httpx.Err(w, http.StatusInternalServerError, "revoke failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
