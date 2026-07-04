-- Switch all UUID primary keys from random v4 (gen_random_uuid) to time-ordered
-- v7 (uuidv7, native in Postgres 18). v7 ids carry a millisecond timestamp
-- prefix, so inserts append to the right of the PK B-tree instead of scattering
-- across it — much less index fragmentation and better cache locality on the
-- insert-heavy tables (requests, events, attempts). Existing v4 rows are left
-- untouched; only future inserts get v7. uuid columns store both versions
-- identically, so mixed v4/v7 data is fine.
ALTER TABLE "public"."organizations"     ALTER COLUMN "id" SET DEFAULT uuidv7();
ALTER TABLE "public"."users"             ALTER COLUMN "id" SET DEFAULT uuidv7();
ALTER TABLE "public"."api_keys"          ALTER COLUMN "id" SET DEFAULT uuidv7();
ALTER TABLE "public"."magic_link_tokens" ALTER COLUMN "id" SET DEFAULT uuidv7();
ALTER TABLE "public"."org_invites"       ALTER COLUMN "id" SET DEFAULT uuidv7();
ALTER TABLE "public"."sources"           ALTER COLUMN "id" SET DEFAULT uuidv7();
ALTER TABLE "public"."destinations"      ALTER COLUMN "id" SET DEFAULT uuidv7();
ALTER TABLE "public"."connections"       ALTER COLUMN "id" SET DEFAULT uuidv7();
ALTER TABLE "public"."requests"          ALTER COLUMN "id" SET DEFAULT uuidv7();
ALTER TABLE "public"."events"            ALTER COLUMN "id" SET DEFAULT uuidv7();
ALTER TABLE "public"."attempts"          ALTER COLUMN "id" SET DEFAULT uuidv7();
ALTER TABLE "public"."cli_sessions"      ALTER COLUMN "id" SET DEFAULT uuidv7();
