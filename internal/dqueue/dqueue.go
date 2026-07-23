// Package dqueue is a Redis-backed per-org fair-scheduling delivery queue.
//
// Fairness: pending events are held in one LIST per org (<p>:pending:{org}) and
// a round-robin ring of org ids (<p>:orgs). FairPick pops the front org, takes
// one event, and re-appends the org to the ring iff it still has pending work —
// so a single org's backlog can never starve the others.
//
// At-least-once: a picked event moves to the processing ZSET under a lease
// deadline; Ack removes it, DeadLetter terminates it, and Recover reinjects any
// event whose lease expired (crashed worker). Scheduled retries/backoff live in
// the scheduled ZSET and are promoted to pending by PromoteDue.
//
// Every multi-key mutation is a single Lua script (atomic under Redis's single
// thread), so the queue is correct across multiple worker nodes with no locks.
// The keyspace prefix is passed into each script as ARGV so tests can run
// against a throwaway prefix on a shared Redis.
package dqueue

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// Payload is the unit of work carried through the queue. It mirrors the fields
// the delivery handler needs: which event/org, the attempt count (retry
// ownership), and a snapshot of the connection's retry policy so backoff can be
// computed without a DB read.
type Payload struct {
	EventID             uuid.UUID         `json:"event_id"`
	OrgID               uuid.UUID         `json:"org_id"`
	Attempt             int               `json:"attempt"`
	EnqueuedAt          int64             `json:"enqueued_at_unix_ms"`
	Manual              bool              `json:"manual,omitempty"`
	RetryStrategy       string            `json:"retry_strategy,omitempty"`
	RetryBaseMs         int32             `json:"retry_base_ms,omitempty"`
	RetryCapMs          int32             `json:"retry_cap_ms,omitempty"`
	RetryJitterPct      int32             `json:"retry_jitter_pct,omitempty"`
	CustomRetrySchedule []byte            `json:"custom_retry_schedule,omitempty"`
	Trace               map[string]string `json:"trace,omitempty"`
	// Kind selects the worker handler. Empty (or "delivery") = webhook event
	// delivery (the default path). "email" = a transactional-email task whose
	// details live in Data.
	Kind string `json:"kind,omitempty"`
	// Data is the kind-specific payload for non-delivery tasks (email: a JSON
	// {template,to,vars}). Unused by the delivery path.
	Data []byte `json:"data,omitempty"`
}

// Client is a handle to the queue on a given Redis + keyspace prefix.
type Client struct {
	rdb    *redis.Client
	prefix string
}

// NewClient returns a queue client with the default "dq" prefix.
func NewClient(rdb *redis.Client) *Client {
	return &Client{rdb: rdb, prefix: "dq"}
}

// WithPrefix returns a copy of the client scoped to prefix p (used by tests to
// stay hermetic on a shared Redis).
func (c *Client) WithPrefix(p string) *Client {
	cp := *c
	cp.prefix = p
	return &cp
}

// enqueueScript: RPUSH the event onto the org's pending list; add the org to the
// ring only if this is its first pending event (n==1) so it's in the ring at
// most once; wake a waiter via notify.
var enqueueScript = redis.NewScript(`
local p = ARGV[1]
local org = ARGV[2]
local n = redis.call('RPUSH', p..':pending:'..org, ARGV[3])
if tonumber(n) == 1 then redis.call('RPUSH', p..':orgs', org) end
redis.call('LPUSH', p..':notify', '1')
-- notify is only a wakeup channel; a blocked BRPOP is served by the LPUSH
-- regardless, so cap it to bound memory when workers stay busy (ok=true) and
-- nobody drains it during a large backlog.
redis.call('LTRIM', p..':notify', 0, 1024)
return n
`)

// fairPickScript: pop the front org from the ring, take one event, lease it in
// the processing ZSET, and re-append the org iff it still has pending work.
var fairPickScript = redis.NewScript(`
local p = ARGV[1]
local org = redis.call('LPOP', p..':orgs')
if not org then return false end
local pkey = p..':pending:'..org
local evt = redis.call('LPOP', pkey)
if not evt then return false end
redis.call('ZADD', p..':processing', ARGV[2], evt)
if tonumber(redis.call('LLEN', pkey)) > 0 then redis.call('RPUSH', p..':orgs', org) end
return evt
`)

// deadListCap bounds <p>:dead to the most recent N entries. The dead list is a
// debug tail only — the authoritative terminal state is in Postgres
// (MarkEventFailed) — so without a cap it would accumulate forever and OOM Redis.
const deadListCap = 10000

// deadLetterScript: terminate an event — move it from processing to the dead
// list, then LTRIM the dead list to its most recent deadListCap entries (ARGV[3])
// so it stays a bounded debug tail rather than growing without bound.
var deadLetterScript = redis.NewScript(`
local p = ARGV[1]
redis.call('RPUSH', p..':dead', ARGV[2])
redis.call('LTRIM', p..':dead', -tonumber(ARGV[3]), -1)
redis.call('ZREM', p..':processing', ARGV[2])
return 1
`)

// promoteDueScript: move scheduled events whose time has come into their org's
// pending list (same ring rule as enqueue) and wake waiters. Returns the count.
var promoteDueScript = redis.NewScript(`
local p = ARGV[1]
local due = redis.call('ZRANGEBYSCORE', p..':scheduled', '-inf', ARGV[2], 'LIMIT', 0, ARGV[3])
for _, evt in ipairs(due) do
  redis.call('ZREM', p..':scheduled', evt)
  local org = cjson.decode(evt)['org_id']
  local n = redis.call('RPUSH', p..':pending:'..org, evt)
  if tonumber(n) == 1 then redis.call('RPUSH', p..':orgs', org) end
  redis.call('LPUSH', p..':notify', '1')
end
redis.call('LTRIM', p..':notify', 0, 1024) -- bound memory (see enqueueScript)
return #due
`)

// recoverScript: reinject events whose lease has expired by moving them from
// processing back to scheduled@now, so PromoteDue re-enqueues them. Returns count.
var recoverScript = redis.NewScript(`
local p = ARGV[1]
local expired = redis.call('ZRANGEBYSCORE', p..':processing', '-inf', ARGV[2])
for _, evt in ipairs(expired) do
  redis.call('ZREM', p..':processing', evt)
  redis.call('ZADD', p..':scheduled', ARGV[2], evt)
end
return #expired
`)

// injectTrace writes the current span context into p.Trace so the worker can
// continue the same trace across the Redis hop. Called on first Enqueue only;
// Schedule (retries) preserves the existing carrier.
func injectTrace(ctx context.Context, p *Payload) {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	if len(carrier) > 0 {
		p.Trace = carrier
	}
}

// Enqueue pushes a payload onto its org's pending list, ready for FairPick.
func (c *Client) Enqueue(ctx context.Context, p Payload) error {
	injectTrace(ctx, &p)
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return enqueueScript.Run(ctx, c.rdb, nil, c.prefix, p.OrgID.String(), raw).Err()
}

// Schedule defers a payload until atUnixMs; a scheduler mover (PromoteDue) later
// injects it into the pending ring.
func (c *Client) Schedule(ctx context.Context, p Payload, atUnixMs int64) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return c.rdb.ZAdd(ctx, c.prefix+":scheduled", redis.Z{Score: float64(atUnixMs), Member: raw}).Err()
}

// FairPick takes one event round-robin across orgs and leases it for leaseMs.
// The returned raw member is what Ack/DeadLetter operate on. ok=false means the
// pending ring is currently empty.
func (c *Client) FairPick(ctx context.Context, leaseMs int64) (raw string, p Payload, ok bool, err error) {
	deadline := time.Now().UnixMilli() + leaseMs
	res, err := fairPickScript.Run(ctx, c.rdb, nil, c.prefix, deadline).Result()
	if err == redis.Nil {
		return "", Payload{}, false, nil
	}
	if err != nil {
		return "", Payload{}, false, err
	}
	s, isStr := res.(string)
	if !isStr {
		// script returned false (empty ring / empty list)
		return "", Payload{}, false, nil
	}
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		return "", Payload{}, false, err
	}
	return s, p, true, nil
}

// Ack removes a successfully-processed event from the processing lease set.
func (c *Client) Ack(ctx context.Context, raw string) error {
	return c.rdb.ZRem(ctx, c.prefix+":processing", raw).Err()
}

// DeadLetter terminates an event: move it from processing to the dead list.
func (c *Client) DeadLetter(ctx context.Context, raw string) error {
	return deadLetterScript.Run(ctx, c.rdb, nil, c.prefix, raw, deadListCap).Err()
}

// PromoteDue moves up to limit scheduled events whose time has come into the
// pending ring. Returns how many were promoted.
func (c *Client) PromoteDue(ctx context.Context, nowUnixMs int64, limit int) (int, error) {
	n, err := promoteDueScript.Run(ctx, c.rdb, nil, c.prefix, nowUnixMs, limit).Int()
	if err == redis.Nil {
		return 0, nil
	}
	return n, err
}

// Recover reinjects events whose lease expired (crashed worker) via scheduled@now.
// Returns how many were recovered.
func (c *Client) Recover(ctx context.Context, nowUnixMs int64) (int, error) {
	n, err := recoverScript.Run(ctx, c.rdb, nil, c.prefix, nowUnixMs).Int()
	if err == redis.Nil {
		return 0, nil
	}
	return n, err
}

// WaitNotify blocks up to timeout for a wakeup pushed by Enqueue/PromoteDue.
// A timeout (redis.Nil) is normal and returns nil.
func (c *Client) WaitNotify(ctx context.Context, timeout time.Duration) error {
	err := c.rdb.BRPop(ctx, timeout, c.prefix+":notify").Err()
	if err == redis.Nil {
		return nil
	}
	return err
}

// Stats is an admin snapshot of queue depth.
type Stats struct {
	Pending    int64        `json:"pending"`
	Scheduled  int64        `json:"scheduled"`
	Processing int64        `json:"processing"`
	Dead       int64        `json:"dead"`
	TopOrgs    []OrgPending `json:"top_orgs"`
}

// OrgPending is one org's pending depth, for the TopOrgs breakdown.
type OrgPending struct {
	OrgID   string `json:"org_id"`
	Pending int64  `json:"pending"`
}

// Stats reports queue depth for the admin console. It scans <p>:pending:* with
// KEYS — this is admin-only and infrequent, so the O(n) scan is acceptable.
func (c *Client) Stats(ctx context.Context) (Stats, error) {
	var s Stats

	pendingPrefix := c.prefix + ":pending:"
	keys, err := c.rdb.Keys(ctx, pendingPrefix+"*").Result()
	if err != nil {
		return s, err
	}
	for _, key := range keys {
		n, err := c.rdb.LLen(ctx, key).Result()
		if err != nil {
			return s, err
		}
		s.Pending += n
		s.TopOrgs = append(s.TopOrgs, OrgPending{
			OrgID:   strings.TrimPrefix(key, pendingPrefix),
			Pending: n,
		})
	}
	sort.Slice(s.TopOrgs, func(i, j int) bool { return s.TopOrgs[i].Pending > s.TopOrgs[j].Pending })
	if len(s.TopOrgs) > 10 {
		s.TopOrgs = s.TopOrgs[:10]
	}

	if s.Scheduled, err = c.rdb.ZCard(ctx, c.prefix+":scheduled").Result(); err != nil {
		return s, err
	}
	if s.Processing, err = c.rdb.ZCard(ctx, c.prefix+":processing").Result(); err != nil {
		return s, err
	}
	if s.Dead, err = c.rdb.LLen(ctx, c.prefix+":dead").Result(); err != nil {
		return s, err
	}
	return s, nil
}
