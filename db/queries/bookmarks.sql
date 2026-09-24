-- name: CreateBookmark :one
INSERT INTO bookmarks (org_id, request_id, name, description, tags)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetBookmarkForOrg :one
SELECT * FROM bookmarks WHERE id = $1 AND org_id = $2;

-- name: ListBookmarksForOrg :many
SELECT b.*, r.source_id, r.http_method, r.http_path, r.received_at AS captured_at
FROM bookmarks b
JOIN requests r ON r.id = b.request_id
WHERE b.org_id = $1
  AND (sqlc.narg('source_id')::uuid IS NULL OR r.source_id = sqlc.narg('source_id'))
  AND (sqlc.narg('tag')::text IS NULL OR sqlc.narg('tag') = ANY(b.tags))
ORDER BY b.created_at DESC;

-- name: DeleteBookmarkForOrg :execrows
DELETE FROM bookmarks WHERE id = $1 AND org_id = $2;
