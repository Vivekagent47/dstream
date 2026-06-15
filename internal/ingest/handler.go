package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"

	"github.com/streamingo/dstream/internal/queue"
	"github.com/streamingo/dstream/internal/store"
)

const (
	MaxBodyBytes   = 5 << 20 // 5 MiB
	DedupWindow    = 60 * time.Second
	SourceCacheTTL = 60 * time.Second
)

type Handler struct {
	Log       *slog.Logger
	Queries   *store.Queries
	Redis     *redis.Client
	Queue     *queue.Client
	BodyStore BodyStore
}

func (h *Handler) Mount(r chi.Router) {
	r.Post("/e/{token}", h.handleIngest)
}

type ingestResponse struct {
	RequestID string   `json:"request_id"`
	EventIDs  []string `json:"event_ids"`
	Deduped   bool     `json:"deduped,omitempty"`
}

func (h *Handler) handleIngest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	token := chi.URLParam(r, "token")

	src, err := h.resolveSource(ctx, token)
	if err != nil {
		if errors.Is(err, ErrSourceNotFound) {
			http.Error(w, "unknown source", http.StatusNotFound)
			return
		}
		h.Log.Error("ingest: resolve source", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		http.Error(w, "body too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}

	sum := sha256.Sum256(body)
	bodyHash := hex.EncodeToString(sum[:])
	sourceID := store.GoUUID(src.ID)

	dup, err := h.checkDedup(ctx, sourceID, bodyHash)
	if err != nil {
		h.Log.Warn("ingest: dedup check failed (ignored)", "err", err)
	}

	reqID := uuid.New()
	bodyRef := "pg:" + reqID.String()

	req, err := h.Queries.CreateRequest(ctx, store.CreateRequestParams{
		ID:          store.UUID(reqID),
		SourceID:    src.ID,
		HTTPMethod:  r.Method,
		HTTPPath:    r.URL.Path,
		Headers:     captureHeaders(r.Header),
		BodyHash:    bodyHash,
		BodyRef:     bodyRef,
		BodySize:    int32(len(body)),
		ContentType: optStr(r.Header.Get("Content-Type")),
		SigVerified: false, // TODO(phase-2): signature verify
		IngestIP:    parseRemoteAddr(r),
	})
	if err != nil {
		h.Log.Error("ingest: create request", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if _, err := h.BodyStore.Put(ctx, store.GoUUID(req.ID), body); err != nil {
		h.Log.Error("ingest: store body", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	resp := ingestResponse{RequestID: reqID.String()}

	if dup {
		resp.Deduped = true
		writeJSON(w, http.StatusAccepted, resp)
		return
	}

	conns, err := h.Queries.ListEnabledConnectionsBySource(ctx, src.ID)
	if err != nil {
		h.Log.Error("ingest: list connections", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	for _, c := range conns {
		ev, err := h.Queries.CreateEvent(ctx, store.CreateEventParams{
			RequestID:    req.ID,
			ConnectionID: c.ID,
		})
		if err != nil {
			h.Log.Error("ingest: create event", "err", err, "connection_id", store.GoUUID(c.ID))
			continue
		}
		if _, err := h.Queue.EnqueueDeliver(ctx, queue.DeliverPayload{
			EventID:    store.GoUUID(ev.ID),
			Attempt:    0,
			EnqueuedAt: time.Now().UnixMilli(),
		}, int(c.MaxRetries)); err != nil {
			h.Log.Error("ingest: enqueue delivery", "err", err, "event_id", store.GoUUID(ev.ID))
			continue
		}
		resp.EventIDs = append(resp.EventIDs, store.GoUUID(ev.ID).String())
	}

	writeJSON(w, http.StatusAccepted, resp)
}

func (h *Handler) resolveSource(ctx context.Context, token string) (store.Source, error) {
	src, err := h.Queries.GetSourceByIngestToken(ctx, token)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Source{}, ErrSourceNotFound
		}
		return store.Source{}, err
	}
	return src, nil
}

// checkDedup returns true if the body is a duplicate of one seen within the
// dedup window for this source.
func (h *Handler) checkDedup(ctx context.Context, sourceID uuid.UUID, bodyHash string) (bool, error) {
	key := "dedup:" + sourceID.String() + ":" + bodyHash
	ok, err := h.Redis.SetNX(ctx, key, 1, DedupWindow).Result()
	if err != nil {
		return false, err
	}
	// SetNX returns true if the key was newly set — i.e. NOT a duplicate.
	return !ok, nil
}

func captureHeaders(h http.Header) []byte {
	out := make(map[string][]string, len(h))
	for k, v := range h {
		out[k] = v
	}
	b, _ := json.Marshal(out)
	return b
}

func parseRemoteAddr(r *http.Request) *netip.Addr {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if comma := strings.IndexByte(xff, ','); comma >= 0 {
			xff = xff[:comma]
		}
		if addr, err := netip.ParseAddr(strings.TrimSpace(xff)); err == nil {
			return &addr
		}
	}
	host := r.RemoteAddr
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return nil
	}
	return &addr
}

func optStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
