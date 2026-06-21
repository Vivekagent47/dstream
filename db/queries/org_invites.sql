-- name: CreateOrgInvite :one
INSERT INTO org_invites (org_id, email, role, token_hash, invited_by, expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetActiveOrgInviteByTokenHash :one
SELECT i.*, o.name AS org_name
  FROM org_invites i
  JOIN organizations o ON o.id = i.org_id
 WHERE i.token_hash = $1
   AND i.accepted_at IS NULL
   AND i.expires_at > now();

-- name: MarkOrgInviteAccepted :exec
UPDATE org_invites SET accepted_at = now() WHERE id = $1;

-- name: ListPendingOrgInvitesByEmail :many
SELECT * FROM org_invites
 WHERE email = $1
   AND accepted_at IS NULL
   AND expires_at > now();

-- name: ListOrgInvitesByOrg :many
-- Explicit column list (no SELECT i.*) — token_hash is a secret-adjacent
-- value (sha256 of the bearer token) and must never leave the database.
-- invited_by UUID is omitted; we surface invited_by_email for the UI.
SELECT i.id,
       i.org_id,
       i.email,
       i.role,
       i.expires_at,
       i.accepted_at,
       i.created_at,
       u.email AS invited_by_email
  FROM org_invites i
  JOIN users u ON u.id = i.invited_by
 WHERE i.org_id = $1
 ORDER BY i.created_at DESC;

-- name: DeleteOrgInvite :exec
DELETE FROM org_invites WHERE id = $1 AND org_id = $2;
