// Package bookmark is the record/replay engine: re-inject a captured request
// through the pipeline, replay it to a URL, or export it as portable JSON.
package bookmark

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/store"
)

// Enqueuer is the minimal queue surface Reinject needs (*dqueue.Client satisfies it).
type Enqueuer interface {
	Enqueue(ctx context.Context, p dqueue.Payload) error
}

// Request is a reconstructed captured request.
type Request struct {
	SourceID    uuid.UUID
	Method      string
	Path        string
	Headers     map[string][]string
	Body        []byte
	ContentType string
}

// Response summarizes a replay-to-URL result.
type Response struct {
	Status     int
	DurationMs int
	Body       []byte
}

// replayRespCap bounds how much of a replay-to-URL response we keep.
const replayRespCap = 64 << 10

// Reinject fans the EXISTING captured request (by requestID) out to the
// source's currently-enabled connections, minting new is_test events off the
// same request_id, and enqueues one delivery task per event. It does NOT go
// through dedup — this is an explicit, intentional re-injection. Reusing the
// request_id is safe: events has no UNIQUE(request_id, connection_id).
//
// ponytail: mirrors the ingest fan-out (list→create→enqueue) but is kept
// separate to avoid touching the ingest hot path; the block is small + stable.
func Reinject(ctx context.Context, q store.Querier, queue Enqueuer, requestID, sourceID, orgID uuid.UUID) ([]uuid.UUID, error) {
	conns, err := q.ListEnabledConnectionsBySource(ctx, store.UUID(sourceID))
	if err != nil {
		return nil, err
	}
	if len(conns) == 0 {
		return nil, nil
	}
	connIDs := make([]pgtype.UUID, len(conns))
	byID := make(map[uuid.UUID]store.Connection, len(conns))
	for i, c := range conns {
		connIDs[i] = c.ID
		byID[store.GoUUID(c.ID)] = c
	}
	events, err := q.CreateEventsBatch(ctx, store.CreateEventsBatchParams{
		RequestID:     store.UUID(requestID),
		OrgID:         store.UUID(orgID),
		ConnectionIds: connIDs,
		IsTest:        true,
	})
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, 0, len(events))
	for _, ev := range events {
		c := byID[store.GoUUID(ev.ConnectionID)]
		if err := queue.Enqueue(ctx, dqueue.Payload{
			EventID:             store.GoUUID(ev.ID),
			OrgID:               store.GoUUID(ev.OrgID),
			Attempt:             0,
			EnqueuedAt:          time.Now().UnixMilli(),
			RetryStrategy:       c.RetryStrategy,
			RetryBaseMs:         c.RetryBaseMs,
			RetryCapMs:          c.RetryCapMs,
			RetryJitterPct:      c.RetryJitterPct,
			CustomRetrySchedule: c.CustomRetrySchedule,
		}); err != nil {
			// Event is durably 'queued' in Postgres; the reaper re-enqueues it.
			// Skip it here and keep fanning out the rest (mirrors ingest).
			continue
		}
		ids = append(ids, store.GoUUID(ev.ID))
	}
	return ids, nil
}

// skipHeader reports whether a captured header must not be forwarded on replay.
func skipHeader(key string, vals []string) bool {
	switch http.CanonicalHeaderKey(key) {
	case "Host", "Content-Length", "Content-Type", "Dstream-Webhook-Hops",
		"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Te", "Trailer", "Transfer-Encoding", "Upgrade",
		"X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host", "X-Real-Ip":
		return true
	}
	// Credential headers are stored redacted at rest — forwarding "[redacted]"
	// is useless/misleading, so drop them.
	for _, v := range vals {
		if v == "[redacted]" {
			return true
		}
	}
	return false
}

// ReplayTo POSTs the captured request to url and returns the response summary.
// The caller supplies the client; for server-side use pass the SSRF-guarded
// SafeHTTPClient. Content-Type is set from req; other non-skipped headers are
// forwarded.
func ReplayTo(ctx context.Context, client *http.Client, req Request, url string) (Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(req.Body))
	if err != nil {
		return Response{}, err
	}
	for k, vals := range req.Headers {
		if skipHeader(k, vals) {
			continue
		}
		for _, v := range vals {
			httpReq.Header.Add(k, v)
		}
	}
	if req.ContentType != "" {
		httpReq.Header.Set("Content-Type", req.ContentType)
	}
	start := time.Now()
	resp, err := client.Do(httpReq)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, replayRespCap))
	_, _ = io.Copy(io.Discard, resp.Body) // drain remainder so the conn can be reused
	return Response{Status: resp.StatusCode, DurationMs: int(time.Since(start).Milliseconds()), Body: body}, nil
}

// exportJSON is the portable fixture format.
type exportJSON struct {
	Method      string              `json:"method"`
	Path        string              `json:"path"`
	Headers     map[string][]string `json:"headers"`
	ContentType string              `json:"content_type"`
	Body        string              `json:"body_base64"`
}

// Export marshals req to portable JSON (body base64-encoded).
func Export(req Request) ([]byte, error) {
	return json.MarshalIndent(exportJSON{
		Method:      req.Method,
		Path:        req.Path,
		Headers:     req.Headers,
		ContentType: req.ContentType,
		Body:        base64.StdEncoding.EncodeToString(req.Body),
	}, "", "  ")
}
