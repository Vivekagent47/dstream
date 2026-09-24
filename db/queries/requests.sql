-- name: CreateRequest :one
INSERT INTO requests (
    id, source_id, http_method, http_path, headers, body_hash, body_ref,
    body_size, content_type, sig_verified, ingest_ip
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING *;

-- name: GetRequestForReplay :one
SELECT r.id, r.source_id, r.http_method, r.http_path, r.headers, r.body_ref,
       r.content_type, r.body_size
FROM requests r
JOIN sources s ON s.id = r.source_id
WHERE r.id = $1 AND s.org_id = $2;
