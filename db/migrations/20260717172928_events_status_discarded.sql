-- Modify "events" table
ALTER TABLE "public"."events" DROP CONSTRAINT "events_status_check", ADD CONSTRAINT "events_status_check" CHECK (status = ANY (ARRAY['queued'::text, 'in_flight'::text, 'delivered'::text, 'failed'::text, 'paused'::text, 'dead'::text, 'discarded'::text]));
