-- name: UpsertUsageRollup :exec
-- Idempotent by (org_id, period_start, metric): the sweep re-runs the current
-- period on every worker restart, so this must converge rather than add.
INSERT INTO usage_rollups (org_id, period_start, metric, count, updated_at)
VALUES ($1, $2, $3, $4, now())
ON CONFLICT (org_id, period_start, metric)
DO UPDATE SET count = EXCLUDED.count, updated_at = now();

-- name: RollupEvents :many
-- is_test events are excluded from the metered count. Fixture replay is the
-- dev-loop feature dstream sells; throttling or billing someone for exercising
-- it is the wrong default. is_test is set only by dstream's own replay and
-- test-connection paths (internal/bookmark, internal/api/pipeline), never by an
-- inbound webhook, so this is not a quota-evasion vector. The health-metric
-- queries at db/queries/events.sql:250,262 exclude them for the same reason.
SELECT org_id, date_trunc(@bucket::text, created_at)::timestamptz AS period_start,
       count(*)::bigint AS count
FROM events
WHERE created_at >= @after::timestamptz
  AND is_test = FALSE
GROUP BY 1, 2;

-- name: RollupRequests :many
-- requests has no org_id and its timestamp is received_at, not created_at.
SELECT s.org_id AS org_id, date_trunc(@bucket::text, r.received_at)::timestamptz AS period_start,
       count(*)::bigint AS count
FROM requests r
JOIN sources s ON s.id = r.source_id
WHERE r.received_at >= @after::timestamptz
GROUP BY 1, 2;

-- name: RollupMessages :many
SELECT org_id, date_trunc(@bucket::text, created_at)::timestamptz AS period_start,
       count(*)::bigint AS count
FROM messages
WHERE created_at >= @after::timestamptz
GROUP BY 1, 2;

-- name: RollupAttempts :many
-- Both delivery surfaces sum into one metric: inbound attempts are org-scoped
-- through the event, outbound through message_deliveries, which carries org_id
-- directly (no join to messages needed).
--
-- NOTE both attempt tables use attempted_at, NOT created_at.
SELECT org_id, period_start, sum(count)::bigint AS count FROM (
    SELECT e.org_id AS org_id,
           date_trunc(@bucket::text, a.attempted_at)::timestamptz AS period_start,
           count(*)::bigint AS count
    FROM attempts a JOIN events e ON e.id = a.event_id
    WHERE a.attempted_at >= @after::timestamptz
    GROUP BY 1, 2
    UNION ALL
    SELECT md.org_id AS org_id,
           date_trunc(@bucket::text, mda.attempted_at)::timestamptz AS period_start,
           count(*)::bigint AS count
    FROM message_delivery_attempts mda
    JOIN message_deliveries md ON md.id = mda.delivery_id
    WHERE mda.attempted_at >= @after::timestamptz
    GROUP BY 1, 2
) t GROUP BY 1, 2;

-- name: GetUsageForPeriod :many
SELECT metric, count FROM usage_rollups
WHERE org_id = $1 AND period_start = $2;

-- name: GetUsageHistory :many
SELECT period_start, metric, count FROM usage_rollups
WHERE org_id = $1 AND metric = $2 AND period_start >= $3
ORDER BY period_start DESC;

-- name: ListOrgQuotas :many
-- Drives both reconciliation and the super-admin view.
SELECT id, name, slug, plan, quota_events_soft, quota_events_hard,
       quota_messages_soft, quota_messages_hard, quota_period
FROM organizations ORDER BY name;

-- name: GetOrgQuota :one
-- One org's plan + limits, pinned to exactly those columns (no name/slug/
-- timestamps) so GET /api/usage can't accidentally grow into a second
-- `SELECT *`-shaped leak the way identity.sql:5,8 already are.
SELECT plan, quota_events_soft, quota_events_hard,
       quota_messages_soft, quota_messages_hard, quota_period
FROM organizations WHERE id = $1;

-- name: UpdateOrgQuota :one
-- Sets plan + limits together (PATCH /admin/orgs/{org_id}/plan, super-admin
-- only — a tenant cannot change its own quota). RETURNING is pinned the same
-- way GetOrgQuota is, for the same reason.
UPDATE organizations
   SET plan = $2,
       quota_events_soft = $3,
       quota_events_hard = $4,
       quota_messages_soft = $5,
       quota_messages_hard = $6,
       quota_period = $7,
       updated_at = now()
 WHERE id = $1
 RETURNING plan, quota_events_soft, quota_events_hard,
           quota_messages_soft, quota_messages_hard, quota_period;
