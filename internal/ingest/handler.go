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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-redis/redis_rate/v10"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
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

	// Per-source ingest rate limit (token bucket in Redis). Limiter is nil or
	// RateLimitRPS<=0 → disabled. Guards the DB/queue from a single source
	// (or a leaked ingest token) flooding the endpoint.
	Limiter        *redis_rate.Limiter
	RateLimitRPS   int
	RateLimitBurst int

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
	// GET is intentionally not registered — dstream never ingests over GET.
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		r.Method(m, "/e/{token}", http.HandlerFunc(h.handleIngest))
	}
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

	sourceID := store.GoUUID(src.ID)

	if !methodAllowed(src.AllowedMethods, r.Method) {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Per-source rate limit BEFORE reading the (up to 5 MiB) body, so a flood
	// can't force large reads. Fail-open on limiter error — availability beats
	// strict limiting for inbound webhooks.
	if h.Limiter != nil && h.RateLimitRPS > 0 {
		burst := h.RateLimitBurst
		if burst <= 0 {
			burst = h.RateLimitRPS
		}
		res, rlErr := h.Limiter.Allow(ctx, "ingest:src:"+sourceID.String(), redis_rate.Limit{
			Rate:   h.RateLimitRPS,
			Burst:  burst,
			Period: time.Second,
		})
		if rlErr == nil && res.Allowed == 0 {
			w.Header().Set("Retry-After", strconv.FormatInt(int64(res.RetryAfter.Seconds())+1, 10))
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		http.Error(w, "body too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}

	sum := sha256.Sum256(body)
	bodyHash := hex.EncodeToString(sum[:])

	dup, err := h.checkDedup(ctx, sourceID, bodyHash)
	if err != nil {
		h.Log.Warn("ingest: dedup check failed (ignored)", "err", err)
	}

	// v7 so the request id sorts by creation time and clusters in the PK
	// B-tree (matches the uuidv7() column defaults). NewV7 only errs if the
	// system RNG fails, which is catastrophic — Must is appropriate.
	reqID := uuid.Must(uuid.NewV7())
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
		// Signature verification removed — auth is post-release scope (see
		// PLAN.md ingest path). Column kept so no migration when it lands.
		SigVerified: false,
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
	if len(conns) == 0 {
		// Source has no enabled connections — nothing to fan out to. Still a
		// successful ingest (the request + body are persisted for replay).
		writeJSON(w, http.StatusAccepted, resp)
		return
	}

	// Fan out all events in ONE insert instead of a per-connection roundtrip.
	// RETURNING order is unspecified, so we key the per-connection retry
	// policy by connection_id rather than positional index.
	connIDs := make([]pgtype.UUID, len(conns))
	connByID := make(map[uuid.UUID]store.Connection, len(conns))
	for i, c := range conns {
		connIDs[i] = c.ID
		connByID[store.GoUUID(c.ID)] = c
	}

	events, err := h.Queries.CreateEventsBatch(ctx, store.CreateEventsBatchParams{
		RequestID:     req.ID,
		OrgID:         src.OrgID,
		ConnectionIds: connIDs,
	})
	if err != nil {
		h.Log.Error("ingest: create events batch", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// asynq exposes no batch-enqueue API, so deliveries are still enqueued
	// one task at a time — but these are local-Redis roundtrips, not the
	// per-connection Postgres roundtrips the batch insert above eliminated.
	for _, ev := range events {
		c := connByID[store.GoUUID(ev.ConnectionID)]
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

// InvalidateSource drops a source from the in-process cache so enable/disable
// and allowed-methods edits take effect immediately instead of after
// SourceCacheTTL.
// ponytail: in-process only. If ingest is ever split into its own process,
// this must become a Redis pub/sub invalidation.
func (h *Handler) InvalidateSource(token string) {
	h.sourceCache.Delete(token)
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
