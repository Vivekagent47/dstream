-- Create "bookmarks" table
CREATE TABLE "public"."bookmarks" (
  "id" uuid NOT NULL DEFAULT uuidv7(),
  "org_id" uuid NOT NULL,
  "request_id" uuid NOT NULL,
  "name" text NOT NULL,
  "description" text NOT NULL DEFAULT '',
  "tags" text[] NOT NULL DEFAULT '{}',
  "created_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id"),
  CONSTRAINT "bookmarks_org_id_name_key" UNIQUE ("org_id", "name"),
  CONSTRAINT "bookmarks_org_id_fkey" FOREIGN KEY ("org_id") REFERENCES "public"."organizations" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
  CONSTRAINT "bookmarks_request_id_fkey" FOREIGN KEY ("request_id") REFERENCES "public"."requests" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
);
-- Create index "bookmarks_org_idx" to table: "bookmarks"
CREATE INDEX "bookmarks_org_idx" ON "public"."bookmarks" ("org_id", "created_at" DESC);
-- Create index "bookmarks_request_idx" to table: "bookmarks"
CREATE INDEX "bookmarks_request_idx" ON "public"."bookmarks" ("request_id");
