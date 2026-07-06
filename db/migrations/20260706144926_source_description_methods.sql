-- Modify "sources" table
ALTER TABLE "public"."sources" ADD COLUMN "description" text NOT NULL DEFAULT '', ADD COLUMN "allowed_methods" text[] NOT NULL DEFAULT '{POST,PUT,PATCH,DELETE}';
