-- name: CreateAPIKey :one
INSERT INTO api_keys (project_id, name, prefix, key_hash)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetAPIKeyByPrefix :one
SELECT * FROM api_keys
WHERE prefix = $1 AND revoked_at IS NULL;

-- name: TouchAPIKey :exec
UPDATE api_keys SET last_used_at = now() WHERE id = $1;

-- name: RevokeAPIKey :exec
UPDATE api_keys SET revoked_at = now() WHERE id = $1;

-- name: ListAPIKeysByProject :many
SELECT * FROM api_keys
WHERE project_id = $1
ORDER BY created_at DESC;
