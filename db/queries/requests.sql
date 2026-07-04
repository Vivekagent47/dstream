-- name: CreateRequest :one
INSERT INTO requests (
    id, source_id, http_method, http_path, headers, body_hash, body_ref,
    body_size, content_type, sig_verified, ingest_ip
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING *;
