-- Modify "events" table
ALTER TABLE "public"."events" ADD COLUMN "is_test" boolean NOT NULL DEFAULT false;
