-- Create "capture_rules" table
CREATE TABLE "public"."capture_rules" (
  "id" uuid NOT NULL DEFAULT uuidv7(),
  "org_id" uuid NOT NULL,
  "source_id" uuid NOT NULL,
  "name" text NOT NULL,
  "filter_expr" text NULL,
  "cap" integer NOT NULL DEFAULT 50,
  "enabled" boolean NOT NULL DEFAULT true,
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id"),
  CONSTRAINT "capture_rules_org_id_name_key" UNIQUE ("org_id", "name"),
  CONSTRAINT "capture_rules_org_id_fkey" FOREIGN KEY ("org_id") REFERENCES "public"."organizations" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
  CONSTRAINT "capture_rules_source_id_fkey" FOREIGN KEY ("source_id") REFERENCES "public"."sources" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
  CONSTRAINT "capture_rules_cap_check" CHECK ((cap > 0) AND (cap <= 1000))
);
-- Create index "capture_rules_source_idx" to table: "capture_rules"
CREATE INDEX "capture_rules_source_idx" ON "public"."capture_rules" ("source_id") WHERE enabled;
-- Modify "bookmarks" table
ALTER TABLE "public"."bookmarks" ADD COLUMN "capture_rule_id" uuid NULL, ADD CONSTRAINT "bookmarks_capture_rule_id_fkey" FOREIGN KEY ("capture_rule_id") REFERENCES "public"."capture_rules" ("id") ON UPDATE NO ACTION ON DELETE SET NULL;
-- Create index "bookmarks_rule_idx" to table: "bookmarks"
CREATE INDEX "bookmarks_rule_idx" ON "public"."bookmarks" ("capture_rule_id", "created_at") WHERE (capture_rule_id IS NOT NULL);
