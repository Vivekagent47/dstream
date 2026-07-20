-- name: CountEventsByStatePerConnection :many
SELECT connection_id, status, count(*) AS n
FROM events
WHERE status IN ('queued', 'failed', 'paused')
GROUP BY connection_id, status;

-- name: ListSourceInfo :many
SELECT id, name FROM sources;

-- name: ListDestinationInfo :many
SELECT id, name FROM destinations;

-- name: ListConnectionInfo :many
SELECT id, name FROM connections;
