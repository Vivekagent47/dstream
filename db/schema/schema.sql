-- =========================================================================
-- Extensions (installed via db/migrations/00000000000000_extensions.sql)
-- pgcrypto, citext
-- =========================================================================

-- =========================================================================
-- Identity
-- =========================================================================

-- organizations: the tenant root. Every resource (sources, destinations,
-- events, keys…) hangs off an org; all API queries scope by org_id.
CREATE TABLE organizations (
    id          UUID PRIMARY KEY DEFAULT uuidv7(),
    name        TEXT NOT NULL,                -- display name shown in the dashboard
    slug        TEXT NOT NULL UNIQUE,         -- URL-safe identifier used in dashboard routes
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- users: humans who log into the dashboard (magic-link auth, no passwords).
-- Machine access uses api_keys instead.
CREATE TABLE users (
    id              UUID PRIMARY KEY DEFAULT uuidv7(),
    email           CITEXT NOT NULL UNIQUE,   -- login identity; case-insensitive
    name            TEXT,                     -- optional display name
    is_super_admin  BOOLEAN NOT NULL DEFAULT FALSE, -- gates /admin/* (cross-tenant ops); set via `dstream admin promote`
    -- Bumped to invalidate all of a user's outstanding session cookies
    -- (logout-all / disable / security events). Embedded in the signed cookie.
    session_epoch   INTEGER NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- org_members: user↔org membership with a role. A user can belong to many
-- orgs; role gates what they can manage inside one.
CREATE TABLE org_members (
    org_id      UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    user_id     UUID NOT NULL REFERENCES users(id)         ON DELETE CASCADE,
    role        TEXT NOT NULL DEFAULT 'member'  -- owner: full control incl. delete org; admin: manage members+resources; member: use resources
                  CHECK (role IN ('owner', 'admin', 'member')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, user_id)
);
-- ListOrgsForUser + CountOrgMembershipsForUser filter by user_id alone; PK
-- order (org_id, user_id) doesn't support those scans. Add a user-leading
-- index so the per-user lookups stay sub-millisecond as the table grows.
CREATE INDEX org_members_user_idx ON org_members (user_id);

-- api_keys: machine credentials for the REST API (`Authorization: Bearer
-- dsk_<prefix>_<secret>`). Only a hash of the secret is stored; the prefix
-- is the lookup handle. Revocation is soft (revoked_at) so old keys stay
-- auditable.
CREATE TABLE api_keys (
    id            UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id        UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,          -- human label ("CI deploy key")
    prefix        TEXT NOT NULL,          -- public, indexed part of the key; identifies the row without exposing the secret
    key_hash      BYTEA NOT NULL,         -- hash of the full secret; raw key shown once at creation, never stored
    last_used_at  TIMESTAMPTZ,            -- updated on successful auth; stale keys are candidates for cleanup
    revoked_at    TIMESTAMPTZ,            -- soft delete; NULL = active
    -- Optional expiry; NULL = never expires. Enforced in GetAPIKeyByPrefix.
    expires_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX api_keys_org_idx ON api_keys (org_id);
CREATE UNIQUE INDEX api_keys_prefix_idx ON api_keys (prefix) WHERE revoked_at IS NULL;

-- magic_link_tokens: single-use email login tokens. The emailed link carries
-- the plaintext token; only its hash lands here. Verifying marks used_at so
-- a link can't be replayed.
CREATE TABLE magic_link_tokens (
    id          UUID PRIMARY KEY DEFAULT uuidv7(),
    email       CITEXT NOT NULL,          -- address the link was issued to (user may not exist yet — first login creates them)
    token_hash  BYTEA NOT NULL,           -- hash of the token embedded in the emailed link
    expires_at  TIMESTAMPTZ NOT NULL,     -- short TTL; expired rows are purge candidates
    used_at     TIMESTAMPTZ,              -- set on verify; non-NULL = already consumed
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX magic_link_tokens_email_idx   ON magic_link_tokens (email);
CREATE INDEX magic_link_tokens_expires_idx ON magic_link_tokens (expires_at);

-- org_invites: pending invitations to join an org, sent by email. Accepting
-- (single-use, token-authenticated) creates the org_members row with the
-- invited role. At most one pending invite per (org, email).
CREATE TABLE org_invites (
    id          UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id      UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    email       CITEXT NOT NULL,          -- invitee address; matched case-insensitively on accept
    role        TEXT NOT NULL CHECK (role IN ('admin', 'member')),  -- role granted on accept; owners aren't invited, they're promoted
    token_hash  BYTEA NOT NULL,           -- hash of the token in the invite link
    invited_by  UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT, -- inviter, kept for the audit trail (RESTRICT: can't delete a user who has live invites)
    expires_at  TIMESTAMPTZ NOT NULL,
    accepted_at TIMESTAMPTZ,              -- non-NULL = consumed; partial indexes below only track pending rows
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX org_invites_org_idx     ON org_invites (org_id);
CREATE INDEX org_invites_email_idx   ON org_invites (email)      WHERE accepted_at IS NULL;
CREATE INDEX org_invites_expires_idx ON org_invites (expires_at) WHERE accepted_at IS NULL;
CREATE UNIQUE INDEX org_invites_pending_unique
  ON org_invites (org_id, email) WHERE accepted_at IS NULL;

-- audit_logs: append-only trail of who did what ("connection.create",
-- "member.remove", …). Written by handlers via audit.Log; never updated.
-- org_id is nullable + ON DELETE SET NULL on purpose: when an org is
-- deleted, its audit trail MUST survive the cascade, otherwise an
-- attacker (or compromised owner) can erase the org and the evidence
-- atomically. The org_name_snapshot column denormalizes the org name at
-- write time so a tombstoned row remains human-readable even after the
-- organizations row is gone.
CREATE TABLE audit_logs (
    id                   BIGSERIAL PRIMARY KEY,
    org_id               UUID REFERENCES organizations(id) ON DELETE SET NULL,
    org_name_snapshot    TEXT,             -- org name at write time; survives org deletion
    actor_user_id        UUID REFERENCES users(id)    ON DELETE SET NULL,  -- who acted, when it was a dashboard user
    actor_api_key_id     UUID REFERENCES api_keys(id) ON DELETE SET NULL,  -- who acted, when it was an API key
    actor_email_snapshot TEXT,             -- actor's email at write time; survives user deletion
    action               TEXT NOT NULL,    -- dotted verb, e.g. "source.delete", "invite.accept"
    target_type          TEXT NOT NULL,    -- kind of resource acted on ("source", "connection", …)
    target_id            UUID,             -- id of that resource, when it has one
    metadata             JSONB NOT NULL DEFAULT '{}'::jsonb,  -- action-specific detail (old/new values, related ids)
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- exactly one actor: a row is either a user action or an API-key action
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

-- sources: inbound webhook endpoints. Each source owns a unique ingest URL
-- (POST /e/{ingest_token}); a provider (Stripe, GitHub, …) is pointed at it
-- and every hit becomes a requests row fanned out through connections.
CREATE TABLE sources (
    id              UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id          UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name            TEXT NOT NULL,            -- unique per org; used in CLI (`dstream listen --source <name>`)
    type            TEXT NOT NULL,            -- provider hint ("stripe", "github", "generic"); informational until per-provider verification lands
    ingest_token    TEXT NOT NULL UNIQUE,     -- opaque secret in the ingest URL /e/{token}; resolving it is how a hit finds its source
    signing_config  JSONB NOT NULL DEFAULT '{}'::jsonb,  -- provider signature-verification settings; stored but UNUSED until the auth phase (post-release)
    description     TEXT NOT NULL DEFAULT '', -- free-text notes shown in the dashboard
    allowed_methods TEXT[] NOT NULL DEFAULT '{POST,PUT,PATCH,DELETE}',  -- HTTP methods the ingest endpoint accepts; anything else gets 405
    enabled         BOOLEAN NOT NULL DEFAULT TRUE,  -- disabled sources reject ingest traffic
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, name)
);
CREATE INDEX sources_org_idx ON sources (org_id);

-- destinations: where events get delivered. 'http' destinations are POSTed
-- to by the worker; 'cli' destinations forward over the WebSocket tunnel to
-- a developer's `dstream listen` session.
CREATE TABLE destinations (
    id                UUID PRIMARY KEY DEFAULT uuidv7(),
    org_id            UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name              TEXT NOT NULL,          -- unique per org
    type              TEXT NOT NULL CHECK (type IN ('http', 'cli')),
    description       TEXT NOT NULL DEFAULT '', -- free-text notes shown in the dashboard
    url               TEXT,                              -- delivery endpoint; NULL for 'cli' type
    auth_config       JSONB NOT NULL DEFAULT '{}'::jsonb, -- delivery auth secrets (HMAC/bearer); never sent to client, API exposes auth_configured flag only. Stored but UNUSED until the auth phase (post-release)
    rate_limit_rps    INTEGER,                           -- max deliveries/sec to this endpoint; NULL = unlimited
    rate_limit_burst  INTEGER,                           -- token-bucket burst on top of rps: spike allowance after idle
    max_inflight      INTEGER,                           -- max concurrent in-flight deliveries; caps parallelism for slow endpoints
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (org_id, name)
);
CREATE INDEX destinations_org_idx ON destinations (org_id);

-- connections: the routing edge source → destination plus the retry policy
-- for deliveries on that edge. One source can fan out to many destinations;
-- each (source, destination) pair exists at most once. Policy is snapshotted
-- into the task at enqueue time, so edits affect future events only.
-- source_id/destination_id transitively scope the row to an org.
CREATE TABLE connections (
    id                      UUID PRIMARY KEY DEFAULT uuidv7(),
    source_id               UUID NOT NULL REFERENCES sources(id)      ON DELETE CASCADE,
    destination_id          UUID NOT NULL REFERENCES destinations(id) ON DELETE CASCADE,
    enabled                 BOOLEAN NOT NULL DEFAULT TRUE,  -- disabled edges are skipped at ingest fan-out; no events created
    name                    TEXT,                           -- optional human label; NULL shows as "(unnamed)" in the dashboard
    max_retries             INTEGER NOT NULL DEFAULT 1,     -- retries after the first attempt before the event is marked failed/dead (1 = one retry, 2 total attempts)
    retry_strategy          TEXT NOT NULL DEFAULT 'exponential'  -- exponential: base*2^n; linear: base*n; fixed: base; custom: explicit schedule
                              CHECK (retry_strategy IN ('exponential', 'linear', 'fixed', 'custom')),
    retry_base_ms           INTEGER NOT NULL DEFAULT 30000,   -- first-retry delay / multiplier input (30s)
    retry_cap_ms            INTEGER NOT NULL DEFAULT 3600000, -- upper bound on any computed delay (1h)
    retry_jitter_pct        INTEGER NOT NULL DEFAULT 20,      -- ± randomization applied to each delay, avoids thundering-herd retries
    custom_retry_schedule   JSONB,                            -- for strategy='custom': JSON array of delays in ms, one per attempt
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (source_id, destination_id)
);
CREATE INDEX connections_source_idx      ON connections (source_id);
CREATE INDEX connections_destination_idx ON connections (destination_id);

-- =========================================================================
-- Traffic: requests, events, attempts
-- =========================================================================

-- requests: one row per inbound HTTP hit on an ingest URL — the immutable
-- record of what the provider sent. The payload itself lives in
-- request_bodies; fan-out to destinations happens via events.
CREATE TABLE requests (
    id            UUID PRIMARY KEY DEFAULT uuidv7(),
    source_id     UUID NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    http_method   TEXT NOT NULL,
    http_path     TEXT NOT NULL,             -- path as received (incl. anything after the token)
    headers       JSONB NOT NULL DEFAULT '{}'::jsonb,  -- original request headers, replayed on delivery
    body_hash     TEXT NOT NULL,             -- SHA-256 of the body; dedup key (60s Redis SETNX window per source)
    body_ref      TEXT NOT NULL,             -- pointer to the stored body; today always the request_bodies row, later an object-store key
    body_size     INTEGER NOT NULL,          -- bytes (ingest caps at 5MB)
    content_type  TEXT,
    sig_verified  BOOLEAN NOT NULL DEFAULT FALSE,  -- provider-signature check result; always FALSE until the auth phase (post-release)
    ingest_ip     INET,                      -- caller IP, for debugging/abuse tracing
    received_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX requests_source_received_idx ON requests (source_id, received_at DESC);
CREATE INDEX requests_body_hash_idx       ON requests (source_id, body_hash);

-- request_bodies: raw payload bytes, split out so the hot requests table
-- stays narrow. 1:1 with requests; swap target for S3/MinIO later without
-- touching requests rows (body_ref indirection).
CREATE TABLE request_bodies (
    request_id  UUID PRIMARY KEY REFERENCES requests(id) ON DELETE CASCADE,
    body        BYTEA NOT NULL,
    stored_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- events: the unit of delivery — one per (request × enabled connection) at
-- ingest fan-out. Tracks the delivery lifecycle; each concrete try is an
-- attempts row. The delivery worker drives state via the fair per-org queue.
CREATE TABLE events (
    id              UUID PRIMARY KEY DEFAULT uuidv7(),
    request_id      UUID NOT NULL REFERENCES requests(id) ON DELETE CASCADE,
    connection_id   UUID NOT NULL REFERENCES connections(id) ON DELETE CASCADE,
    org_id          UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,  -- denormalized from the connection so org-scoped listing needs no joins
    status          TEXT NOT NULL DEFAULT 'queued'  -- queued: awaiting worker; in_flight: being delivered; delivered: 2xx; failed: retries exhausted; paused: CLI offline; dead: retries exhausted; discarded: CLI tunnel deadline passed with no listener (manual retry only)
                      CHECK (status IN ('queued', 'in_flight', 'delivered', 'failed', 'paused', 'dead', 'discarded')),
    attempt_count   INTEGER NOT NULL DEFAULT 0,     -- deliveries tried so far; compared against connection.max_retries
    last_attempt_at TIMESTAMPTZ,
    next_retry_at   TIMESTAMPTZ,                    -- when the next retry is scheduled; NULL once terminal
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    is_test         BOOLEAN NOT NULL DEFAULT FALSE  -- synthetic events from the test-connection endpoint; excluded from health metrics
);
CREATE INDEX events_connection_status_idx ON events (connection_id, status);
CREATE INDEX events_request_idx           ON events (request_id);
CREATE INDEX events_status_created_idx    ON events (status, created_at DESC);
CREATE INDEX events_org_created_idx       ON events (org_id, created_at DESC, id DESC);
-- Keyset pagination for ListEventsByOrgAndConnection (connection detail Events
-- tab): connection_id equality + (created_at DESC, id DESC) range/order in one
-- index, so a page is a bounded range scan instead of sorting the connection's
-- full event set. events_connection_status_idx can't serve the ordering.
CREATE INDEX events_connection_created_idx ON events (connection_id, created_at DESC, id DESC);
-- Cross-tenant created_at range scans by the super-admin console (HotDestinations,
-- AdminEventsSince, AdminTopSources) have no org/status prefix, so the org- and
-- status-leading indexes above can't serve them — this plain created_at index
-- turns those seq scans into range scans (audit #17).
CREATE INDEX events_created_at_idx ON events (created_at DESC);

-- attempts: one row per concrete delivery try of an event — the response
-- (or error) the destination gave. Immutable; the dashboard's event detail
-- page is a read of these.
CREATE TABLE attempts (
    id                UUID PRIMARY KEY DEFAULT uuidv7(),
    event_id          UUID NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    attempt_num       INTEGER NOT NULL,       -- 1-based try counter, unique per event
    response_status   INTEGER,                -- HTTP status from the destination; NULL when the request never completed (timeout/conn refused)
    response_headers  JSONB,
    response_body     BYTEA,                  -- captured (truncated) response payload, for debugging
    duration_ms       INTEGER,                -- wall time of the delivery HTTP call
    queued_in_ms      INTEGER,                -- time spent waiting in the queue before this try started
    error_message     TEXT,                   -- transport-level failure description when there was no HTTP response
    attempted_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (event_id, attempt_num)
);
CREATE INDEX attempts_event_idx ON attempts (event_id);

-- =========================================================================
-- CLI tunnel
-- =========================================================================

-- cli_sessions: live `dstream listen` WebSocket sessions. The worker
-- delivers events for type='cli' destinations over the socket registered
-- here; heartbeats bump last_seen_at, and events pause while no session is
-- connected.
CREATE TABLE cli_sessions (
    id            UUID PRIMARY KEY DEFAULT uuidv7(),
    source_id     UUID NOT NULL REFERENCES sources(id) ON DELETE CASCADE,  -- source whose traffic this session is forwarding
    token_hash    BYTEA NOT NULL,           -- hash of the session token the CLI authenticates the socket with
    last_seen_at  TIMESTAMPTZ,              -- bumped by the 15s heartbeat; staleness = dead session
    expires_at    TIMESTAMPTZ NOT NULL,     -- hard cutoff; expired rows are purge candidates
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX cli_sessions_source_idx  ON cli_sessions (source_id);
CREATE INDEX cli_sessions_expires_idx ON cli_sessions (expires_at);
