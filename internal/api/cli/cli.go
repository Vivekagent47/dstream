package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Vivekagent47/dstream/internal/api/httpx"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/ingest"
	"github.com/Vivekagent47/dstream/internal/metrics"
	"github.com/Vivekagent47/dstream/internal/store"
)

const (
	cliSessionTTL = 30 * time.Second
	cliPingEvery  = 10 * time.Second
	// cliReadLimit caps a single inbound WS frame (CLI response). Matches the
	// 1 MiB attempt-body cap; without it the library's 32 KiB default tears the
	// whole tunnel down on a larger local response.
	cliReadLimit = 1 << 20
	// maxConcurrentDispatch bounds in-flight event goroutines per tunnel so an
	// event burst can't spawn unbounded goroutines.
	maxConcurrentDispatch = 64
)

// SessionKey returns the Redis key marking that a CLI is currently
// connected for the given source.
func SessionKey(sourceID uuid.UUID) string { return "cli:source:" + sourceID.String() }

// DispatchKey returns the Redis list key where the delivery worker pushes
// events destined for the CLI tunnel.
func DispatchKey(sourceID uuid.UUID) string { return "cli:dispatch:" + sourceID.String() }

// originHost extracts the host[:port] from the configured public base URL, used
// to pin the WebSocket Origin allowlist. Returns "" if unparseable, in which
// case Accept falls back to its default same-origin (Origin host == Host) check.
func originHost(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	return u.Host
}

// GET /api/cli/sources — minimal lookup used by the CLI to resolve `--source <name>`.
func (d Handlers) ListSources(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	rows, err := d.Queries.ListSourcesByOrg(r.Context(), store.UUID(p.OrgID))
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, "list")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, s := range rows {
		out = append(out, map[string]any{
			"id":   store.GoUUID(s.ID).String(),
			"name": s.Name,
			"type": s.Type,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// WS /api/cli/connect?source_id=...
//
// Registers a CLI tunnel session against the source and consumes events the
// delivery worker pushes onto the dispatch list. For each event:
//   - load the request body from BodyStore
//   - forward over the WebSocket as a "deliver" frame
//   - read the "response" frame back from the CLI
//   - record the attempt and update event status in Postgres
func (d Handlers) Connect(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpx.Err(w, http.StatusUnauthorized, "active org required")
		return
	}
	sourceIDStr := r.URL.Query().Get("source_id")
	if sourceIDStr == "" {
		httpx.Err(w, http.StatusBadRequest, "source_id required")
		return
	}
	sid, err := uuid.Parse(sourceIDStr)
	if err != nil {
		httpx.Err(w, http.StatusBadRequest, "invalid source_id")
		return
	}
	if _, err := d.Queries.GetSourceForOrg(r.Context(), store.GetSourceForOrgParams{
		ID:    store.UUID(sid),
		OrgID: store.UUID(p.OrgID),
	}); err != nil {
		httpx.Err(w, http.StatusNotFound, "source not found")
		return
	}

	// Origin check defeats cross-site WebSocket hijacking: without it, a page on
	// any origin could open this socket riding the victim's session cookie and
	// exfiltrate their webhook payloads. Browsers send Origin; the Go CLI client
	// (Bearer-authed, no Origin header) is unaffected. OriginPatterns pins to the
	// dashboard host so the check survives reverse-proxy Host rewrites.
	acceptOpts := &websocket.AcceptOptions{}
	if host := originHost(d.PublicBaseURL); host != "" {
		acceptOpts.OriginPatterns = []string{host}
	}
	conn, err := websocket.Accept(w, r, acceptOpts)
	if err != nil {
		return
	}
	metrics.CLIConnected()
	disconnectReason := "closed"
	defer func() { metrics.CLIDisconnected(disconnectReason) }()
	conn.SetReadLimit(cliReadLimit)
	defer conn.Close(websocket.StatusNormalClosure, "bye")

	// Detach from r.Context() — the router-wide chi.Timeout(30s) middleware
	// cancels r.Context() after 30s, which would kill the WebSocket mid-
	// session. We still propagate cancellation from process shutdown via
	// our own derived ctx; CLI client disconnects are observed via the
	// websocket reader returning an error.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// coder/websocket forbids concurrent writers; we write from the ping loop
	// plus one goroutine per dispatched event. Serialize every write behind this
	// mutex. sem bounds the dispatch goroutines.
	var writeMu sync.Mutex
	writeJSON := func(v any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return wsjson.Write(ctx, conn, v)
	}
	sem := make(chan struct{}, maxConcurrentDispatch)

	sessionKey := SessionKey(sid)
	if err := d.Redis.Set(ctx, sessionKey, "active", cliSessionTTL).Err(); err != nil {
		d.Log.Error("cli: register session", "err", err)
		disconnectReason = "register_failed"
		return
	}
	defer d.Redis.Del(context.Background(), sessionKey)

	_ = writeJSON(map[string]any{
		"type":      "hello",
		"source_id": sid.String(),
		"now":       time.Now().UTC(),
	})

	// Ping loop keeps the session key alive.
	go func() {
		t := time.NewTicker(cliPingEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				d.Redis.Expire(ctx, sessionKey, cliSessionTTL)
				if err := writeJSON(map[string]any{"type": "ping"}); err != nil {
					return
				}
			}
		}
	}()

	bs := ingest.NewPostgresBodyStore(d.Queries)
	pending := newPendingMap()

	// Reader: handles response frames from the CLI.
	go func() {
		for {
			var frame map[string]json.RawMessage
			if err := wsjson.Read(ctx, conn, &frame); err != nil {
				cancel()
				return
			}
			var ftype string
			_ = json.Unmarshal(frame["type"], &ftype)
			if ftype != "response" {
				continue
			}
			var evID string
			_ = json.Unmarshal(frame["event_id"], &evID)
			ch, ok := pending.pop(evID)
			if ok {
				ch <- frame
			}
		}
	}()

	// Dispatch loop: BLPOP from the worker-fed list.
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		res, err := d.Redis.BLPop(ctx, 5*time.Second, DispatchKey(sid)).Result()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		if len(res) < 2 {
			continue
		}
		var p struct {
			EventID string `json:"event_id"`
		}
		if err := json.Unmarshal([]byte(res[1]), &p); err != nil {
			continue
		}
		evUUID, err := uuid.Parse(p.EventID)
		if err != nil {
			continue
		}
		// Bound concurrent dispatch goroutines; block (not drop) so no event is
		// lost, but bail if the session is shutting down.
		select {
		case <-ctx.Done():
			return
		case sem <- struct{}{}:
		}
		go func() {
			defer func() { <-sem }()
			d.dispatchEventToCLI(ctx, writeJSON, bs, pending, evUUID)
		}()
	}
}

func (d Handlers) dispatchEventToCLI(
	ctx context.Context,
	writeFn func(any) error,
	bs ingest.BodyStore,
	pending *pendingMap,
	eventID uuid.UUID,
) {
	row, err := d.Queries.GetEventForDelivery(ctx, store.UUID(eventID))
	if err != nil {
		d.Log.Error("cli dispatch: load event", "err", err)
		return
	}
	destID := store.GoUUID(row.DestinationID)
	connID := store.GoUUID(row.ConnectionID)
	body, err := bs.Get(ctx, row.BodyRef)
	if err != nil {
		d.Log.Error("cli dispatch: body", "err", err)
		return
	}
	var headers map[string][]string
	_ = json.Unmarshal(row.RequestHeaders, &headers)

	_ = d.Queries.MarkEventInFlight(ctx, row.ID)

	ch := pending.add(eventID.String())
	defer pending.remove(eventID.String())

	frame := map[string]any{
		"type":     "event",
		"event_id": eventID.String(),
		"method":   "POST",
		"path":     "/",
		"headers":  headers,
		"body":     body,
	}
	if err := writeFn(frame); err != nil {
		d.recordCLIFailure(ctx, destID, connID, row.ID, row.AttemptCount+1, err)
		return
	}

	start := time.Now()
	select {
	case <-ctx.Done():
		return
	case <-time.After(35 * time.Second):
		d.recordCLIFailure(ctx, destID, connID, row.ID, row.AttemptCount+1, fmt.Errorf("cli response timeout"))
		return
	case resp := <-ch:
		dur := int32(time.Since(start) / time.Millisecond)
		var status int32
		_ = json.Unmarshal(resp["status"], &status)
		var respHeaders json.RawMessage
		if h, ok := resp["headers"]; ok {
			respHeaders = h
		}
		var respBody []byte
		_ = json.Unmarshal(resp["body"], &respBody)
		var errStr string
		_ = json.Unmarshal(resp["error"], &errStr)
		var errPtr *string
		if errStr != "" {
			errPtr = &errStr
		}
		var statusPtr *int32
		if status != 0 {
			statusPtr = &status
		}
		if _, err := d.Queries.CreateAttempt(ctx, store.CreateAttemptParams{
			EventID:         row.ID,
			AttemptNum:      row.AttemptCount + 1,
			ResponseStatus:  statusPtr,
			ResponseHeaders: respHeaders,
			ResponseBody:    respBody,
			DurationMs:      &dur,
			ErrorMessage:    errPtr,
		}); err != nil {
			d.Log.Error("cli dispatch: attempt", "err", err)
		}
		if errStr == "" && status >= 200 && status < 300 {
			_ = d.Queries.MarkEventDelivered(ctx, row.ID)
			metrics.Delivery(destID, connID, "delivered")
			metrics.Attempt(connID, "success")
		} else {
			_ = d.Queries.MarkEventFailed(ctx, row.ID)
			// CLI deliveries are terminal (no retry budget), so a bad response is
			// a dead-letter-equivalent outcome.
			metrics.Delivery(destID, connID, "failed")
			metrics.Attempt(connID, "deadletter")
		}
	}
}

func (d Handlers) recordCLIFailure(ctx context.Context, destID, connID uuid.UUID, eventID pgtype.UUID, attemptNum int32, deliverErr error) {
	msg := deliverErr.Error()
	if _, err := d.Queries.CreateAttempt(ctx, store.CreateAttemptParams{
		EventID:      eventID,
		AttemptNum:   attemptNum,
		ErrorMessage: &msg,
	}); err != nil {
		d.Log.Error("cli dispatch: record failure", "err", err)
	}
	_ = d.Queries.MarkEventFailed(ctx, eventID)
	metrics.Delivery(destID, connID, "failed")
	metrics.Attempt(connID, "deadletter")
}

// pendingMap correlates outbound event frames with the response frames that
// will eventually come back over the same socket.
type pendingMap struct {
	mu  sync.Mutex
	chs map[string]chan map[string]json.RawMessage
}

func newPendingMap() *pendingMap {
	return &pendingMap{chs: map[string]chan map[string]json.RawMessage{}}
}

func (p *pendingMap) add(id string) chan map[string]json.RawMessage {
	ch := make(chan map[string]json.RawMessage, 1)
	p.mu.Lock()
	p.chs[id] = ch
	p.mu.Unlock()
	return ch
}

func (p *pendingMap) pop(id string) (chan map[string]json.RawMessage, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ch, ok := p.chs[id]
	if ok {
		delete(p.chs, id)
	}
	return ch, ok
}

func (p *pendingMap) remove(id string) {
	p.mu.Lock()
	delete(p.chs, id)
	p.mu.Unlock()
}
