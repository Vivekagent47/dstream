-- Modify "endpoints" table
ALTER TABLE "public"."endpoints" ADD COLUMN "headers" jsonb NOT NULL DEFAULT '{}', ADD COLUMN "rate_limit" integer NULL, ADD COLUMN "channels" text[] NULL;
-- Modify "messages" table
ALTER TABLE "public"."messages" ADD COLUMN "channels" text[] NULL;
