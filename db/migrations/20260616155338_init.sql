-- Create "magic_link_tokens" table
CREATE TABLE "public"."magic_link_tokens" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "email" public.citext NOT NULL,
  "token_hash" bytea NOT NULL,
  "expires_at" timestamptz NOT NULL,
  "used_at" timestamptz NULL,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id")
);
-- Create index "magic_link_tokens_email_idx" to table: "magic_link_tokens"
CREATE INDEX "magic_link_tokens_email_idx" ON "public"."magic_link_tokens" ("email");
-- Create index "magic_link_tokens_expires_idx" to table: "magic_link_tokens"
CREATE INDEX "magic_link_tokens_expires_idx" ON "public"."magic_link_tokens" ("expires_at");
-- Create "organizations" table
CREATE TABLE "public"."organizations" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "name" text NOT NULL,
  "slug" text NOT NULL,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id"),
  CONSTRAINT "organizations_slug_key" UNIQUE ("slug")
);
-- Create "api_keys" table
CREATE TABLE "public"."api_keys" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "org_id" uuid NOT NULL,
  "name" text NOT NULL,
  "prefix" text NOT NULL,
  "key_hash" bytea NOT NULL,
  "last_used_at" timestamptz NULL,
  "revoked_at" timestamptz NULL,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id"),
  CONSTRAINT "api_keys_org_id_fkey" FOREIGN KEY ("org_id") REFERENCES "public"."organizations" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
);
-- Create index "api_keys_org_idx" to table: "api_keys"
CREATE INDEX "api_keys_org_idx" ON "public"."api_keys" ("org_id");
-- Create index "api_keys_prefix_idx" to table: "api_keys"
CREATE UNIQUE INDEX "api_keys_prefix_idx" ON "public"."api_keys" ("prefix") WHERE (revoked_at IS NULL);
-- Create "destinations" table
CREATE TABLE "public"."destinations" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "org_id" uuid NOT NULL,
  "name" text NOT NULL,
  "type" text NOT NULL,
  "url" text NULL,
  "auth_config" jsonb NOT NULL DEFAULT '{}',
  "rate_limit_rps" integer NULL,
  "rate_limit_burst" integer NULL,
  "max_inflight" integer NULL,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id"),
  CONSTRAINT "destinations_org_id_name_key" UNIQUE ("org_id", "name"),
  CONSTRAINT "destinations_org_id_fkey" FOREIGN KEY ("org_id") REFERENCES "public"."organizations" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
  CONSTRAINT "destinations_type_check" CHECK (type = ANY (ARRAY['http'::text, 'cli'::text]))
);
-- Create index "destinations_org_idx" to table: "destinations"
CREATE INDEX "destinations_org_idx" ON "public"."destinations" ("org_id");
-- Create "sources" table
CREATE TABLE "public"."sources" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "org_id" uuid NOT NULL,
  "name" text NOT NULL,
  "type" text NOT NULL,
  "ingest_token" text NOT NULL,
  "signing_config" jsonb NOT NULL DEFAULT '{}',
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id"),
  CONSTRAINT "sources_ingest_token_key" UNIQUE ("ingest_token"),
  CONSTRAINT "sources_org_id_name_key" UNIQUE ("org_id", "name"),
  CONSTRAINT "sources_org_id_fkey" FOREIGN KEY ("org_id") REFERENCES "public"."organizations" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
);
-- Create index "sources_org_idx" to table: "sources"
CREATE INDEX "sources_org_idx" ON "public"."sources" ("org_id");
-- Create "connections" table
CREATE TABLE "public"."connections" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "source_id" uuid NOT NULL,
  "destination_id" uuid NOT NULL,
  "enabled" boolean NOT NULL DEFAULT true,
  "max_retries" integer NOT NULL DEFAULT 8,
  "retry_strategy" text NOT NULL DEFAULT 'exponential',
  "retry_base_ms" integer NOT NULL DEFAULT 30000,
  "retry_cap_ms" integer NOT NULL DEFAULT 3600000,
  "retry_jitter_pct" integer NOT NULL DEFAULT 20,
  "custom_retry_schedule" jsonb NULL,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id"),
  CONSTRAINT "connections_source_id_destination_id_key" UNIQUE ("source_id", "destination_id"),
  CONSTRAINT "connections_destination_id_fkey" FOREIGN KEY ("destination_id") REFERENCES "public"."destinations" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
  CONSTRAINT "connections_source_id_fkey" FOREIGN KEY ("source_id") REFERENCES "public"."sources" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
  CONSTRAINT "connections_retry_strategy_check" CHECK (retry_strategy = ANY (ARRAY['exponential'::text, 'linear'::text, 'fixed'::text, 'custom'::text]))
);
-- Create index "connections_destination_idx" to table: "connections"
CREATE INDEX "connections_destination_idx" ON "public"."connections" ("destination_id");
-- Create index "connections_source_idx" to table: "connections"
CREATE INDEX "connections_source_idx" ON "public"."connections" ("source_id");
-- Create "requests" table
CREATE TABLE "public"."requests" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "source_id" uuid NOT NULL,
  "http_method" text NOT NULL,
  "http_path" text NOT NULL,
  "headers" jsonb NOT NULL DEFAULT '{}',
  "body_hash" text NOT NULL,
  "body_ref" text NOT NULL,
  "body_size" integer NOT NULL,
  "content_type" text NULL,
  "sig_verified" boolean NOT NULL DEFAULT false,
  "ingest_ip" inet NULL,
  "received_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id"),
  CONSTRAINT "requests_source_id_fkey" FOREIGN KEY ("source_id") REFERENCES "public"."sources" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
);
-- Create index "requests_body_hash_idx" to table: "requests"
CREATE INDEX "requests_body_hash_idx" ON "public"."requests" ("source_id", "body_hash");
-- Create index "requests_source_received_idx" to table: "requests"
CREATE INDEX "requests_source_received_idx" ON "public"."requests" ("source_id", "received_at" DESC);
-- Create "events" table
CREATE TABLE "public"."events" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "request_id" uuid NOT NULL,
  "connection_id" uuid NOT NULL,
  "status" text NOT NULL DEFAULT 'queued',
  "attempt_count" integer NOT NULL DEFAULT 0,
  "last_attempt_at" timestamptz NULL,
  "next_retry_at" timestamptz NULL,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id"),
  CONSTRAINT "events_connection_id_fkey" FOREIGN KEY ("connection_id") REFERENCES "public"."connections" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
  CONSTRAINT "events_request_id_fkey" FOREIGN KEY ("request_id") REFERENCES "public"."requests" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
  CONSTRAINT "events_status_check" CHECK (status = ANY (ARRAY['queued'::text, 'in_flight'::text, 'delivered'::text, 'failed'::text, 'paused'::text, 'dead'::text]))
);
-- Create index "events_connection_status_idx" to table: "events"
CREATE INDEX "events_connection_status_idx" ON "public"."events" ("connection_id", "status");
-- Create index "events_request_idx" to table: "events"
CREATE INDEX "events_request_idx" ON "public"."events" ("request_id");
-- Create index "events_status_created_idx" to table: "events"
CREATE INDEX "events_status_created_idx" ON "public"."events" ("status", "created_at" DESC);
-- Create "attempts" table
CREATE TABLE "public"."attempts" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "event_id" uuid NOT NULL,
  "attempt_num" integer NOT NULL,
  "response_status" integer NULL,
  "response_headers" jsonb NULL,
  "response_body" bytea NULL,
  "duration_ms" integer NULL,
  "queued_in_ms" integer NULL,
  "error_message" text NULL,
  "attempted_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id"),
  CONSTRAINT "attempts_event_id_attempt_num_key" UNIQUE ("event_id", "attempt_num"),
  CONSTRAINT "attempts_event_id_fkey" FOREIGN KEY ("event_id") REFERENCES "public"."events" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
);
-- Create index "attempts_event_idx" to table: "attempts"
CREATE INDEX "attempts_event_idx" ON "public"."attempts" ("event_id");
-- Create "users" table
CREATE TABLE "public"."users" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "email" public.citext NOT NULL,
  "name" text NULL,
  "is_super_admin" boolean NOT NULL DEFAULT false,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id"),
  CONSTRAINT "users_email_key" UNIQUE ("email")
);
-- Create "audit_logs" table
CREATE TABLE "public"."audit_logs" (
  "id" bigserial NOT NULL,
  "org_id" uuid NOT NULL,
  "actor_user_id" uuid NULL,
  "actor_api_key_id" uuid NULL,
  "actor_email_snapshot" text NULL,
  "action" text NOT NULL,
  "target_type" text NOT NULL,
  "target_id" uuid NULL,
  "metadata" jsonb NOT NULL DEFAULT '{}',
  "created_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id"),
  CONSTRAINT "audit_logs_actor_api_key_id_fkey" FOREIGN KEY ("actor_api_key_id") REFERENCES "public"."api_keys" ("id") ON UPDATE NO ACTION ON DELETE SET NULL,
  CONSTRAINT "audit_logs_actor_user_id_fkey" FOREIGN KEY ("actor_user_id") REFERENCES "public"."users" ("id") ON UPDATE NO ACTION ON DELETE SET NULL,
  CONSTRAINT "audit_logs_org_id_fkey" FOREIGN KEY ("org_id") REFERENCES "public"."organizations" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
  CONSTRAINT "audit_logs_check" CHECK (((actor_user_id IS NOT NULL) AND (actor_api_key_id IS NULL)) OR ((actor_user_id IS NULL) AND (actor_api_key_id IS NOT NULL)))
);
-- Create index "audit_logs_actor_user_idx" to table: "audit_logs"
CREATE INDEX "audit_logs_actor_user_idx" ON "public"."audit_logs" ("org_id", "actor_user_id", "created_at" DESC) WHERE (actor_user_id IS NOT NULL);
-- Create index "audit_logs_org_time_idx" to table: "audit_logs"
CREATE INDEX "audit_logs_org_time_idx" ON "public"."audit_logs" ("org_id", "created_at" DESC);
-- Create index "audit_logs_target_idx" to table: "audit_logs"
CREATE INDEX "audit_logs_target_idx" ON "public"."audit_logs" ("org_id", "target_type", "target_id") WHERE (target_id IS NOT NULL);
-- Create "cli_sessions" table
CREATE TABLE "public"."cli_sessions" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "source_id" uuid NOT NULL,
  "token_hash" bytea NOT NULL,
  "last_seen_at" timestamptz NULL,
  "expires_at" timestamptz NOT NULL,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id"),
  CONSTRAINT "cli_sessions_source_id_fkey" FOREIGN KEY ("source_id") REFERENCES "public"."sources" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
);
-- Create index "cli_sessions_expires_idx" to table: "cli_sessions"
CREATE INDEX "cli_sessions_expires_idx" ON "public"."cli_sessions" ("expires_at");
-- Create index "cli_sessions_source_idx" to table: "cli_sessions"
CREATE INDEX "cli_sessions_source_idx" ON "public"."cli_sessions" ("source_id");
-- Create "org_invites" table
CREATE TABLE "public"."org_invites" (
  "id" uuid NOT NULL DEFAULT gen_random_uuid(),
  "org_id" uuid NOT NULL,
  "email" public.citext NOT NULL,
  "role" text NOT NULL,
  "token_hash" bytea NOT NULL,
  "invited_by" uuid NOT NULL,
  "expires_at" timestamptz NOT NULL,
  "accepted_at" timestamptz NULL,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id"),
  CONSTRAINT "org_invites_invited_by_fkey" FOREIGN KEY ("invited_by") REFERENCES "public"."users" ("id") ON UPDATE NO ACTION ON DELETE RESTRICT,
  CONSTRAINT "org_invites_org_id_fkey" FOREIGN KEY ("org_id") REFERENCES "public"."organizations" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
  CONSTRAINT "org_invites_role_check" CHECK (role = ANY (ARRAY['admin'::text, 'member'::text]))
);
-- Create index "org_invites_email_idx" to table: "org_invites"
CREATE INDEX "org_invites_email_idx" ON "public"."org_invites" ("email") WHERE (accepted_at IS NULL);
-- Create index "org_invites_expires_idx" to table: "org_invites"
CREATE INDEX "org_invites_expires_idx" ON "public"."org_invites" ("expires_at") WHERE (accepted_at IS NULL);
-- Create index "org_invites_org_idx" to table: "org_invites"
CREATE INDEX "org_invites_org_idx" ON "public"."org_invites" ("org_id");
-- Create index "org_invites_pending_unique" to table: "org_invites"
CREATE UNIQUE INDEX "org_invites_pending_unique" ON "public"."org_invites" ("org_id", "email") WHERE (accepted_at IS NULL);
-- Create "org_members" table
CREATE TABLE "public"."org_members" (
  "org_id" uuid NOT NULL,
  "user_id" uuid NOT NULL,
  "role" text NOT NULL DEFAULT 'member',
  "created_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("org_id", "user_id"),
  CONSTRAINT "org_members_org_id_fkey" FOREIGN KEY ("org_id") REFERENCES "public"."organizations" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
  CONSTRAINT "org_members_user_id_fkey" FOREIGN KEY ("user_id") REFERENCES "public"."users" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
  CONSTRAINT "org_members_role_check" CHECK (role = ANY (ARRAY['owner'::text, 'admin'::text, 'member'::text]))
);
-- Create "request_bodies" table
CREATE TABLE "public"."request_bodies" (
  "request_id" uuid NOT NULL,
  "body" bytea NOT NULL,
  "stored_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("request_id"),
  CONSTRAINT "request_bodies_request_id_fkey" FOREIGN KEY ("request_id") REFERENCES "public"."requests" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
);
