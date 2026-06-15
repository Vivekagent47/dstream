package api

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/streamingo/dstream/internal/auth"
	"github.com/streamingo/dstream/internal/store"
)

type createSourceReq struct {
	Name          string          `json:"name"`
	Type          string          `json:"type"`
	SigningConfig json.RawMessage `json:"signing_config,omitempty"`
}

func (d Deps) createSource(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.ProjectID == uuid.Nil {
		httpErr(w, http.StatusUnauthorized, "api key required")
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
		ProjectID:     store.UUID(p.ProjectID),
		Name:          body.Name,
		Type:          body.Type,
		IngestToken:   token,
		SigningConfig: signing,
	})
	if err != nil {
		httpErr(w, http.StatusInternalServerError, "create source: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, sourceView(row))
}

func (d Deps) listSources(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.ProjectID == uuid.Nil {
		httpErr(w, http.StatusUnauthorized, "api key required")
		return
	}
	rows, err := d.Queries.ListSourcesByProject(r.Context(), store.UUID(p.ProjectID))
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
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpErr(w, http.StatusBadRequest, "invalid id")
		return
	}
	row, err := d.Queries.GetSourceByID(r.Context(), store.UUID(id))
	if err != nil {
		httpErr(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, sourceView(row))
}

func sourceView(s store.Source) map[string]any {
	return map[string]any{
		"id":             store.GoUUID(s.ID).String(),
		"project_id":     store.GoUUID(s.ProjectID).String(),
		"name":           s.Name,
		"type":           s.Type,
		"ingest_token":   s.IngestToken,
		"signing_config": json.RawMessage(s.SigningConfig),
		"created_at":     s.CreatedAt.Time,
		"updated_at":     s.UpdatedAt.Time,
	}
}

func generateIngestToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
