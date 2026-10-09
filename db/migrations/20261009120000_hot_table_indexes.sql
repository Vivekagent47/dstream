-- Create index "attempts_attempted_at_idx" to table: "attempts"
CREATE INDEX "attempts_attempted_at_idx" ON "public"."attempts" ("attempted_at");
-- Create index "mda_attempted_at_idx" to table: "message_delivery_attempts"
CREATE INDEX "mda_attempted_at_idx" ON "public"."message_delivery_attempts" ("attempted_at");
-- Create index "messages_created_at_idx" to table: "messages"
CREATE INDEX "messages_created_at_idx" ON "public"."messages" ("created_at");
-- Create index "request_bodies_stored_at_idx" to table: "request_bodies"
CREATE INDEX "request_bodies_stored_at_idx" ON "public"."request_bodies" ("stored_at");
-- Create index "message_deliveries_stuck_idx" to table: "message_deliveries"
CREATE INDEX "message_deliveries_stuck_idx" ON "public"."message_deliveries" ("updated_at") WHERE ((status = 'queued'::text) AND (next_retry_at IS NULL));
