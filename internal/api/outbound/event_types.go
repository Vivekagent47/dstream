package outbound

import (
	"bytes"
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

// isJSONNull reports whether raw is the JSON literal null — what the client
// sends to clear an optional field. It is not a schema document, so it must be
// normalized away before compileSchema (which would reject "null") and stored
// as SQL NULL, not the literal jsonb null.
func isJSONNull(raw []byte) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func eventTypeView(e store.EventType) map[string]any {
	var schema any
	if len(e.Schema) > 0 {
		schema = json.RawMessage(e.Schema)
	}
	return map[string]any{
		"id":          store.GoUUID(e.ID).String(),
		"org_id":      store.GoUUID(e.OrgID).String(),
		"name":        e.Name,
		"description": e.Description,
		"schema":      schema,
		"archived":    e.Archived,
		"created_at":  e.CreatedAt.Time,
		"updated_at":  e.UpdatedAt.Time,
	}
}

type createEventTypeReq struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema,omitempty"`
}

func (d Handlers) CreateEventType(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	var body createEventTypeReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid json")
		return
	}
	if body.Name == "" {
		httpx.Err(w, http.StatusBadRequest, "name required")
		return
	}
	var schema []byte
	if len(body.Schema) > 0 && !isJSONNull(body.Schema) {
		schema = []byte(body.Schema)
	}
	if err := compileSchema(schema); err != nil {
		httpx.Err(w, http.StatusBadRequest, "schema is not a valid JSON Schema: "+err.Error())
		return
	}
	row, err := d.Queries.CreateEventType(r.Context(), store.CreateEventTypeParams{
		OrgID: store.UUID(p.OrgID), Name: body.Name, Description: body.Description, Schema: schema,
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpx.Err(w, http.StatusConflict, "event type already exists")
			return
		}
		d.Log.Error("create event type", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "create event type")
		return
	}
	audit.Log(r.Context(), d.Queries, d.Log, audit.Entry{
		Action: "event_type.create", TargetType: "event_type",
		TargetID: audit.PtrUUID(store.GoUUID(row.ID)), Metadata: map[string]any{"name": row.Name},
	})
	httpx.WriteJSON(w, http.StatusCreated, eventTypeView(row))
}

func (d Handlers) ListEventTypes(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	rows, err := d.Queries.ListEventTypesByOrg(r.Context(), store.ListEventTypesByOrgParams{
		OrgID: store.UUID(p.OrgID), Limit: listLimit,
	})
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "list event types")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, e := range rows {
		out = append(out, eventTypeView(e))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (d Handlers) GetEventType(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	row, err := d.Queries.GetEventTypeForOrg(r.Context(), store.GetEventTypeForOrgParams{
		OrgID: store.UUID(p.OrgID), Name: chi.URLParam(r, "name"),
	})
	if err != nil {
		httpx.Err(w, http.StatusNotFound, "not found")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, eventTypeView(row))
}

type patchEventTypeReq struct {
	Description *string         `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema,omitempty"`
	Archived    *bool           `json:"archived,omitempty"`
}

func (d Handlers) PatchEventType(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	var body patchEventTypeReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid json")
		return
	}
	// schema: omitted → leave unchanged; JSON null → clear (SQL NULL);
	// an object → validate and set. (json.RawMessage distinguishes omitted
	// (nil) from an explicit null, so the client can clear a stored schema.)
	var schema []byte
	setSchema := len(body.Schema) > 0
	if setSchema && !isJSONNull(body.Schema) {
		schema = []byte(body.Schema)
		if err := compileSchema(schema); err != nil {
			httpx.Err(w, http.StatusBadRequest, "schema is not a valid JSON Schema: "+err.Error())
			return
		}
	}
	row, err := d.Queries.UpdateEventType(r.Context(), store.UpdateEventTypeParams{
		OrgID: store.UUID(p.OrgID), Name: chi.URLParam(r, "name"),
		Description: body.Description, SetSchema: setSchema, Schema: schema, Archived: body.Archived,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Err(w, http.StatusNotFound, "not found")
			return
		}
		httpx.Err(w, http.StatusInternalServerError, "update event type")
		return
	}
	audit.Log(r.Context(), d.Queries, d.Log, audit.Entry{
		Action: "event_type.update", TargetType: "event_type",
		TargetID: audit.PtrUUID(store.GoUUID(row.ID)),
		Metadata: map[string]any{"name": row.Name, "archived": row.Archived},
	})
	httpx.WriteJSON(w, http.StatusOK, eventTypeView(row))
}

func (d Handlers) DeleteEventType(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	name := chi.URLParam(r, "name")
	if _, err := d.Queries.DeleteEventTypeForOrg(r.Context(), store.DeleteEventTypeForOrgParams{
		OrgID: store.UUID(p.OrgID), Name: name,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Err(w, http.StatusNotFound, "not found")
			return
		}
		httpx.Err(w, http.StatusInternalServerError, "delete event type")
		return
	}
	audit.Log(r.Context(), d.Queries, d.Log, audit.Entry{
		Action: "event_type.delete", TargetType: "event_type", Metadata: map[string]any{"name": name},
	})
	w.WriteHeader(http.StatusNoContent)
}
