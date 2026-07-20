# dstream

> **The open-source dev IDE for webhooks.**
> Receive webhooks, store every request durably, and deliver them reliably — with a local tunnel, automatic retries, and full observability.

dstream sits between webhook senders (Stripe, GitHub, Shopify, your own services) and your app. It accepts inbound webhooks, persists every request, applies per-connection delivery + retry policy, and forwards to your endpoints — while you watch every attempt in a dashboard.

**Status:** Phase 1 (core inbound gateway) is shipped and security-hardened. Later phases (outbound, transforms, record/replay, visual builder) are on the roadmap below. `PLAN.md` is the live design doc.

---

## Run it (one command)

You need **Docker** (Desktop / OrbStack / Colima). Nothing else.

```bash
cp .env.example .env    # one-time; sane local defaults, dev login enabled
docker compose -f deploy/docker/docker-compose.yml up -d --build
```

That single command builds and starts everything:

| Service    | What it does                                    | Address |
| ---------- | ----------------------------------------------- | ------- |
| `web`      | Dashboard (Tanstack Start)                      | http://localhost:3000 |
| `server`   | HTTP API + ingest endpoint                      | http://localhost:8080 |
| `worker`   | Delivery worker + stuck-event reaper            | —       |
| `migrate`  | **Runs DB migrations automatically**, then exits | —       |
| `postgres` | State of record                                 | host :5433 |
| `redis`    | Queue + rate limit + cache                      | :6379   |
| `jaeger`   | Trace collector + UI (OpenTelemetry)            | http://localhost:16686 |

**Migrations are automatic** — the `migrate` service runs on every `up` and `server`/`worker` wait for it to finish, so the database is always current. You never run a migrate command by hand.

Check it's healthy:

```bash
docker compose -f deploy/docker/docker-compose.yml ps
docker compose -f deploy/docker/docker-compose.yml logs -f server
```

Traces: with the dev stack up, open the Jaeger UI at http://localhost:16686 and
select the `dstream` service to see a webhook's ingest→queue→delivery trace.
Tracing is off by default (`DSTREAM_TRACING_ENABLED`); the compose stack turns it
on and points it at the `jaeger` service. If you enable tracing without a
reachable OTLP collector, the server/worker still start but log periodic span
export errors — expected, not a crash.

Metrics: `/metrics` (Prometheus text format) is gated to super-admins because it
exposes tenant ids and names. It is therefore browse-only in Phase 1 — a logged-in
super-admin can view it, but no automated scraper ships in the dev stack (a stock
Prometheus can't present the session cookie).

### Sign in

1. Open **http://localhost:3000** and enter any email.
2. In dev mode the magic-link URL is printed in the **server logs** (no SMTP needed) — copy it from the `logs -f server` output and open it.
3. You're in. A personal workspace is created automatically on first login.

### Stop / reset

```bash
docker compose -f deploy/docker/docker-compose.yml down       # stop
docker compose -f deploy/docker/docker-compose.yml down -v    # stop + wipe data
```

---

## Try it: webhook in → delivered

After signing in, create an **API key** in the dashboard (Settings → API Keys) and export it:

```bash
export DSTREAM_API_KEY=dsk_...   # from the dashboard
API=http://localhost:8080
```

Then wire up a source → destination → connection and fire a webhook:

```bash
# 1. Source (gives you an ingest token/URL)
SRC=$(curl -sX POST $API/api/sources \
  -H "Authorization: Bearer $DSTREAM_API_KEY" -H "Content-Type: application/json" \
  -d '{"name":"stripe-prod","type":"stripe"}')
SRC_ID=$(echo "$SRC" | jq -r .id); TOKEN=$(echo "$SRC" | jq -r .ingest_token)

# 2. Destination (where events get delivered)
DEST=$(curl -sX POST $API/api/destinations \
  -H "Authorization: Bearer $DSTREAM_API_KEY" -H "Content-Type: application/json" \
  -d '{"name":"echo","type":"http","url":"https://httpbin.org/anything","rate_limit_rps":5}')
DEST_ID=$(echo "$DEST" | jq -r .id)

# 3. Connect them
curl -sX POST $API/api/connections \
  -H "Authorization: Bearer $DSTREAM_API_KEY" -H "Content-Type: application/json" \
  -d "{\"source_id\":\"$SRC_ID\",\"destination_id\":\"$DEST_ID\"}"

# 4. Send a webhook
curl -sX POST $API/e/$TOKEN -H "Content-Type: application/json" -d '{"hello":"world"}'
```

Open **http://localhost:3000/events** — the event appears, delivered, with every attempt recorded.

> Prefer the CLI to bootstrap? `docker compose -f deploy/docker/docker-compose.yml exec server dstream admin bootstrap --email you@example.com --org acme` creates a user + org and prints an API key.

---

## Forward to your laptop (CLI tunnel)

The headline dev feature: pipe live webhooks straight to a local server, no ngrok.

The `dstream` CLI runs on **your machine** (it forwards to your local port, so it can't run inside a container). Build it once — needs Go — or grab a release when available:

```bash
go build -o dstream ./cmd/dstream    # then use ./dstream, or move it onto your PATH
```

```bash
# a CLI-type destination + connection (once)
CLI_DEST=$(curl -sX POST $API/api/destinations \
  -H "Authorization: Bearer $DSTREAM_API_KEY" -H "Content-Type: application/json" \
  -d '{"name":"local","type":"cli"}')
curl -sX POST $API/api/connections \
  -H "Authorization: Bearer $DSTREAM_API_KEY" -H "Content-Type: application/json" \
  -d "{\"source_id\":\"$SRC_ID\",\"destination_id\":\"$(echo "$CLI_DEST" | jq -r .id)\"}"

# open the tunnel (uses your API key)
export DSTREAM_API_KEY=dsk_...
dstream cli listen --source stripe-prod --forward http://localhost:3001
```

Now every webhook to that source is forwarded to `localhost:3001`, and the response is captured as the delivery attempt.

---

## Why dstream

Teams that consume webhooks rebuild the same operational layer in every service: retries, backpressure, replay, a place to see what happened. dstream does it once.

- **Providers retry on their own schedule** — you find out about dropped events too late.
- **One noisy sender swamps your handler** — no built-in rate limiting or backpressure.
- **Local development is `ngrok` + manual replays + hand-built fixtures.**
- **Debugging a failed delivery** means correlating a provider dashboard, your logs, and a queue tool nobody owns.

### The three bets

| Bet | What it is | Status |
| --- | ---------- | ------ |
| **Best local dev loop** | First-class CLI: tunnel, replay, fixture library. Test webhook handlers like unit tests. | Tunnel shipped; replay/fixtures planned |
| **Visual workflow builder** | Node-based UI: source → filter → transform → destination. | Planned (Phase 5) |
| **Record / replay providers** | VCR-style capture of live provider traffic for deterministic CI. | Planned (Phase 4) |

Combined positioning: **the dev IDE for webhooks** — OSS-first, self-hostable from one binary, SaaS-able from the same codebase.

### How it compares

|                                   | dstream       | Hookdeck | Convoy | Svix    | webhook.site |
| --------------------------------- | ------------- | -------- | ------ | ------- | ------------ |
| Inbound gateway                   | ✅ shipped    | ✅       | ✅     | partial | partial      |
| Per-connection retry + RPS policy | ✅ shipped    | ✅       | ✅     | ✅      | ❌           |
| CLI tunnel                        | ✅ shipped    | basic    | ❌     | ❌      | view only    |
| OSS + self-host                   | ✅            | ❌       | ✅     | partial | ❌           |
| Outbound (publish)                | planned (P2)  | ✅       | ✅     | ✅      | ❌           |
| Record / replay fixtures          | planned (P4)  | ❌       | ❌     | ❌      | ❌           |
| Visual workflow                   | planned (P5)  | ❌       | ❌     | ❌      | ❌           |

---

## How it works

```
  webhook sender
        │  POST /e/{ingest_token}
        ▼
   ┌─────────┐   store request+body    ┌──────────┐
   │ ingest  │ ──────────────────────► │ Postgres │
   └────┬────┘   dedup + enqueue       └──────────┘
        │  fair-queue task
        ▼
   ┌─────────┐   ┌────────────────────────────────────────┐
   │  Redis  │◄──│ worker: rate-limit → deliver → retry     │
   │ (queue) │   │   ├─ HTTP destination (SSRF-guarded)      │
   └─────────┘   │   └─ CLI tunnel (WebSocket to your laptop)│
                 └────────────────────────────────────────┘
        ▲
   dashboard (:3000) + admin queue stats (/admin/queues)
```

One Go binary, several subcommands (`server`, `worker`, `cli`, `migrate`, `admin`) — a **modular monolith**. Self-hosters run one container set; scale by running more `server`/`worker` replicas of the same image.

- **Backend:** Go, chi router, sqlc-generated Postgres access, Atlas-managed migrations.
- **Queue:** a custom Redis-backed per-org fair scheduler (`internal/dqueue`) — round-robin across orgs, at-least-once via a processing lease + recoverer, own retry/backoff + dead-letter. Aggregate queue stats at `/admin/queues`.
- **Frontend:** Tanstack Start (React 19, Vite, Tailwind).
- **Storage:** request bodies in Postgres (`bytea`) behind a `BodyStore` interface (object-store backend can drop in later). Postgres 18 for native `uuidv7()` — time-ordered ids keep insert-heavy tables clustered.

---

## Configuration

All config is via environment variables in `.env` (copied from `.env.example`). The defaults are safe for local dev out of the box. Highlights:

| Var | Default | Purpose |
| --- | ------- | ------- |
| `DSTREAM_SESSION_SECRET` | (set for prod, ≥32 bytes) | HMAC secret for session cookies. Generate: `openssl rand -hex 32` |
| `DSTREAM_DEV_MODE` | `true` (in example) | Logs magic-link tokens to stdout so you can sign in without SMTP. **Must be false in production** (server refuses to boot dev-mode on a non-localhost URL). |
| `DSTREAM_COOKIE_SECURE` | `false` (in example) | `false` for local HTTP; **set true behind TLS** (server refuses to boot insecure on a non-localhost URL). |
| `DSTREAM_ALLOW_PRIVATE_DESTINATIONS` | `false` | Keep `false`: outbound delivery blocks loopback/private/metadata IPs (SSRF guard). Only enable on trusted self-host that delivers to private ranges. |
| `DSTREAM_INGEST_RATE_LIMIT_RPS` | `100` | Per-source ingest rate limit (`0` disables). |
| `DSTREAM_WORKER_CONCURRENCY` | `50` | Delivery worker pool size (goroutines per worker process). |
| `DSTREAM_WORKER_PER_ORG_MAX_INFLIGHT` | `0` (off) | Max concurrent in-flight deliveries per org, **fleet-wide**. Set `>0` (e.g. `20`) in multi-tenant deployments so one org can't monopolize the worker pool. |
| `DSTREAM_MAGIC_LINK_TTL` | `15m` | Sign-in link validity. |
| `DSTREAM_DB_URL`, `DSTREAM_REDIS_ADDR` | local defaults | Overridden automatically inside Docker to the in-network services. |

For production: set a real `DSTREAM_SESSION_SECRET`, `DSTREAM_DEV_MODE=false`, `DSTREAM_COOKIE_SECURE=true`, and a non-localhost `DSTREAM_PUBLIC_BASE_URL` (served over TLS).

### Scaling workers

Delivery scales horizontally — run more `worker` processes. They all drain the
**same Redis fair queue**, and dequeue is a single atomic Lua script, so each
task is processed by **exactly one** worker; no double-processing, no locks, no
coordination to configure.

```bash
# run 3 worker replicas of the same image
docker compose -f deploy/docker/docker-compose.yml up -d --scale worker=3
```

- **Total throughput** = `replicas × DSTREAM_WORKER_CONCURRENCY` (e.g. 3 × 50 = 150 concurrent deliveries).
- **No leader election needed.** Each worker runs the scheduler + recoverer loops; they're idempotent (atomic Lua), so running them on every replica is safe.
- **Per-org fairness and the per-org cap are fleet-wide.** The round-robin ring and the `DSTREAM_WORKER_PER_ORG_MAX_INFLIGHT` counter live in Redis, so they're enforced across *all* replicas combined — size the cap against the total pool (`replicas × concurrency`), not per node.
- **At-least-once across restarts.** If a worker crashes mid-delivery, its in-flight events are re-delivered by the recoverer once their lease expires; destinations should dedupe on the `Dstream-Event-Id` header. Shut down with `SIGTERM` (`docker compose stop`) for a graceful drain.
- **Kubernetes / other orchestrators:** same idea — scale the `worker` Deployment's replica count. All replicas share one Redis + Postgres; nothing is pinned to a node.

---

## Security

Secure by default:

- **SSRF-guarded delivery** — the worker refuses to POST to loopback/private/link-local (cloud-metadata) addresses; checked at dial time to defeat DNS rebinding.
- **Session revocation** — signed cookies carry an epoch; logout invalidates all of a user's sessions.
- **CSRF** double-submit on the dashboard; API keys are exempt by construction.
- **Rate limits** on ingest and magic-link issuance; **per-destination** rate + in-flight caps on delivery.
- **HMAC signature verification** of inbound webhooks (per-source config; recorded on each request).
- Sensitive inbound headers (`Authorization`, `Cookie`) are stripped before forwarding to destinations.

---

## Roadmap

| # | Phase | Status |
| - | ----- | ------ |
| 1 | **Core inbound gateway** — ingest → dedup → deliver → retry → dashboard | ✅ shipped + hardened |
| 2 | Outbound webhooks (publish + subscriber fan-out with signing) | planned |
| 3 | Transformations + filters (per-connection JS via `goja`) | planned |
| 4 | Record / replay + fixture library | planned |
| 5 | Visual workflow builder | planned |
| 6 | Multi-tenant hardening — full RBAC, SSO, audit, billing hooks | planned |
| 7 | Self-host packaging — Helm, single-binary release | planned |

---

## Repo layout

```
cmd/dstream/      CLI entry — server | worker | cli | migrate | admin
internal/
  ingest/         HTTP receiver, dedup, signature verify, enqueue, body store
  deliver/        HTTP delivery, retry policy, rate limit, SSRF guard, reaper
  dqueue/         Redis per-org fair-scheduling delivery queue (Lua + client)
  api/            REST API (sources, destinations, connections, events, CLI tunnel)
  admin/          /admin/* routes (overview, orgs, queue stats)
  auth/           API keys, signed sessions, magic links, CSRF, middleware
  store/          sqlc-generated Postgres access
  config/ logging/  Viper config, slog
db/
  migrations/     Atlas migrations (embedded in the binary, auto-applied)
  queries/        sqlc query inputs   schema/  reference schema
deploy/docker/    Dockerfile, web.Dockerfile, docker-compose.yml
web/              Tanstack Start dashboard
PLAN.md           live design doc — single source of truth
```

---

## Troubleshooting

| Symptom | Fix |
| ------- | --- |
| `up` fails on a stale Postgres volume | PG18 won't start on older data. `docker compose -f deploy/docker/docker-compose.yml down -v` then `up` again (dev data is disposable). |
| Can't sign in / no magic link | Ensure `DSTREAM_DEV_MODE=true` in `.env`, then read the link from `logs -f server`. |
| Port already in use | Free host ports 3000 (web), 8080 (server), 6379 (redis), 5433 (postgres) — a local Redis on 6379 is the usual clash. |
| `/admin/queues` returns 403 | Promote yourself: `docker compose -f deploy/docker/docker-compose.yml exec server dstream admin promote you@example.com`, then re-login. |
| Delivery to a localhost destination fails | The SSRF guard blocks private IPs. Use the **CLI tunnel** for local forwarding, or set `DSTREAM_ALLOW_PRIVATE_DESTINATIONS=true` for trusted local testing. |

---

## License

TBD before first public release. Current intent: AGPL-3.0, with a separate commercial license for hosted SaaS.
