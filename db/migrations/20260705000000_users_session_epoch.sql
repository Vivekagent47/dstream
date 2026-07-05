-- Add session_epoch for stateless-session revocation. Bumping a user's epoch
-- invalidates every outstanding signed session cookie for that user (logout-all,
-- account disable, security events) without a global session_secret rotation.
-- The epoch is embedded in the signed cookie and compared on every request.
ALTER TABLE "public"."users" ADD COLUMN "session_epoch" integer NOT NULL DEFAULT 0;
