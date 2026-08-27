-- Create "applications" table
CREATE TABLE "public"."applications" (
  "id" uuid NOT NULL DEFAULT uuidv7(),
  "org_id" uuid NOT NULL,
  "uid" text NULL,
  "name" text NOT NULL,
  "metadata" jsonb NOT NULL DEFAULT '{}',
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id"),
  CONSTRAINT "applications_org_id_fkey" FOREIGN KEY ("org_id") REFERENCES "public"."organizations" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
);
-- Create index "applications_org_idx" to table: "applications"
CREATE INDEX "applications_org_idx" ON "public"."applications" ("org_id");
-- Create index "applications_org_uid_idx" to table: "applications"
CREATE UNIQUE INDEX "applications_org_uid_idx" ON "public"."applications" ("org_id", "uid") WHERE (uid IS NOT NULL);
-- Create "endpoints" table
CREATE TABLE "public"."endpoints" (
  "id" uuid NOT NULL DEFAULT uuidv7(),
  "app_id" uuid NOT NULL,
  "org_id" uuid NOT NULL,
  "uid" text NULL,
  "url" text NOT NULL,
  "description" text NOT NULL DEFAULT '',
  "secret" text NOT NULL,
  "filter_event_types" text[] NULL,
  "disabled" boolean NOT NULL DEFAULT false,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id"),
  CONSTRAINT "endpoints_app_id_fkey" FOREIGN KEY ("app_id") REFERENCES "public"."applications" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
  CONSTRAINT "endpoints_org_id_fkey" FOREIGN KEY ("org_id") REFERENCES "public"."organizations" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
);
-- Create index "endpoints_app_idx" to table: "endpoints"
CREATE INDEX "endpoints_app_idx" ON "public"."endpoints" ("app_id");
-- Create index "endpoints_app_uid_idx" to table: "endpoints"
CREATE UNIQUE INDEX "endpoints_app_uid_idx" ON "public"."endpoints" ("app_id", "uid") WHERE (uid IS NOT NULL);
-- Create "event_types" table
CREATE TABLE "public"."event_types" (
  "id" uuid NOT NULL DEFAULT uuidv7(),
  "org_id" uuid NOT NULL,
  "name" text NOT NULL,
  "description" text NOT NULL DEFAULT '',
  "schema" jsonb NULL,
  "archived" boolean NOT NULL DEFAULT false,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id"),
  CONSTRAINT "event_types_org_id_fkey" FOREIGN KEY ("org_id") REFERENCES "public"."organizations" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
);
-- Create index "event_types_org_name_idx" to table: "event_types"
CREATE UNIQUE INDEX "event_types_org_name_idx" ON "public"."event_types" ("org_id", "name");
-- Create "messages" table
CREATE TABLE "public"."messages" (
  "id" uuid NOT NULL DEFAULT uuidv7(),
  "app_id" uuid NOT NULL,
  "org_id" uuid NOT NULL,
  "event_type" text NOT NULL,
  "payload" bytea NOT NULL,
  "payload_hash" text NOT NULL,
  "event_id" text NULL,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id"),
  CONSTRAINT "messages_app_id_fkey" FOREIGN KEY ("app_id") REFERENCES "public"."applications" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
  CONSTRAINT "messages_org_id_fkey" FOREIGN KEY ("org_id") REFERENCES "public"."organizations" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
);
-- Create index "messages_app_created_idx" to table: "messages"
CREATE INDEX "messages_app_created_idx" ON "public"."messages" ("app_id", "created_at" DESC);
-- Create index "messages_app_event_id_idx" to table: "messages"
CREATE UNIQUE INDEX "messages_app_event_id_idx" ON "public"."messages" ("app_id", "event_id") WHERE (event_id IS NOT NULL);
-- Create "message_deliveries" table
CREATE TABLE "public"."message_deliveries" (
  "id" uuid NOT NULL DEFAULT uuidv7(),
  "message_id" uuid NOT NULL,
  "endpoint_id" uuid NOT NULL,
  "org_id" uuid NOT NULL,
  "status" text NOT NULL DEFAULT 'queued',
  "attempt_count" integer NOT NULL DEFAULT 0,
  "next_retry_at" timestamptz NULL,
  "last_attempt_at" timestamptz NULL,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id"),
  CONSTRAINT "message_deliveries_endpoint_id_fkey" FOREIGN KEY ("endpoint_id") REFERENCES "public"."endpoints" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
  CONSTRAINT "message_deliveries_message_id_fkey" FOREIGN KEY ("message_id") REFERENCES "public"."messages" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
  CONSTRAINT "message_deliveries_org_id_fkey" FOREIGN KEY ("org_id") REFERENCES "public"."organizations" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
  CONSTRAINT "message_deliveries_status_check" CHECK (status = ANY (ARRAY['queued'::text, 'in_flight'::text, 'delivered'::text, 'failed'::text, 'dead'::text, 'disabled'::text]))
);
-- Create index "message_deliveries_endpoint_idx" to table: "message_deliveries"
CREATE INDEX "message_deliveries_endpoint_idx" ON "public"."message_deliveries" ("endpoint_id", "created_at" DESC);
-- Create index "message_deliveries_message_idx" to table: "message_deliveries"
CREATE INDEX "message_deliveries_message_idx" ON "public"."message_deliveries" ("message_id");
-- Create "message_delivery_attempts" table
CREATE TABLE "public"."message_delivery_attempts" (
  "id" uuid NOT NULL DEFAULT uuidv7(),
  "delivery_id" uuid NOT NULL,
  "attempt_num" integer NOT NULL,
  "response_status" integer NULL,
  "response_headers" jsonb NULL,
  "response_body" bytea NULL,
  "duration_ms" integer NULL,
  "error_message" text NULL,
  "attempted_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id"),
  CONSTRAINT "message_delivery_attempts_delivery_id_fkey" FOREIGN KEY ("delivery_id") REFERENCES "public"."message_deliveries" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
);
-- Create index "mda_delivery_attempt_idx" to table: "message_delivery_attempts"
CREATE UNIQUE INDEX "mda_delivery_attempt_idx" ON "public"."message_delivery_attempts" ("delivery_id", "attempt_num");
