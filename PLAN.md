# dstream — Design & Roadmap

Live design doc: what dstream is, how it's built, what has shipped, what's next.

- **Per-phase designs:** `docs/superpowers/specs/` (27 design docs, one per slice)
- **User-facing overview:** `README.md`

**Status:** Phases 1–4 shipped, plus 5a (RBAC enforcement + API-key roles) and 5b (instance-level OIDC single sign-on). The rest of Phase 5 (5c billing hooks) and Phase 6 remain.

---

## 1. What it is

An open-source webhook management, monitoring and testing platform, comparable to Hookdeck. It sits between webhook senders (Stripe, GitHub, Shopify, your own services) and your app: it accepts inbound webhooks, persists every request, applies per-connection filter/transform/retry policy, and forwards to your endpoints — while every attempt stays inspectable in a dashboard. It also publishes outbound webhooks to your own customers (Svix-style signed fan-out).

**Positioning:** *the dev IDE for webhooks.* Two differentiator bets:

1. **Best local dev loop** — a first-class CLI: tunnel to localhost, replay, fixture library, ordered scenarios. Test webhook handlers like unit tests.
2. **Record/replay third-party providers** — VCR-style capture of live provider traffic for deterministic CI.

**Deploy model:** one codebase serves SaaS (multi-tenant) and self-host (Docker Compose / Helm / single binary), like PostHog or Convoy.

---

## 2. Architecture

A **modular monolith**: one Go binary with subcommands. Self-hosters run one container plus Postgres and Redis. SaaS runs N replicas of the same binary, optionally split by subcommand to scale horizontally. Splitting further later is mechanical; microservices up front would punish the self-host UX.

```
cmd/dstream/          subcommands: server | worker | cli | migrate | admin | maintenance
internal/
  ingest/             HTTP receiver, dedup, body persistence, fan-out to events
  dqueue/             Redis per-org fair delivery queue (Lua-atomic; replaced asynq)
  deliver/            outbound HTTP delivery, retry policy, rate/in-flight gates
  webhook/            outbound (Svix-style) publish + signed subscriber fan-out
  opevents/           operational webhooks (per-org app, lifecycle triggers)
  filter/             CEL filter evaluation (cost-bounded)
  transform/          JS sandbox (goja) for per-connection/endpoint transforms
  bookmark/           fixture capture, replay, reinject, export
  store/              Postgres data access (sqlc-generated)
  api/                REST API for dashboard + CLI control plane
  admin/              super-admin console endpoints (cross-tenant, queue ops)
  auth/               API keys, sessions, magic links, OIDC SSO, portal tokens, RBAC
  audit/              audit-log writes
  config/             Viper env config
  logging/  metrics/  tracing/  middleware/  mailer/
web/                  TanStack Start dashboard (+ customer-facing App Portal)
db/                   schema.sql (source of truth), migrations (Atlas), sqlc queries
deploy/docker/        Dockerfile + docker-compose.yml (dev stack)
deploy/helm/          empty — Phase 6
tools/loadtest/       ingest load harness (`make load`)
```

### Inbound flow

`POST /e/{ingest_token}` → resolve source (in-process cache) → read body (5 MB cap) → dedup (`SETNX dedup:{source_id}:{body_hash} EX 60`) → persist request + body → fan out one `events` row per enabled connection → enqueue each onto `dqueue`. Responds `202 {request_id, event_ids[]}` without waiting on delivery.

A worker pool drains `dqueue`, and per event: applies the per-destination rate-limit and max-in-flight gates (deferring rather than burning retry budget), evaluates the connection's CEL filter (a miss is terminal `filtered`), runs its goja transform, then POSTs to the destination with `Dstream-Event-Id` / `Dstream-Event-Attempt` headers. Non-2xx retries on the connection's policy (`exponential` | `linear` | `fixed` | `custom` schedule, with jitter and a cap) until the budget is exhausted, then dead-letters.

### Fair queueing

`dqueue` replaced asynq (commit 61b9c20) to get **absolute per-org fairness**: pending events sit in one Redis LIST per org with a round-robin ring of org ids, so one org's backlog can never delay another's. Every multi-key mutation is a single Lua script, making it correct across worker nodes without locks. At-least-once: a picked event holds a lease in a processing ZSET, and a recoverer reinjects anything whose lease expires. See `docs/superpowers/specs/2026-07-18-fair-delivery-queue-design.md`.

### Outbound flow

Applications own endpoints and subscribe to event types. Publishing a message validates it against the event type's JSON schema, fans out to every matching endpoint (event-type filter ∧ channel overlap), signs the post-transform bytes, and delivers through the same queue and retry machinery. Endpoints auto-disable after sustained failure and can be recovered; secrets rotate. Customers manage their own endpoints through the App Portal (scoped token, epoch kill-switch).

---

## 3. Tech stack

| Concern | Choice |
| --- | --- |
| Backend | Go (one binary, subcommands) |
| Frontend | TanStack Start (React 19, Router + Query), bun |
| Database | Postgres via `pgx/v5`; `sqlc` for compile-time-safe queries |
| Migrations | **Atlas** (`schema.sql` is the source of truth; migrations generated by diff) |
| Queue | **`internal/dqueue`** — Redis lists/ZSETs + Lua (no asynq) |
| Redis | `redis/go-redis/v9` — dedup, source cache, in-flight counters, CLI sessions |
| Rate limiting | `go-redis/redis_rate/v10` (token bucket) |
| Filters | `google/cel-go` (compile on write, evaluate on delivery) |
| Transforms | `dop251/goja` (locked-down sandbox, interrupt + output cap) |
| Payload storage | Postgres (`request_bodies`) behind a `BodyStore` interface |
| CLI tunnel | `coder/websocket` |
| SSO | `coreos/go-oidc/v3` (discovery, JWKS rotation, ID-token verification) + `golang.org/x/oauth2` (code exchange) |
| Config | `spf13/viper` (env vars, documented in `.env.example`) |
| Observability | slog, Prometheus `/metrics`, OpenTelemetry → Jaeger (`otelpgx` for DB spans) |
| Router | `go-chi/chi/v5` |

---

## 4. Data model

Tenancy is **org-scoped** (`organizations` + `org_members`; there is no separate project layer). Every row is owned by an org, and every query scopes by it — middleware resolves the org from an API key or session.

**Identity / tenancy:** `organizations`, `org_members`, `users`, `api_keys`, `org_invites`, `magic_link_tokens`, `audit_logs`

SSO adds **no table and no column**. An OIDC login joins on the existing `users.email` (already `CITEXT UNIQUE`), so there is no `sso_identities` mapping and Phase 5b shipped without a migration — the deliberate consequence of one IdP per deployment. A per-org IdP model would need that mapping, since one address could then legitimately exist under two issuers (§8).

**Inbound pipeline:** `sources` (ingest token, signing config, allowed methods) → `requests` + `request_bodies` → `events` (status: queued | in_flight | delivered | failed | dead | discarded | filtered) → `attempts`; routed by `connections` (retry policy, `filter_expr`, `transform_js`) to `destinations` (http | cli, rate limits, max in-flight). `cli_sessions` backs the tunnel.

**Outbound pipeline:** `applications` → `endpoints` (secret, event-type filter, channels, headers, rate limit) and `event_types` (JSON schema) → `messages` → `message_deliveries` → `message_delivery_attempts`.

**Testing / fixtures:** `bookmarks` (a retention-pinned pointer to a stored request), `capture_rules` (auto-capture by CEL filter, capped), `scenarios` + `scenario_steps` (ordered replay with per-step delay).

---

## 5. Roadmap

| # | Phase | Status |
| --- | --- | --- |
| 1 | Core inbound gateway | ✅ shipped (hardened) |
| 2 | Outbound webhooks — Svix model | ✅ shipped |
| 3 | Transforms (goja) + filters (CEL) | ✅ shipped |
| 4 | Record/replay + fixture library | ✅ shipped |
| 5 | Multi-tenant hardening — full RBAC, SSO, billing hooks | 🚧 5a + 5b shipped (RBAC, key roles, OIDC SSO); 5c billing planned |
| 6 | Self-host packaging — Helm chart, single-binary release | planned (`deploy/helm/` is empty; compose ships) |

A visual workflow builder held the fifth slot until 2026-09-29, when it was dropped and the remaining phases moved up — see §7.

---

## 6. What shipped

### Phase 1 — core inbound gateway ✅

Ingest → dedup → durable store → fair-queued delivery → retry → dashboard, plus the CLI tunnel (`dstream cli listen --forward`) for localhost forwarding. Magic-link auth, org membership, API keys, audit log, super-admin console. Security-hardened (SSRF guards on every outbound fetch, loop guard against dstream's own hosts, body caps, org-scoped everything).

Tracing and the load harness landed in Phase 3. Measured on a local dev box: 100 req/s × 60 s = 5,989 events, 0 errors, **ingest p99 14.1 ms**, **delivery-start p99 267 ms** — both inside the targets (local hardware, not a clean cloud baseline; see `tools/loadtest/README.md`).

### Phase 2 — outbound webhooks ✅

Svix-style apps / endpoints / event types / signed messages, with endpoint lifecycle (rotation, auto-disable, recover, test-send), replay, and a loop guard. **2c** added the customer-facing App Portal (app-scoped token, `/api/portal/*` + `/portal` SPA, mint/revoke with an epoch kill-switch). **2d** added delivery controls (per-endpoint headers, per-endpoint rate limit with defer-on-no-budget, channels with `∧` overlap fan-out, event-type JSON-schema validation refusing external `$ref`s). **2e** added operational webhooks (a per-org operational app reusing the same engine, triggered on `endpoint.disabled` and `message.attempt.exhausted`) plus payload retention/expiry sweeps.

Specs: `2026-07-24-phase-2a-outbound-webhooks-svix`, `2026-08-27-phase-2b-endpoint-lifecycle`, `2026-08-31-cycle-b-outbound-dashboard-ui`, `2026-09-01-phase-2c-app-portal`, `2026-09-04-phase-2d-delivery-controls`, `2026-09-16-phase-2e-operational-webhooks-retention`.

### Phase 3 — transforms + filters ✅

`internal/filter` (cel-go, cost-bounded, compiled on write and evaluated on delivery, fail-open) and `internal/transform` (goja in a locked-down sandbox: no injected capabilities, 1 s interrupt, output cap, object/array returns only), wired into **both** pipelines — the outbound side signs post-transform bytes. A delivery-time filter miss is a terminal `filtered` status. Editable per connection and per endpoint from the dashboard, the App Portal, and preview endpoints.

Also completed the tracing story (ingest child spans, an outbound `webhook.deliver` span, and `trace_id`/`span_id` stamped onto log lines) and shipped the load harness.

Spec: `2026-09-18-phase-3-filters-transforms-design.md`.

### Phase 4 — record/replay + fixtures ✅

- **4a** — `bookmarks`: a retention-pinned pointer to a stored request, so a fixture never expires. `internal/bookmark` engine: reinject through the pipeline as `is_test` events, replay to an SSRF-guarded URL, or export portable JSON. CLI `dstream cli fixtures` / `replay --forward`; dashboard Fixtures page.
- **4b-i** — **import** (`POST /api/bookmarks/import`) materialises an exported fixture as a real request + body + bookmark under a chosen source, closing the export → import → replay loop.
- **4b-ii** — **auto-capture rules**: per-source CEL rules cached in the ingest source-cache, so a source with no rules pays nothing per request. Matching requests best-effort auto-bookmark and roll off past a cap; it can never slow or fail ingest.
- **4b-iii** — **ordered scenarios**: a named sequence of fixtures, each with a pre-delay, replayed in order to an SSRF-guarded URL (delay capped at 60 s, 50 steps max, context-cancelable, stops on first error). CLI `dstream cli scenario run --forward`; dashboard builder.

Specs: `2026-09-19-phase-4a-record-replay-fixtures`, `-4b-fixture-import`, `-4b-ii-auto-capture`, `-4b-iii-scenarios`.

### Phase 5a — RBAC enforcement + API-key roles ✅

The `owner | admin | member` ladder now gates the tenant traffic plane, not
just org administration. `AdminForDestructive` gates every `DELETE` in the
group at admin — so a destructive route added later is covered without an
opt-in — and five privileged routes carry an explicit admin mark: endpoint
secret read and rotate, outbound publish, portal-access mint and revoke.
Members keep read, create and edit, plus the operational actions (retry,
replay, test-send, recover). API keys carry their own role (`api_keys.role`,
default `admin`, `owner` refused by CHECK), so a CI key can be restricted. The
App Portal is unchanged.

A 78-row route matrix test asserts every traffic-plane route against every
role, and a `chi.Walk` coverage assertion fails if a route is added without a
matrix entry.

Spec: `2026-09-30-phase-5a-rbac-enforcement-design.md`.

### Phase 5b — OIDC single sign-on ✅

**Instance-level** SSO: one IdP for the whole deployment, enabled by a
non-empty `DSTREAM_OIDC_ISSUER` (there is no separate on/off flag to drift out
of sync with the credentials). Discovery runs once at boot, so a wrong issuer
fails the boot instead of every login, and the startup log prints the exact
redirect URI to register at the IdP. Per-org IdP connections are **not**
implemented — see §7 for that decision and its cost.

Three unauthenticated routes, all `GET`:

- `/api/auth/sso/start` — writes a single-use state + nonce to Redis (10 min,
  the IdP round-trip budget), sets a short-lived `dstream_sso_state` cookie
  carrying the same state, and 302s to the IdP. Rate-limited per client IP at
  300/hour, because each call writes a key into the same Redis instance that
  carries the delivery queue.
- `/api/auth/sso/callback` — `GETDEL`s the state, **requires the cookie to
  equal the `state` query parameter**, verifies the ID token via `go-oidc`,
  binds the nonce, then provisions and issues the session.
- `/api/auth/methods` — `{sso, magic_link}`, so the login page renders the
  controls that actually work instead of a button that always fails.

**The cookie binding is what makes a `GET` callback safe**, and it is one of
two corrections the spec took mid-implementation (the other being the dropped
boot-time default-org check, below). Single-use state alone is not
sufficient: `/sso/start` is unauthenticated, so an attacker can mint a valid
state, complete the IdP login with *his own* account, and phish a victim into
a top-level `GET` of the callback inside the TTL — every other check passes
and the victim's browser is handed a session for the attacker's account.
`SameSite=Lax` constrains cookie *sending*, not *setting*. OAuth 2.0 Security
BCP §4.7. The named residual is cookie tossing from a compromised sibling
subdomain, the same caveat `internal/middleware/csrf.go` carries.

The verified email is the join key against `users.email`, so a **missing or
false `email_verified` is refused** with a distinct message — an IdP issuing
unverified email claims would otherwise let someone assert an address they do
not control. `return_to` is reduced to an app-relative path before the final
redirect; an open redirect on a login callback is a credential-phishing
primitive.

`ConsumeMagicLink`'s account bootstrap was extracted to
`auth.BootstrapSession`, and **both** login methods now route through it, so
they cannot drift on invite application, personal-workspace creation, or the
rule that a fresh login never escalates an existing member's role. The
callback then calls the same `Signer.Issue` the magic-link path does, so
`session_epoch` revocation and cookie flags are shared rather than
reimplemented — a divergent session here would have meant logout-all silently
not covering SSO users. Every existing magic-link test passes **unmodified**,
which was the extraction's gate.

`DSTREAM_OIDC_DEFAULT_ORG` / `DSTREAM_OIDC_DEFAULT_ROLE` join a first-time SSO
user to one org instead of minting them a personal workspace. `owner` is
refused at boot; an existing membership always wins, so flipping the role never
re-grades anyone. The slug is deliberately **not** validated at boot (that
check cannot run before the DB pool opens, and running it after would brick a
deployment whose default org is created by a later seed step) — instead
`auth.ErrDefaultOrgNotFound` surfaces at the first SSO login as a `500` naming
the variable, distinct from the `401` an auth failure returns.

**No new table, no column, no migration** (§4). The App Portal and API-key
auth are untouched — machines do not do SSO.

**Enforcement.** `DSTREAM_OIDC_ENFORCE=true` refuses magic-link **minting** on
both HTTP paths — `POST /api/auth/magic-link/request` *and*
`POST /api/invites/{token}/accept`, whose signed-out fallback also mails a
link. Guarding only the first would have left anyone holding a live invite
token a working bypass. **Redemption stays open** (`/magic-link/verify` is not
gated), which is what keeps the break-glass usable. API keys are unaffected.

**Break-glass: `dstream admin magic-link <email>`** mints a link for an
existing user and prints it, bypassing enforcement. Reaching it requires shell
access to the host, which is a stronger factor than any IdP — but say it
plainly: **this command is as powerful as host shell access, because it mints a
login for any existing user.** That is intentional, not a gap.

It writes an audit row (`auth.break_glass_magic_link`) directly via
`InsertAuditLog` rather than through `audit.Log`, which is a documented no-op
when there is no Principal in ctx — exactly the CLI case, so the promised trail
would not have existed. Two known limitations of that row:

- **Its actor columns name the *target* user, not the operator.**
  `audit_logs`' actor `CHECK` requires exactly one of `actor_user_id` /
  `actor_api_key_id` to be non-null, and the real actor is whoever holds the
  shell. The operator's identity is in the metadata instead (`actor: "cli"`,
  `host`, `os_user`). A self-referential row beats no row.
- **It is filed in the user's oldest-membership org only** (`ListOrgsForUser`
  is ordered by `created_at ASC`, and the row takes `orgs[0]`). A multi-org
  user's break-glass is therefore invisible in their other orgs' audit trails.

**`DSTREAM_MAGIC_LINK_TTL` currently affects only the break-glass.** Both HTTP
magic-link paths hardcode 15 minutes (`identity.magicLinkTTL` and the literal
in `invites.go`); the CLI is the config value's first and only reader. The
variable is not yet the global knob its name implies.

**Not yet exercised against a real IdP.** The protocol layer is tested against
a locally-signing fake IdP — including wrong-audience and unpublished-signing-
key rejection, absent vs. false `email_verified`, and an error path that omits
the authorization code — and the callback against a fake `Authenticator`
(state replay, browser binding, nonce mismatch, foreign `return_to`, default-org
join, enforcement). The browser flow was checked against a stubbed methods
endpoint. **No Okta / Entra ID / Google Workspace round-trip has been run**, so
the first operator to enable SSO is the first person to exercise the real
handshake. That is the honest ceiling on this slice.

Deferred: per-org IdP connections, SAML, SCIM, and group-to-role mapping from
IdP claims — all §8. **SSO changes nothing about webhook auth** — inbound
signature verification and outbound delivery auth remain deliberately deferred
(§7, 2026-07-06).

Spec: `2026-10-01-phase-5b-oidc-sso-design.md`.

### Beyond the phases

**Super-admin queue ops** — `/console/queues`: per-lane drill-down (dead / scheduled / processing / pending), an all-orgs pending table, and safe ops (requeue a dead event, force-promote a scheduled one, drain the dead list), each a single atomic Lua script. Spec: `2026-09-26-admin-queue-ops-design.md`.

---

## 7. Decisions log

| Date | Decision |
| --- | --- |
| 2026-07-06 | **Webhook auth deferred to post-release.** No inbound signature verification and no outbound delivery auth. Plain forwarding only: `requests.sig_verified` is always false and `destinations.auth_config` is stored-but-unused. Columns and API fields are kept so auth lands without a migration. |
| 2026-07-18 | **asynq → `dqueue`.** Replaced asynq/asynqmon with a hand-rolled Redis fair queue to get absolute per-org fairness. Accepted cost: reimplementing retry, backoff, dead-letter, scheduling, crash recovery and monitoring. |
| 2026-09-29 | **Visual workflow builder dropped**, and the phases after it renumbered (multi-tenant hardening → 5, self-host packaging → 6). It held the fifth slot while it lasted, which is why its spec is filed as `2026-09-29-phase-5-visual-workflow-builder-design.md`. A drag-to-connect canvas over the existing connection model was built, reviewed, and removed: the connections page's structured view already reads the topology, and the table view plus the create dialog already build it — so the canvas added a dependency and a third way to do the same thing. A visual builder only earns its place alongside a real multi-step pipeline model (source → chained filter/transform/branch nodes → fan-out), which stays deferred. |
| 2026-09-30 | **Members cannot delete anything**, fixtures and scenarios included. A carve-out for disposable test artifacts was considered and rejected: an exemption list beside the blanket `DELETE ⇒ admin` rule is a second source of truth, and the uniform rule is one sentence to document. Cost: a member must ask an admin to clean up their own fixtures. |
| 2026-10-01 | **SSO is instance-level, not per-org.** One IdP per deployment, configured by env var. Per-org connections would need client secrets encrypted at rest, and dstream has no secret-encryption facility or key management — that is a subsystem, not a slice. They would also need the `sso_identities` mapping this slice avoided, since one address could then legitimately exist under two issuers. Cost: a multi-tenant SaaS deployment cannot offer per-tenant SSO until that is built. |
| 2026-10-01 | **`DSTREAM_OIDC_DEFAULT_ORG` is not validated at boot** (amended mid-implementation; an earlier draft promised it would be). The check cannot live in config validation — that runs before the DB pool opens — and placing it after the pool would couple startup to database seeding state, bricking a deployment whose default org is created by a seed job *after* first boot. Cost: a bad slug surfaces at the first SSO login instead, so it must surface legibly — a `500` naming the variable plus a server log line, never the `401` an auth failure returns. |
| 2026-10-01 | **The SSO callback binds its state to the browser with a cookie**, correcting this spec's own first draft, which claimed single-use state was sufficient for a `GET` callback. It was not: `/sso/start` is unauthenticated, so an attacker mints a valid state for free, logs in as himself, and phishes a victim into a top-level `GET` of the callback — a working session-fixation bug that the slice's first implementation carried, caught in review before it was committed. Accepted residual: `dstream_sso_state` is not `__Host-` prefixed (that needs `Secure`, false in local HTTP dev), so cookie tossing from a compromised sibling subdomain remains in scope, as it does for the CSRF cookie. |

---

## 8. Deferred & out of scope

**Deferred:** pause/resume an org's delivery lane (needs a change to the correctness-critical `FairPick` Lua); per-connection and per-destination queue breakdowns; historical queue metrics.

**Deferred to later phases:** billing hooks (Phase 5c — RBAC shipped in 5a, OIDC SSO in 5b); cross-org fixture sharing (Phase 5); Helm chart and single-binary release (Phase 6).

**Deferred SSO surface (Phase 5b shipped instance-level only):** per-org IdP connections and domain-routed login — blocked on dstream having no secret-encryption facility or key management for per-tenant client secrets, plus the `sso_identities` mapping they imply; **SAML** (XML canonicalisation, signature verification, metadata exchange, a heavy dependency — OIDC already covers Okta, Entra ID, Google Workspace, Auth0 and Keycloak); **SCIM / directory sync** (deprovisioning stays manual — remove the member); **group-to-role mapping from IdP claims** (two mapped groups, a claim absent on one login and present on the next, and whether a mapping may demote an owner are a slice's worth of decisions that silently change privileges when wrong). Also still open from 5a: members keep `PATCH` on destination and endpoint URLs, so a member can repoint live traffic — a decision about where the admin line sits, independent of SSO.

**Out of scope entirely:** managed cloud signup, mobile apps, alerting beyond email/webhook, custom domains, payload encryption at rest.

---

## 9. Operations & verification

`docker compose -f deploy/docker/docker-compose.yml up -d --build` brings up the whole dev stack (server, worker, web, Postgres, Redis, Jaeger) and runs migrations. Config is env-var driven via Viper — see `.env.example`. Note the split hosts in dev: the API and ingest are on `:8080`, the dashboard on `:3000`.

`/metrics` is Prometheus-format but **cookie-gated to super-admins**, so a stock scraper can't read it — it's browse-only today, and no scraper ships in the dev compose. Tracing is off unless `DSTREAM_TRACING_ENABLED` is set.

**Smoke path:** create a source → `curl` its ingest URL → the event appears and reaches `delivered` against a live destination. Point a connection at a 500 and watch the configured backoff play out in the attempts table, then "Retry now". Send the same body twice inside 60 s and confirm only the first creates events. Run `dstream cli listen --source X --forward http://localhost:3000/hook` and confirm the local response is captured as the attempt.

Go tests need a migrated test database and Redis:

```
DSTREAM_TEST_DB_URL="postgres://dstream:dstream@127.0.0.1:5433/dstream_test?sslmode=disable" \
DSTREAM_REDIS_ADDR=127.0.0.1:6379 go test ./... -count=1
```

The web app has **no test suite** — `tsc --noEmit` plus `bun run build` are its only gates, so UI regressions surface by hand.

**SSO needs a manual first pass.** Phase 5b's protocol layer is tested against
a locally-signing fake IdP and its callback against a fake `Authenticator`, but
**no real-IdP handshake has ever been run** — no Okta, Entra ID or Google
Workspace round-trip. The first operator to set `DSTREAM_OIDC_ISSUER` is
exercising it first. The pass to run: register
`<DSTREAM_PUBLIC_BASE_URL>/api/auth/sso/callback` at the IdP (the server logs
the exact string at startup), sign in from the login page, confirm the session
lands and the user holds the expected role in the expected org, then log out
and confirm the session is dead. If the IdP omits `email_verified`, the
callback refuses by design — add the claim at the IdP.
