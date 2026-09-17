-- name: CreateEndpoint :one
INSERT INTO endpoints (app_id, org_id, uid, url, description, secret, filter_event_types, headers, rate_limit, channels)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
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
       consecutive_failures = CASE WHEN sqlc.narg('disabled') = FALSE THEN 0 ELSE consecutive_failures END,
       disabled_at          = CASE WHEN sqlc.narg('disabled') = FALSE THEN NULL ELSE disabled_at END,
       filter_event_types = CASE WHEN sqlc.arg('set_filter')::bool
                                 THEN sqlc.narg('filter_event_types')::text[]
                                 ELSE filter_event_types END,
       headers    = CASE WHEN sqlc.arg('set_headers')::bool
                         THEN sqlc.narg('headers')::jsonb ELSE headers END,
       rate_limit = COALESCE(sqlc.narg('rate_limit'), rate_limit),
       channels   = CASE WHEN sqlc.arg('set_channels')::bool
                         THEN sqlc.narg('channels')::text[] ELSE channels END,
       updated_at  = now()
 WHERE id = sqlc.arg('id') AND app_id = sqlc.arg('app_id')
 RETURNING *;

-- name: IncrEndpointFailures :one
WITH prev AS (SELECT ep.id, ep.disabled AS was_disabled FROM endpoints ep WHERE ep.id = sqlc.arg('id'))
UPDATE endpoints e
   SET consecutive_failures = e.consecutive_failures + 1,
       disabled    = (e.consecutive_failures + 1 >= sqlc.arg('threshold')::int) OR e.disabled,
       disabled_at = CASE WHEN (e.consecutive_failures + 1 >= sqlc.arg('threshold')::int) AND NOT e.disabled
                          THEN now() ELSE e.disabled_at END,
       updated_at  = now()
  FROM prev
 WHERE e.id = prev.id
 RETURNING e.id, e.org_id, e.app_id, e.url, e.consecutive_failures, e.disabled_at,
           (NOT prev.was_disabled AND e.disabled) AS just_disabled;

-- name: ResetEndpointFailures :exec
UPDATE endpoints SET consecutive_failures = 0, updated_at = now()
 WHERE id = $1 AND consecutive_failures > 0;

-- name: RotateEndpointSecret :one
UPDATE endpoints
   SET prev_secret            = secret,
       prev_secret_expires_at = sqlc.arg('prev_expires_at'),
       secret                 = sqlc.arg('new_secret'),
       updated_at             = now()
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
        OR sqlc.arg('event_type')::text = ANY(filter_event_types))
   AND (channels IS NULL
        OR cardinality(channels) = 0
        OR channels && sqlc.arg('msg_channels')::text[]);
