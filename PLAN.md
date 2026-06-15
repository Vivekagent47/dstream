# Plan: dstream — Hookdeck-style Webhook Platform (Vision Doc + Phase 1 Spec)

## Context

User is starting a new project (`/Users/apple/Work/dstream` — empty directory). Goal: build an open-source webhook management + monitoring + testing platform comparable to Hookdeck, with three positioning bets:

1. **Best local dev loop** — first-class CLI with tunnel, replay, fixture library, scenario scripts.
2. **Visual workflow builder** — node-based UI for source → filter → transform → destination.
3. **Record/replay 3rd-party providers** — VCR-style fixture capture for deterministic CI tests.

Combined positioning: **"the dev IDE for webhooks"**.

Deploy model: single codebase serves SaaS (multi-tenant) + self-host (Docker Compose / Helm / single-binary). Like PostHog or Convoy.

This planning session produces **two documents**, not code:
- `docs/vision/dstream-platform.md` — long-horizon vision + architecture overview across all 7 sub-projects.
- `docs/specs/2026-06-15-phase-1-core-inbound-gateway.md` — detailed implementation spec for the first executable slice.

The repo is greenfield; no constraints to respect.

## Decisions Already Locked (from clarifying Qs)

| Topic            | Choice                                                                 |
|------------------|------------------------------------------------------------------------|
| MVP scope        | Inbound + Outbound (full Hookdeck parity, phased)                      |
| Distribution     | OSS-first, SaaS-able, self-hostable from single codebase               |
| Backend          | Go                                                                     |
| Frontend         | Tanstack Start (React + Vinxi SSR)                                     |
| Queue            | Redis queues via `asynq` (Lists + sorted sets) + Postgres (state of record) |
| Differentiator   | Dev-first webhook IDE (CLI + visual builder + record/replay)           |

## Architecture (Modular Monolith)

One Go binary with subcommands. Self-hosters run one container + Postgres + Redis. SaaS runs N replicas of the same binary, optionally split by subcommand for horizontal scale.

```
/cmd/dstream            main entry — subcommands: server | worker | cli | migrate
/internal/
  ingest/               HTTP receiver, signature verify, dedup, enqueue
  queue/                asynq client + server wrapper (Redis-backed task queue)
  deliver/              outbound HTTP delivery, retries, backoff
  transform/            JS sandbox (goja) for per-connection transforms (Phase 3)
  filter/               JSONPath/CEL filter eval (Phase 3)
  source/               provider plugins: Stripe, GitHub, Shopify, generic
  destination/          destination types: HTTP, CLI-tunnel
  bookmark/             capture + replay (Phase 4)
  store/                Postgres data access (sqlc-generated)
  api/                  REST API for dashboard + CLI control plane
  tenant/               org/project scoping, isolation
  auth/                 API keys, sessions, RBAC (RBAC stub now, full Phase 6)
/web/                   Tanstack Start dashboard
/deploy/
  docker/               Dockerfile, docker-compose.yml
  helm/                 Helm chart for k8s self-host
/docs/
  vision/               vision doc
  specs/                phase specs
```

**Why modular monolith over microservices:** self-host UX wins. Splitting later by extracting subcommands is mechanical. Microservices upfront punishes the OSS community.

## Phased Roadmap

| #  | Phase                              | Why it's in this position                             |
|----|------------------------------------|-------------------------------------------------------|
| 1  | Core inbound gateway               | Foundation; everything else reuses queue + delivery   |
| 2  | Outbound webhooks (subscriptions)  | Reuses delivery worker; adds publish API + signing    |
| 3  | Transformations + filters          | Needs `transform/` + `filter/` packages built out     |
| 4  | Record/replay + fixture library    | Moat #1 (test better); needs Phase 1 data             |
| 5  | Visual workflow builder            | Moat #2; UI-heavy, no infra change                    |
| 6  | Multi-tenant hardening + RBAC + SSO + billing hooks | Required before SaaS launch      |
| 7  | Self-host packaging (Helm, single-binary)           | Hardening + distribution           |

## Documents To Write

### Document 1: `docs/vision/dstream-platform.md`

Cover at high level (target ~1500 words):
- Problem statement, target user (dev teams shipping webhook integrations)
- Positioning vs Hookdeck / Convoy / Svix / webhook.site
- Three differentiator bets (CLI, visual builder, record/replay) with concrete examples
- Architecture diagram (text/Mermaid): ingress → queue → workers → destinations, with control plane and dashboard
- Data model overview: Org, Project, Source, Destination, Connection, Request, Event, Attempt, Bookmark, Subscription
- 7-phase roadmap with one paragraph per phase: scope, exit criteria, non-goals
- Self-host vs SaaS architecture differences (none expected at code level; only config)
- Out-of-scope (don't promise): managed cloud signup, mobile apps, alerting beyond email/webhook, enterprise audit logs beyond basic

### Document 2: `docs/specs/2026-06-15-phase-1-core-inbound-gateway.md`

Detailed enough to execute. Target ~2500 words. Sections:

**1. Goals & non-goals**
- Goal: receive webhook → durably store → reliably deliver to one destination → observable in dashboard → forwardable to localhost via CLI.
- Non-goals (defer): transformations, filters, outbound publish, record/replay, visual builder, billing, SSO.

**2. User stories**
- "As a dev, I create a Source for Stripe, get an ingest URL, point Stripe at it, see events in dashboard."
- "As a dev, I run `dstream listen --source stripe-prod` and incoming events forward to my localhost:3000/webhook."
- "As a dev, my destination 500s; dstream retries with exponential backoff; I can manually retry from dashboard."

**3. Data model (Postgres)**
- `organizations`, `projects`, `users`, `api_keys`
- `sources` (id, project_id, type, ingest_token, signing_config, created_at)
- `destinations` (id, project_id, type [http|cli], url, auth_config, rate_limit_rps int null, rate_limit_burst int null, max_inflight int null)
- `connections` (id, source_id, destination_id, enabled, max_retries int default 8, retry_strategy enum [exponential|linear|fixed|custom], retry_base_ms int default 30000, retry_cap_ms int default 3600000, retry_jitter_pct int default 20, custom_retry_schedule jsonb null)
- `requests` (id, source_id, headers, body_hash, body_ref [object-store ref], received_at, sig_verified bool)
- `events` (id, request_id, connection_id, status [queued|delivered|failed|paused], next_retry_at)
- `attempts` (id, event_id, attempt_num, response_status, response_headers, response_body, duration_ms, attempted_at, error)
- `cli_sessions` (id, source_id, token, last_seen_at) — for tunnel

**4. Ingest path**
- `POST /e/{ingest_token}` — accept any method/headers/body up to 5MB.
- Resolve source by token (cached in Redis, 60s TTL).
- Compute body hash; store request row + body (start: Postgres LO or `bytea`; later: S3/MinIO via interface).
- If source has signing config: verify signature, set `sig_verified` flag (do not reject — observability over enforcement; configurable later).
- Dedup window: `SETNX dedup:{source_id}:{body_hash} 1 EX 60`. Skip enqueue if dup, still record request.
- For each enabled connection on source: create `events` row + `asynq.Enqueue("deliver", {event_id})` onto the `deliveries` queue.
- Respond `200 {request_id, event_ids:[]}` within 50ms p99 (no synchronous delivery).

**5. Delivery worker**
- `asynq.Server` consuming `deliveries` queue, configurable concurrency (default 50).
- Handler steps per task:
  1. Load event + connection + destination from Postgres.
  2. **Rate-limit gate** (if destination has limits): token bucket via `go-redis/redis_rate/v10` keyed on `dest:{destination_id}`. If denied, return `asynq.SkipRetry` and re-enqueue with `ProcessIn(retryAfter)` (where `retryAfter` = time until bucket refill). Does NOT count toward retry budget.
  3. **Max in-flight gate** (optional, per destination): Redis `INCR inflight:{destination_id}` with `EXPIRE` slot; if over `max_inflight`, defer like rate-limit miss.
  4. HTTP POST destination.url with original headers + `Dstream-Event-Id`, `Dstream-Event-Attempt` headers. Timeout 30s.
  5. Treat 2xx as success → write attempt row, status=`delivered`, decrement in-flight.
  6. On failure (non-2xx / network / timeout): write attempt row with error, decrement in-flight, return error.
- `asynq.RetryDelayFunc` is global and per-task consults policy:
  - Read `connections.retry_strategy` + params from task payload (cache snapshot at enqueue time).
  - `exponential` → `min(retry_base_ms * 2^attempt, retry_cap_ms)` ± jitter
  - `linear` → `min(retry_base_ms * attempt, retry_cap_ms)` ± jitter
  - `fixed` → `retry_base_ms` ± jitter
  - `custom` → next value from `custom_retry_schedule` jsonb array (e.g., `[10s, 30s, 1m, 5m, 30m]`)
- `MaxRetry` from policy (snapshotted into task options at enqueue). Dead-letter after exhaustion; event row flipped `status=failed`.
- Manual retry from dashboard re-enqueues with attempt counter reset.

**6. CLI destination (local forward)**
- CLI command: `dstream listen --source <id-or-name> --forward <local-url>`.
- CLI opens WebSocket to `/api/cli/connect`, authenticates via API key, registers as the destination for any connection of type `cli`.
- Delivery worker, when destination type is `cli`, instead of HTTP POSTs the event over the WS to the connected CLI session.
- CLI POSTs to `<local-url>`, returns response status/body back over WS; worker records as the attempt.
- Heartbeat every 15s; if CLI disconnects, events pause until reconnect.

**7. Dashboard (Tanstack Start)**

Project-scoped pages (per tenant):
- `/login` (email + magic link; postpone SSO).
- `/orgs/{slug}/projects/{slug}/sources` — list, create, copy ingest URL, view source config.
- `.../destinations` — list, create HTTP destination.
- `.../connections` — create source↔destination connection.
- `.../events` — paginated list, filter by source/status, click into event detail.
- `.../events/{id}` — request headers/body, all attempts, "Retry now" button.

> **Phase 1 gap (intentional):** retry policy + rate-limit fields on `connections`/`destinations` are configurable via API (PATCH) + DB seed but lack user-facing dashboard forms in Phase 1. Edit-policy UI is deferred — track as Phase 1.5 follow-up or fold into Phase 3 with transformations/filters UI.

Root-admin pages (super-admin role only; `/admin/*` routes, gated by `users.is_super_admin`):
- `/admin/queues` — embedded **asynqmon** UI: live queue stats (active, pending, scheduled, retry, archived/dead-letter), per-queue throughput, task inspection, pause/resume queue, drain dead-letter. Sidekiq/BullMQ-equivalent. Mounted as HTTP sub-handler from the `hibiken/asynqmon` package.
- `/admin/overview` — cross-tenant metrics: total events/min, top sources by volume, top failing destinations, total orgs/projects/users.
- `/admin/orgs` — list all organizations; click through to read-only inspection of their sources/destinations/events. For support.
- `/admin/destinations/hot` — destinations breaching rate limits or with elevated failure rate, sorted by impact.
- `/admin/system` — Redis info, Postgres pool stats, worker count, version, build SHA.

Super-admin bootstrapped via `dstream admin promote <email>` CLI on first run.

**8. API surface (REST, JSON)**
- `POST /api/sources` / `GET /api/sources` / `GET /api/sources/{id}`
- `POST /api/destinations` / `GET /api/destinations` / `GET /api/destinations/{id}` / `PATCH /api/destinations/{id}` (PATCH accepts `rate_limit_rps`, `rate_limit_burst`, `max_inflight`)
- `POST /api/connections` / `GET /api/connections` / `GET /api/connections/{id}` / `PATCH /api/connections/{id}` (PATCH accepts retry policy fields)
- `GET /api/events?source_id=&status=&cursor=`
- `GET /api/events/{id}`
- `POST /api/events/{id}/retry`
- `GET /api/cli/sources` (CLI bootstrap), `WS /api/cli/connect`
- Admin (super-admin only): `GET /api/admin/overview`, `GET /api/admin/orgs`, `GET /api/admin/destinations/hot`, `GET /api/admin/system`. asynqmon mounted at `/admin/queues`.

**9. Auth**
- API keys per project (`Authorization: Bearer dsk_...`).
- Dashboard sessions via cookie (magic-link auth).
- Minimum RBAC: project member or not (full RBAC = Phase 6).

**10. Multi-tenant**
- Every row owned by `project_id`. All queries scope by project. Middleware extracts project from API key or session.
- No org-level cross-project queries in Phase 1.

**11. Observability**
- Structured logs (slog) with `request_id`, `event_id`.
- Prometheus metrics on `/metrics`: ingest count/latency, queue depth (asynq `Inspector`), delivery success/fail by destination, retry count.
- Mount `asynqmon` at `/admin/queues` (auth-gated) for ops visibility into queue state.
- No tracing yet (Phase 2 candidate).

**12. Operations**
- `docker-compose.yml`: dstream + postgres + redis + minio (bodies) all up with one command.
- `dstream migrate` runs DB migrations on boot.
- Env-var config loader (Viper). Documented `.env.example`.

**13. Verification (end-to-end test plan)**
- `docker compose up` brings stack online; dashboard reachable at `localhost:8080`.
- Smoke: create source via dashboard → curl POST to ingest URL → event appears in dashboard, status `delivered` after worker hits a mock destination.
- Retry: configure destination returning 500 → see 8 attempts with backoff in attempts table; "Retry now" works.
- Dedup: send same body twice within 60s → only first creates event.
- CLI tunnel: `dstream listen --source X --forward http://localhost:3000/hook` → hitting ingest delivers to local server; response captured in attempt.
- Load: 1k events in 60s sustained, p99 ingest < 100ms, p99 delivery start < 500ms (single-node baseline).
- Self-host: fresh VM, `git clone && docker compose up`, walk through smoke test.

**14. Out of scope for Phase 1 (explicit list)**
Transforms, filters, outbound webhooks, record/replay, visual builder, billing, SSO, audit log, alerting, custom domains, payload encryption at rest.

## Reusable / External Patterns

- DB access: `sqlc` (compile-time-safe Go from SQL).
- Migrations: `goose` or `golang-migrate`.
- HTTP server: `chi` router.
- Task queue: `hibiken/asynq` (retries, scheduling, dead-letter) + `hibiken/asynqmon` (mounted UI at `/admin/queues`).
- Rate limiting: `go-redis/redis_rate/v10` (token bucket on Redis).
- Redis client: `go-redis/v9` (for non-queue uses: dedup `SETNX`, source cache, in-flight counters, CLI session registry).
- JS sandbox (later phases): `dop251/goja`.
- CLI WS: `nhooyr.io/websocket`.
- Config: `spf13/viper`.
- Frontend data: Tanstack Query + Tanstack Router (bundled in Tanstack Start).

## How to Execute This Plan (post-approval)

1. Create `docs/vision/` and `docs/specs/` directories.
2. Write Document 1 (`dstream-platform.md`) following the outline above.
3. Write Document 2 (`2026-06-15-phase-1-core-inbound-gateway.md`) following the section outline above.
4. `git init` and commit both docs as `docs: add platform vision and phase 1 spec`.
5. Stop. No code yet. Next session executes Phase 1 spec via `writing-plans` skill.

## Verification of This Planning Step

After both docs written:
- Both files exist at their paths and parse as valid Markdown.
- Vision doc references every phase 1–7 with a one-paragraph scope.
- Phase 1 spec contains all 14 numbered sections from outline.
- A reader unfamiliar with Hookdeck can finish the vision doc and explain dstream's three differentiator bets.
- Phase 1 spec is detailed enough that a competent Go developer can scaffold the repo without further questions on data model, ingest path, or retry policy.
