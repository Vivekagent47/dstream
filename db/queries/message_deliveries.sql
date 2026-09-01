-- name: CreateMessageDeliveriesBatch :many
INSERT INTO message_deliveries (message_id, endpoint_id, org_id, status)
SELECT @message_id::uuid, ep_id, @org_id::uuid, 'queued'
  FROM unnest(@endpoint_ids::uuid[]) AS ep_id
RETURNING id, endpoint_id;

-- name: ListDeliveriesForMessage :many
SELECT d.id AS delivery_id, d.endpoint_id, e.url AS endpoint_url,
       d.status, d.attempt_count, d.next_retry_at, d.last_attempt_at
  FROM message_deliveries d
  JOIN endpoints e ON e.id = d.endpoint_id
 WHERE d.message_id = $1
 ORDER BY d.created_at, d.id;

-- name: GetMessageDeliveryForSend :one
SELECT d.id AS delivery_id, d.status AS delivery_status, d.attempt_count, d.org_id,
       d.endpoint_id AS endpoint_id,
       m.id AS message_id, m.event_type, m.payload, m.created_at AS message_created_at,
       e.url AS endpoint_url, e.secret AS endpoint_secret, e.disabled AS endpoint_disabled,
       e.prev_secret            AS endpoint_secret_prev,
       e.prev_secret_expires_at AS endpoint_prev_expires_at
  FROM message_deliveries d
  JOIN messages m  ON m.id = d.message_id
  JOIN endpoints e ON e.id = d.endpoint_id
 WHERE d.id = $1;

-- name: MarkDeliveryInFlight :exec
UPDATE message_deliveries
   SET status='in_flight', attempt_count=attempt_count+1, last_attempt_at=now(), updated_at=now()
 WHERE id=$1;

-- name: MarkDeliveryDelivered :exec
UPDATE message_deliveries SET status='delivered', updated_at=now() WHERE id=$1;

-- name: MarkDeliveryDead :exec
UPDATE message_deliveries SET status='dead', updated_at=now() WHERE id=$1;

-- name: MarkDeliveryDisabled :exec
UPDATE message_deliveries SET status='disabled', updated_at=now() WHERE id=$1;

-- name: MarkDeliveryForRetry :exec
UPDATE message_deliveries SET status='queued', next_retry_at=$2, updated_at=now() WHERE id=$1;

-- name: GetDeliveryByMessageEndpoint :one
SELECT * FROM message_deliveries WHERE message_id = $1 AND endpoint_id = $2;

-- name: ResetDeliveryForReplay :exec
UPDATE message_deliveries
   SET status = 'queued', attempt_count = 0, next_retry_at = NULL, updated_at = now()
 WHERE id = $1;

-- name: ListDeadDeliveriesForEndpointSince :many
SELECT id FROM message_deliveries
 WHERE endpoint_id = $1 AND status = 'dead' AND created_at >= sqlc.arg('since')
 ORDER BY created_at
 LIMIT sqlc.arg('lim');

-- name: ClaimStuckMessageDeliveries :many
UPDATE message_deliveries SET updated_at=now()
 WHERE id IN (
   SELECT id FROM message_deliveries
    WHERE status='queued' AND next_retry_at IS NULL AND created_at < now() - interval '15 minutes'
    ORDER BY created_at
    FOR UPDATE SKIP LOCKED
    LIMIT 100)
RETURNING id, org_id;
