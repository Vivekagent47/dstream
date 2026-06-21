-- name: CreateAPIKey :one
INSERT INTO api_keys (org_id, name, prefix, key_hash)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetAPIKeyByPrefix :one
SELECT * FROM api_keys
WHERE prefix = $1 AND revoked_at IS NULL;

-- name: ListAPIKeysByOrg :many
SELECT * FROM api_keys
WHERE org_id = $1 AND revoked_at IS NULL
ORDER BY created_at DESC;

-- name: TouchAPIKey :exec
-- Debounced: skip the write (and the WAL row / heap update / index churn)
-- when the key was already touched within the past minute. Authenticated
-- API traffic can exceed 1 req/s per key; without this gate the api_keys
-- table is a write hotspot on the hot path. last_used_at accuracy at
-- 1-minute granularity is the trade-off — good enough for "when did this
-- key last work?" answer in the UI.
UPDATE api_keys
   SET last_used_at = now()
 WHERE id = $1
   AND (last_used_at IS NULL OR last_used_at < now() - INTERVAL '1 minute');

-- name: RevokeAPIKeyForOrg :exec
UPDATE api_keys
   SET revoked_at = now()
 WHERE id = $1 AND org_id = $2 AND revoked_at IS NULL;
