-- name: CreateMagicLinkToken :one
INSERT INTO magic_link_tokens (email, token_hash, expires_at)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetActiveMagicLinkToken :one
-- FOR UPDATE locks the row for the consume transaction so two concurrent
-- verifies can't both see it active: the second blocks, then re-checks the
-- WHERE after the first commits used_at and finds no row (single-use enforced).
SELECT * FROM magic_link_tokens
WHERE token_hash = $1
  AND used_at IS NULL
  AND expires_at > now()
FOR UPDATE;

-- name: MarkMagicLinkUsed :exec
UPDATE magic_link_tokens SET used_at = now() WHERE id = $1;
