package outbound

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/Vivekagent47/dstream/internal/api/httpx"
	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/opevents"
	"github.com/Vivekagent47/dstream/internal/store"
)

// GetOperationalApp serves GET /api/operational-app — returns (get-or-create)
// the caller org's reserved operational-webhooks application.
func (d Handlers) GetOperationalApp(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	appID, err := opevents.SeedOperationalApp(r.Context(), d.Queries, p.OrgID)
	if err != nil {
		d.Log.Error("operational app", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "operational app")
		return
	}
	app, err := d.Queries.GetApplicationForOrg(r.Context(), store.GetApplicationForOrgParams{
		ID: store.UUID(appID), OrgID: store.UUID(p.OrgID),
	})
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "load operational app")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, applicationView(app))
}
