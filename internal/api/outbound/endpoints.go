package outbound

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Vivekagent47/dstream/internal/api/httpx"
	"github.com/Vivekagent47/dstream/internal/audit"
	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/deliver"
	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/store"
	"github.com/Vivekagent47/dstream/internal/webhook"
)

func endpointView(e store.Endpoint) map[string]any {
	return map[string]any{
		"id":                 store.GoUUID(e.ID).String(),
		"app_id":             store.GoUUID(e.AppID).String(),
		"org_id":             store.GoUUID(e.OrgID).String(),
		"uid":                httpx.DerefString(e.Uid),
		"url":                e.Url,
		"description":        e.Description,
		"filter_event_types": e.FilterEventTypes,
		"disabled":           e.Disabled,
		"created_at":         e.CreatedAt.Time,
		"updated_at":         e.UpdatedAt.Time,
	}
}

// endpointCreateView is the only view that includes the secret (shown once).
func endpointCreateView(e store.Endpoint) map[string]any {
	v := endpointView(e)
	v["secret"] = e.Secret
	return v
}

type createEndpointReq struct {
	UID              *string  `json:"uid,omitempty"`
	URL              string   `json:"url"`
	Description      string   `json:"description"`
	FilterEventTypes []string `json:"filter_event_types,omitempty"`
}

func (d Handlers) CreateEndpoint(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	app, ok := d.appForOrg(w, r, p.OrgID)
	if !ok {
		return
	}
	var body createEndpointReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid json")
		return
	}
	if err := deliver.ValidateDestinationURL(body.URL); err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid url: "+err.Error())
		return
	}
	if deliver.IsSelfHost(body.URL, d.SelfHosts) {
		httpx.Err(w, http.StatusBadRequest, "url must not point at dstream itself (loop guard)")
		return
	}
	secret, err := webhook.GenerateSecret()
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "secret gen")
		return
	}
	row, err := d.Queries.CreateEndpoint(r.Context(), store.CreateEndpointParams{
		AppID:            app.ID,
		OrgID:            store.UUID(p.OrgID),
		Uid:              body.UID,
		Url:              body.URL,
		Description:      body.Description,
		Secret:           secret,
		FilterEventTypes: body.FilterEventTypes,
	})
	if err != nil {
		d.Log.Error("create endpoint", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "create endpoint")
		return
	}
	audit.Log(r.Context(), d.Queries, d.Log, audit.Entry{
		Action: "endpoint.create", TargetType: "endpoint",
		TargetID: audit.PtrUUID(store.GoUUID(row.ID)),
		Metadata: map[string]any{"app_id": store.GoUUID(app.ID).String(), "url": row.Url},
	})
	httpx.WriteJSON(w, http.StatusCreated, endpointCreateView(row))
}

func (d Handlers) ListEndpoints(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	app, ok := d.appForOrg(w, r, p.OrgID)
	if !ok {
		return
	}
	rows, err := d.Queries.ListEndpointsByApp(r.Context(), store.ListEndpointsByAppParams{
		AppID: app.ID, Limit: listLimit,
	})
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "list endpoints")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, e := range rows {
		out = append(out, endpointView(e))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// endpointForApp resolves {id} within the caller's app. Writes error + returns
// ok=false on failure.
func (d Handlers) endpointForApp(w http.ResponseWriter, r *http.Request, appID uuid.UUID) (store.Endpoint, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid endpoint id")
		return store.Endpoint{}, false
	}
	row, err := d.Queries.GetEndpointForApp(r.Context(), store.GetEndpointForAppParams{
		ID: store.UUID(id), AppID: store.UUID(appID),
	})
	if err != nil {
		httpx.Err(w, http.StatusNotFound, "endpoint not found")
		return store.Endpoint{}, false
	}
	return row, true
}

// endpointForAppID resolves an explicit endpoint id within the caller's app
// (unlike endpointForApp, which reads {id} from the URL). Writes error +
// returns ok=false on failure.
func (d Handlers) endpointForAppID(w http.ResponseWriter, r *http.Request, appID uuid.UUID, epID uuid.UUID) (store.Endpoint, bool) {
	row, err := d.Queries.GetEndpointForApp(r.Context(), store.GetEndpointForAppParams{
		ID: store.UUID(epID), AppID: store.UUID(appID),
	})
	if err != nil {
		httpx.Err(w, http.StatusNotFound, "endpoint not found")
		return store.Endpoint{}, false
	}
	return row, true
}

func (d Handlers) GetEndpoint(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	app, ok := d.appForOrg(w, r, p.OrgID)
	if !ok {
		return
	}
	row, ok := d.endpointForApp(w, r, store.GoUUID(app.ID))
	if !ok {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, endpointView(row))
}

func (d Handlers) GetEndpointSecret(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	app, ok := d.appForOrg(w, r, p.OrgID)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid endpoint id")
		return
	}
	secret, err := d.Queries.GetEndpointSecret(r.Context(), store.GetEndpointSecretParams{
		ID: store.UUID(id), AppID: app.ID,
	})
	if err != nil {
		httpx.Err(w, http.StatusNotFound, "endpoint not found")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"secret": secret})
}

type rotateSecretReq struct {
	Secret *string `json:"secret,omitempty"` // optional bring-your-own
}

func (d Handlers) RotateEndpointSecret(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	app, ok := d.appForOrg(w, r, p.OrgID)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid endpoint id")
		return
	}
	var body rotateSecretReq
	if r.ContentLength != 0 {
		_ = json.NewDecoder(r.Body).Decode(&body) // body optional
	}
	newSecret := ""
	if body.Secret != nil && *body.Secret != "" {
		if !webhook.ValidSecret(*body.Secret) {
			httpx.Err(w, http.StatusBadRequest, "secret is not a valid signing key")
			return
		}
		newSecret = *body.Secret
	} else {
		s, gerr := webhook.GenerateSecret()
		if gerr != nil {
			httpx.Err(w, http.StatusInternalServerError, "secret gen")
			return
		}
		newSecret = s
	}
	grace := d.SecretGrace
	if grace <= 0 {
		grace = 24 * time.Hour
	}
	row, err := d.Queries.RotateEndpointSecret(r.Context(), store.RotateEndpointSecretParams{
		ID:            store.UUID(id),
		AppID:         app.ID,
		NewSecret:     newSecret,
		PrevExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(grace), Valid: true},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Err(w, http.StatusNotFound, "endpoint not found")
			return
		}
		d.Log.Error("rotate endpoint secret", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "rotate secret")
		return
	}
	audit.Log(r.Context(), d.Queries, d.Log, audit.Entry{
		Action: "endpoint.rotate_secret", TargetType: "endpoint", TargetID: audit.PtrUUID(id),
		Metadata: map[string]any{},
	})
	httpx.WriteJSON(w, http.StatusOK, endpointCreateView(row)) // includes the new secret
}

type patchEndpointReq struct {
	URL              *string   `json:"url,omitempty"`
	Description      *string   `json:"description,omitempty"`
	Disabled         *bool     `json:"disabled,omitempty"`
	FilterEventTypes *[]string `json:"filter_event_types,omitempty"`
}

func (d Handlers) PatchEndpoint(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	app, ok := d.appForOrg(w, r, p.OrgID)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid endpoint id")
		return
	}
	var body patchEndpointReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid json")
		return
	}
	if body.URL != nil {
		if err := deliver.ValidateDestinationURL(*body.URL); err != nil {
			httpx.Err(w, http.StatusBadRequest, "invalid url: "+err.Error())
			return
		}
		if deliver.IsSelfHost(*body.URL, d.SelfHosts) {
			httpx.Err(w, http.StatusBadRequest, "url must not point at dstream itself (loop guard)")
			return
		}
	}
	var filter []string
	setFilter := body.FilterEventTypes != nil
	if setFilter {
		filter = *body.FilterEventTypes
	}
	row, err := d.Queries.UpdateEndpoint(r.Context(), store.UpdateEndpointParams{
		ID: store.UUID(id), AppID: app.ID,
		Url: body.URL, Description: body.Description, Disabled: body.Disabled,
		SetFilter: setFilter, FilterEventTypes: filter,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Err(w, http.StatusNotFound, "not found")
			return
		}
		d.Log.Error("update endpoint", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "update endpoint")
		return
	}
	audit.Log(r.Context(), d.Queries, d.Log, audit.Entry{
		Action: "endpoint.update", TargetType: "endpoint",
		TargetID: audit.PtrUUID(id),
		Metadata: map[string]any{"url": row.Url, "disabled": row.Disabled},
	})
	httpx.WriteJSON(w, http.StatusOK, endpointView(row))
}

const recoverMaxBatch = 1000

type recoverReq struct {
	Since string `json:"since"`
}

func (d Handlers) RecoverEndpoint(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	app, ok := d.appForOrg(w, r, p.OrgID)
	if !ok {
		return
	}
	ep, ok := d.endpointForApp(w, r, store.GoUUID(app.ID))
	if !ok {
		return
	}
	var body recoverReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid json")
		return
	}
	since, err := time.Parse(time.RFC3339, body.Since)
	if err != nil {
		httpx.Err(w, http.StatusBadRequest, "since must be RFC3339")
		return
	}
	ids, err := d.Queries.ListDeadDeliveriesForEndpointSince(r.Context(), store.ListDeadDeliveriesForEndpointSinceParams{
		EndpointID: ep.ID,
		Since:      pgtype.Timestamptz{Time: since, Valid: true},
		Lim:        recoverMaxBatch,
	})
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "list dead")
		return
	}
	recovered := 0
	for _, id := range ids {
		if err := d.Queries.ResetDeliveryForReplay(r.Context(), id); err != nil {
			d.Log.Error("recover reset", "err", err, "delivery_id", store.GoUUID(id))
			continue
		}
		data, _ := json.Marshal(map[string]string{"delivery_id": store.GoUUID(id).String()})
		if err := d.Queue.Enqueue(r.Context(), dqueue.Payload{Kind: "message", OrgID: p.OrgID, EnqueuedAt: time.Now().UnixMilli(), Data: data}); err != nil {
			d.Log.Error("recover enqueue", "err", err, "delivery_id", store.GoUUID(id))
			continue
		}
		recovered++
	}
	truncated := len(ids) == recoverMaxBatch
	if truncated {
		d.Log.Warn("recover truncated at cap", "endpoint_id", store.GoUUID(ep.ID), "cap", recoverMaxBatch)
	}
	audit.Log(r.Context(), d.Queries, d.Log, audit.Entry{Action: "endpoint.recover", TargetType: "endpoint", TargetID: audit.PtrUUID(store.GoUUID(ep.ID)), Metadata: map[string]any{"recovered": recovered}})
	httpx.WriteJSON(w, http.StatusAccepted, map[string]any{"recovered": recovered, "truncated": truncated})
}

type testSendReq struct {
	EventType string          `json:"event_type"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

func (d Handlers) TestEndpoint(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	app, ok := d.appForOrg(w, r, p.OrgID)
	if !ok {
		return
	}
	ep, ok := d.endpointForApp(w, r, store.GoUUID(app.ID))
	if !ok {
		return
	}
	var body testSendReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid json")
		return
	}
	if body.EventType == "" {
		httpx.Err(w, http.StatusBadRequest, "event_type required")
		return
	}
	et, err := d.Queries.GetEventTypeForOrg(r.Context(), store.GetEventTypeForOrgParams{OrgID: store.UUID(p.OrgID), Name: body.EventType})
	if err != nil || et.Archived {
		httpx.Err(w, http.StatusUnprocessableEntity, "unknown or archived event_type")
		return
	}
	raw := body.Payload
	if len(raw) == 0 || !json.Valid(raw) {
		raw = json.RawMessage(`{}`)
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid payload json")
		return
	}
	payload := buf.Bytes()
	sum := sha256.Sum256(payload)
	msg, err := d.Queries.CreateMessage(r.Context(), store.CreateMessageParams{
		AppID: app.ID, OrgID: store.UUID(p.OrgID), EventType: body.EventType,
		Payload: payload, PayloadHash: hex.EncodeToString(sum[:]), EventID: nil,
	})
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "create message")
		return
	}
	// one delivery to THIS endpoint, bypassing filter matching
	dels, err := d.Queries.CreateMessageDeliveriesBatch(r.Context(), store.CreateMessageDeliveriesBatchParams{
		MessageID: msg.ID, OrgID: store.UUID(p.OrgID), EndpointIds: []pgtype.UUID{ep.ID},
	})
	if err != nil || len(dels) != 1 {
		httpx.Err(w, http.StatusInternalServerError, "create delivery")
		return
	}
	data, _ := json.Marshal(map[string]string{"delivery_id": store.GoUUID(dels[0].ID).String()})
	if err := d.Queue.Enqueue(r.Context(), dqueue.Payload{Kind: "message", OrgID: p.OrgID, EnqueuedAt: time.Now().UnixMilli(), Data: data}); err != nil {
		d.Log.Error("enqueue test", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "enqueue")
		return
	}
	audit.Log(r.Context(), d.Queries, d.Log, audit.Entry{Action: "endpoint.test", TargetType: "endpoint", TargetID: audit.PtrUUID(store.GoUUID(ep.ID)), Metadata: map[string]any{"event_type": body.EventType}})
	httpx.WriteJSON(w, http.StatusAccepted, map[string]any{"message_id": store.GoUUID(msg.ID).String(), "delivery_id": store.GoUUID(dels[0].ID).String()})
}

func (d Handlers) DeleteEndpoint(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	app, ok := d.appForOrg(w, r, p.OrgID)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid endpoint id")
		return
	}
	if _, err := d.Queries.DeleteEndpointForApp(r.Context(), store.DeleteEndpointForAppParams{
		ID: store.UUID(id), AppID: app.ID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Err(w, http.StatusNotFound, "not found")
			return
		}
		httpx.Err(w, http.StatusInternalServerError, "delete endpoint")
		return
	}
	audit.Log(r.Context(), d.Queries, d.Log, audit.Entry{
		Action: "endpoint.delete", TargetType: "endpoint", TargetID: audit.PtrUUID(id),
		Metadata: map[string]any{},
	})
	w.WriteHeader(http.StatusNoContent)
}
