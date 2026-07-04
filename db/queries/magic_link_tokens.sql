-- name: CreateMagicLinkToken :one
INSERT INTO magic_link_tokens (email, token_hash, expires_at)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetActiveMagicLinkToken :one
SELECT * FROM magic_link_tokens
WHERE token_hash = $1
  AND used_at IS NULL
  AND expires_at > now();

-- name: MarkMagicLinkUsed :exec
UPDATE magic_link_tokens SET used_at = now() WHERE id = $1;
