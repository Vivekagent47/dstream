-- Atlas Free tier cannot diff CREATE EXTENSION statements (extensions are a
-- paid feature). This migration is authored by hand and is applied before the
-- Atlas-generated init migration.
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE EXTENSION IF NOT EXISTS citext;
