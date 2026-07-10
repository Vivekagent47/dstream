-- Create index "events_connection_created_idx" to table: "events"
CREATE INDEX "events_connection_created_idx" ON "public"."events" ("connection_id", "created_at" DESC, "id" DESC);
