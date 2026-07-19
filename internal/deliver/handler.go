package deliver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-redis/redis_rate/v10"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"

	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/ingest"
	"github.com/Vivekagent47/dstream/internal/store"
)

const (
	DeliveryTimeout = 30 * time.Second

	// Reaper cadence + safety window. reapStuckAfter must exceed the delivery
	// timeout AND the CLI response timeout so live deliveries are never reaped.
	reapInterval   = 30 * time.Second
	reapStuckAfter = 2 * time.Minute
	reapBatch      = 100

	// cliWaitTimeout is how long a CLI-destined event keeps re-queuing while no
	// tunnel is connected before it's discarded (terminal; manual retry only).
	cliWaitTimeout = 2 * time.Minute
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
	Queue     *dqueue.Client

	// PerOrgMaxInflight caps concurrent in-flight deliveries per org across the
	// whole worker fleet (0 = disabled). Set by the worker from config.
	PerOrgMaxInflight int
}

func New(
	log *slog.Logger,
	q *store.Queries,
	rdb *redis.Client,
	bs ingest.BodyStore,
	dq *dqueue.Client,
	allowPrivateDestinations bool,
) *Handler {
	return &Handler{
		Log:       log,
		Queries:   q,
		Redis:     rdb,
		Limiter:   redis_rate.NewLimiter(rdb),
		BodyStore: bs,
		HTTP:      newSafeHTTPClient(DeliveryTimeout, allowPrivateDestinations),
		Queue:     dq,
	}
}

// Process delivers one event picked off the fair queue. The payload arrives
// already decoded; raw is the queue member Ack/DeadLetter operate on. At-least-
// once: the event stays leased in dq:processing until this returns after a
// terminal step (delivered / dead-lettered / rescheduled+Ack). Returning a
// non-nil error WITHOUT Ack leaves it leased so the recoverer redelivers it.
func (h *Handler) Process(ctx context.Context, p dqueue.Payload, raw string) error {
	queuedFor := time.Duration(0)
	if p.EnqueuedAt > 0 {
		queuedFor = time.Since(time.UnixMilli(p.EnqueuedAt))
	}

	row, err := h.Queries.GetEventForDelivery(ctx, store.UUID(p.EventID))
	if err != nil {
		return fmt.Errorf("load event: %w", err)
	}

	if row.DestinationType == "cli" {
		return h.dispatchToCLI(ctx, row, p, raw)
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
		_ = h.Queue.DeadLetter(ctx, raw)
		return nil
	}

	// Per-org in-flight gate. Caps how many deliveries a single org runs at once
	// across the whole worker fleet (the counter lives in Redis), so one org's
	// slow/failing endpoint can't occupy the shared pool and starve other orgs.
	// Same lease pattern as the per-destination gate below. Disabled (no-op) when
	// PerOrgMaxInflight <= 0, which is the single-tenant default.
	if h.PerOrgMaxInflight > 0 {
		orgKey := "inflight:org:" + store.GoUUID(row.OrgID).String()
		ttlSec := int(DeliveryTimeout * 5 / time.Second)
		n, err := inflightIncrScript.Run(ctx, h.Redis, []string{orgKey}, ttlSec).Int64()
		if err == nil {
			if n > int64(h.PerOrgMaxInflight) {
				_, _ = h.Redis.Decr(ctx, orgKey).Result()
				return h.defer_(ctx, p, raw, 500*time.Millisecond, "per-org inflight")
			}
			defer h.Redis.Decr(ctx, orgKey)
		}
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
			// Re-schedule with delay; do NOT count against retry budget.
			retryAfter := res.RetryAfter
			if retryAfter <= 0 {
				retryAfter = 100 * time.Millisecond
			}
			return h.defer_(ctx, p, raw, retryAfter, "rate-limit")
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
				return h.defer_(ctx, p, raw, 250*time.Millisecond, "dest inflight")
			}
			defer h.Redis.Decr(ctx, inflightKey)
		}
	}

	// Mark in-flight.
	_ = h.Queries.MarkEventInFlight(ctx, row.ID)

	// failAttempt is the shared retry/terminate tail: bump the attempt and either
	// dead-letter (budget exhausted) or re-schedule with the policy backoff, then
	// Ack. p.Attempt plays asynq's old retry count; max_retries=N ⇒ N+1 total
	// executions (fail#N+1 dead-letters). A Schedule/Ack Redis error returns
	// without Ack so the leased member stays in dq:processing for the recoverer —
	// never Ack before the event terminates. Used by the delivery-failure branch
	// below and the two infra-error paths (missing body ref, request build) so a
	// permanently-broken event terminates instead of being redelivered forever.
	failAttempt := func() error {
		p.Attempt++
		if p.Attempt > int(row.MaxRetries) {
			_ = h.Queries.MarkEventFailed(ctx, row.ID)
			// DeadLetter is terminal (it ZREMs from processing itself, so no Ack).
			// On error, leave the event leased so the recoverer redelivers it.
			return h.Queue.DeadLetter(ctx, raw)
		}
		delay := RetryDelay(connFromRow(row), p.Attempt)
		if err := h.Queue.Schedule(ctx, p, time.Now().Add(delay).UnixMilli()); err != nil {
			return err // leave in processing; recoverer will retry
		}
		return h.Queue.Ack(ctx, raw)
	}

	body, err := h.BodyStore.Get(ctx, row.BodyRef)
	if err != nil {
		// Body ref may be permanently missing; count it against the retry budget
		// and terminate rather than looping the recoverer forever on a poison pill.
		h.recordAttempt(ctx, row.ID, int(row.AttemptCount)+1, nil, nil, nil, queuedFor, time.Duration(0), err)
		return failAttempt()
	}

	headers, _ := unmarshalHeaders(row.RequestHeaders)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", *row.DestinationUrl, bytes.NewReader(body))
	if err != nil {
		h.recordAttempt(ctx, row.ID, int(row.AttemptCount)+1, nil, nil, nil, queuedFor, time.Duration(0), err)
		return failAttempt()
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
		// Delivery failure: run the shared retry/terminate tail (attempt already
		// recorded above).
		return failAttempt()
	}

	_ = h.Queries.MarkEventDelivered(ctx, row.ID)
	return h.Queue.Ack(ctx, raw)
}

// defer_ re-schedules p after delay (a gate deferral: rate-limit / in-flight
// backoff, NOT a retry — does not touch Attempt) and Acks the leased member so
// the slot frees. On a Schedule/Ack error it returns the error without Ack so
// the recoverer redelivers rather than dropping the event.
func (h *Handler) defer_(ctx context.Context, p dqueue.Payload, raw string, delay time.Duration, what string) error {
	p.EnqueuedAt = time.Now().UnixMilli()
	if err := h.Queue.Schedule(ctx, p, time.Now().Add(delay).UnixMilli()); err != nil {
		return fmt.Errorf("%s reschedule: %w", what, err)
	}
	return h.Queue.Ack(ctx, raw)
}

// connFromRow projects the delivery row's retry-policy columns into a
// store.Connection so RetryDelay (which reads only those fields) can compute
// the backoff without a second DB read.
func connFromRow(row store.GetEventForDeliveryRow) store.Connection {
	return store.Connection{
		MaxRetries:          row.MaxRetries,
		RetryStrategy:       row.RetryStrategy,
		RetryBaseMs:         row.RetryBaseMs,
		RetryCapMs:          row.RetryCapMs,
		RetryJitterPct:      row.RetryJitterPct,
		CustomRetrySchedule: row.CustomRetrySchedule,
	}
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

// dispatchToCLI hands the event off to a live CLI tunnel via Redis. The CLI
// WebSocket handler does the actual forwarding + attempt recording, so on
// handoff this path just Acks the leased member.
func (h *Handler) dispatchToCLI(ctx context.Context, row store.GetEventForDeliveryRow, p dqueue.Payload, raw string) error {
	manual := p.Manual
	sessionKey := "cli:source:" + store.GoUUID(row.SourceID).String()
	exists, err := h.Redis.Exists(ctx, sessionKey).Result()
	if err != nil {
		return fmt.Errorf("cli session check: %w", err)
	}
	if exists == 0 {
		// No live CLI. Give the tunnel a grace window to (re)connect, but don't
		// re-queue forever: once the event has waited past cliWaitTimeout, drop it
		// as `discarded`. A manual retry skips the grace window — the user asked
		// explicitly, so with no tunnel we discard immediately. Either way they
		// reconnect the CLI and retry if they still want it delivered.
		if manual || !row.CreatedAt.Valid || time.Since(row.CreatedAt.Time) > cliWaitTimeout {
			if err := h.Queries.MarkEventDiscarded(ctx, row.ID); err != nil {
				h.Log.Error("cli discard", "err", err, "event_id", store.GoUUID(row.ID))
			}
			return h.Queue.Ack(ctx, raw)
		}
		// Still within the grace window: re-schedule with backoff so the event
		// isn't dropped. Not a retry — leave p.Attempt untouched.
		_ = h.Queries.ResetEventForRetry(ctx, store.ResetEventForRetryParams{
			ID: row.ID,
			NextRetryAt: pgtype.Timestamptz{
				Time:  time.Now().Add(15 * time.Second),
				Valid: true,
			},
		})
		return h.defer_(ctx, p, raw, 15*time.Second, "cli")
	}

	dispatchKey := "cli:dispatch:" + store.GoUUID(row.SourceID).String()
	payload, _ := json.Marshal(map[string]any{"event_id": store.GoUUID(row.ID).String()})
	if err := h.Redis.RPush(ctx, dispatchKey, payload).Err(); err != nil {
		return fmt.Errorf("cli rpush: %w", err)
	}
	// CLI WS handler will record attempt + update status; we hand off and Ack.
	return h.Queue.Ack(ctx, raw)
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
		if eerr := h.Queue.Enqueue(ctx, dqueue.Payload{
			EventID:             store.GoUUID(ev.ID),
			OrgID:               store.GoUUID(ev.OrgID),
			Attempt:             0,
			EnqueuedAt:          time.Now().UnixMilli(),
			RetryStrategy:       conn.RetryStrategy,
			RetryBaseMs:         conn.RetryBaseMs,
			RetryCapMs:          conn.RetryCapMs,
			RetryJitterPct:      conn.RetryJitterPct,
			CustomRetrySchedule: conn.CustomRetrySchedule,
		}); eerr != nil {
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
