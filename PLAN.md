# dstream — Design & Roadmap

Live design doc: what dstream is, how it's built, what has shipped, what's next.

- **Per-phase designs:** `docs/superpowers/specs/` (25 design docs, one per slice)
- **User-facing overview:** `README.md`

**Status:** Phases 1–4 shipped. Phase 5 (visual workflow builder) was dropped. Phases 6–7 remain.

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
  auth/               API keys, sessions, magic links, portal tokens, RBAC stub
  audit/              audit-log writes
  config/             Viper env config
  logging/  metrics/  tracing/  middleware/  mailer/
web/                  TanStack Start dashboard (+ customer-facing App Portal)
db/                   schema.sql (source of truth), migrations (Atlas), sqlc queries
deploy/docker/        Dockerfile + docker-compose.yml (dev stack)
deploy/helm/          empty — Phase 7
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
| Config | `spf13/viper` (env vars, documented in `.env.example`) |
| Observability | slog, Prometheus `/metrics`, OpenTelemetry → Jaeger (`otelpgx` for DB spans) |
| Router | `go-chi/chi/v5` |

---

## 4. Data model

Tenancy is **org-scoped** (`organizations` + `org_members`; there is no separate project layer). Every row is owned by an org, and every query scopes by it — middleware resolves the org from an API key or session.

**Identity / tenancy:** `organizations`, `org_members`, `users`, `api_keys`, `org_invites`, `magic_link_tokens`, `audit_logs`

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
| 5 | ~~Visual workflow builder~~ | ❌ dropped — see §7 |
| 6 | Multi-tenant hardening — full RBAC, SSO, billing hooks | planned |
| 7 | Self-host packaging — Helm chart, single-binary release | planned (`deploy/helm/` is empty; compose ships) |

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

### Beyond the phases

**Super-admin queue ops** — `/console/queues`: per-lane drill-down (dead / scheduled / processing / pending), an all-orgs pending table, and safe ops (requeue a dead event, force-promote a scheduled one, drain the dead list), each a single atomic Lua script. Spec: `2026-09-26-admin-queue-ops-design.md`.

---

## 7. Decisions log

| Date | Decision |
| --- | --- |
| 2026-07-06 | **Webhook auth deferred to post-release.** No inbound signature verification and no outbound delivery auth. Plain forwarding only: `requests.sig_verified` is always false and `destinations.auth_config` is stored-but-unused. Columns and API fields are kept so auth lands without a migration. |
| 2026-07-18 | **asynq → `dqueue`.** Replaced asynq/asynqmon with a hand-rolled Redis fair queue to get absolute per-org fairness. Accepted cost: reimplementing retry, backoff, dead-letter, scheduling, crash recovery and monitoring. |
| 2026-09-29 | **Visual workflow builder (Phase 5) dropped.** A drag-to-connect canvas over the existing connection model was built, reviewed, and removed: the connections page's structured view already reads the topology, and the table view plus the create dialog already build it — so the canvas added a dependency and a third way to do the same thing. A visual builder only earns its place alongside a real multi-step pipeline model (source → chained filter/transform/branch nodes → fan-out), which stays deferred. |

---

## 8. Deferred & out of scope

**Deferred:** pause/resume an org's delivery lane (needs a change to the correctness-critical `FairPick` Lua); per-connection and per-destination queue breakdowns; historical queue metrics.

**Deferred to later phases:** full RBAC roles, SSO, billing hooks (Phase 6); cross-org fixture sharing (Phase 6); Helm chart and single-binary release (Phase 7).

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
