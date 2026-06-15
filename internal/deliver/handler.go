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

	"github.com/streamingo/dstream/internal/ingest"
	"github.com/streamingo/dstream/internal/queue"
	"github.com/streamingo/dstream/internal/store"
)

const (
	DeliveryTimeout = 30 * time.Second
)

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
) *Handler {
	return &Handler{
		Log:       log,
		Queries:   q,
		Redis:     rdb,
		Limiter:   redis_rate.NewLimiter(rdb),
		BodyStore: bs,
		HTTP:      &http.Client{Timeout: DeliveryTimeout},
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

	if row.DestinationType != "http" {
		// Phase 1.3: CLI tunnel delivery handled separately.
		return fmt.Errorf("delivery type %q not implemented", row.DestinationType)
	}
	if row.DestinationUrl == nil || *row.DestinationUrl == "" {
		return fmt.Errorf("destination has no URL")
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
				EventID:    p.EventID,
				Attempt:    p.Attempt,
				EnqueuedAt: time.Now().UnixMilli(),
			}, int(row.MaxRetries), asynq.ProcessIn(retryAfter)); err != nil {
				return fmt.Errorf("rate-limit reenqueue: %w", err)
			}
			return asynq.SkipRetry
		}
	}

	// Max in-flight gate (per destination).
	if row.DestinationMaxInflight != nil && *row.DestinationMaxInflight > 0 {
		inflightKey := "inflight:dest:" + store.GoUUID(row.DestinationID).String()
		count, err := h.Redis.Incr(ctx, inflightKey).Result()
		if err == nil {
			// Slot lease: ~5x delivery timeout so a crashed worker eventually frees it.
			h.Redis.Expire(ctx, inflightKey, DeliveryTimeout*5)
			if count > int64(*row.DestinationMaxInflight) {
				_, _ = h.Redis.Decr(ctx, inflightKey).Result()
				if _, err := h.Enqueuer.EnqueueDeliver(ctx, queue.DeliverPayload{
					EventID:    p.EventID,
					Attempt:    p.Attempt,
					EnqueuedAt: time.Now().UnixMilli(),
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
// policy for the given task. It loads the connection from the DB on each
// invocation so live policy edits take effect on the very next retry.
func (h *Handler) RetryDelayFunc() asynq.RetryDelayFunc {
	return func(n int, taskErr error, task *asynq.Task) time.Duration {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		var p queue.DeliverPayload
		if err := json.Unmarshal(task.Payload(), &p); err != nil {
			return defaultRetry(n)
		}
		ev, err := h.Queries.GetEventByID(ctx, store.UUID(p.EventID))
		if err != nil {
			return defaultRetry(n)
		}
		conn, err := h.Queries.GetConnectionByID(ctx, ev.ConnectionID)
		if err != nil {
			return defaultRetry(n)
		}
		// asynq calls RetryDelayFunc with n = attempt number that just failed (1-based).
		return RetryDelay(conn, n+1)
	}
}

func defaultRetry(n int) time.Duration {
	d := time.Duration(30*int64(n*n)) * time.Second
	if d > time.Hour {
		d = time.Hour
	}
	return d
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
