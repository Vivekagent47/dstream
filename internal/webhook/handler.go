package webhook

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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"

	"github.com/Vivekagent47/dstream/internal/deliver"
	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/filter"
	"github.com/Vivekagent47/dstream/internal/opevents"
	"github.com/Vivekagent47/dstream/internal/store"
	"github.com/Vivekagent47/dstream/internal/transform"
)

// Handler delivers one signed outbound webhook (a message_delivery) off the
// queue. It owns the queue lifecycle (Ack/Schedule/DeadLetter) for its task,
// mirroring mailer.EmailHandler.
type Handler struct {
	Log     *slog.Logger
	Queries *store.Queries
	HTTP    *http.Client
	Redis   *redis.Client
	// MaxConsecutiveFailures auto-disables an endpoint after this many
	// back-to-back dead deliveries. 0 disables the feature.
	MaxConsecutiveFailures int
	// PerOrgMaxInflight caps concurrent in-flight deliveries per org across the
	// whole worker fleet, sharing inbound deliver's inflight:org:<org> budget
	// (0 = disabled). Set by the worker from config.
	PerOrgMaxInflight int
	// Limiter enforces the per-endpoint rate_limit (nil disables the gate).
	Limiter *redis_rate.Limiter
	// TransformTimeout bounds each outbound transform's wall-clock run.
	// Zero → effTransformTimeout's 1s default (Task 6 sets it from config).
	TransformTimeout time.Duration
	// TransformMaxOutput caps a transform's output in bytes.
	// Zero → effTransformMaxOutput's 5 MiB default.
	TransformMaxOutput int
}

// effTransformTimeout / effTransformMaxOutput default a zero-value Handler at
// point-of-use — the struct has no constructor, and a literal 0 timeout would
// make time.AfterFunc fire instantly.
func (h Handler) effTransformTimeout() time.Duration {
	if h.TransformTimeout > 0 {
		return h.TransformTimeout
	}
	return time.Second
}

func (h Handler) effTransformMaxOutput() int {
	if h.TransformMaxOutput > 0 {
		return h.TransformMaxOutput
	}
	return 5 << 20
}

const inflightTTL = 150 * time.Second // 5x the delivery timeout, matches deliver's lease

var inflightScript = redis.NewScript(`
local n = redis.call('INCR', KEYS[1])
redis.call('EXPIRE', KEYS[1], ARGV[1])
return n`)

func deliveryID(p dqueue.Payload) (uuid.UUID, error) {
	var t struct {
		DeliveryID string `json:"delivery_id"`
	}
	if err := json.Unmarshal(p.Data, &t); err != nil {
		return uuid.Nil, err
	}
	return uuid.Parse(t.DeliveryID)
}

// signHeader returns the space-separated webhook-signature value: the current
// secret always, plus the previous secret while it is within its grace window.
func signHeader(cur string, prev *string, prevExp pgtype.Timestamptz, msgID string, ts int64, payload []byte) (string, error) {
	sig, err := Sign(cur, msgID, ts, payload)
	if err != nil {
		return "", err
	}
	if prev != nil && *prev != "" && prevExp.Valid && prevExp.Time.After(time.Now()) {
		if ps, perr := Sign(*prev, msgID, ts, payload); perr == nil {
			sig = sig + " " + ps
		}
	}
	return sig, nil
}

// Process handles one leased Kind:"message" task.
func (h Handler) Process(ctx context.Context, p dqueue.Payload, raw string, q *dqueue.Client) error {
	did, err := deliveryID(p)
	if err != nil {
		h.Log.Error("outbound: bad message task, dead-lettering", "err", err)
		return q.DeadLetter(ctx, raw)
	}
	row, err := h.Queries.GetMessageDeliveryForSend(ctx, store.UUID(did))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return q.Ack(ctx, raw) // delivery/message/endpoint deleted
		}
		return err // transient DB error: leave leased for the recoverer
	}

	// Terminal idempotency guard.
	switch row.DeliveryStatus {
	case "delivered", "dead", "disabled", "filtered":
		return q.Ack(ctx, raw)
	}
	// Endpoint disabled after enqueue: skip, no attempt, no retry.
	if row.EndpointDisabled {
		_ = h.Queries.MarkDeliveryDisabled(ctx, store.UUID(did))
		return q.Ack(ctx, raw)
	}

	// Per-org in-flight gate, shared with inbound deliver via inflight:org:<org>:
	// an org's total inbound+outbound in-flight is one budget. Over cap → defer
	// (reschedule + Ack) with no attempt and no retry budget consumed.
	orgID := store.GoUUID(row.OrgID)
	if h.PerOrgMaxInflight > 0 && h.Redis != nil {
		key := fmt.Sprintf("inflight:org:%s", orgID)
		n, ierr := inflightScript.Run(ctx, h.Redis, []string{key}, int(inflightTTL.Seconds())).Int()
		if ierr == nil {
			if n > h.PerOrgMaxInflight {
				h.Redis.Decr(context.Background(), key)
				return h.deferSend(ctx, q, p, raw, 500*time.Millisecond)
			}
			// release the slot after the send completes (background ctx survives cancel)
			defer h.Redis.Decr(context.Background(), key)
		}
	}

	attemptNum := int(row.AttemptCount) + 1

	// Payload expunged by the retention sweep: nothing to sign or send, and no
	// retry can ever recover it → terminal. An empty JSON body compacts to `{}`
	// (2 bytes), so len==0 uniquely means NULL/expunged.
	if len(row.Payload) == 0 {
		h.recordAttempt(ctx, did, attemptNum, 0, nil, nil, 0, "message payload expunged (retention)")
		_ = h.Queries.MarkDeliveryDead(ctx, store.UUID(did))
		return q.DeadLetter(ctx, raw)
	}

	// Filter → transform, before signing. Placed ahead of the rate-limit/URL
	// gates so a filtered event drops cleanly (no defer churn, no dead-letter,
	// no auto-disable count) regardless of endpoint URL or rate state.
	epHeaders, _ := unmarshalHeaderMap(row.EndpointHeaders) // nil on parse error is fine here
	if row.EndpointFilterExpr != nil && *row.EndpointFilterExpr != "" {
		ok, ferr := filter.Match(*row.EndpointFilterExpr, true, row.Payload, epHeaders,
			filter.Meta{EventType: row.EventType, Channels: row.Channels})
		if ferr != nil {
			h.Log.Warn("filter eval error; failing open", "delivery_id", did, "err", ferr) // fail-open: deliver
		} else if !ok {
			_ = h.Queries.MarkDeliveryFiltered(ctx, store.UUID(did))
			return q.Ack(ctx, raw) // terminal 'filtered': no HTTP, no sign
		}
	}
	if row.EndpointTransformJs != nil && *row.EndpointTransformJs != "" {
		out, terr := transform.Apply(*row.EndpointTransformJs, row.Payload, epHeaders, h.effTransformTimeout(), h.effTransformMaxOutput())
		if terr != nil {
			// Deterministic failure → terminal, no retry (same non-retryable
			// sequence as the sign/url branches below).
			h.recordAttempt(ctx, did, attemptNum, 0, nil, nil, 0, "transform: "+terr.Error())
			_ = h.Queries.MarkDeliveryDead(ctx, store.UUID(did))
			return q.DeadLetter(ctx, raw)
		}
		// Reassign ONCE: signHeader and bytes.NewReader below both read
		// row.Payload, so the signature covers the transformed bytes.
		row.Payload = out
	}

	msgID := store.GoUUID(row.MessageID).String()
	epID := store.GoUUID(row.EndpointID)
	// Loop guard: an operational-app delivery that itself exhausts/auto-disables
	// must emit NO operational event, or op events would recurse forever.
	ev := opEventCtx{
		OrgID:         store.GoUUID(row.OrgID),
		MessageID:     store.GoUUID(row.MessageID),
		EventType:     row.EventType,
		IsOperational: row.EndpointAppIsOperational,
	}

	// Per-endpoint rate-limit gate. Over-limit → defer (no attempt, no budget).
	if row.EndpointRateLimit != nil && *row.EndpointRateLimit > 0 && h.Limiter != nil {
		rlKey := "rl:ep:" + epID.String()
		res, rerr := h.Limiter.Allow(ctx, rlKey, redis_rate.Limit{
			Rate:   int(*row.EndpointRateLimit),
			Burst:  int(*row.EndpointRateLimit),
			Period: time.Second,
		})
		if rerr == nil && res.Allowed == 0 {
			delay := res.RetryAfter
			if delay <= 0 {
				delay = 100 * time.Millisecond
			}
			return h.deferSend(ctx, q, p, raw, delay)
		}
	}

	// Structural URL check (dial-time guard is the real boundary). Can never
	// succeed → terminal, does not consume budget.
	if err := deliver.ValidateDestinationURL(row.EndpointUrl); err != nil {
		h.recordAttempt(ctx, did, attemptNum, 0, nil, nil, 0, "invalid url: "+err.Error())
		_ = h.Queries.MarkDeliveryDead(ctx, store.UUID(did))
		return q.DeadLetter(ctx, raw)
	}

	// Sign. A malformed secret can never succeed → terminal.
	ts := row.MessageCreatedAt.Time.Unix()
	sig, err := signHeader(row.EndpointSecret, row.EndpointSecretPrev, row.EndpointPrevExpiresAt, msgID, ts, row.Payload)
	if err != nil {
		h.recordAttempt(ctx, did, attemptNum, 0, nil, nil, 0, "sign: "+err.Error())
		_ = h.Queries.MarkDeliveryDead(ctx, store.UUID(did))
		return q.DeadLetter(ctx, raw)
	}

	// Mark in-flight (the sole attempt_count incrementer).
	if err := h.Queries.MarkDeliveryInFlight(ctx, store.UUID(did)); err != nil {
		return err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, row.EndpointUrl, bytes.NewReader(row.Payload))
	if err != nil {
		return h.failAttempt(ctx, q, p, raw, did, epID, attemptNum, err.Error(), ev)
	}
	// Custom endpoint headers first; the reserved webhook-*/Dstream-*/Content-Type
	// below always overwrite, so a stored custom header can never shadow them.
	if custom, herr := unmarshalHeaderMap(row.EndpointHeaders); herr == nil {
		for k, v := range custom {
			httpReq.Header.Set(k, v)
		}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("webhook-id", msgID)
	httpReq.Header.Set("webhook-timestamp", fmt.Sprintf("%d", ts))
	httpReq.Header.Set("webhook-signature", sig)
	httpReq.Header.Set("Dstream-Webhook-Hops", "1") // outbound send originates the chain

	start := time.Now()
	resp, doErr := h.HTTP.Do(httpReq)
	dur := int(time.Since(start).Milliseconds())
	if doErr != nil {
		h.recordAttempt(ctx, did, attemptNum, 0, nil, nil, dur, doErr.Error())
		return h.failAttempt(ctx, q, p, raw, did, epID, attemptNum, doErr.Error(), ev)
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	h.recordAttempt(ctx, did, attemptNum, resp.StatusCode, headerJSON(resp.Header), respBody, dur, "")

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if err := h.Queries.MarkDeliveryDelivered(ctx, store.UUID(did)); err != nil {
			return err
		}
		if h.MaxConsecutiveFailures > 0 {
			_ = h.Queries.ResetEndpointFailures(ctx, store.UUID(epID))
		}
		return q.Ack(ctx, raw)
	}
	return h.failAttempt(ctx, q, p, raw, did, epID, attemptNum, fmt.Sprintf("status %d", resp.StatusCode), ev)
}

// deferSend reschedules p after delay and Acks the leased member — a gate
// deferral (rate-limit / inflight), NOT a retry: it touches neither
// attempt_count nor the retry budget.
func (h Handler) deferSend(ctx context.Context, q *dqueue.Client, p dqueue.Payload, raw string, delay time.Duration) error {
	p.EnqueuedAt = time.Now().UnixMilli()
	if err := q.Schedule(ctx, p, time.Now().Add(delay).UnixMilli()); err != nil {
		return err
	}
	return q.Ack(ctx, raw)
}

// opEventCtx carries the loop-guard flag + identifiers needed to emit
// operational events when a delivery dead-letters or auto-disables its endpoint.
// IsOperational is true when the delivery itself belongs to the org's
// operational app — such a delivery must emit NO operational event (recursion).
type opEventCtx struct {
	OrgID         uuid.UUID
	MessageID     uuid.UUID
	EventType     string
	IsOperational bool
}

// failAttempt schedules the next retry, or dead-letters when the budget is spent.
func (h Handler) failAttempt(ctx context.Context, q *dqueue.Client, p dqueue.Payload, raw string, did, epID uuid.UUID, attemptNum int, reason string, ev opEventCtx) error {
	delay, ok := nextDelay(attemptNum)
	if !ok {
		h.Log.Warn("outbound: delivery exhausted, dead-lettering", "delivery_id", did, "reason", reason)
		if err := h.Queries.MarkDeliveryDead(ctx, store.UUID(did)); err != nil {
			return err
		}
		// Best-effort operational trigger; never abort the delivery outcome.
		if !ev.IsOperational {
			_ = opevents.Publish(ctx, h.Queries, q, ev.OrgID, "message.attempt.exhausted", map[string]any{
				"type": "message.attempt.exhausted", "message_id": ev.MessageID.String(),
				"endpoint_id": epID.String(), "event_type": ev.EventType, "attempt_count": attemptNum,
			})
		}
		if h.MaxConsecutiveFailures > 0 {
			dr, err := h.Queries.IncrEndpointFailures(ctx, store.IncrEndpointFailuresParams{
				ID: store.UUID(epID), Threshold: int32(h.MaxConsecutiveFailures),
			})
			if err == nil && dr.JustDisabled != nil && *dr.JustDisabled && !ev.IsOperational {
				_ = opevents.Publish(ctx, h.Queries, q, ev.OrgID, "endpoint.disabled", map[string]any{
					"type": "endpoint.disabled", "endpoint_id": epID.String(),
					"app_id": store.GoUUID(dr.AppID).String(), "url": dr.Url,
					"consecutive_failures": dr.ConsecutiveFailures, "disabled_at": dr.DisabledAt.Time,
				})
			}
		}
		return q.DeadLetter(ctx, raw)
	}
	next := time.Now().Add(delay)
	if err := h.Queries.MarkDeliveryForRetry(ctx, store.MarkDeliveryForRetryParams{
		ID:          store.UUID(did),
		NextRetryAt: pgtype.Timestamptz{Time: next, Valid: true},
	}); err != nil {
		return err
	}
	// Schedule THEN Ack (the mailer lesson): Ack-first or Schedule-omitted
	// would let Recover reinject the leased copy → duplicate send.
	if err := q.Schedule(ctx, p, next.UnixMilli()); err != nil {
		return err
	}
	return q.Ack(ctx, raw)
}

func (h Handler) recordAttempt(ctx context.Context, did uuid.UUID, num, status int, headers, body []byte, dur int, errMsg string) {
	var st, dv *int32
	if status != 0 {
		s := int32(status)
		st = &s
	}
	if dur != 0 {
		d := int32(dur)
		dv = &d
	}
	var em *string
	if errMsg != "" {
		em = &errMsg
	}
	if _, err := h.Queries.CreateMessageDeliveryAttempt(ctx, store.CreateMessageDeliveryAttemptParams{
		DeliveryID:      store.UUID(did),
		AttemptNum:      int32(num),
		ResponseStatus:  st,
		ResponseHeaders: headers,
		ResponseBody:    body,
		DurationMs:      dv,
		ErrorMessage:    em,
	}); err != nil {
		h.Log.Error("outbound: record attempt", "err", err, "delivery_id", did)
	}
}

func unmarshalHeaderMap(raw []byte) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func headerJSON(h http.Header) []byte {
	m := make(map[string]string, len(h))
	for k := range h {
		m[k] = h.Get(k)
	}
	b, _ := json.Marshal(m)
	return b
}

// RunReaper re-enqueues message_deliveries stuck 'queued' with no queue entry
// (an Enqueue that failed after the row was written). Runs until ctx is done.
func (h Handler) RunReaper(ctx context.Context, q *dqueue.Client) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			rows, err := h.Queries.ClaimStuckMessageDeliveries(ctx)
			if err != nil {
				h.Log.Error("outbound reaper", "err", err)
				continue
			}
			for _, r := range rows {
				data, _ := json.Marshal(map[string]string{"delivery_id": store.GoUUID(r.ID).String()})
				_ = q.Enqueue(ctx, dqueue.Payload{
					Kind: "message", OrgID: store.GoUUID(r.OrgID),
					EnqueuedAt: time.Now().UnixMilli(), Data: data,
				})
			}
		}
	}
}
