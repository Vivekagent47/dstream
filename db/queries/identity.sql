-- name: CreateOrganization :one
INSERT INTO organizations (name, slug) VALUES ($1, $2) RETURNING *;

-- name: GetOrganizationBySlug :one
SELECT * FROM organizations WHERE slug = $1;

-- name: GetOrganizationByID :one
SELECT * FROM organizations WHERE id = $1;

-- name: UpdateOrgName :one
UPDATE organizations
   SET name = $2, updated_at = now()
 WHERE id = $1
 RETURNING *;

-- name: DeleteOrganization :exec
DELETE FROM organizations WHERE id = $1;

-- name: CreateUser :one
INSERT INTO users (email, name) VALUES ($1, $2) RETURNING *;

-- name: GetUserByEmail :one
SELECT * FROM users WHERE email = $1;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = $1;

-- name: PromoteUserToSuperAdmin :exec
UPDATE users SET is_super_admin = TRUE, updated_at = now() WHERE email = $1;

-- name: CountOrgMembershipsForUser :one
SELECT count(*) FROM org_members WHERE user_id = $1;

-- name: AddOrgMember :exec
INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, $3);

-- name: ListOrgMembersByOrg :many
SELECT m.org_id, m.user_id, m.role, m.created_at,
       u.email, u.name
  FROM org_members m
  JOIN users u ON u.id = m.user_id
 WHERE m.org_id = $1
 ORDER BY m.created_at ASC;

-- name: GetOrgMember :one
SELECT * FROM org_members WHERE org_id = $1 AND user_id = $2;

-- name: UpdateOrgMemberRole :exec
UPDATE org_members SET role = $3 WHERE org_id = $1 AND user_id = $2;

-- name: DeleteOrgMember :exec
DELETE FROM org_members WHERE org_id = $1 AND user_id = $2;

-- name: CountOrgOwners :one
SELECT count(*) FROM org_members WHERE org_id = $1 AND role = 'owner';

-- name: DemoteOrgOwnerIfNotLast :execrows
-- Atomic demote: change role to $3 ONLY IF at least one other owner
-- remains. The whole operation is one statement, so two concurrent
-- demote requests can't both pass a count-then-update race.
UPDATE org_members
   SET role = $3
 WHERE org_id = $1
   AND user_id = $2
   AND role = 'owner'
   AND (
     SELECT count(*) FROM org_members
      WHERE org_id = $1 AND role = 'owner'
   ) > 1;

-- name: DeleteOrgOwnerIfNotLast :execrows
-- Atomic remove: delete the owner row ONLY IF at least one other owner
-- remains. Same race-free invariant as DemoteOrgOwnerIfNotLast.
DELETE FROM org_members
 WHERE org_id = $1
   AND user_id = $2
   AND role = 'owner'
   AND (
     SELECT count(*) FROM org_members
      WHERE org_id = $1 AND role = 'owner'
   ) > 1;

-- name: ListOrgsForUser :many
SELECT o.*, m.role
  FROM organizations o
  JOIN org_members m ON m.org_id = o.id
 WHERE m.user_id = $1
 ORDER BY m.created_at ASC, o.id ASC;

-- name: GetFirstOrgForUser :one
SELECT m.org_id
  FROM org_members m
 WHERE m.user_id = $1
 ORDER BY m.created_at ASC, m.org_id ASC
 LIMIT 1;

-- name: TransferOrgOwnership :execrows
-- Atomic transfer: promote $2 to owner, demote the CURRENT owner (which
-- must be the caller, $3) to admin. The two EXISTS guards make the whole
-- thing self-cancelling under races — if the caller has been demoted by a
-- concurrent op, or the target has been removed, the UPDATE matches 0
-- rows and the handler returns 403/400. No SELECT-then-UPDATE TOCTOU.
UPDATE org_members
   SET role = CASE
     WHEN user_id = $2 THEN 'owner'
     WHEN role   = 'owner' AND user_id = $3 THEN 'admin'
     ELSE role
   END
 WHERE org_id = $1
   AND EXISTS (
     SELECT 1 FROM org_members
      WHERE org_id = $1 AND user_id = $3 AND role = 'owner'
   )
   AND EXISTS (
     SELECT 1 FROM org_members
      WHERE org_id = $1 AND user_id = $2
   );
