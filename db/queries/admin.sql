-- name: CountOrganizations :one
SELECT COUNT(*) FROM organizations;

-- name: CountUsers :one
SELECT COUNT(*) FROM users;

-- name: ListAllOrganizations :many
-- Pinned to the four columns the console's org list renders, not `SELECT *`.
-- The quota and plan columns live on this table, so a bare star widens this
-- row — and anything that serializes it — every time a column is added. The
-- same pin was applied to GetOrganizationByID, GetOrganizationBySlug,
-- CreateOrganization and UpdateOrgName on 2026-10-02 after exactly that
-- happened; this query was the one missed. Widening the list is a deliberate
-- API change, so make it one.
SELECT id, name, slug, created_at
FROM organizations ORDER BY created_at DESC LIMIT 200;
