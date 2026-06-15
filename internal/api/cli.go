package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/google/uuid"

	"github.com/streamingo/dstream/internal/auth"
	"github.com/streamingo/dstream/internal/store"
)

// GET /api/cli/sources — used by CLI to find a source by name or list all
// sources for the authenticated project. Returns minimal info (no signing
// config) so CLI can resolve a `--source <name>` flag locally.
func (d Deps) cliListSources(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.ProjectID == uuid.Nil {
		httpErr(w, http.StatusUnauthorized, "api key required")
		return
	}
	rows, err := d.Queries.ListSourcesByProject(r.Context(), store.UUID(p.ProjectID))
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
// Phase 1.3: registers the CLI as the live destination for any connection
// pointing at a `cli` destination on the given source. The delivery worker
// looks up the live WS session in Redis (key: cli:source:<id>) and pushes
// events; the CLI POSTs to its local URL and writes the response back.
func (d Deps) cliConnect(w http.ResponseWriter, r *http.Request) {
	p, err := auth.FromContext(r.Context())
	if err != nil || p.ProjectID == uuid.Nil {
		httpErr(w, http.StatusUnauthorized, "api key required")
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
	src, err := d.Queries.GetSourceByID(r.Context(), store.UUID(sid))
	if err != nil || store.GoUUID(src.ProjectID) != p.ProjectID {
		httpErr(w, http.StatusNotFound, "source not found")
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // CLI doesn't send Origin header
	})
	if err != nil {
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "bye")

	sessionKey := "cli:source:" + sid.String()
	// Register the session so delivery worker can route to it.
	if err := d.Redis.Set(r.Context(), sessionKey, "active", 30*time.Second).Err(); err != nil {
		d.Log.Error("cli: register session", "err", err)
		return
	}
	defer d.Redis.Del(context.Background(), sessionKey)

	// Hello frame.
	_ = wsjson.Write(r.Context(), conn, map[string]any{
		"type":      "hello",
		"source_id": sid.String(),
		"now":       time.Now().UTC(),
	})

	// Heartbeat loop — refreshes session TTL. Real event push happens in
	// Phase 1.4 when delivery worker learns to dispatch via WS.
	heartbeat := time.NewTicker(10 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			d.Redis.Expire(r.Context(), sessionKey, 30*time.Second)
			if err := wsjson.Write(r.Context(), conn, map[string]any{"type": "ping"}); err != nil {
				return
			}
		}
	}
}

// Compile-time guard.
var _ = json.Marshal
