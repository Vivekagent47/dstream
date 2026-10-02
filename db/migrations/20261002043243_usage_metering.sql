-- Modify "organizations" table
ALTER TABLE "public"."organizations" ADD CONSTRAINT "organizations_plan_check" CHECK (plan = ANY (ARRAY['free'::text, 'pro'::text, 'enterprise'::text, 'custom'::text])), ADD CONSTRAINT "organizations_quota_period_check" CHECK (quota_period = ANY (ARRAY['day'::text, 'month'::text])), ADD COLUMN "plan" text NOT NULL DEFAULT 'free', ADD COLUMN "quota_events_soft" bigint NOT NULL DEFAULT 0, ADD COLUMN "quota_events_hard" bigint NOT NULL DEFAULT 0, ADD COLUMN "quota_messages_soft" bigint NOT NULL DEFAULT 0, ADD COLUMN "quota_messages_hard" bigint NOT NULL DEFAULT 0, ADD COLUMN "quota_period" text NOT NULL DEFAULT 'month';
-- Create "usage_rollups" table
CREATE TABLE "public"."usage_rollups" (
  "org_id" uuid NOT NULL,
  "period_start" timestamptz NOT NULL,
  "metric" text NOT NULL,
  "count" bigint NOT NULL DEFAULT 0,
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("org_id", "period_start", "metric"),
  CONSTRAINT "usage_rollups_org_id_fkey" FOREIGN KEY ("org_id") REFERENCES "public"."organizations" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
  CONSTRAINT "usage_rollups_metric_check" CHECK (metric = ANY (ARRAY['requests'::text, 'events'::text, 'messages'::text, 'attempts'::text]))
);
-- Create index "usage_rollups_period_idx" to table: "usage_rollups"
CREATE INDEX "usage_rollups_period_idx" ON "public"."usage_rollups" ("period_start" DESC);
