-- name: InsertRequestBody :exec
INSERT INTO request_bodies (request_id, body) VALUES ($1, $2);

-- name: GetRequestBody :one
-- Excludes a retention-expunged (NULL) body so it surfaces as ErrNoRows, taking
-- the delivery worker's missing-body terminate path instead of sending empty.
SELECT body FROM request_bodies WHERE request_id = $1 AND body IS NOT NULL;

-- name: ExpireOldRequestBodies :execrows
UPDATE request_bodies SET body = NULL
 WHERE stored_at < sqlc.arg('cutoff') AND body IS NOT NULL;
