-- name: CreateEventsBatch :many
-- Fan-out insert: one queued event per connection_id, all sharing the same
-- request_id and org_id (a source belongs to exactly one org). One statement
-- instead of a roundtrip per destination. RETURNING order is not guaranteed,
-- so callers key the returned rows by connection_id rather than positional
-- index. @is_test is a per-request scalar (a request is wholly test or not).
INSERT INTO events (request_id, connection_id, org_id, status, is_test)
SELECT @request_id, conn_id, @org_id, 'queued', @is_test
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
       e.created_at      AS created_at,
       e.org_id          AS org_id,
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

-- name: MarkEventDiscarded :exec
-- Terminal state for a CLI event that waited past the tunnel deadline with no
-- live listener. Unlike 'failed' it never got a delivery attempt; it's dropped
-- until a user manually retries (ResetEventForManualRetry re-queues it).
UPDATE events
SET status        = 'discarded',
    next_retry_at = NULL,
    updated_at    = now()
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
-- the fair-queue retry counter resets separately by re-enqueuing with Attempt=0.
UPDATE events
SET status        = 'queued',
    next_retry_at = NULL,
    updated_at    = now()
WHERE id = $1;

-- name: ClaimStuckEvents :many
-- Reaper: atomically re-queue events that never entered the dqueue or lost their
-- owner, and are therefore genuinely stuck. Two cases:
--   * 'queued' older than @stuck_before — the ingest Enqueue failed, so the event
--     was never put on the dqueue.
--   * 'in_flight' on a CLI destination — the CLI dispatch path Acks the leased
--     member at handoff to the WS tunnel, so if the tunnel died mid-flight nothing
--     owns the event any more.
-- HTTP 'in_flight' events are deliberately excluded: the dqueue recoverer (the
-- dq:processing lease) plus the scheduled ZSET own their retry backoff AND their
-- worker-death recovery, so reaping them would double-deliver and bypass the
-- backoff schedule. @stuck_before must exceed the delivery + CLI response
-- timeouts. FOR UPDATE OF e SKIP LOCKED lets replicas run concurrently without
-- double-claiming (and locks only events, not the joined rows).
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
RETURNING id, org_id, connection_id, attempt_count;

-- name: ListEvents :many
-- Keyset pagination on (created_at DESC, id DESC). Optional connection_id and
-- status filters use the narg NULL-guard idiom (see audit_logs.sql): a nil
-- param drops the clause. The handler passes connection_id as a Valid pgtype
-- whenever the query param is present (even an all-zero UUID), so a present
-- filter that matches nothing returns empty rather than falling through to
-- unfiltered. events_connection_created_idx serves the connection+order path;
-- events_org_created_idx the org-only path.
SELECT e.*
FROM events e
WHERE e.org_id = @org_id
  AND (sqlc.narg('connection_id')::uuid IS NULL OR e.connection_id = sqlc.narg('connection_id'))
  AND (sqlc.narg('status')::text        IS NULL OR e.status        = sqlc.narg('status'))
  AND (sqlc.narg('after')::timestamptz  IS NULL OR e.created_at >= sqlc.narg('after'))
  AND (
    @before_created_at::timestamptz IS NULL
    OR (e.created_at, e.id) < (@before_created_at::timestamptz, @before_id::uuid)
  )
ORDER BY e.created_at DESC, e.id DESC
LIMIT @page_limit;

-- name: EventsHistogram :many
-- Time-bucketed event counts by status for the events-page timeline graph.
-- @bucket is a date_trunc unit ('minute' | 'hour' | 'day' | 'week') chosen by
-- the handler from the selected range. The series is GAP-FILLED in SQL:
-- generate_series emits every bucket from date_trunc(@after) through now(), so
-- quiet buckets come back as one row with a NULL status and count 0 — the client
-- plots the rows as-is, no reconstruction. Buckets are UTC-aligned. Same optional
-- connection_id/status filters as ListEvents; includes test events.
WITH series AS (
  SELECT gs AS bucket
  FROM generate_series(
    date_trunc(@bucket::text, @after::timestamptz),
    date_trunc(@bucket::text, now()),
    ('1 ' || @bucket::text)::interval
  ) AS gs
),
counts AS (
  SELECT date_trunc(@bucket::text, e.created_at) AS bucket,
         e.status AS status,
         count(*) AS count
  FROM events e
  WHERE e.org_id = @org_id
    AND (sqlc.narg('connection_id')::uuid IS NULL OR e.connection_id = sqlc.narg('connection_id'))
    AND (sqlc.narg('status')::text        IS NULL OR e.status        = sqlc.narg('status'))
    AND e.created_at >= @after::timestamptz
  GROUP BY 1, 2
)
SELECT s.bucket::timestamptz     AS bucket,
       c.status                  AS status,
       coalesce(c.count, 0)::bigint AS count
FROM series s
LEFT JOIN counts c ON c.bucket = s.bucket
ORDER BY s.bucket;

-- name: GetEventForOrg :one
-- org_id lives on events now, so this is a direct two-column lookup (PK + org)
-- instead of a join through connections/sources.
SELECT e.*
  FROM events e
 WHERE e.id = $1
   AND e.org_id = $2;

-- name: GetEventDetailForOrg :one
-- Full event view for the detail page: the event, its connection's
-- source/destination, the destination endpoint, and the originating request
-- (method/path/headers/body pointer). One org-scoped row that backs the
-- Overview + Request-data panels.
SELECT e.id, e.request_id, e.connection_id, e.status, e.attempt_count,
       e.last_attempt_at, e.next_retry_at, e.created_at, e.updated_at, e.is_test,
       c.source_id      AS source_id,
       c.destination_id AS destination_id,
       d.type           AS destination_type,
       d.url            AS destination_url,
       r.http_method    AS http_method,
       r.http_path      AS http_path,
       r.headers        AS request_headers,
       r.body_ref       AS body_ref,
       r.body_size      AS body_size,
       r.content_type   AS content_type
  FROM events e
  JOIN connections c  ON c.id = e.connection_id
  JOIN destinations d ON d.id = c.destination_id
  JOIN requests r     ON r.id = e.request_id
 WHERE e.id = $1
   AND e.org_id = $2;

-- name: CountEventsByConnectionSince :many
-- Per-status event counts for one connection over a recent window, excluding
-- synthetic test events so health metrics reflect real traffic. Caller passes
-- the window start; folds the rows into delivered/failed/pending buckets.
SELECT e.status AS status, count(*) AS count
FROM events e
WHERE e.connection_id = @connection_id
  AND e.org_id = @org_id
  AND e.is_test = FALSE
  AND e.created_at > @since::timestamptz
GROUP BY e.status;

-- name: CountEventsByOrgGroupedByConnection :many
-- Per-connection, per-status counts for a whole org over a recent window,
-- excluding test events. One query feeds the connections-list stat column
-- (avoids an N+1 of CountEventsByConnectionSince). Handler folds by
-- connection_id into delivered/failed/pending buckets.
SELECT e.connection_id AS connection_id, e.status AS status, count(*) AS count
FROM events e
WHERE e.org_id = @org_id
  AND e.is_test = FALSE
  AND e.created_at > @since::timestamptz
GROUP BY e.connection_id, e.status;

-- name: DestinationDeliveryHistogram :many
-- Gap-filled delivery outcomes over time for ONE destination. Events reach a
-- destination through their connection, so join connections to filter. Same
-- generate_series gap-fill + UTC buckets as EventsHistogram; empty buckets come
-- back as a NULL-status row with count 0.
WITH series AS (
  SELECT gs AS bucket
  FROM generate_series(
    date_trunc(@bucket::text, @after::timestamptz),
    date_trunc(@bucket::text, now()),
    ('1 ' || @bucket::text)::interval
  ) AS gs
),
counts AS (
  SELECT date_trunc(@bucket::text, e.created_at) AS bucket, e.status AS status, count(*) AS count
  FROM events e
  JOIN connections c ON c.id = e.connection_id
  WHERE c.destination_id = @destination_id
    AND e.org_id = @org_id
    AND e.created_at >= @after::timestamptz
  GROUP BY 1, 2
)
SELECT s.bucket::timestamptz     AS bucket,
       c.status                  AS status,
       coalesce(c.count, 0)::bigint AS count
FROM series s
LEFT JOIN counts c ON c.bucket = s.bucket
ORDER BY s.bucket;

-- name: DestinationDeliveryStats :one
-- Window totals for the delivery-rate + avg-latency cards. delivered/total gives
-- the rate; avg_latency_ms averages the delivery HTTP call time recorded per
-- attempt (NULL when no completed attempts in the window).
SELECT
  count(*)::bigint AS total,
  count(*) FILTER (WHERE e.status = 'delivered')::bigint AS delivered,
  coalesce((SELECT avg(a.duration_ms)
   FROM attempts a
   JOIN events e2 ON e2.id = a.event_id
   JOIN connections c2 ON c2.id = e2.connection_id
   WHERE c2.destination_id = @destination_id
     AND e2.org_id = @org_id
     AND a.attempted_at >= @after::timestamptz
     AND a.duration_ms IS NOT NULL), 0)::float8 AS avg_latency_ms
FROM events e
JOIN connections c ON c.id = e.connection_id
WHERE c.destination_id = @destination_id
  AND e.org_id = @org_id
  AND e.created_at >= @after::timestamptz;

-- name: SourceRequestHistogram :many
-- Gap-filled ingest-request volume over time for ONE source (single series, no
-- status dimension). Same gap-fill contract as the delivery histogram.
WITH series AS (
  SELECT gs AS bucket
  FROM generate_series(
    date_trunc(@bucket::text, @after::timestamptz),
    date_trunc(@bucket::text, now()),
    ('1 ' || @bucket::text)::interval
  ) AS gs
),
counts AS (
  SELECT date_trunc(@bucket::text, r.received_at) AS bucket, count(*) AS count
  FROM requests r
  WHERE r.source_id = @source_id
    AND r.received_at >= @after::timestamptz
  GROUP BY 1
)
SELECT s.bucket::timestamptz     AS bucket,
       coalesce(c.count, 0)::bigint AS count
FROM series s
LEFT JOIN counts c ON c.bucket = s.bucket
ORDER BY s.bucket;

-- name: SourceRequestStats :one
-- Window totals for the requests-rate + avg-events-per-request (fan-out) cards.
SELECT
  (SELECT count(*) FROM requests r
   WHERE r.source_id = @source_id AND r.received_at >= @after::timestamptz)::bigint AS requests,
  (SELECT count(*) FROM events e
   JOIN requests r ON r.id = e.request_id
   WHERE r.source_id = @source_id AND e.created_at >= @after::timestamptz)::bigint AS events;
