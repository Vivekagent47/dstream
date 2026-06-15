-- name: CreateConnection :one
INSERT INTO connections (source_id, destination_id, enabled)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetConnectionByID :one
SELECT * FROM connections WHERE id = $1;

-- name: ListEnabledConnectionsBySource :many
SELECT * FROM connections
WHERE source_id = $1 AND enabled = TRUE;

-- name: ListConnectionsBySource :many
SELECT * FROM connections
WHERE source_id = $1
ORDER BY created_at DESC;

-- name: UpdateConnection :one
UPDATE connections
SET enabled               = COALESCE(sqlc.narg('enabled'),               enabled),
    max_retries           = COALESCE(sqlc.narg('max_retries'),           max_retries),
    retry_strategy        = COALESCE(sqlc.narg('retry_strategy'),        retry_strategy),
    retry_base_ms         = COALESCE(sqlc.narg('retry_base_ms'),         retry_base_ms),
    retry_cap_ms          = COALESCE(sqlc.narg('retry_cap_ms'),          retry_cap_ms),
    retry_jitter_pct      = COALESCE(sqlc.narg('retry_jitter_pct'),      retry_jitter_pct),
    custom_retry_schedule = COALESCE(sqlc.narg('custom_retry_schedule'), custom_retry_schedule),
    updated_at            = now()
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: DeleteConnection :exec
DELETE FROM connections WHERE id = $1;
