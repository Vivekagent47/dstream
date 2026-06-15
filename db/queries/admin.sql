-- name: CountOrganizations :one
SELECT COUNT(*) FROM organizations;

-- name: CountUsers :one
SELECT COUNT(*) FROM users;

-- name: ListAllOrganizations :many
SELECT * FROM organizations ORDER BY created_at DESC LIMIT 200;
