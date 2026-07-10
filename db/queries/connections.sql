-- name: CreateConnection :one
INSERT INTO connections (source_id, destination_id, enabled, name)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetConnectionByID :one
SELECT * FROM connections WHERE id = $1;

-- name: ListEnabledConnectionsBySource :many
SELECT * FROM connections
WHERE source_id = $1 AND enabled = TRUE;

-- name: ListConnectionsByOrg :many
SELECT c.*
  FROM connections c
  JOIN sources s ON s.id = c.source_id
 WHERE s.org_id = $1
 ORDER BY c.created_at DESC;

-- name: GetConnectionForOrg :one
SELECT c.*
  FROM connections c
  JOIN sources s ON s.id = c.source_id
 WHERE c.id = $1
   AND s.org_id = $2;

-- name: DeleteConnectionForOrg :exec
DELETE FROM connections AS c
 WHERE c.id = $1
   AND c.source_id IN (SELECT s.id FROM sources s WHERE s.org_id = $2);

-- name: PatchConnectionForOrg :one
-- COALESCE pattern so unspecified fields keep current values.
-- Tenancy is enforced by joining through sources.org_id.
UPDATE connections AS c
   SET enabled               = COALESCE(sqlc.narg('enabled'),               c.enabled),
       name                  = COALESCE(sqlc.narg('name'),                  c.name),
       max_retries           = COALESCE(sqlc.narg('max_retries'),           c.max_retries),
       retry_strategy        = COALESCE(sqlc.narg('retry_strategy'),        c.retry_strategy),
       retry_base_ms         = COALESCE(sqlc.narg('retry_base_ms'),         c.retry_base_ms),
       retry_cap_ms          = COALESCE(sqlc.narg('retry_cap_ms'),          c.retry_cap_ms),
       retry_jitter_pct      = COALESCE(sqlc.narg('retry_jitter_pct'),      c.retry_jitter_pct),
       custom_retry_schedule = COALESCE(sqlc.narg('custom_retry_schedule'), c.custom_retry_schedule),
       updated_at            = now()
  FROM sources s
 WHERE c.id = sqlc.arg('id')
   AND c.source_id = s.id
   AND s.org_id = sqlc.arg('org_id')
 RETURNING c.*;
