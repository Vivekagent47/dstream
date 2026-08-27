-- name: CreateApplication :one
INSERT INTO applications (org_id, uid, name, metadata)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: ListApplicationsByOrg :many
SELECT * FROM applications WHERE org_id = $1 ORDER BY created_at DESC LIMIT $2;

-- name: GetApplicationForOrg :one
SELECT * FROM applications WHERE id = $1 AND org_id = $2;

-- name: UpdateApplication :one
UPDATE applications
   SET name     = COALESCE(sqlc.narg('name'), name),
       uid      = COALESCE(sqlc.narg('uid'), uid),
       metadata = COALESCE(sqlc.narg('metadata')::jsonb, metadata),
       updated_at = now()
 WHERE id = sqlc.arg('id') AND org_id = sqlc.arg('org_id')
 RETURNING *;

-- name: DeleteApplicationForOrg :one
DELETE FROM applications WHERE id = $1 AND org_id = $2 RETURNING id;
