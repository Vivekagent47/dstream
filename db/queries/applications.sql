-- name: CreateApplication :one
INSERT INTO applications (org_id, uid, name, metadata)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: ListApplicationsByOrg :many
SELECT * FROM applications
 WHERE org_id = $1
   AND is_operational = FALSE
   AND (created_at, id) < (sqlc.arg('cursor_ts')::timestamptz, sqlc.arg('cursor_id')::uuid)
 ORDER BY created_at DESC, id DESC
 LIMIT sqlc.arg('lim');

-- name: EnsureOperationalApp :one
-- Idempotent create-or-get of the org's operational app (one per org via the
-- partial unique index). Returns the existing or newly-created row.
WITH ins AS (
  INSERT INTO applications (org_id, name, is_operational)
  VALUES (sqlc.arg('org_id'), 'Operational Webhooks', TRUE)
  ON CONFLICT (org_id) WHERE is_operational DO NOTHING
  RETURNING *
)
SELECT * FROM ins
UNION ALL
SELECT * FROM applications WHERE org_id = sqlc.arg('org_id') AND is_operational
LIMIT 1;

-- name: GetOperationalApp :one
SELECT * FROM applications WHERE org_id = $1 AND is_operational;

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

-- name: BumpApplicationPortalEpoch :exec
UPDATE applications SET portal_epoch = portal_epoch + 1, updated_at = now()
WHERE id = $1 AND org_id = $2;
