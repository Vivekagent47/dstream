-- name: CreateSource :one
INSERT INTO sources (org_id, name, type, description, ingest_token, signing_config)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: ListSourcesByOrg :many
SELECT * FROM sources
WHERE org_id = $1
ORDER BY created_at DESC;

-- name: GetSourceForOrg :one
SELECT * FROM sources WHERE id = $1 AND org_id = $2;

-- name: GetSourceByIngestToken :one
SELECT * FROM sources WHERE ingest_token = $1 AND enabled = TRUE;

-- name: UpdateSource :one
UPDATE sources
   SET name            = COALESCE(sqlc.narg('name'), name),
       description     = COALESCE(sqlc.narg('description'), description),
       allowed_methods = COALESCE(sqlc.narg('allowed_methods')::text[], allowed_methods),
       enabled         = COALESCE(sqlc.narg('enabled'), enabled),
       updated_at      = now()
 WHERE id = sqlc.arg('id') AND org_id = sqlc.arg('org_id')
 RETURNING *;

-- name: DeleteSourceForOrg :one
DELETE FROM sources WHERE id = $1 AND org_id = $2 RETURNING ingest_token;
