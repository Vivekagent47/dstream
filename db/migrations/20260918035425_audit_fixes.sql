-- Create index "message_deliveries_msg_ep_idx" to table: "message_deliveries"
CREATE UNIQUE INDEX "message_deliveries_msg_ep_idx" ON "public"."message_deliveries" ("message_id", "endpoint_id");
-- Modify "request_bodies" table
ALTER TABLE "public"."request_bodies" ALTER COLUMN "body" DROP NOT NULL;
