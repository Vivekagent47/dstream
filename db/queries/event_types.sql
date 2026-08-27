-- name: CreateEventType :one
INSERT INTO event_types (org_id, name, description, schema)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: ListEventTypesByOrg :many
SELECT * FROM event_types WHERE org_id = $1 ORDER BY name ASC LIMIT $2;

-- name: GetEventTypeForOrg :one
SELECT * FROM event_types WHERE org_id = $1 AND name = $2;

-- name: UpdateEventType :one
UPDATE event_types
   SET description = COALESCE(sqlc.narg('description'), description),
       schema      = COALESCE(sqlc.narg('schema')::jsonb, schema),
       archived    = COALESCE(sqlc.narg('archived'), archived),
       updated_at  = now()
 WHERE org_id = sqlc.arg('org_id') AND name = sqlc.arg('name')
 RETURNING *;

-- name: DeleteEventTypeForOrg :one
DELETE FROM event_types WHERE org_id = $1 AND name = $2 RETURNING id;
