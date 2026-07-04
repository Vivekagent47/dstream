-- Denormalize org_id onto events. Event listing pages by org and orders by
-- created_at; without org_id on the row the only path was events → connections
-- → sources, and no index could support (org_id, created_at DESC) across that
-- join. Deep OFFSET pages had to materialize-then-discard. With org_id local we
-- add a covering index and switch the listing to keyset pagination.

-- Add nullable first so the backfill can run, then enforce NOT NULL.
ALTER TABLE "public"."events" ADD COLUMN "org_id" uuid;

UPDATE "public"."events" e
   SET org_id = s.org_id
  FROM "public"."connections" c
  JOIN "public"."sources" s ON s.id = c.source_id
 WHERE c.id = e.connection_id;

ALTER TABLE "public"."events" ALTER COLUMN "org_id" SET NOT NULL;

ALTER TABLE "public"."events"
  ADD CONSTRAINT "events_org_id_fkey" FOREIGN KEY ("org_id")
  REFERENCES "public"."organizations" ("id") ON UPDATE NO ACTION ON DELETE CASCADE;

-- Keyset pagination support: WHERE org_id = $1 ORDER BY created_at DESC, id DESC.
CREATE INDEX "events_org_created_idx" ON "public"."events" ("org_id", "created_at" DESC, "id" DESC);
