-- Modify "applications" table
ALTER TABLE "public"."applications" ADD COLUMN "portal_epoch" bigint NOT NULL DEFAULT 0;
