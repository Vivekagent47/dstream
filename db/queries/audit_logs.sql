-- name: InsertAuditLog :exec
INSERT INTO audit_logs (
    org_id, actor_user_id, actor_api_key_id, actor_email_snapshot,
    action, target_type, target_id, metadata
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: ListAuditLogsByOrg :many
-- LEFT JOIN may yield NULL for u/k columns; COALESCE so sqlc generates
-- non-nullable string fields (sqlc + LEFT JOIN nullability is awkward).
SELECT a.*,
       COALESCE(u.email::text, '')::text AS actor_user_email_join,
       COALESCE(u.name,        '')::text AS actor_user_name_join,
       COALESCE(k.name,        '')::text AS actor_api_key_name_join
  FROM audit_logs a
  LEFT JOIN users    u ON u.id = a.actor_user_id
  LEFT JOIN api_keys k ON k.id = a.actor_api_key_id
 WHERE a.org_id = $1
   AND (sqlc.narg('before_id')::bigint    IS NULL OR a.id           < sqlc.narg('before_id'))
   AND (sqlc.narg('actor_user_id')::uuid  IS NULL OR a.actor_user_id = sqlc.narg('actor_user_id'))
   AND (sqlc.narg('target_type')::text    IS NULL OR a.target_type   = sqlc.narg('target_type'))
   AND (sqlc.narg('action')::text         IS NULL OR a.action        = sqlc.narg('action'))
 ORDER BY a.id DESC
 LIMIT $2;
