-- Modify "organizations" table
ALTER TABLE "public"."organizations" ALTER COLUMN "quota_events_soft" SET DEFAULT 8000, ALTER COLUMN "quota_events_hard" SET DEFAULT 10000, ALTER COLUMN "quota_messages_soft" SET DEFAULT 8000, ALTER COLUMN "quota_messages_hard" SET DEFAULT 10000;

-- Data migration: adopt the free tier for orgs that were never configured.
--
-- Phase 5c shipped every quota column defaulting to 0 (= unlimited), so an
-- org nobody ever touched is indistinguishable from one deliberately left
-- uncapped — except that "plan = 'free' AND all four limits = 0" is exactly
-- the row those defaults produced, and nothing else writes that combination:
-- the admin PATCH applies a preset (non-zero for free and pro) or requires
-- plan = 'custom' to write raw numbers.
--
-- So this touches only never-configured free orgs. A pro org, an org with any
-- single limit set, and a deliberately-uncapped org on plan = 'custom' are
-- all left alone. An operator who wants free-and-unlimited after this sets
-- plan = 'custom', which is what the precedence rule asks of them anyway.
--
-- BEHAVIOR CHANGE: orgs matched here start being enforced. One already past
-- 10,000 events this period is over its hard ceiling the moment
-- internal/usage/gate.go's 60s limits snapshot refreshes.
UPDATE organizations
   SET quota_events_soft = 8000, quota_events_hard = 10000,
       quota_messages_soft = 8000, quota_messages_hard = 10000
 WHERE plan = 'free'
   AND quota_events_soft = 0 AND quota_events_hard = 0
   AND quota_messages_soft = 0 AND quota_messages_hard = 0;
