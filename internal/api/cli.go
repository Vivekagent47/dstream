package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/ingest"
	"github.com/Vivekagent47/dstream/internal/store"
)

const (
	cliSessionTTL = 30 * time.Second
	cliPingEvery  = 10 * time.Second
)

// SessionKey returns the Redis key marking that a CLI is currently
// connected for the given source.
func SessionKey(sourceID uuid.UUID) string { return "cli:source:" + sourceID.String() }

// DispatchKey returns the Redis list key where the delivery worker pushes
// events destined for the CLI tunnel.
func DispatchKey(sourceID uuid.UUID) string { return "cli:dispatch:" + sourceID.String() }

// GET /api/cli/sources — minimal lookup used by the CLI to resolve `--source <name>`.
func (d Deps) cliListSources(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpErr(w, http.StatusUnauthorized, "active org required")
		return
	}
	rows, err := d.Queries.ListSourcesByOrg(r.Context(), store.UUID(p.OrgID))
	if err != nil {
		httpErr(w, http.StatusInternalServerError, "list")
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
	writeJSON(w, http.StatusOK, out)
}

// WS /api/cli/connect?source_id=...
//
// Registers a CLI tunnel session against the source and consumes events the
// delivery worker pushes onto the dispatch list. For each event:
//   - load the request body from BodyStore
//   - forward over the WebSocket as a "deliver" frame
//   - read the "response" frame back from the CLI
//   - record the attempt and update event status in Postgres
func (d Deps) cliConnect(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.OrgID == uuid.Nil {
		httpErr(w, http.StatusUnauthorized, "active org required")
		return
	}
	sourceIDStr := r.URL.Query().Get("source_id")
	if sourceIDStr == "" {
		httpErr(w, http.StatusBadRequest, "source_id required")
		return
	}
	sid, err := uuid.Parse(sourceIDStr)
	if err != nil {
		httpErr(w, http.StatusBadRequest, "invalid source_id")
		return
	}
	if _, err := d.Queries.GetSourceForOrg(r.Context(), store.GetSourceForOrgParams{
		ID:    store.UUID(sid),
		OrgID: store.UUID(p.OrgID),
	}); err != nil {
		httpErr(w, http.StatusNotFound, "source not found")
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "bye")

	// Detach from r.Context() — the router-wide chi.Timeout(30s) middleware
	// cancels r.Context() after 30s, which would kill the WebSocket mid-
	// session. We still propagate cancellation from process shutdown via
	// our own derived ctx; CLI client disconnects are observed via the
	// websocket reader returning an error.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sessionKey := SessionKey(sid)
	if err := d.Redis.Set(ctx, sessionKey, "active", cliSessionTTL).Err(); err != nil {
		d.Log.Error("cli: register session", "err", err)
		return
	}
	defer d.Redis.Del(context.Background(), sessionKey)

	_ = wsjson.Write(ctx, conn, map[string]any{
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
				if err := wsjson.Write(ctx, conn, map[string]any{"type": "ping"}); err != nil {
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
		go d.dispatchEventToCLI(ctx, conn, bs, pending, evUUID)
	}
}

func (d Deps) dispatchEventToCLI(
	ctx context.Context,
	conn *websocket.Conn,
	bs ingest.BodyStore,
	pending *pendingMap,
	eventID uuid.UUID,
) {
	row, err := d.Queries.GetEventForDelivery(ctx, store.UUID(eventID))
	if err != nil {
		d.Log.Error("cli dispatch: load event", "err", err)
		return
	}
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
	if err := wsjson.Write(ctx, conn, frame); err != nil {
		d.recordCLIFailure(ctx, row.ID, row.AttemptCount+1, err)
		return
	}

	start := time.Now()
	select {
	case <-ctx.Done():
		return
	case <-time.After(35 * time.Second):
		d.recordCLIFailure(ctx, row.ID, row.AttemptCount+1, fmt.Errorf("cli response timeout"))
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
		} else {
			_ = d.Queries.MarkEventFailed(ctx, row.ID)
		}
	}
}

func (d Deps) recordCLIFailure(ctx context.Context, eventID pgtype.UUID, attemptNum int32, deliverErr error) {
	msg := deliverErr.Error()
	if _, err := d.Queries.CreateAttempt(ctx, store.CreateAttemptParams{
		EventID:      eventID,
		AttemptNum:   attemptNum,
		ErrorMessage: &msg,
	}); err != nil {
		d.Log.Error("cli dispatch: record failure", "err", err)
	}
	_ = d.Queries.MarkEventFailed(ctx, eventID)
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
