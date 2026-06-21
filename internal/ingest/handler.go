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
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"

	"github.com/Vivekagent47/dstream/internal/queue"
	"github.com/Vivekagent47/dstream/internal/store"
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

	// In-process cache for source lookups keyed by ingest_token. The
	// ingest hot path was hitting Postgres on every webhook (~0.5–1ms per
	// request) even though source rows change rarely; this collapses
	// repeat lookups within the TTL into a single map probe. Cache
	// invalidation on source deletion is implicit via the TTL — a
	// just-deleted source remains addressable for up to SourceCacheTTL,
	// which is acceptable for v1 (the worst case is one extra request
	// queued for an org that just rotated tokens).
	sourceCache sync.Map // map[string]sourceCacheEntry, keyed by ingest_token
}

type sourceCacheEntry struct {
	src     store.Source
	expires time.Time
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
			EventID:             store.GoUUID(ev.ID),
			Attempt:             0,
			EnqueuedAt:          time.Now().UnixMilli(),
			RetryStrategy:       c.RetryStrategy,
			RetryBaseMs:         c.RetryBaseMs,
			RetryCapMs:          c.RetryCapMs,
			RetryJitterPct:      c.RetryJitterPct,
			CustomRetrySchedule: c.CustomRetrySchedule,
		}, int(c.MaxRetries)); err != nil {
			h.Log.Error("ingest: enqueue delivery", "err", err, "event_id", store.GoUUID(ev.ID))
			continue
		}
		resp.EventIDs = append(resp.EventIDs, store.GoUUID(ev.ID).String())
	}

	writeJSON(w, http.StatusAccepted, resp)
}

func (h *Handler) resolveSource(ctx context.Context, token string) (store.Source, error) {
	// Cache hit?
	if v, ok := h.sourceCache.Load(token); ok {
		entry := v.(sourceCacheEntry)
		if time.Now().Before(entry.expires) {
			return entry.src, nil
		}
		// Expired — fall through to a fresh lookup. We delete eagerly to
		// keep the map size bounded even for tokens that stop being
		// presented.
		h.sourceCache.Delete(token)
	}
	src, err := h.Queries.GetSourceByIngestToken(ctx, token)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Source{}, ErrSourceNotFound
		}
		return store.Source{}, err
	}
	h.sourceCache.Store(token, sourceCacheEntry{
		src:     src,
		expires: time.Now().Add(SourceCacheTTL),
	})
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
