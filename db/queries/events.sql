-- name: CreateEvent :one
INSERT INTO events (request_id, connection_id, status)
VALUES ($1, $2, 'queued')
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
UPDATE events
SET status          = 'delivered',
    attempt_count   = attempt_count + 1,
    last_attempt_at = now(),
    next_retry_at   = NULL,
    updated_at      = now()
WHERE id = $1;

-- name: MarkEventFailed :exec
UPDATE events
SET status          = 'failed',
    attempt_count   = attempt_count + 1,
    last_attempt_at = now(),
    next_retry_at   = NULL,
    updated_at      = now()
WHERE id = $1;

-- name: MarkEventInFlight :exec
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
UPDATE events
SET status        = 'queued',
    attempt_count = 0,
    next_retry_at = NULL,
    updated_at    = now()
WHERE id = $1;

-- name: ListEventsByOrg :many
SELECT e.*
FROM events e
JOIN connections c ON c.id = e.connection_id
JOIN sources s     ON s.id = c.source_id
WHERE s.org_id = $1
ORDER BY e.created_at DESC
LIMIT $2 OFFSET $3;

-- name: GetEventForOrg :one
SELECT e.*
  FROM events e
  JOIN connections c ON c.id = e.connection_id
  JOIN sources s     ON s.id = c.source_id
 WHERE e.id = $1
   AND s.org_id = $2;
