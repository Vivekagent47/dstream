-- name: CreateCaptureRule :one
INSERT INTO capture_rules (org_id, source_id, name, filter_expr, cap, enabled)
VALUES ($1, $2, $3, $4, $5, $6) RETURNING *;

-- name: GetCaptureRuleForOrg :one
SELECT * FROM capture_rules WHERE id = $1 AND org_id = $2;

-- name: ListCaptureRulesForOrg :many
SELECT * FROM capture_rules
WHERE org_id = $1 AND (sqlc.narg('source_id')::uuid IS NULL OR source_id = sqlc.narg('source_id'))
ORDER BY created_at DESC;

-- name: ListEnabledCaptureRulesBySource :many
SELECT id, name, cap, filter_expr FROM capture_rules
WHERE source_id = $1 AND enabled = TRUE;

-- name: UpdateCaptureRule :one
UPDATE capture_rules
SET name = $3, filter_expr = $4, cap = $5, enabled = $6, updated_at = now()
WHERE id = $1 AND org_id = $2 RETURNING *;

-- name: DeleteCaptureRuleForOrg :execrows
DELETE FROM capture_rules WHERE id = $1 AND org_id = $2;
