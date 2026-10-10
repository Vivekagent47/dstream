// Package dqueue is a Redis-backed per-org fair-scheduling delivery queue.
//
// Fairness: pending events are held in one LIST per org (<p>:pending:{org}) and
// a round-robin ring of org ids (<p>:orgs). FairPick pops the front org, takes
// one event, and re-appends the org to the ring iff it still has pending work —
// so a single org's backlog can never starve the others.
//
// At-least-once: a picked event moves to the processing ZSET under a lease
// deadline and a per-pick fencing token; Ack removes it, DeadLetter terminates
// it, and Recover reinjects any event whose lease expired (crashed worker). The
// token (prefixed to the processing member, "<token>\x1f<evt>") ensures a stale
// worker whose lease was already recovered can't Ack away the event's newer
// lease — its Ack targets a token that no longer exists. Scheduled retries/
// backoff live in the scheduled ZSET and are promoted to pending by PromoteDue.
//
// Every multi-key mutation is a single Lua script (atomic under Redis's single
// thread), so the queue is correct across multiple worker nodes with no locks.
// The keyspace prefix is passed into each script as ARGV so tests can run
// against a throwaway prefix on a shared Redis.
package dqueue

import (
	"context"
	"encoding/json"
	"fmt"
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
// the processing ZSET under a fencing token, and re-append the org iff it still
// has pending work. The processing member is "<token>\x1f<evt>": the token
// (ARGV[3], unique per pick) fences the lease so a stale holder's Ack/DeadLetter
// — which target this exact member — can't remove a newer lease of the same
// event. \x1f (unit separator) can never occur in JSON, so an untagged member
// left in processing across a rollout still splits to itself (see stripToken).
var fairPickScript = redis.NewScript(`
local p = ARGV[1]
local org = redis.call('LPOP', p..':orgs')
if not org then return false end
local pkey = p..':pending:'..org
local evt = redis.call('LPOP', pkey)
if not evt then return false end
local member = ARGV[3]..'\31'..evt
redis.call('ZADD', p..':processing', ARGV[2], member)
if tonumber(redis.call('LLEN', pkey)) > 0 then redis.call('RPUSH', p..':orgs', org) end
return member
`)

// deadListCap bounds <p>:dead to the most recent N entries. The dead list is a
// debug tail only — the authoritative terminal state is in Postgres
// (MarkEventFailed) — so without a cap it would accumulate forever and OOM Redis.
const deadListCap = 10000

// deadLetterScript: terminate an event — ZREM the exact leased member from
// processing, push the token-stripped payload onto the dead list, then LTRIM the
// dead list to its most recent deadListCap entries (ARGV[3]) so it stays a
// bounded debug tail. The dead list stores the plain payload (valid JSON for
// RequeueDead), never the fencing-tokened member.
var deadLetterScript = redis.NewScript(`
local p = ARGV[1]
local member = ARGV[2]
local evt = member
local i = string.find(member, '\31', 1, true)
if i then evt = string.sub(member, i + 1) end
redis.call('RPUSH', p..':dead', evt)
redis.call('LTRIM', p..':dead', -tonumber(ARGV[3]), -1)
redis.call('ZREM', p..':processing', member)
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
for _, member in ipairs(expired) do
  redis.call('ZREM', p..':processing', member)
  local evt = member
  local i = string.find(member, '\31', 1, true)
  if i then evt = string.sub(member, i + 1) end
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

// leaseSep separates the fencing token from the payload in a processing member.
// \x1f (ASCII unit separator) never appears in JSON, so stripToken is a no-op on
// an untagged member (e.g. one left in processing across a rollout).
const leaseSep = '\x1f'

// stripToken returns the payload portion of a (possibly) fencing-tokened
// processing member: everything after the first leaseSep, or the whole string if
// there is none.
func stripToken(member string) string {
	if i := strings.IndexByte(member, leaseSep); i >= 0 {
		return member[i+1:]
	}
	return member
}

// FairPick takes one event round-robin across orgs and leases it for leaseMs
// under a unique fencing token. The returned raw member (token-prefixed) is what
// Ack/DeadLetter operate on. ok=false means the pending ring is currently empty.
func (c *Client) FairPick(ctx context.Context, leaseMs int64) (raw string, p Payload, ok bool, err error) {
	deadline := time.Now().UnixMilli() + leaseMs
	token := uuid.NewString()
	res, err := fairPickScript.Run(ctx, c.rdb, nil, c.prefix, deadline, token).Result()
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
	if err := json.Unmarshal([]byte(stripToken(s)), &p); err != nil {
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
	// Non-nil so the admin API serializes an empty queue as [] rather than
	// null: every client would otherwise have to special-case null where the
	// field is documented as an array.
	s := Stats{TopOrgs: []OrgPending{}}

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

// Item is one inspectable queue entry for the admin console. Raw is the exact
// Redis member (the identity ops act on). Score-derived fields are 0 when the
// lane has no score (dead/pending). DecodeError is set (and the payload fields
// left zero) when the stored member isn't valid Payload JSON.
type Item struct {
	Raw         string    `json:"raw"`
	EventID     uuid.UUID `json:"event_id"`
	OrgID       uuid.UUID `json:"org_id"`
	Attempt     int       `json:"attempt"`
	EnqueuedAt  int64     `json:"enqueued_at_unix_ms"`
	NextRunMs   int64     `json:"next_run_ms,omitempty"`
	LeaseMs     int64     `json:"lease_deadline_ms,omitempty"`
	DecodeError string    `json:"decode_error,omitempty"`
}

func (c *Client) decodeItem(raw string) Item {
	it := Item{Raw: raw}
	var p Payload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		it.DecodeError = err.Error()
		return it
	}
	it.EventID, it.OrgID, it.Attempt, it.EnqueuedAt = p.EventID, p.OrgID, p.Attempt, p.EnqueuedAt
	return it
}

// Items lists up to `limit` entries in a lane. lane ∈ {dead,scheduled,processing,
// pending}; pending requires org. Returns items + whether more exist beyond limit.
func (c *Client) Items(ctx context.Context, lane, org string, limit int) ([]Item, bool, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 200 {
		limit = 200
	}
	probe := int64(limit) // fetch limit+1 to detect truncation
	switch lane {
	case "dead", "pending":
		key := c.prefix + ":dead"
		if lane == "pending" {
			if org == "" {
				return nil, false, fmt.Errorf("pending lane requires org")
			}
			key = c.prefix + ":pending:" + org
		}
		raws, err := c.rdb.LRange(ctx, key, 0, probe).Result() // 0..limit inclusive = limit+1
		if err != nil {
			return nil, false, err
		}
		trunc := len(raws) > limit
		if trunc {
			raws = raws[:limit]
		}
		items := make([]Item, 0, len(raws))
		for _, r := range raws {
			items = append(items, c.decodeItem(r))
		}
		return items, trunc, nil
	case "scheduled", "processing":
		key := c.prefix + ":scheduled"
		if lane == "processing" {
			key = c.prefix + ":processing"
		}
		zs, err := c.rdb.ZRangeWithScores(ctx, key, 0, probe).Result()
		if err != nil {
			return nil, false, err
		}
		trunc := len(zs) > limit
		if trunc {
			zs = zs[:limit]
		}
		items := make([]Item, 0, len(zs))
		for _, z := range zs {
			// stripToken is a no-op on scheduled members (plain JSON, no token).
			it := c.decodeItem(stripToken(z.Member.(string)))
			if lane == "scheduled" {
				it.NextRunMs = int64(z.Score)
			} else {
				it.LeaseMs = int64(z.Score)
			}
			items = append(items, it)
		}
		return items, trunc, nil
	default:
		return nil, false, fmt.Errorf("unknown lane %q", lane)
	}
}

// AllOrgPending returns every org's pending depth (Stats caps at top-10).
func (c *Client) AllOrgPending(ctx context.Context) ([]OrgPending, error) {
	prefix := c.prefix + ":pending:"
	keys, err := c.rdb.Keys(ctx, prefix+"*").Result()
	if err != nil {
		return nil, err
	}
	out := make([]OrgPending, 0, len(keys))
	for _, k := range keys {
		n, err := c.rdb.LLen(ctx, k).Result()
		if err != nil {
			return nil, err
		}
		out = append(out, OrgPending{OrgID: strings.TrimPrefix(k, prefix), Pending: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pending > out[j].Pending })
	return out, nil
}

// requeueDeadScript: remove the exact member from the dead list; if it was there,
// enqueue the (attempt-reset) payload onto its org's pending list with the same
// ring/notify rules as enqueueScript. ARGV: prefix, oldRaw, newRaw. Returns the
// LREM count (0 = the item had already moved).
var requeueDeadScript = redis.NewScript(`
local p = ARGV[1]
local removed = redis.call('LREM', p..':dead', 1, ARGV[2])
if removed > 0 then
  local org = cjson.decode(ARGV[3])['org_id']
  local n = redis.call('RPUSH', p..':pending:'..org, ARGV[3])
  if tonumber(n) == 1 then redis.call('RPUSH', p..':orgs', org) end
  redis.call('LPUSH', p..':notify', '1')
  redis.call('LTRIM', p..':notify', 0, 1024)
end
return removed
`)

// promoteScheduledScript: remove the member from the scheduled ZSET; if present,
// enqueue it now (attempt unchanged). ARGV: prefix, raw. Returns ZREM count.
var promoteScheduledScript = redis.NewScript(`
local p = ARGV[1]
local removed = redis.call('ZREM', p..':scheduled', ARGV[2])
if removed > 0 then
  local org = cjson.decode(ARGV[2])['org_id']
  local n = redis.call('RPUSH', p..':pending:'..org, ARGV[2])
  if tonumber(n) == 1 then redis.call('RPUSH', p..':orgs', org) end
  redis.call('LPUSH', p..':notify', '1')
  redis.call('LTRIM', p..':notify', 0, 1024)
end
return removed
`)

// drainDeadScript: clear the dead list atomically, returning how many it held.
var drainDeadScript = redis.NewScript(`
local p = ARGV[1]
local n = redis.call('LLEN', p..':dead')
redis.call('DEL', p..':dead')
return n
`)

// RequeueDead moves a dead item back to its org's pending list with attempt
// reset to 0. Returns false if the item was no longer in the dead list.
func (c *Client) RequeueDead(ctx context.Context, raw string) (bool, error) {
	var p Payload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return false, fmt.Errorf("decode dead payload: %w", err)
	}
	p.Attempt = 0
	p.EnqueuedAt = time.Now().UnixMilli()
	newRaw, err := json.Marshal(p)
	if err != nil {
		return false, err
	}
	n, err := requeueDeadScript.Run(ctx, c.rdb, nil, c.prefix, raw, string(newRaw)).Int64()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// PromoteScheduled moves a scheduled item into pending now (attempt unchanged).
// Returns false if it was no longer scheduled.
func (c *Client) PromoteScheduled(ctx context.Context, raw string) (bool, error) {
	var p Payload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return false, fmt.Errorf("decode scheduled payload: %w", err)
	}
	n, err := promoteScheduledScript.Run(ctx, c.rdb, nil, c.prefix, raw).Int64()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// DrainDead clears the dead list, returning the number of entries removed.
func (c *Client) DrainDead(ctx context.Context) (int64, error) {
	return drainDeadScript.Run(ctx, c.rdb, nil, c.prefix).Int64()
}
