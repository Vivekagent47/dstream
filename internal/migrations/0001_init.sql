-- +goose Up
-- +goose StatementBegin

CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE EXTENSION IF NOT EXISTS citext;

-- ---------------------------------------------------------------------------
-- Identity: organizations, users, memberships, projects, API keys
-- ---------------------------------------------------------------------------

CREATE TABLE organizations (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL,
    slug        TEXT NOT NULL UNIQUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE users (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email           CITEXT NOT NULL UNIQUE,
    name            TEXT,
    is_super_admin  BOOLEAN NOT NULL DEFAULT FALSE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE org_members (
    org_id      UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role        TEXT NOT NULL DEFAULT 'member',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, user_id)
);

CREATE TABLE projects (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id      UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    slug        TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, slug)
);

CREATE TABLE api_keys (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id    UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    prefix        TEXT NOT NULL,
    key_hash      BYTEA NOT NULL,
    last_used_at  TIMESTAMPTZ,
    revoked_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX api_keys_project_idx ON api_keys (project_id);
CREATE UNIQUE INDEX api_keys_prefix_idx ON api_keys (prefix) WHERE revoked_at IS NULL;

CREATE TABLE magic_link_tokens (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email       CITEXT NOT NULL,
    token_hash  BYTEA NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    used_at     TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX magic_link_tokens_email_idx ON magic_link_tokens (email);
CREATE INDEX magic_link_tokens_expires_idx ON magic_link_tokens (expires_at);

-- ---------------------------------------------------------------------------
-- Sources, Destinations, Connections
-- ---------------------------------------------------------------------------

CREATE TABLE sources (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id      UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name            TEXT NOT NULL,
    type            TEXT NOT NULL, -- e.g., 'generic', 'stripe', 'github', 'shopify'
    ingest_token    TEXT NOT NULL UNIQUE,
    signing_config  JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (project_id, name)
);

CREATE INDEX sources_project_idx ON sources (project_id);

CREATE TABLE destinations (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id        UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name              TEXT NOT NULL,
    type              TEXT NOT NULL CHECK (type IN ('http', 'cli')),
    url               TEXT,
    auth_config       JSONB NOT NULL DEFAULT '{}'::jsonb,
    rate_limit_rps    INTEGER,
    rate_limit_burst  INTEGER,
    max_inflight      INTEGER,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (project_id, name)
);

CREATE INDEX destinations_project_idx ON destinations (project_id);

CREATE TABLE connections (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_id               UUID NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
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

CREATE INDEX connections_source_idx ON connections (source_id);
CREATE INDEX connections_destination_idx ON connections (destination_id);

-- ---------------------------------------------------------------------------
-- Requests, Events, Attempts
-- ---------------------------------------------------------------------------

CREATE TABLE requests (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_id     UUID NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    http_method   TEXT NOT NULL,
    http_path     TEXT NOT NULL,
    headers       JSONB NOT NULL DEFAULT '{}'::jsonb,
    body_hash     TEXT NOT NULL,
    body_ref      TEXT NOT NULL, -- key into object storage (or postgres bytea ref)
    body_size     INTEGER NOT NULL,
    content_type  TEXT,
    sig_verified  BOOLEAN NOT NULL DEFAULT FALSE,
    ingest_ip     INET,
    received_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX requests_source_received_idx ON requests (source_id, received_at DESC);
CREATE INDEX requests_body_hash_idx ON requests (source_id, body_hash);

CREATE TABLE request_bodies (
    request_id   UUID PRIMARY KEY REFERENCES requests(id) ON DELETE CASCADE,
    body         BYTEA NOT NULL,
    stored_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE events (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    request_id      UUID NOT NULL REFERENCES requests(id) ON DELETE CASCADE,
    connection_id   UUID NOT NULL REFERENCES connections(id) ON DELETE CASCADE,
    status          TEXT NOT NULL DEFAULT 'queued'
                    CHECK (status IN ('queued', 'in_flight', 'delivered', 'failed', 'paused', 'dead')),
    attempt_count   INTEGER NOT NULL DEFAULT 0,
    last_attempt_at TIMESTAMPTZ,
    next_retry_at   TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX events_connection_status_idx ON events (connection_id, status);
CREATE INDEX events_request_idx ON events (request_id);
CREATE INDEX events_status_created_idx ON events (status, created_at DESC);

CREATE TABLE attempts (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
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

-- ---------------------------------------------------------------------------
-- CLI sessions (local-forward tunnel)
-- ---------------------------------------------------------------------------

CREATE TABLE cli_sessions (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_id     UUID NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    token_hash    BYTEA NOT NULL,
    last_seen_at  TIMESTAMPTZ,
    expires_at    TIMESTAMPTZ NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX cli_sessions_source_idx ON cli_sessions (source_id);
CREATE INDEX cli_sessions_expires_idx ON cli_sessions (expires_at);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS cli_sessions;
DROP TABLE IF EXISTS attempts;
DROP TABLE IF EXISTS events;
DROP TABLE IF EXISTS request_bodies;
DROP TABLE IF EXISTS requests;
DROP TABLE IF EXISTS connections;
DROP TABLE IF EXISTS destinations;
DROP TABLE IF EXISTS sources;
DROP TABLE IF EXISTS magic_link_tokens;
DROP TABLE IF EXISTS api_keys;
DROP TABLE IF EXISTS projects;
DROP TABLE IF EXISTS org_members;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS organizations;

-- +goose StatementEnd
