-- name: CreateScenario :one
INSERT INTO scenarios (org_id, name, description) VALUES ($1, $2, $3) RETURNING *;

-- name: GetScenarioForOrg :one
SELECT * FROM scenarios WHERE id = $1 AND org_id = $2;

-- name: ListScenariosForOrg :many
SELECT * FROM scenarios WHERE org_id = $1 ORDER BY created_at DESC;

-- name: UpdateScenario :one
UPDATE scenarios SET name = $3, description = $4, updated_at = now()
WHERE id = $1 AND org_id = $2 RETURNING *;

-- name: DeleteScenarioForOrg :execrows
DELETE FROM scenarios WHERE id = $1 AND org_id = $2;

-- name: InsertScenarioStep :one
INSERT INTO scenario_steps (scenario_id, position, bookmark_id, delay_ms)
VALUES ($1, $2, $3, $4) RETURNING *;

-- name: DeleteScenarioSteps :exec
DELETE FROM scenario_steps WHERE scenario_id = $1;

-- name: ListScenarioSteps :many
SELECT s.id, s.scenario_id, s.position, s.bookmark_id, s.delay_ms, b.name AS bookmark_name
FROM scenario_steps s JOIN bookmarks b ON b.id = s.bookmark_id
WHERE s.scenario_id = $1 ORDER BY s.position;
