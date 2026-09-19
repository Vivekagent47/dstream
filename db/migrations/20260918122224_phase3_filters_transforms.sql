-- Modify "connections" table
ALTER TABLE "public"."connections" ADD COLUMN "filter_expr" text NULL, ADD COLUMN "transform_js" text NULL;
-- Modify "endpoints" table
ALTER TABLE "public"."endpoints" ADD COLUMN "filter_expr" text NULL, ADD COLUMN "transform_js" text NULL;
-- Modify "events" table
ALTER TABLE "public"."events" DROP CONSTRAINT "events_status_check", ADD CONSTRAINT "events_status_check" CHECK (status = ANY (ARRAY['queued'::text, 'in_flight'::text, 'delivered'::text, 'failed'::text, 'paused'::text, 'dead'::text, 'discarded'::text, 'filtered'::text]));
-- Modify "message_deliveries" table
ALTER TABLE "public"."message_deliveries" DROP CONSTRAINT "message_deliveries_status_check", ADD CONSTRAINT "message_deliveries_status_check" CHECK (status = ANY (ARRAY['queued'::text, 'in_flight'::text, 'delivered'::text, 'dead'::text, 'disabled'::text, 'filtered'::text]));
