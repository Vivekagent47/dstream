-- name: CreateEndpoint :one
INSERT INTO endpoints (app_id, org_id, uid, url, description, secret, filter_event_types)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: ListEndpointsByApp :many
SELECT * FROM endpoints WHERE app_id = $1 ORDER BY created_at DESC LIMIT $2;

-- name: GetEndpointForApp :one
SELECT * FROM endpoints WHERE id = $1 AND app_id = $2;

-- name: GetEndpointSecret :one
SELECT secret FROM endpoints WHERE id = $1 AND app_id = $2;

-- name: UpdateEndpoint :one
UPDATE endpoints
   SET url         = COALESCE(sqlc.narg('url'), url),
       description = COALESCE(sqlc.narg('description'), description),
       disabled    = COALESCE(sqlc.narg('disabled'), disabled),
       filter_event_types = CASE WHEN sqlc.arg('set_filter')::bool
                                 THEN sqlc.narg('filter_event_types')::text[]
                                 ELSE filter_event_types END,
       updated_at  = now()
 WHERE id = sqlc.arg('id') AND app_id = sqlc.arg('app_id')
 RETURNING *;

-- name: DeleteEndpointForApp :one
DELETE FROM endpoints WHERE id = $1 AND app_id = $2 RETURNING id;

-- name: ListMatchingEndpoints :many
SELECT id FROM endpoints
 WHERE app_id = sqlc.arg('app_id')
   AND disabled = FALSE
   AND (filter_event_types IS NULL
        OR cardinality(filter_event_types) = 0
        OR sqlc.arg('event_type')::text = ANY(filter_event_types));
