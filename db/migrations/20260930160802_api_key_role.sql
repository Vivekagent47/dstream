-- Modify "api_keys" table
ALTER TABLE "public"."api_keys" ADD CONSTRAINT "api_keys_role_check" CHECK (role = ANY (ARRAY['admin'::text, 'member'::text])), ADD COLUMN "role" text NOT NULL DEFAULT 'admin';
