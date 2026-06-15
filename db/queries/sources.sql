-- name: CreateSource :one
INSERT INTO sources (project_id, name, type, ingest_token, signing_config)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetSourceByID :one
SELECT * FROM sources WHERE id = $1;

-- name: GetSourceByIngestToken :one
SELECT * FROM sources WHERE ingest_token = $1;

-- name: ListSourcesByProject :many
SELECT * FROM sources
WHERE project_id = $1
ORDER BY created_at DESC;

-- name: UpdateSource :one
UPDATE sources
SET name = COALESCE(sqlc.narg('name'), name),
    signing_config = COALESCE(sqlc.narg('signing_config'), signing_config),
    updated_at = now()
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: DeleteSource :exec
DELETE FROM sources WHERE id = $1;
