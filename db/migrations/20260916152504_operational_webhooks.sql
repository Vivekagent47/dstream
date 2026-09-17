-- Modify "applications" table
ALTER TABLE "public"."applications" ADD COLUMN "is_operational" boolean NOT NULL DEFAULT false;
-- Create index "applications_org_operational_idx" to table: "applications"
CREATE UNIQUE INDEX "applications_org_operational_idx" ON "public"."applications" ("org_id") WHERE is_operational;
-- Backfill: give every existing org its reserved operational app and the core
-- operational event types (data DML; Atlas only generates DDL).
INSERT INTO applications (org_id, name, is_operational)
SELECT id, 'Operational Webhooks', TRUE FROM organizations o
WHERE NOT EXISTS (SELECT 1 FROM applications a WHERE a.org_id = o.id AND a.is_operational);

INSERT INTO event_types (org_id, name, description)
SELECT o.id, t.name, t.descr FROM organizations o
CROSS JOIN (VALUES
  ('endpoint.disabled','dstream auto-disabled an endpoint after repeated failures'),
  ('message.attempt.exhausted','a message delivery exhausted its retries and was dead-lettered')
) t(name,descr)
ON CONFLICT (org_id, name) DO NOTHING;
