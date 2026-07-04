-- =========================================================================
-- Extensions (installed via db/migrations/00000000000000_extensions.sql)
-- pgcrypto, citext
-- =========================================================================

-- =========================================================================
-- Identity
-- =========================================================================

-- unchanged
CREATE TABLE organizations (
    id          UUID PRIMARY KEY DEFAULT uuidv7(),
    name        TEXT NOT NULL,
    slug        TEXT NOT NULL UNIQUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- unchanged
CREATE TABLE users (
    id              UUID PRIMARY KEY DEFAULT uuidv7(),
    email           CITEXT NOT NULL UNIQUE,
    name            TEXT,
    is_super_admin  BOOLEAN NOT NULL DEFAULT FALSE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- changed: added CHECK on role
CREATE TABLE org_members (
    org_id      UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    user_id     UUID NOT NULL REFERENCES users(id)         ON DELETE CASCADE,
    role        TEXT NOT NULL DEFAULT 'member'
                  CHECK (role IN ('owner', 'admin', 'member')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, user_id)
);
-- ListOrgsForUser + CountOrgMembershipsForUser filter by user_id alone; PK
-- order (org_id, user_id) doesn't support those scans. Add a user-leading
-- index so the per-user lookups stay sub-millisecond as the table grows.
CREATE INDEX org_members_user_idx ON org_members (user_id);

-- NOTE: `projects` table is DROPPED. No project layer.

-- changed: project_id -> org_id; unique scope changed
CREATE TABLE api_keys (
    id            UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id        UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    prefix        TEXT NOT NULL,
    key_hash      BYTEA NOT NULL,
    last_used_at  TIMESTAMPTZ,
    revoked_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX api_keys_org_idx ON api_keys (org_id);
CREATE UNIQUE INDEX api_keys_prefix_idx ON api_keys (prefix) WHERE revoked_at IS NULL;

-- unchanged
CREATE TABLE magic_link_tokens (
    id          UUID PRIMARY KEY DEFAULT uuidv7(),
    email       CITEXT NOT NULL,
    token_hash  BYTEA NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    used_at     TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX magic_link_tokens_email_idx   ON magic_link_tokens (email);
CREATE INDEX magic_link_tokens_expires_idx ON magic_link_tokens (expires_at);

-- new
CREATE TABLE org_invites (
    id          UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id      UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    email       CITEXT NOT NULL,
    role        TEXT NOT NULL CHECK (role IN ('admin', 'member')),
    token_hash  BYTEA NOT NULL,
    invited_by  UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    expires_at  TIMESTAMPTZ NOT NULL,
    accepted_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX org_invites_org_idx     ON org_invites (org_id);
CREATE INDEX org_invites_email_idx   ON org_invites (email)      WHERE accepted_at IS NULL;
CREATE INDEX org_invites_expires_idx ON org_invites (expires_at) WHERE accepted_at IS NULL;
CREATE UNIQUE INDEX org_invites_pending_unique
  ON org_invites (org_id, email) WHERE accepted_at IS NULL;

-- new
-- org_id is nullable + ON DELETE SET NULL on purpose: when an org is
-- deleted, its audit trail MUST survive the cascade, otherwise an
-- attacker (or compromised owner) can erase the org and the evidence
-- atomically. The org_name_snapshot column denormalizes the org name at
-- write time so a tombstoned row remains human-readable even after the
-- organizations row is gone.
CREATE TABLE audit_logs (
    id                   BIGSERIAL PRIMARY KEY,
    org_id               UUID REFERENCES organizations(id) ON DELETE SET NULL,
    org_name_snapshot    TEXT,
    actor_user_id        UUID REFERENCES users(id)    ON DELETE SET NULL,
    actor_api_key_id     UUID REFERENCES api_keys(id) ON DELETE SET NULL,
    actor_email_snapshot TEXT,
    action               TEXT NOT NULL,
    target_type          TEXT NOT NULL,
    target_id            UUID,
    metadata             JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (
        (actor_user_id IS NOT NULL AND actor_api_key_id IS NULL) OR
        (actor_user_id IS NULL     AND actor_api_key_id IS NOT NULL)
    )
);
CREATE INDEX audit_logs_org_time_idx
  ON audit_logs (org_id, created_at DESC);
CREATE INDEX audit_logs_target_idx
  ON audit_logs (org_id, target_type, target_id) WHERE target_id IS NOT NULL;
CREATE INDEX audit_logs_actor_user_idx
  ON audit_logs (org_id, actor_user_id, created_at DESC) WHERE actor_user_id IS NOT NULL;

-- =========================================================================
-- Tenant resources
-- =========================================================================

-- changed: project_id -> org_id; UNIQUE scope; index renamed
CREATE TABLE sources (
    id              UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id          UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name            TEXT NOT NULL,
    type            TEXT NOT NULL,
    ingest_token    TEXT NOT NULL UNIQUE,
    signing_config  JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, name)
);
CREATE INDEX sources_org_idx ON sources (org_id);

-- changed: project_id -> org_id; UNIQUE scope; index renamed
CREATE TABLE destinations (
    id                UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id            UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name              TEXT NOT NULL,
    type              TEXT NOT NULL CHECK (type IN ('http', 'cli')),
    url               TEXT,
    auth_config       JSONB NOT NULL DEFAULT '{}'::jsonb,
    rate_limit_rps    INTEGER,
    rate_limit_burst  INTEGER,
    max_inflight      INTEGER,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, name)
);
CREATE INDEX destinations_org_idx ON destinations (org_id);

-- unchanged (source_id/destination_id transitively scope to org)
CREATE TABLE connections (
    id                      UUID PRIMARY KEY DEFAULT uuidv7(),
    source_id               UUID NOT NULL REFERENCES sources(id)      ON DELETE CASCADE,
    destination_id          UUID NOT NULL REFERENCES destinations(id) ON DELETE CASCADE,
    enabled                 BOOLEAN NOT NULL DEFAULT TRUE,
    max_retries             INTEGER NOT NULL DEFAULT 8,
    retry_strategy          TEXT NOT NULL DEFAULT 'exponential'
                              CHECK (retry_strategy IN ('exponential', 'linear', 'fixed', 'custom')),
    retry_base_ms           INTEGER NOT NULL DEFAULT 30000,
    retry_cap_ms            INTEGER NOT NULL DEFAULT 3600000,
    retry_jitter_pct        INTEGER NOT NULL DEFAULT 20,
    custom_retry_schedule   JSONB,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (source_id, destination_id)
);
CREATE INDEX connections_source_idx      ON connections (source_id);
CREATE INDEX connections_destination_idx ON connections (destination_id);

-- =========================================================================
-- Traffic: requests, events, attempts (all unchanged)
-- =========================================================================

CREATE TABLE requests (
    id            UUID PRIMARY KEY DEFAULT uuidv7(),
    source_id     UUID NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    http_method   TEXT NOT NULL,
    http_path     TEXT NOT NULL,
    headers       JSONB NOT NULL DEFAULT '{}'::jsonb,
    body_hash     TEXT NOT NULL,
    body_ref      TEXT NOT NULL,
    body_size     INTEGER NOT NULL,
    content_type  TEXT,
    sig_verified  BOOLEAN NOT NULL DEFAULT FALSE,
    ingest_ip     INET,
    received_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX requests_source_received_idx ON requests (source_id, received_at DESC);
CREATE INDEX requests_body_hash_idx       ON requests (source_id, body_hash);

CREATE TABLE request_bodies (
    request_id  UUID PRIMARY KEY REFERENCES requests(id) ON DELETE CASCADE,
    body        BYTEA NOT NULL,
    stored_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE events (
    id              UUID PRIMARY KEY DEFAULT uuidv7(),
    request_id      UUID NOT NULL REFERENCES requests(id) ON DELETE CASCADE,
    connection_id   UUID NOT NULL REFERENCES connections(id) ON DELETE CASCADE,
    org_id          UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    status          TEXT NOT NULL DEFAULT 'queued'
                      CHECK (status IN ('queued', 'in_flight', 'delivered', 'failed', 'paused', 'dead')),
    attempt_count   INTEGER NOT NULL DEFAULT 0,
    last_attempt_at TIMESTAMPTZ,
    next_retry_at   TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX events_connection_status_idx ON events (connection_id, status);
CREATE INDEX events_request_idx           ON events (request_id);
CREATE INDEX events_status_created_idx    ON events (status, created_at DESC);
CREATE INDEX events_org_created_idx       ON events (org_id, created_at DESC, id DESC);

CREATE TABLE attempts (
    id                UUID PRIMARY KEY DEFAULT uuidv7(),
    event_id          UUID NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    attempt_num       INTEGER NOT NULL,
    response_status   INTEGER,
    response_headers  JSONB,
    response_body     BYTEA,
    duration_ms       INTEGER,
    queued_in_ms      INTEGER,
    error_message     TEXT,
    attempted_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (event_id, attempt_num)
);
CREATE INDEX attempts_event_idx ON attempts (event_id);

-- =========================================================================
-- CLI tunnel (unchanged)
-- =========================================================================

CREATE TABLE cli_sessions (
    id            UUID PRIMARY KEY DEFAULT uuidv7(),
    source_id     UUID NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    token_hash    BYTEA NOT NULL,
    last_seen_at  TIMESTAMPTZ,
    expires_at    TIMESTAMPTZ NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX cli_sessions_source_idx  ON cli_sessions (source_id);
CREATE INDEX cli_sessions_expires_idx ON cli_sessions (expires_at);
