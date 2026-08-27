-- name: CreateMessage :one
INSERT INTO messages (app_id, org_id, event_type, payload, payload_hash, event_id)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (app_id, event_id) WHERE event_id IS NOT NULL DO NOTHING
RETURNING *;

-- name: GetMessageByAppEventID :one
SELECT * FROM messages WHERE app_id = $1 AND event_id = $2;

-- name: ListMessagesByApp :many
SELECT id, app_id, org_id, event_type, payload_hash, event_id, created_at
  FROM messages WHERE app_id = $1 ORDER BY created_at DESC LIMIT $2;

-- name: GetMessageForApp :one
SELECT * FROM messages WHERE id = $1 AND app_id = $2;
