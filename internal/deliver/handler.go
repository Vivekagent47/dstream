package deliver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-redis/redis_rate/v10"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"

	"github.com/Vivekagent47/dstream/internal/ingest"
	"github.com/Vivekagent47/dstream/internal/queue"
	"github.com/Vivekagent47/dstream/internal/store"
)

const (
	DeliveryTimeout = 30 * time.Second

	// Reaper cadence + safety window. reapStuckAfter must exceed the delivery
	// timeout AND the CLI response timeout so live deliveries are never reaped.
	reapInterval   = 30 * time.Second
	reapStuckAfter = 2 * time.Minute
	reapBatch      = 100
)

// inflightIncrScript atomically increments the per-destination in-flight
// counter and (re)sets its TTL, so the slot lease can never be left without an
// expiry — the non-atomic INCR-then-EXPIRE it replaces could leak a slot
// permanently (wedging the destination) if the worker died between the calls.
var inflightIncrScript = redis.NewScript(`
local n = redis.call('INCR', KEYS[1])
redis.call('EXPIRE', KEYS[1], ARGV[1])
return n
`)

type Handler struct {
	Log       *slog.Logger
	Queries   *store.Queries
	Redis     *redis.Client
	Limiter   *redis_rate.Limiter
	BodyStore ingest.BodyStore
	HTTP      *http.Client
	Enqueuer  *queue.Client
}

func New(
	log *slog.Logger,
	q *store.Queries,
	rdb *redis.Client,
	bs ingest.BodyStore,
	enq *queue.Client,
	allowPrivateDestinations bool,
) *Handler {
	return &Handler{
		Log:       log,
		Queries:   q,
		Redis:     rdb,
		Limiter:   redis_rate.NewLimiter(rdb),
		BodyStore: bs,
		HTTP:      newSafeHTTPClient(DeliveryTimeout, allowPrivateDestinations),
		Enqueuer:  enq,
	}
}

// Register binds the handler to its asynq task name.
func (h *Handler) Register(mux *asynq.ServeMux) {
	mux.HandleFunc(queue.TaskDeliver, h.handle)
}

func (h *Handler) handle(ctx context.Context, task *asynq.Task) error {
	var p queue.DeliverPayload
	if err := json.Unmarshal(task.Payload(), &p); err != nil {
		return fmt.Errorf("unmarshal payload: %w", err)
	}
	queuedFor := time.Duration(0)
	if p.EnqueuedAt > 0 {
		queuedFor = time.Since(time.UnixMilli(p.EnqueuedAt))
	}

	row, err := h.Queries.GetEventForDelivery(ctx, store.UUID(p.EventID))
	if err != nil {
		return fmt.Errorf("load event: %w", err)
	}

	if row.DestinationType == "cli" {
		return h.dispatchToCLI(ctx, row)
	}
	if row.DestinationType != "http" {
		return fmt.Errorf("delivery type %q not implemented", row.DestinationType)
	}
	if row.DestinationUrl == nil || *row.DestinationUrl == "" {
		return fmt.Errorf("destination has no URL")
	}
	// Reject structurally-unsafe URLs up front. A bad scheme can never
	// succeed, so don't burn the retry budget on it — record and skip. IP-level
	// blocking (internal/metadata addresses, DNS-rebinding) is enforced at dial
	// time by the SSRF guard in newSafeHTTPClient and surfaces as a normal
	// delivery failure below.
	if err := ValidateDestinationURL(*row.DestinationUrl); err != nil {
		h.recordAttempt(ctx, row.ID, int(row.AttemptCount)+1, nil, nil, nil, queuedFor, time.Duration(0), err)
		_ = h.Queries.MarkEventFailed(ctx, row.ID)
		return asynq.SkipRetry
	}

	// Rate-limit gate (per destination).
	if row.DestinationRateLimitRps != nil && *row.DestinationRateLimitRps > 0 {
		key := "rl:dest:" + store.GoUUID(row.DestinationID).String()
		burst := int(*row.DestinationRateLimitRps)
		if row.DestinationRateLimitBurst != nil && *row.DestinationRateLimitBurst > 0 {
			burst = int(*row.DestinationRateLimitBurst)
		}
		res, err := h.Limiter.Allow(ctx, key, redis_rate.Limit{
			Rate:   int(*row.DestinationRateLimitRps),
			Burst:  burst,
			Period: time.Second,
		})
		if err == nil && res.Allowed == 0 {
			// Re-enqueue with delay; do NOT count against retry budget.
			retryAfter := res.RetryAfter
			if retryAfter <= 0 {
				retryAfter = 100 * time.Millisecond
			}
			if _, err := h.Enqueuer.EnqueueDeliver(ctx, queue.DeliverPayload{
				EventID:             p.EventID,
				Attempt:             p.Attempt,
				EnqueuedAt:          time.Now().UnixMilli(),
				RetryStrategy:       p.RetryStrategy,
				RetryBaseMs:         p.RetryBaseMs,
				RetryCapMs:          p.RetryCapMs,
				RetryJitterPct:      p.RetryJitterPct,
				CustomRetrySchedule: p.CustomRetrySchedule,
			}, int(row.MaxRetries), asynq.ProcessIn(retryAfter)); err != nil {
				return fmt.Errorf("rate-limit reenqueue: %w", err)
			}
			return asynq.SkipRetry
		}
	}

	// Max in-flight gate (per destination).
	if row.DestinationMaxInflight != nil && *row.DestinationMaxInflight > 0 {
		inflightKey := "inflight:dest:" + store.GoUUID(row.DestinationID).String()
		// Slot lease: INCR + EXPIRE atomically (~5x delivery timeout) so the
		// counter always carries a TTL and a crashed worker can't leak a slot.
		ttlSec := int(DeliveryTimeout * 5 / time.Second)
		count, err := inflightIncrScript.Run(ctx, h.Redis, []string{inflightKey}, ttlSec).Int64()
		if err == nil {
			if count > int64(*row.DestinationMaxInflight) {
				_, _ = h.Redis.Decr(ctx, inflightKey).Result()
				if _, err := h.Enqueuer.EnqueueDeliver(ctx, queue.DeliverPayload{
					EventID:             p.EventID,
					Attempt:             p.Attempt,
					EnqueuedAt:          time.Now().UnixMilli(),
					RetryStrategy:       p.RetryStrategy,
					RetryBaseMs:         p.RetryBaseMs,
					RetryCapMs:          p.RetryCapMs,
					RetryJitterPct:      p.RetryJitterPct,
					CustomRetrySchedule: p.CustomRetrySchedule,
				}, int(row.MaxRetries), asynq.ProcessIn(250*time.Millisecond)); err != nil {
					return fmt.Errorf("inflight reenqueue: %w", err)
				}
				return asynq.SkipRetry
			}
			defer h.Redis.Decr(ctx, inflightKey)
		}
	}

	// Mark in-flight.
	_ = h.Queries.MarkEventInFlight(ctx, row.ID)

	body, err := h.BodyStore.Get(ctx, row.BodyRef)
	if err != nil {
		// Body ref must exist; treat as permanent failure for this attempt.
		h.recordAttempt(ctx, row.ID, int(row.AttemptCount)+1, nil, nil, nil, queuedFor, time.Duration(0), err)
		return err
	}

	headers, _ := unmarshalHeaders(row.RequestHeaders)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", *row.DestinationUrl, bytes.NewReader(body))
	if err != nil {
		return err
	}
	for k, vs := range headers {
		// Never forward credentials meant for dstream (Authorization, Cookie)
		// or hop-by-hop headers to a user-controlled destination URL.
		if !forwardableHeader(k) {
			continue
		}
		for _, v := range vs {
			httpReq.Header.Add(k, v)
		}
	}
	httpReq.Header.Set("Dstream-Event-Id", store.GoUUID(row.ID).String())
	httpReq.Header.Set("Dstream-Event-Attempt", fmt.Sprintf("%d", row.AttemptCount+1))

	start := time.Now()
	resp, doErr := h.HTTP.Do(httpReq)
	dur := time.Since(start)

	var (
		respStatus  *int32
		respHeaders []byte
		respBody    []byte
	)
	if doErr == nil {
		defer resp.Body.Close()
		s := int32(resp.StatusCode)
		respStatus = &s
		respHeaders, _ = json.Marshal(resp.Header)
		respBody, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // cap 1 MiB
	}

	h.recordAttempt(ctx, row.ID, int(row.AttemptCount)+1, respStatus, respHeaders, respBody, queuedFor, dur, doErr)

	if doErr != nil || respStatus == nil || *respStatus < 200 || *respStatus >= 300 {
		// Failure — return error so asynq retries. RetryDelayFunc decides the schedule.
		if doErr != nil {
			return doErr
		}
		return fmt.Errorf("non-2xx response: %d", *respStatus)
	}

	_ = h.Queries.MarkEventDelivered(ctx, row.ID)
	return nil
}

func (h *Handler) recordAttempt(
	ctx context.Context,
	eventID pgtype.UUID,
	attemptNum int,
	respStatus *int32,
	respHeaders []byte,
	respBody []byte,
	queuedFor time.Duration,
	dur time.Duration,
	deliverErr error,
) {
	durMs := int32(dur / time.Millisecond)
	queuedMs := int32(queuedFor / time.Millisecond)
	var errStr *string
	if deliverErr != nil {
		s := deliverErr.Error()
		errStr = &s
	}
	if _, err := h.Queries.CreateAttempt(ctx, store.CreateAttemptParams{
		EventID:         eventID,
		AttemptNum:      int32(attemptNum),
		ResponseStatus:  respStatus,
		ResponseHeaders: respHeaders,
		ResponseBody:    respBody,
		DurationMs:      &durMs,
		QueuedInMs:      &queuedMs,
		ErrorMessage:    errStr,
	}); err != nil {
		h.Log.Error("deliver: record attempt", "err", err, "event_id", store.GoUUID(eventID))
	}
}

// RetryDelayFunc returns an asynq RetryDelayFunc that consults the connection
// policy attached to the task payload. If the payload has the policy fields
// populated (new code path), we compute the delay with ZERO DB queries. If
// the payload is older / lacks the snapshot, we fall back to the legacy
// 2-query lookup so in-flight tasks during a deploy don't break.
//
// Trade-off: policy edits made AFTER an event is enqueued won't apply to
// subsequent retries of that event — only to events enqueued post-edit.
// For a retry policy this is acceptable; operators rarely tune retry
// strategy mid-incident, and live edits already weren't atomic.
func (h *Handler) RetryDelayFunc() asynq.RetryDelayFunc {
	return func(n int, taskErr error, task *asynq.Task) time.Duration {
		var p queue.DeliverPayload
		if err := json.Unmarshal(task.Payload(), &p); err != nil {
			return defaultRetry(n)
		}
		// asynq calls RetryDelayFunc with n = attempt number that just failed (1-based).
		attempt := n + 1
		if p.RetryStrategy != "" {
			return RetryDelay(store.Connection{
				RetryStrategy:       p.RetryStrategy,
				RetryBaseMs:         p.RetryBaseMs,
				RetryCapMs:          p.RetryCapMs,
				RetryJitterPct:      p.RetryJitterPct,
				CustomRetrySchedule: p.CustomRetrySchedule,
			}, attempt)
		}
		// Legacy fallback for tasks enqueued before payload carried policy.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		ev, err := h.Queries.GetEventByID(ctx, store.UUID(p.EventID))
		if err != nil {
			return defaultRetry(n)
		}
		conn, err := h.Queries.GetConnectionByID(ctx, ev.ConnectionID)
		if err != nil {
			return defaultRetry(n)
		}
		return RetryDelay(conn, attempt)
	}
}

func defaultRetry(n int) time.Duration {
	d := time.Duration(30*int64(n*n)) * time.Second
	if d > time.Hour {
		d = time.Hour
	}
	return d
}

// dispatchToCLI hands the event off to a live CLI tunnel via Redis. The CLI
// WebSocket handler does the actual forwarding + attempt recording, so this
// path returns asynq.SkipRetry on success.
func (h *Handler) dispatchToCLI(ctx context.Context, row store.GetEventForDeliveryRow) error {
	sessionKey := "cli:source:" + store.GoUUID(row.SourceID).String()
	exists, err := h.Redis.Exists(ctx, sessionKey).Result()
	if err != nil {
		return fmt.Errorf("cli session check: %w", err)
	}
	if exists == 0 {
		// No live CLI: re-queue with backoff so the event isn't dropped.
		_ = h.Queries.ResetEventForRetry(ctx, store.ResetEventForRetryParams{
			ID: row.ID,
			NextRetryAt: pgtype.Timestamptz{
				Time:  time.Now().Add(15 * time.Second),
				Valid: true,
			},
		})
		if _, err := h.Enqueuer.EnqueueDeliver(ctx, queue.DeliverPayload{
			EventID:             store.GoUUID(row.ID),
			Attempt:             int(row.AttemptCount),
			EnqueuedAt:          time.Now().UnixMilli(),
			RetryStrategy:       row.RetryStrategy,
			RetryBaseMs:         row.RetryBaseMs,
			RetryCapMs:          row.RetryCapMs,
			RetryJitterPct:      row.RetryJitterPct,
			CustomRetrySchedule: row.CustomRetrySchedule,
		}, int(row.MaxRetries), asynq.ProcessIn(15*time.Second)); err != nil {
			return fmt.Errorf("cli reenqueue: %w", err)
		}
		return asynq.SkipRetry
	}

	dispatchKey := "cli:dispatch:" + store.GoUUID(row.SourceID).String()
	payload, _ := json.Marshal(map[string]any{"event_id": store.GoUUID(row.ID).String()})
	if err := h.Redis.RPush(ctx, dispatchKey, payload).Err(); err != nil {
		return fmt.Errorf("cli rpush: %w", err)
	}
	// CLI WS handler will record attempt + update status; we hand off.
	return asynq.SkipRetry
}

// RunReaper periodically re-queues stuck events until ctx is cancelled. Run one
// per worker process; ClaimStuckEvents is safe across replicas.
func (h *Handler) RunReaper(ctx context.Context) {
	t := time.NewTicker(reapInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := h.ReapStuckEvents(ctx); err != nil {
				h.Log.Error("reaper: sweep failed", "err", err)
			}
		}
	}
}

// ReapStuckEvents re-enqueues events that are stuck with no forward progress:
// an ingest enqueue that failed (status stays 'queued') or a worker/CLI that
// died mid-delivery (stuck 'in_flight'). Returns the count reclaimed. The claim
// is atomic (FOR UPDATE SKIP LOCKED) so concurrent reapers don't double-claim.
func (h *Handler) ReapStuckEvents(ctx context.Context) (int, error) {
	rows, err := h.Queries.ClaimStuckEvents(ctx, store.ClaimStuckEventsParams{
		StuckBefore: pgtype.Timestamptz{Time: time.Now().Add(-reapStuckAfter), Valid: true},
		RowLimit:    reapBatch,
	})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, ev := range rows {
		conn, cerr := h.Queries.GetConnectionByID(ctx, ev.ConnectionID)
		if cerr != nil {
			h.Log.Error("reaper: load connection", "err", cerr, "event_id", store.GoUUID(ev.ID))
			continue
		}
		if _, eerr := h.Enqueuer.EnqueueDeliver(ctx, queue.DeliverPayload{
			EventID:             store.GoUUID(ev.ID),
			Attempt:             0,
			EnqueuedAt:          time.Now().UnixMilli(),
			RetryStrategy:       conn.RetryStrategy,
			RetryBaseMs:         conn.RetryBaseMs,
			RetryCapMs:          conn.RetryCapMs,
			RetryJitterPct:      conn.RetryJitterPct,
			CustomRetrySchedule: conn.CustomRetrySchedule,
		}, int(conn.MaxRetries)); eerr != nil {
			h.Log.Error("reaper: re-enqueue", "err", eerr, "event_id", store.GoUUID(ev.ID))
			continue
		}
		n++
	}
	if n > 0 {
		h.Log.Info("reaper: re-queued stuck events", "count", n)
	}
	return n, nil
}

func unmarshalHeaders(raw []byte) (map[string][]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var m map[string][]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// Compile-time unused-import guards.
var _ = errors.New
var _ = uuid.Nil
var _ = pgtype.UUID{}
