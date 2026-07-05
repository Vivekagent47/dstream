-- name: CreateSource :one
INSERT INTO sources (org_id, name, type, ingest_token, signing_config)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: ListSourcesByOrg :many
SELECT * FROM sources
WHERE org_id = $1
ORDER BY created_at DESC;

-- name: GetSourceForOrg :one
SELECT * FROM sources WHERE id = $1 AND org_id = $2;

-- name: GetSourceByIngestToken :one
SELECT * FROM sources WHERE ingest_token = $1 AND enabled = TRUE;

-- name: SetSourceEnabled :one
UPDATE sources
   SET enabled = $3, updated_at = now()
 WHERE id = $1 AND org_id = $2
 RETURNING *;

-- name: DeleteSourceForOrg :exec
DELETE FROM sources WHERE id = $1 AND org_id = $2;
