-- name: CreateAttempt :one
INSERT INTO attempts (
    event_id, attempt_num, response_status, response_headers, response_body,
    duration_ms, queued_in_ms, error_message
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: ListAttemptsByEvent :many
SELECT * FROM attempts
WHERE event_id = $1
ORDER BY attempt_num ASC;

-- name: ExpireOldInboundAttemptBodies :execrows
UPDATE attempts SET response_body = NULL
 WHERE attempted_at < sqlc.arg('cutoff') AND response_body IS NOT NULL;
