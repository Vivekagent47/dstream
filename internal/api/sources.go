package api

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Vivekagent47/dstream/internal/audit"
	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/store"
)

type createSourceReq struct {
	Name          string          `json:"name"`
	Type          string          `json:"type"`
	Description   string          `json:"description"`
	SigningConfig json.RawMessage `json:"signing_config,omitempty"`
}

func (d Deps) createSource(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpErr(w, http.StatusUnauthorized, "active org required")
		return
	}
	var body createSourceReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if body.Name == "" {
		httpErr(w, http.StatusBadRequest, "name required")
		return
	}
	if body.Type == "" {
		body.Type = "generic"
	}
	token, err := generateIngestToken()
	if err != nil {
		httpErr(w, http.StatusInternalServerError, "token gen")
		return
	}
	signing := body.SigningConfig
	if len(signing) == 0 {
		signing = json.RawMessage(`{}`)
	}
	row, err := d.Queries.CreateSource(r.Context(), store.CreateSourceParams{
		OrgID:         store.UUID(p.OrgID),
		Name:          body.Name,
		Type:          body.Type,
		Description:   body.Description,
		IngestToken:   token,
		SigningConfig: signing,
	})
	if err != nil {
		d.Log.Error("create source", "err", err)
		httpErr(w, http.StatusInternalServerError, "create source")
		return
	}
	audit.Log(r.Context(), d.Queries, d.Log, audit.Entry{
		Action:     "source.create",
		TargetType: "source",
		TargetID:   audit.PtrUUID(store.GoUUID(row.ID)),
		Metadata: map[string]any{
			"name": row.Name,
			"type": row.Type,
		},
	})
	writeJSON(w, http.StatusCreated, sourceView(row))
}

func (d Deps) listSources(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpErr(w, http.StatusUnauthorized, "active org required")
		return
	}
	rows, err := d.Queries.ListSourcesByOrg(r.Context(), store.UUID(p.OrgID))
	if err != nil {
		httpErr(w, http.StatusInternalServerError, "list sources")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, s := range rows {
		out = append(out, sourceView(s))
	}
	writeJSON(w, http.StatusOK, out)
}

func (d Deps) getSource(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpErr(w, http.StatusUnauthorized, "active org required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpErr(w, http.StatusBadRequest, "invalid id")
		return
	}
	row, err := d.Queries.GetSourceForOrg(r.Context(), store.GetSourceForOrgParams{
		ID:    store.UUID(id),
		OrgID: store.UUID(p.OrgID),
	})
	if err != nil {
		httpErr(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, sourceView(row))
}

type patchSourceReq struct {
	Name           *string   `json:"name,omitempty"`
	Description    *string   `json:"description,omitempty"`
	AllowedMethods *[]string `json:"allowed_methods,omitempty"`
	Enabled        *bool     `json:"enabled,omitempty"`
}

// patchSource serves PATCH /api/sources/{id}: partial update of name,
// description, allowed_methods, or enabled. On success it evicts the ingest
// source cache so the change is effective immediately (not after the 60s
// SourceCacheTTL).
func (d Deps) patchSource(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpErr(w, http.StatusUnauthorized, "active org required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpErr(w, http.StatusBadRequest, "invalid id")
		return
	}
	var body patchSourceReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if body.Name == nil && body.Description == nil && body.AllowedMethods == nil && body.Enabled == nil {
		httpErr(w, http.StatusBadRequest, "nothing to update")
		return
	}

	var methods []string
	if body.AllowedMethods != nil {
		methods, err = validateMethods(*body.AllowedMethods)
		if err != nil {
			httpErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	row, err := d.Queries.UpdateSource(r.Context(), store.UpdateSourceParams{
		ID:             store.UUID(id),
		OrgID:          store.UUID(p.OrgID),
		Name:           body.Name,
		Description:    body.Description,
		AllowedMethods: methods, // nil when not provided → COALESCE keeps existing
		Enabled:        body.Enabled,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpErr(w, http.StatusNotFound, "not found")
			return
		}
		d.Log.Error("update source", "err", err)
		httpErr(w, http.StatusInternalServerError, "update source")
		return
	}

	if d.EvictSourceCache != nil {
		d.EvictSourceCache(row.IngestToken)
	}

	audit.Log(r.Context(), d.Queries, d.Log, audit.Entry{
		Action:     "source.update",
		TargetType: "source",
		TargetID:   audit.PtrUUID(id),
		Metadata: map[string]any{
			"name":            row.Name,
			"description":     row.Description,
			"allowed_methods": row.AllowedMethods,
			"enabled":         row.Enabled,
		},
	})
	writeJSON(w, http.StatusOK, sourceView(row))
}

func (d Deps) deleteSource(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpErr(w, http.StatusUnauthorized, "active org required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpErr(w, http.StatusBadRequest, "invalid id")
		return
	}
	token, err := d.Queries.DeleteSourceForOrg(r.Context(), store.DeleteSourceForOrgParams{
		ID:    store.UUID(id),
		OrgID: store.UUID(p.OrgID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpErr(w, http.StatusNotFound, "not found")
			return
		}
		d.Log.Error("delete source", "err", err)
		httpErr(w, http.StatusInternalServerError, "delete source")
		return
	}
	if d.EvictSourceCache != nil {
		d.EvictSourceCache(token)
	}
	audit.Log(r.Context(), d.Queries, d.Log, audit.Entry{
		Action:     "source.delete",
		TargetType: "source",
		TargetID:   audit.PtrUUID(id),
		Metadata:   map[string]any{},
	})
	w.WriteHeader(http.StatusNoContent)
}

func sourceView(s store.Source) map[string]any {
	return map[string]any{
		"id":              store.GoUUID(s.ID).String(),
		"org_id":          store.GoUUID(s.OrgID).String(),
		"name":            s.Name,
		"type":            s.Type,
		"description":     s.Description,
		"allowed_methods": s.AllowedMethods,
		"enabled":         s.Enabled,
		"ingest_token":    s.IngestToken,
		"signing_config":  json.RawMessage(s.SigningConfig),
		"created_at":      s.CreatedAt.Time,
		"updated_at":      s.UpdatedAt.Time,
	}
}

func generateIngestToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
