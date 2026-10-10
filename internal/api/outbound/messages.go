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
	"github.com/Vivekagent47/dstream/internal/usage"
)

type sendMessageReq struct {
	EventType string          `json:"event_type"`
	Payload   json.RawMessage `json:"payload"`
	EventID   *string         `json:"event_id,omitempty"`
	Channels  []string        `json:"channels,omitempty"`
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
	// Per-org usage quota, before the (up to 5 MiB) body read for the same
	// reason the ingest gate sits there: an over-quota flood must not force
	// large reads. p.OrgID is already on the Principal and the limits come from
	// the gate's snapshot, so this costs one Redis INCR and no query. Only the
	// hard ceiling rejects; past the soft limit the publish is accepted as
	// overage and the gate warns once per period. Any Redis error fails open.
	if dec := d.Quota.CheckPublish(r.Context(), p.OrgID); dec == usage.OverHard {
		w.Header().Set("Retry-After", usage.RetryAfter)
		httpx.Err(w, http.StatusTooManyRequests, "quota exceeded")
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
	// An empty event_id is not an idempotency key — treat it as absent, else
	// every publish with "event_id":"" collides on the first and silently drops.
	if req.EventID != nil && *req.EventID == "" {
		req.EventID = nil
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
	if err := validateChannels(req.Channels); err != nil {
		httpx.Err(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := validatePayloadAgainstSchema(et.Schema, req.Payload); err != nil {
		httpx.Err(w, http.StatusUnprocessableEntity, err.Error())
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

	// Create the message and fan out to matching endpoints in ONE transaction, so a
	// message can never persist with zero deliveries. Without it, a fan-out failure
	// after the message commits leaves a delivery-less message, and the client's
	// idempotent retry then returns success (the ON CONFLICT path) without ever
	// creating the deliveries. Enqueue is Redis, so it runs after commit.
	tx, err := d.Pool.Begin(r.Context())
	if err != nil {
		d.Log.Error("begin publish tx", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "create message")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }() // no-op after Commit
	qtx := d.Queries.WithTx(tx)

	msg, err := qtx.CreateMessage(r.Context(), store.CreateMessageParams{
		AppID:       app.ID,
		OrgID:       store.UUID(p.OrgID),
		EventType:   req.EventType,
		Payload:     payload,
		PayloadHash: hex.EncodeToString(sum[:]),
		EventID:     req.EventID,
		Channels:    req.Channels,
	})
	if err != nil {
		// ON CONFLICT DO NOTHING returns no row on an idempotency collision. A DO
		// NOTHING conflict doesn't abort the tx, but we're returning either way, so
		// read the existing row outside it and let the rollback drop the empty tx.
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
	epIDs, err := qtx.ListMatchingEndpoints(r.Context(), store.ListMatchingEndpointsParams{
		AppID: app.ID, EventType: req.EventType, MsgChannels: req.Channels,
	})
	if err != nil {
		d.Log.Error("match endpoints", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "fan-out")
		return
	}
	var dels []store.CreateMessageDeliveriesBatchRow
	if len(epIDs) > 0 {
		dels, err = qtx.CreateMessageDeliveriesBatch(r.Context(), store.CreateMessageDeliveriesBatchParams{
			MessageID:   msg.ID,
			OrgID:       store.UUID(p.OrgID),
			EndpointIds: epIDs,
		})
		if err != nil {
			d.Log.Error("create deliveries", "err", err)
			httpx.Err(w, http.StatusInternalServerError, "fan-out")
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		d.Log.Error("commit publish tx", "err", err)
		httpx.Err(w, http.StatusInternalServerError, "create message")
		return
	}

	// Message + deliveries are durable now; enqueue the Redis tasks. A failed
	// enqueue leaves the row 'queued' for the outbound reaper to re-enqueue.
	for _, del := range dels {
		data, _ := json.Marshal(map[string]string{"delivery_id": store.GoUUID(del.ID).String()})
		if err := d.Queue.Enqueue(r.Context(), dqueue.Payload{
			Kind:       "message",
			OrgID:      p.OrgID,
			EnqueuedAt: time.Now().UnixMilli(),
			Data:       data,
		}); err != nil {
			d.Log.Error("enqueue delivery", "err", err, "delivery_id", store.GoUUID(del.ID))
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
	msg, err := d.Queries.GetMessageForApp(r.Context(), store.GetMessageForAppParams{ID: store.UUID(msgID), AppID: app.ID})
	if err != nil {
		httpx.Err(w, http.StatusNotFound, "message not found")
		return
	}
	// Payload expunged by the retention sweep: nothing left to deliver.
	if len(msg.Payload) == 0 {
		httpx.Err(w, http.StatusUnprocessableEntity, "message payload has been expunged and can no longer be delivered")
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
		if cerr != nil {
			httpx.Err(w, http.StatusInternalServerError, "create delivery")
			return
		}
		if len(created) == 1 {
			delID = created[0].ID
		} else {
			// Lost a concurrent replay race — ON CONFLICT DO NOTHING inserted no
			// row because the other request created it. Load + reset that row.
			ex, gerr := d.Queries.GetDeliveryByMessageEndpoint(r.Context(), store.GetDeliveryByMessageEndpointParams{
				MessageID: store.UUID(msgID), EndpointID: store.UUID(epID),
			})
			if gerr != nil {
				httpx.Err(w, http.StatusInternalServerError, "load delivery")
				return
			}
			delID = ex.ID
			if err := d.Queries.ResetDeliveryForReplay(r.Context(), delID); err != nil {
				httpx.Err(w, http.StatusInternalServerError, "reset delivery")
				return
			}
		}
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
