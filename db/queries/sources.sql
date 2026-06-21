-- name: CreateSource :one
INSERT INTO sources (org_id, name, type, ingest_token, signing_config)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: ListSourcesByOrg :many
SELECT * FROM sources
WHERE org_id = $1
ORDER BY created_at DESC;

-- name: GetSourceByID :one
SELECT * FROM sources WHERE id = $1;

-- name: GetSourceForOrg :one
SELECT * FROM sources WHERE id = $1 AND org_id = $2;

-- name: GetSourceByIngestToken :one
SELECT * FROM sources WHERE ingest_token = $1;

-- name: UpdateSourceForOrg :one
UPDATE sources
SET name = COALESCE(sqlc.narg('name'), name),
    signing_config = COALESCE(sqlc.narg('signing_config'), signing_config),
    updated_at = now()
WHERE id = sqlc.arg('id') AND org_id = sqlc.arg('org_id')
RETURNING *;

-- name: DeleteSourceForOrg :exec
DELETE FROM sources WHERE id = $1 AND org_id = $2;
