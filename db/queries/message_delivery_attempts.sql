-- name: CreateMessageDeliveryAttempt :one
INSERT INTO message_delivery_attempts
  (delivery_id, attempt_num, response_status, response_headers, response_body, duration_ms, error_message, attempted_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, now())
RETURNING *;

-- name: ListAttemptsByMessage :many
SELECT a.* FROM message_delivery_attempts a
  JOIN message_deliveries d ON d.id = a.delivery_id
 WHERE d.message_id = $1
 ORDER BY a.attempted_at DESC;

-- name: ListAttemptsByEndpoint :many
SELECT a.* FROM message_delivery_attempts a
  JOIN message_deliveries d ON d.id = a.delivery_id
 WHERE d.endpoint_id = $1
 ORDER BY a.attempted_at DESC LIMIT $2;
