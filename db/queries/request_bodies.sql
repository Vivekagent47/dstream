-- name: InsertRequestBody :exec
INSERT INTO request_bodies (request_id, body) VALUES ($1, $2);

-- name: GetRequestBody :one
SELECT body FROM request_bodies WHERE request_id = $1;
