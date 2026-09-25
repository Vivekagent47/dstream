-- Create "scenarios" table
CREATE TABLE "public"."scenarios" (
  "id" uuid NOT NULL DEFAULT uuidv7(),
  "org_id" uuid NOT NULL,
  "name" text NOT NULL,
  "description" text NOT NULL DEFAULT '',
  "created_at" timestamptz NOT NULL DEFAULT now(),
  "updated_at" timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY ("id"),
  CONSTRAINT "scenarios_org_id_name_key" UNIQUE ("org_id", "name"),
  CONSTRAINT "scenarios_org_id_fkey" FOREIGN KEY ("org_id") REFERENCES "public"."organizations" ("id") ON UPDATE NO ACTION ON DELETE CASCADE
);
-- Create "scenario_steps" table
CREATE TABLE "public"."scenario_steps" (
  "id" uuid NOT NULL DEFAULT uuidv7(),
  "scenario_id" uuid NOT NULL,
  "position" integer NOT NULL,
  "bookmark_id" uuid NOT NULL,
  "delay_ms" integer NOT NULL DEFAULT 0,
  PRIMARY KEY ("id"),
  CONSTRAINT "scenario_steps_scenario_id_position_key" UNIQUE ("scenario_id", "position"),
  CONSTRAINT "scenario_steps_bookmark_id_fkey" FOREIGN KEY ("bookmark_id") REFERENCES "public"."bookmarks" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
  CONSTRAINT "scenario_steps_scenario_id_fkey" FOREIGN KEY ("scenario_id") REFERENCES "public"."scenarios" ("id") ON UPDATE NO ACTION ON DELETE CASCADE,
  CONSTRAINT "scenario_steps_delay_ms_check" CHECK ((delay_ms >= 0) AND (delay_ms <= 60000))
);
-- Create index "scenario_steps_scenario_idx" to table: "scenario_steps"
CREATE INDEX "scenario_steps_scenario_idx" ON "public"."scenario_steps" ("scenario_id", "position");
