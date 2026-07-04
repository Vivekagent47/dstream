-- name: CreateDestination :one
INSERT INTO destinations (org_id, name, type, url, auth_config,
                           rate_limit_rps, rate_limit_burst, max_inflight)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: ListDestinationsByOrg :many
SELECT * FROM destinations
WHERE org_id = $1
ORDER BY created_at DESC;

-- name: GetDestinationForOrg :one
SELECT * FROM destinations WHERE id = $1 AND org_id = $2;

-- name: PatchDestinationForOrg :one
-- COALESCE pattern so unspecified fields keep current values.
UPDATE destinations
   SET name             = COALESCE(sqlc.narg('name'),             name),
       type             = COALESCE(sqlc.narg('type'),             type),
       url              = COALESCE(sqlc.narg('url'),              url),
       auth_config      = COALESCE(sqlc.narg('auth_config'),      auth_config),
       rate_limit_rps   = COALESCE(sqlc.narg('rate_limit_rps'),   rate_limit_rps),
       rate_limit_burst = COALESCE(sqlc.narg('rate_limit_burst'), rate_limit_burst),
       max_inflight     = COALESCE(sqlc.narg('max_inflight'),     max_inflight),
       updated_at       = now()
 WHERE id = $1 AND org_id = $2
 RETURNING *;

-- name: DeleteDestinationForOrg :exec
DELETE FROM destinations WHERE id = $1 AND org_id = $2;
