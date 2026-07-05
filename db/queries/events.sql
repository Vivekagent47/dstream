-- name: CreateEventsBatch :many
-- Fan-out insert: one queued event per connection_id, all sharing the same
-- request_id and org_id (a source belongs to exactly one org). One statement
-- instead of a roundtrip per destination. RETURNING order is not guaranteed,
-- so callers key the returned rows by connection_id rather than positional
-- index.
INSERT INTO events (request_id, connection_id, org_id, status)
SELECT @request_id, conn_id, @org_id, 'queued'
FROM unnest(@connection_ids::uuid[]) AS conn_id
RETURNING *;

-- name: GetEventByID :one
SELECT * FROM events WHERE id = $1;

-- name: GetEventForDelivery :one
SELECT e.id              AS id,
       e.request_id      AS request_id,
       e.connection_id   AS connection_id,
       e.status          AS status,
       e.attempt_count   AS attempt_count,
       c.source_id       AS source_id,
       c.destination_id  AS destination_id,
       c.max_retries     AS max_retries,
       c.retry_strategy  AS retry_strategy,
       c.retry_base_ms   AS retry_base_ms,
       c.retry_cap_ms    AS retry_cap_ms,
       c.retry_jitter_pct AS retry_jitter_pct,
       c.custom_retry_schedule AS custom_retry_schedule,
       d.type            AS destination_type,
       d.url             AS destination_url,
       d.auth_config     AS destination_auth_config,
       d.rate_limit_rps  AS destination_rate_limit_rps,
       d.rate_limit_burst AS destination_rate_limit_burst,
       d.max_inflight    AS destination_max_inflight,
       r.body_ref        AS body_ref,
       r.headers         AS request_headers
FROM events e
JOIN connections c ON c.id = e.connection_id
JOIN destinations d ON d.id = c.destination_id
JOIN requests r ON r.id = e.request_id
WHERE e.id = $1;

-- name: MarkEventDelivered :exec
-- attempt_count is bumped once per delivery cycle by MarkEventInFlight; the
-- terminal transitions must NOT increment again (that double-counted attempts
-- and made recorded attempt_num values skip).
UPDATE events
SET status          = 'delivered',
    last_attempt_at = now(),
    next_retry_at   = NULL,
    updated_at      = now()
WHERE id = $1;

-- name: MarkEventFailed :exec
UPDATE events
SET status          = 'failed',
    last_attempt_at = now(),
    next_retry_at   = NULL,
    updated_at      = now()
WHERE id = $1;

-- name: MarkEventInFlight :exec
-- The single attempt_count incrementer. recordAttempt derives attempt_num from
-- the pre-increment count, so this keeps attempt_num monotonic and gap-free.
UPDATE events
SET status          = 'in_flight',
    attempt_count   = attempt_count + 1,
    last_attempt_at = now(),
    updated_at      = now()
WHERE id = $1;

-- name: ResetEventForRetry :exec
UPDATE events
SET status        = 'queued',
    next_retry_at = $2,
    updated_at    = now()
WHERE id = $1;

-- name: ResetEventForManualRetry :exec
-- Do NOT reset attempt_count: zeroing it made the next attempt reuse
-- attempt_num=1, colliding with the original attempt on UNIQUE(event_id,
-- attempt_num) and silently dropping the retry's attempt row. Keep it monotonic;
-- asynq's own retry budget is reset separately by re-enqueuing with Attempt=0.
UPDATE events
SET status        = 'queued',
    next_retry_at = NULL,
    updated_at    = now()
WHERE id = $1;

-- name: ClaimStuckEvents :many
-- Reaper: atomically re-queue events that have NO pending asynq task and are
-- therefore genuinely stuck. Two cases:
--   * 'queued' older than @stuck_before — the ingest enqueue failed, so asynq
--     never received a task for it.
--   * 'in_flight' on a CLI destination — the CLI dispatch path returns
--     asynq.SkipRetry (handing off to the WS), so if the tunnel died mid-flight
--     nothing will ever retry it.
-- HTTP 'in_flight' events are deliberately excluded: asynq owns their retry
-- backoff AND their worker-death recovery, so reaping them would double-deliver
-- and bypass the backoff schedule. @stuck_before must exceed the delivery + CLI
-- response timeouts. FOR UPDATE OF e SKIP LOCKED lets replicas run concurrently
-- without double-claiming (and locks only events, not the joined rows).
-- ponytail: a pathologically low rate limit (refill > @stuck_before) could let
-- a rate-limit-deferred 'queued' event be reaped early → one duplicate delivery;
-- acceptable under the system's at-least-once contract.
UPDATE events
   SET status = 'queued', next_retry_at = now(), updated_at = now()
 WHERE id IN (
   SELECT e.id
     FROM events e
     JOIN connections c  ON c.id = e.connection_id
     JOIN destinations d ON d.id = c.destination_id
    WHERE e.updated_at < @stuck_before
      AND (
        e.status = 'queued'
        OR (e.status = 'in_flight' AND d.type = 'cli')
      )
    ORDER BY e.updated_at ASC
    LIMIT @row_limit
    FOR UPDATE OF e SKIP LOCKED
 )
RETURNING id, connection_id, attempt_count;

-- name: ListEventsByOrg :many
-- Keyset pagination on (created_at DESC, id DESC), backed by
-- events_org_created_idx. First page passes NULL cursor; each subsequent page
-- passes the last row's (created_at, id). No OFFSET, so page cost is constant
-- regardless of depth.
SELECT e.*
FROM events e
WHERE e.org_id = @org_id
  AND (
    @before_created_at::timestamptz IS NULL
    OR (e.created_at, e.id) < (@before_created_at::timestamptz, @before_id::uuid)
  )
ORDER BY e.created_at DESC, e.id DESC
LIMIT @page_limit;

-- name: GetEventForOrg :one
-- org_id lives on events now, so this is a direct two-column lookup (PK + org)
-- instead of a join through connections/sources.
SELECT e.*
  FROM events e
 WHERE e.id = $1
   AND e.org_id = $2;
