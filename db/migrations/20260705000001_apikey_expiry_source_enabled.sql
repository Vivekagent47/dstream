-- L1: optional API-key expiry. NULL = never expires (existing behavior);
-- enforced in GetAPIKeyByPrefix so an expired key stops authenticating.
ALTER TABLE "public"."api_keys" ADD COLUMN "expires_at" timestamptz;

-- L8: source enable/disable. Disabled sources reject ingest
-- (GetSourceByIngestToken filters enabled), letting an operator pause a source
-- without deleting it or rotating its token.
ALTER TABLE "public"."sources" ADD COLUMN "enabled" boolean NOT NULL DEFAULT true;
