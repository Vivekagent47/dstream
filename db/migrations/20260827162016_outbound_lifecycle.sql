-- Modify "endpoints" table
ALTER TABLE "endpoints" ADD COLUMN "prev_secret" text NULL, ADD COLUMN "prev_secret_expires_at" timestamptz NULL, ADD COLUMN "consecutive_failures" integer NOT NULL DEFAULT 0, ADD COLUMN "disabled_at" timestamptz NULL;
-- Modify "message_deliveries" table
ALTER TABLE "message_deliveries" DROP CONSTRAINT "message_deliveries_status_check", ADD CONSTRAINT "message_deliveries_status_check" CHECK (status = ANY (ARRAY['queued'::text, 'in_flight'::text, 'delivered'::text, 'dead'::text, 'disabled'::text]));
