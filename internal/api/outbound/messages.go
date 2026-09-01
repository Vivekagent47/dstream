package outbound

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Vivekagent47/dstream/internal/api/httpx"
	"github.com/Vivekagent47/dstream/internal/audit"
	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/store"
)

type sendMessageReq struct {
	EventType string          `json:"event_type"`
	Payload   json.RawMessage `json:"payload"`
	EventID   *string         `json:"event_id,omitempty"`
}

func (d Handlers) CreateMessage(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	app, ok := d.appForOrg(w, r, p.OrgID)
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 5<<20))
	if err != nil {
		httpx.Err(w, http.StatusBadRequest, "body too large or unreadable")
		return
	}
	var req sendMessageReq
	if err := json.Unmarshal(body, &req); err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid json")
		return
	}
	if req.EventType == "" {
		httpx.Err(w, http.StatusBadRequest, "event_type required")
		return
	}
	if len(req.Payload) == 0 || !json.Valid(req.Payload) {
		httpx.Err(w, http.StatusBadRequest, "payload must be a valid json value")
		return
	}
	// Event type must be registered and not archived.
	et, err := d.Queries.GetEventTypeForOrg(r.Context(), store.GetEventTypeForOrgParams{
		OrgID: store.UUID(p.OrgID), Name: req.EventType,
	})
	if err != nil || et.Archived {
		httpx.Err(w, http.StatusUnprocessableEntity, "unknown or archived event_type")
		return
	}
	// Serialize the payload once → the exact bytes we store, sign, and send.
	var buf bytes.Buffer
	if err := json.Compact(&buf, req.Payload); err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid payload json")
		return
	}
	payload := buf.Bytes()
	sum := sha256.Sum256(payload)

	msg, err := d.Queries.CreateMessage(r.Context(), store.CreateMessageParams{
		AppID:       app.ID,
		OrgID:       store.UUID(p.OrgID),
		EventType:   req.EventType,
		Payload:     payload,
		PayloadHash: hex.EncodeToString(sum[:]),
		EventID:     req.EventID,
	})
	if err != nil {
		// ON CONFLICT DO NOTHING returns no row on an idempotency collision.
		if errors.Is(err, pgx.ErrNoRows) && req.EventID != nil {
			existing, gerr := d.Queries.GetMessageByAppEventID(r.Context(), store.GetMessageByAppEventIDParams{
				AppID: app.ID, EventID: req.EventID,
			})
			if gerr != nil {
				httpx.Err(w, http.StatusInternalServerError, "idempotency lookup")
				return
			}
			httpx.WriteJSON(w, http.StatusOK, map[string]any{
				"message_id":        store.GoUUID(existing.ID).String(),
				"event_id":          httpx.DerefString(existing.EventID),
				"idempotent_replay": true,
			})
			return
		}
		d.Log.Error("create message", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "create message")
		return
	}

	// Fan out to matching, enabled endpoints.
	epIDs, err := d.Queries.ListMatchingEndpoints(r.Context(), store.ListMatchingEndpointsParams{
		AppID: app.ID, EventType: req.EventType,
	})
	if err != nil {
		d.Log.Error("match endpoints", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "fan-out")
		return
	}
	if len(epIDs) > 0 {
		dels, err := d.Queries.CreateMessageDeliveriesBatch(r.Context(), store.CreateMessageDeliveriesBatchParams{
			MessageID:   msg.ID,
			OrgID:       store.UUID(p.OrgID),
			EndpointIds: epIDs,
		})
		if err != nil {
			d.Log.Error("create deliveries", "err", err)
			httpx.Err(w, http.StatusInternalServerError, "fan-out")
			return
		}
		for _, del := range dels {
			data, _ := json.Marshal(map[string]string{"delivery_id": store.GoUUID(del.ID).String()})
			if err := d.Queue.Enqueue(r.Context(), dqueue.Payload{
				Kind:       "message",
				OrgID:      p.OrgID,
				EnqueuedAt: time.Now().UnixMilli(),
				Data:       data,
			}); err != nil {
				// Leave the row 'queued'; the outbound reaper re-enqueues it.
				d.Log.Error("enqueue delivery", "err", err, "delivery_id", store.GoUUID(del.ID))
			}
		}
	}

	httpx.WriteJSON(w, http.StatusAccepted, map[string]any{
		"message_id":        store.GoUUID(msg.ID).String(),
		"event_id":          httpx.DerefString(req.EventID),
		"idempotent_replay": false,
	})
}

// ReplayDelivery re-enqueues delivery of one message to one endpoint. It
// find-or-creates the (message, endpoint) delivery row, resets it to queued,
// and pushes a message task onto the dqueue. The message id comes from {id}
// (shared with the sibling message routes to avoid a chi param collision).
func (d Handlers) ReplayDelivery(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	app, ok := d.appForOrg(w, r, p.OrgID)
	if !ok {
		return
	}
	msgID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid message id")
		return
	}
	epID, err := uuid.Parse(chi.URLParam(r, "endpoint_id"))
	if err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid endpoint id")
		return
	}
	// ownership: message + endpoint both belong to this app
	if _, err := d.Queries.GetMessageForApp(r.Context(), store.GetMessageForAppParams{ID: store.UUID(msgID), AppID: app.ID}); err != nil {
		httpx.Err(w, http.StatusNotFound, "message not found")
		return
	}
	if _, ok := d.endpointForAppID(w, r, store.GoUUID(app.ID), epID); !ok {
		return
	}
	// find-or-create the (msg,ep) delivery
	del, err := d.Queries.GetDeliveryByMessageEndpoint(r.Context(), store.GetDeliveryByMessageEndpointParams{
		MessageID: store.UUID(msgID), EndpointID: store.UUID(epID),
	})
	var delID pgtype.UUID
	if errors.Is(err, pgx.ErrNoRows) {
		created, cerr := d.Queries.CreateMessageDeliveriesBatch(r.Context(), store.CreateMessageDeliveriesBatchParams{
			MessageID: store.UUID(msgID), OrgID: store.UUID(p.OrgID), EndpointIds: []pgtype.UUID{store.UUID(epID)},
		})
		if cerr != nil || len(created) != 1 {
			httpx.Err(w, http.StatusInternalServerError, "create delivery")
			return
		}
		delID = created[0].ID
	} else if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "load delivery")
		return
	} else {
		delID = del.ID
		if err := d.Queries.ResetDeliveryForReplay(r.Context(), delID); err != nil {
			httpx.Err(w, http.StatusInternalServerError, "reset delivery")
			return
		}
	}
	data, _ := json.Marshal(map[string]string{"delivery_id": store.GoUUID(delID).String()})
	if err := d.Queue.Enqueue(r.Context(), dqueue.Payload{Kind: "message", OrgID: p.OrgID, EnqueuedAt: time.Now().UnixMilli(), Data: data}); err != nil {
		d.Log.Error("enqueue replay", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "enqueue")
		return
	}
	audit.Log(r.Context(), d.Queries, d.Log, audit.Entry{Action: "message.replay", TargetType: "message_delivery", TargetID: audit.PtrUUID(store.GoUUID(delID)), Metadata: map[string]any{}})
	httpx.WriteJSON(w, http.StatusAccepted, map[string]any{"delivery_id": store.GoUUID(delID).String()})
}
