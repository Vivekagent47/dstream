// Package opevents publishes dstream's own operational webhooks (endpoint
// disabled, delivery exhausted) through the reserved per-org operational
// application, reusing the outbound delivery engine.
package opevents

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/store"
)

// opEventTypes are the operational event types seeded per org this phase.
var opEventTypes = []struct{ Name, Desc string }{
	{"endpoint.disabled", "dstream auto-disabled an endpoint after repeated failures"},
	{"message.attempt.exhausted", "a message delivery exhausted its retries and was dead-lettered"},
}

// SeedOperationalApp ensures orgID has its operational application and the core
// operational event types. Idempotent; returns the op app id.
func SeedOperationalApp(ctx context.Context, q *store.Queries, orgID uuid.UUID) (uuid.UUID, error) {
	app, err := q.EnsureOperationalApp(ctx, store.UUID(orgID))
	if err != nil {
		return uuid.Nil, err
	}
	for _, et := range opEventTypes {
		if err := q.SeedEventType(ctx, store.SeedEventTypeParams{
			OrgID: store.UUID(orgID), Name: et.Name, Description: et.Desc,
		}); err != nil {
			return uuid.Nil, err
		}
	}
	return store.GoUUID(app.ID), nil
}

// Publish emits one operational event for orgID by creating a message on the
// org's operational app and fanning it out to that app's endpoints. Best-effort:
// callers log and continue on error (an operational event must never fail the
// delivery that triggered it). Idempotently ensures the op app exists.
// opevents: no per-org rate limit on op publishes yet — add one if a mass
// outage floods the op app's endpoints.
func Publish(ctx context.Context, q *store.Queries, dq *dqueue.Client, orgID uuid.UUID, eventType string, payload any) error {
	appID, err := SeedOperationalApp(ctx, q, orgID)
	if err != nil {
		return err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, body); err != nil {
		return err
	}
	b := buf.Bytes()
	sum := sha256.Sum256(b)
	msg, err := q.CreateMessage(ctx, store.CreateMessageParams{
		AppID: store.UUID(appID), OrgID: store.UUID(orgID), EventType: eventType,
		Payload: b, PayloadHash: hex.EncodeToString(sum[:]),
	})
	if err != nil {
		return err
	}
	epIDs, err := q.ListMatchingEndpoints(ctx, store.ListMatchingEndpointsParams{
		AppID: store.UUID(appID), EventType: eventType,
	})
	if err != nil || len(epIDs) == 0 {
		return err
	}
	dels, err := q.CreateMessageDeliveriesBatch(ctx, store.CreateMessageDeliveriesBatchParams{
		MessageID: msg.ID, OrgID: store.UUID(orgID), EndpointIds: epIDs,
	})
	if err != nil {
		return err
	}
	for _, d := range dels {
		data, _ := json.Marshal(map[string]string{"delivery_id": store.GoUUID(d.ID).String()})
		if err := dq.Enqueue(ctx, dqueue.Payload{
			Kind: "message", OrgID: orgID, EnqueuedAt: time.Now().UnixMilli(), Data: data,
		}); err != nil {
			// reaper re-enqueues 'queued' rows
			continue
		}
	}
	return nil
}
