-- name: CreateOrganization :one
INSERT INTO organizations (name, slug) VALUES ($1, $2) RETURNING *;

-- name: GetOrganizationBySlug :one
SELECT * FROM organizations WHERE slug = $1;

-- name: CreateProject :one
INSERT INTO projects (org_id, name, slug) VALUES ($1, $2, $3) RETURNING *;

-- name: GetProjectByOrgAndSlug :one
SELECT * FROM projects WHERE org_id = $1 AND slug = $2;

-- name: ListProjectsByOrg :many
SELECT * FROM projects WHERE org_id = $1 ORDER BY created_at DESC;

-- name: CreateUser :one
INSERT INTO users (email, name) VALUES ($1, $2) RETURNING *;

-- name: GetUserByEmail :one
SELECT * FROM users WHERE email = $1;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = $1;

-- name: PromoteUserToSuperAdmin :exec
UPDATE users SET is_super_admin = TRUE, updated_at = now() WHERE email = $1;

-- name: AddOrgMember :exec
INSERT INTO org_members (org_id, user_id, role)
VALUES ($1, $2, $3)
ON CONFLICT (org_id, user_id) DO NOTHING;

-- name: ListOrgMembers :many
SELECT u.* FROM users u
JOIN org_members m ON m.user_id = u.id
WHERE m.org_id = $1;
