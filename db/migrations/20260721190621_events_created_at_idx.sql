-- Create index "events_created_at_idx" to table: "events"
CREATE INDEX "events_created_at_idx" ON "public"."events" ("created_at" DESC);
