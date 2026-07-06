-- Modify "destinations" table
ALTER TABLE "public"."destinations" ADD COLUMN "description" text NOT NULL DEFAULT '';
