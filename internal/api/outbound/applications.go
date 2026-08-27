package outbound

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Vivekagent47/dstream/internal/api/httpx"
	"github.com/Vivekagent47/dstream/internal/audit"
	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/store"
)

type createAppReq struct {
	UID      *string         `json:"uid,omitempty"`
	Name     string          `json:"name"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
}

func (d Handlers) CreateApplication(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	var body createAppReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid json")
		return
	}
	if body.Name == "" {
		httpx.Err(w, http.StatusBadRequest, "name required")
		return
	}
	meta := []byte(body.Metadata)
	if len(meta) == 0 {
		meta = []byte(`{}`)
	}
	row, err := d.Queries.CreateApplication(r.Context(), store.CreateApplicationParams{
		OrgID:    store.UUID(p.OrgID),
		Uid:      body.UID,
		Name:     body.Name,
		Metadata: meta,
	})
	if err != nil {
		d.Log.Error("create application", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "create application")
		return
	}
	audit.Log(r.Context(), d.Queries, d.Log, audit.Entry{
		Action: "application.create", TargetType: "application",
		TargetID: audit.PtrUUID(store.GoUUID(row.ID)),
		Metadata: map[string]any{"name": row.Name},
	})
	httpx.WriteJSON(w, http.StatusCreated, applicationView(row))
}

func (d Handlers) ListApplications(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	rows, err := d.Queries.ListApplicationsByOrg(r.Context(), store.ListApplicationsByOrgParams{
		OrgID: store.UUID(p.OrgID), Limit: listLimit,
	})
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "list applications")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, a := range rows {
		out = append(out, applicationView(a))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// appForOrg resolves {app_id} and asserts org ownership. Writes the error
// response and returns ok=false on any failure.
func (d Handlers) appForOrg(w http.ResponseWriter, r *http.Request, orgID uuid.UUID) (store.Application, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "app_id"))
	if err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid app id")
		return store.Application{}, false
	}
	row, err := d.Queries.GetApplicationForOrg(r.Context(), store.GetApplicationForOrgParams{
		ID: store.UUID(id), OrgID: store.UUID(orgID),
	})
	if err != nil {
		httpx.Err(w, http.StatusNotFound, "application not found")
		return store.Application{}, false
	}
	return row, true
}

func (d Handlers) GetApplication(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	row, ok := d.appForOrg(w, r, p.OrgID)
	if !ok {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, applicationView(row))
}

type patchAppReq struct {
	Name     *string         `json:"name,omitempty"`
	UID      *string         `json:"uid,omitempty"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
}

func (d Handlers) PatchApplication(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "app_id"))
	if err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid app id")
		return
	}
	var body patchAppReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid json")
		return
	}
	var meta []byte
	if len(body.Metadata) > 0 {
		meta = []byte(body.Metadata)
	}
	row, err := d.Queries.UpdateApplication(r.Context(), store.UpdateApplicationParams{
		ID: store.UUID(id), OrgID: store.UUID(p.OrgID),
		Name: body.Name, Uid: body.UID, Metadata: meta,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Err(w, http.StatusNotFound, "not found")
			return
		}
		d.Log.Error("update application", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "update application")
		return
	}
	audit.Log(r.Context(), d.Queries, d.Log, audit.Entry{
		Action: "application.update", TargetType: "application",
		TargetID: audit.PtrUUID(id), Metadata: map[string]any{"name": row.Name},
	})
	httpx.WriteJSON(w, http.StatusOK, applicationView(row))
}

func (d Handlers) DeleteApplication(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "app_id"))
	if err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid app id")
		return
	}
	if _, err := d.Queries.DeleteApplicationForOrg(r.Context(), store.DeleteApplicationForOrgParams{
		ID: store.UUID(id), OrgID: store.UUID(p.OrgID),
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Err(w, http.StatusNotFound, "not found")
			return
		}
		d.Log.Error("delete application", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "delete application")
		return
	}
	audit.Log(r.Context(), d.Queries, d.Log, audit.Entry{
		Action: "application.delete", TargetType: "application",
		TargetID: audit.PtrUUID(id), Metadata: map[string]any{},
	})
	w.WriteHeader(http.StatusNoContent)
}
