# dstream

> Open-source webhook management, monitoring, and testing platform. **The dev IDE for webhooks.**

Status: Phase 1 (core inbound gateway). See [`PLAN.md`](./PLAN.md) for the live design doc and phased roadmap.

---

## About

dstream sits between webhook senders (Stripe, GitHub, Shopify, your own services) and your application. It receives webhook traffic, durably stores every request, applies routing + delivery policy, and forwards to your endpoints with retries — observable end-to-end.

### Problem

Teams that consume webhooks deal with the same pile of pain every quarter:

- 3rd-party providers retry on their own schedules; you discover dropped events too late.
- A single noisy sender can swamp your handler; no built-in backpressure.
- Local development is a parade of `ngrok` + manual replays + hand-built fixtures.
- Debugging a failed delivery means correlating provider dashboards, your logs, and a queue admin tool nobody owns.

dstream solves the operational layer once so you stop rebuilding it in every service.

### Differentiator bets

1. **Best local dev loop** — first-class CLI: tunnel + replay + fixture library + scenario scripts. Test webhook handlers like unit tests.
2. **Visual workflow builder** — node-based UI for source → filter → transform → destination. Non-devs can compose.
3. **Record/replay 3rd-party providers** — VCR-style fixture capture for deterministic CI tests.

Combined positioning: **the dev IDE for webhooks**.

### How it compares

|                                   | dstream      | Hookdeck | Convoy | Svix    | webhook.site |
| --------------------------------- | ------------ | -------- | ------ | ------- | ------------ |
| Inbound gateway                   | ✅           | ✅       | ✅     | partial | partial      |
| Outbound (publish)                | planned (P2) | ✅       | ✅     | ✅      | ❌           |
| OSS + self-host                   | ✅           | ❌       | ✅     | partial | ❌           |
| CLI tunnel + replay               | ✅           | basic    | ❌     | ❌      | view only    |
| Visual workflow                   | planned (P5) | ❌       | ❌     | ❌      | ❌           |
| Record / replay fixtures          | planned (P4) | ❌       | ❌     | ❌      | ❌           |
| Per-connection retry + RPS policy | ✅           | ✅       | ✅     | ✅      | ❌           |

---

## Stack

- **Backend**: Go modular monolith, single binary, subcommands (`server`, `worker`, `cli`, `migrate`, `admin`)
- **Frontend**: Tanstack Start (React 19, Vite, Tailwind 4, Nitro nightly, file-based routing)
- **State**: Postgres 18+ (sqlc-generated access, Atlas-managed migrations). Requires 18 for the native `uuidv7()` used as the default for all UUID primary keys — time-ordered ids keep insert-heavy tables (`requests`, `events`, `attempts`) clustered in the PK B-tree instead of scattering like random v4.
- **Queue**: Redis via `hibiken/asynq` (retries, scheduling, dead-letter) + `asynqmon` ops UI
- **Rate limiting**: Redis token bucket via `go-redis/redis_rate`
- **Request bodies**: stored in Postgres (`bytea`), behind a `BodyStore` interface so an S3/object-store backend can drop in later

Distribution model: OSS-first, SaaS-able, self-hostable — one codebase. Modeled on PostHog / Convoy.

---

## Quick start (end-to-end)

### 0. Prerequisites

- Go 1.22+
- Node 22+ (TanStack Start requires ≥22.12) and one of `bun` / `pnpm` / `npm`
- Docker (or OrbStack / Colima) — the compose file pins **Postgres 18** (needed for `uuidv7()`)
- `openssl` for the session secret

> **Upgrading from a pre-18 checkout:** Postgres 18 will not start on a data
> directory created by an older major version. If you have an existing dev
> volume, reset it (dev data is disposable):
> ```bash
> docker compose -f deploy/docker/docker-compose.yml down
> rm -rf deploy/docker/.data/postgres
> docker compose -f deploy/docker/docker-compose.yml up -d
> ```
> Then re-run the migrate step below.

Optional helpers:

```bash
go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest   # regenerate db access (`make sqlc`)
# add $HOME/go/bin to your PATH
```

Migrations are embedded in the binary and applied by `dstream migrate up` — no
external tool needed to run them. The [`atlas`](https://atlasgo.io) CLI is only
required to *author* new migrations (`make schema-diff`, `make migrate-hash`).

### 1. Env

```bash
cp .env.example .env
SECRET=$(openssl rand -hex 32)
sed -i '' "s|DSTREAM_SESSION_SECRET=.*|DSTREAM_SESSION_SECRET=$SECRET|" .env
set -a; source .env; set +a
```

### 2. Bring up the stack

The compose file defines both the infra (Postgres, Redis) and the app
(`migrate`, `server`, `worker`). Pick a mode:

**Mode A — everything in Docker, one command.** Builds the app image, runs
migrations, then starts server + worker + the Vite frontend:

```bash
docker compose -f deploy/docker/docker-compose.yml up -d --build
docker compose -f deploy/docker/docker-compose.yml ps          # all healthy / migrate exited 0
docker compose -f deploy/docker/docker-compose.yml logs -f server   # magic-link URL lands here
```

Dashboard on http://localhost:3000. Then just **step 4** (bootstrap —
`docker compose ... exec server dstream admin bootstrap --email you@example.com --org acme`).
The host `migrate`/`server`/`worker` and step 6 don't apply.

**Mode B — infra in Docker, app on the host** (faster iteration; hot rebuilds
with `make server`). Bring up only the infra services:

```bash
docker compose -f deploy/docker/docker-compose.yml up -d postgres redis
docker compose -f deploy/docker/docker-compose.yml ps          # all healthy
```

Either way: Postgres :5432, Redis :6379. The app reads config from `.env`; in
Docker the DB/Redis hosts are overridden to the in-network service names
automatically.

> `env_file` points at `.env`, so step 1 must run before `up`.

### 3. Migrate *(Mode B only — Mode A runs the `migrate` service for you)*

```bash
go run ./cmd/dstream migrate up
```

### 4. Bootstrap the first user + org + API key

```bash
# Mode B (host):
go run ./cmd/dstream admin bootstrap --email you@example.com --org acme
# Mode A (Docker): docker compose -f deploy/docker/docker-compose.yml \
#   exec server dstream admin bootstrap --email you@example.com --org acme
# prints: api key: dsk_<prefix>_<secret>
export DSTREAM_API_KEY=dsk_<prefix>_<secret>
```

Optional — promote yourself to super-admin (unlocks `/admin/queues`):

```bash
go run ./cmd/dstream admin promote you@example.com
```

### 5. Run backend *(Mode B only — in Mode A these run as containers)*

Terminal A — HTTP server:

```bash
go run ./cmd/dstream server
# listening on :8080
```

Terminal B — delivery worker:

```bash
go run ./cmd/dstream worker
# concurrency=50, consuming "deliveries" queue
```

### 6. Run frontend

```bash
cd web
bun install      # or: pnpm install / npm install
bun run dev
# dashboard on http://localhost:3000
# /api/*, /admin/*, /e/* proxied to :8080
```

### 7. Smoke test — webhook in, event delivered

```bash
# create a source
SRC=$(curl -sX POST http://localhost:8080/api/sources \
  -H "Authorization: Bearer $DSTREAM_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name":"stripe-prod","type":"stripe"}')
SRC_ID=$(echo "$SRC" | jq -r .id)
INGEST_TOKEN=$(echo "$SRC" | jq -r .ingest_token)

# create a destination with rate limit
DEST=$(curl -sX POST http://localhost:8080/api/destinations \
  -H "Authorization: Bearer $DSTREAM_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name":"echo","type":"http","url":"https://httpbin.org/anything","rate_limit_rps":5}')
DEST_ID=$(echo "$DEST" | jq -r .id)

# connect them
curl -sX POST http://localhost:8080/api/connections \
  -H "Authorization: Bearer $DSTREAM_API_KEY" \
  -H "Content-Type: application/json" \
  -d "{\"source_id\":\"$SRC_ID\",\"destination_id\":\"$DEST_ID\"}"

# send a webhook
curl -sX POST http://localhost:8080/e/$INGEST_TOKEN \
  -H "Content-Type: application/json" \
  -d '{"event":"test","amount":4200}'
# 202 with request_id + event_ids
```

Open `http://localhost:3000/events` to see it land.

### 8. CLI tunnel (local forward)

```bash
# run a local handler (any HTTP server on a port)
python3 -m http.server 3001

# create a CLI-type destination + connection
CLI_DEST=$(curl -sX POST http://localhost:8080/api/destinations \
  -H "Authorization: Bearer $DSTREAM_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name":"local","type":"cli"}')
CLI_DEST_ID=$(echo "$CLI_DEST" | jq -r .id)
curl -sX POST http://localhost:8080/api/connections \
  -H "Authorization: Bearer $DSTREAM_API_KEY" \
  -H "Content-Type: application/json" \
  -d "{\"source_id\":\"$SRC_ID\",\"destination_id\":\"$CLI_DEST_ID\"}"

# open the tunnel
go run ./cmd/dstream cli listen \
  --source stripe-prod \
  --forward http://localhost:3001
# subsequent POSTs to /e/$INGEST_TOKEN now flow to localhost:3001
```

### 9. Admin queue UI

1. `dstream admin promote you@example.com`
2. Visit `http://localhost:3000/login`, enter your email
3. Look at `dstream server` stdout for the magic-link URL, visit it
4. Open `http://localhost:8080/admin/queues` — `asynqmon` (Sidekiq/BullMQ-equivalent)

### Tear down

```bash
docker compose -f deploy/docker/docker-compose.yml down       # stop
docker compose -f deploy/docker/docker-compose.yml down -v    # nuke volumes
```

---

## Configuration

All config via env vars (see `.env.example` for the full list). Highlights:

| Var                          | Default                                                             | Purpose                            |
| ---------------------------- | ------------------------------------------------------------------- | ---------------------------------- |
| `DSTREAM_HTTP_ADDR`          | `:8080`                                                             | Server bind address                |
| `DSTREAM_DB_URL`             | `postgres://dstream:dstream@localhost:5432/dstream?sslmode=disable` | Postgres DSN                       |
| `DSTREAM_REDIS_ADDR`         | `localhost:6379`                                                    | Redis (queue + dedup + rate limit) |
| `DSTREAM_WORKER_CONCURRENCY` | `50`                                                                | Per-process delivery worker pool   |
| `DSTREAM_SESSION_SECRET`     | (required, ≥32 bytes)                                               | HMAC secret for session cookies    |
| `DSTREAM_MAGIC_LINK_TTL`     | `15m`                                                               | Sign-in link validity              |

---

## Repo layout

```
cmd/dstream/         CLI entry — subcommands: server | worker | cli | migrate | admin
internal/
  ingest/            HTTP receiver, dedup, enqueue, body store
  queue/             asynq client + task payload types
  deliver/           HTTP delivery, retry policy, rate limit, max-inflight
  api/               REST API (sources, destinations, connections, events, CLI tunnel)
  admin/             /admin/* routes (asynqmon mount + custom admin pages)
  auth/              API keys, signed sessions, magic links, middleware
  store/             sqlc-generated Postgres access
  migrations/        embedded SQL migrations (goose)
  config/, logging/  Viper config loader, slog setup
db/queries/          sqlc query inputs
deploy/docker/       Dockerfile, docker-compose.yml
web/                 Tanstack Start dashboard
docs/                vision + per-phase specs (planned)
PLAN.md              live design doc — single source of truth
```

---

## Roadmap

Phased, per `PLAN.md`:

1. **Core inbound gateway** — _in progress_ (this branch). Ingest → dedup → enqueue → deliver → retry → dashboard.
2. **Outbound webhooks (subscriptions)** — your platform emits events, fan-out to subscriber endpoints with signing.
3. **Transformations + filters** — per-connection JS transforms via `goja`, filter expressions.
4. **Record/replay + fixture library** — capture live provider traffic, replay deterministically in CI.
5. **Visual workflow builder** — node-based UI for source → filter → transform → destination.
6. **Multi-tenant hardening** — full RBAC, SSO, audit log, billing hooks.
7. **Self-host packaging** — Helm chart, single-binary release, upgrade story.

---

## Trouble

| Symptom                                       | Likely cause                                                                     |
| --------------------------------------------- | -------------------------------------------------------------------------------- |
| `migrate up` errors with connection refused   | Compose not up. `docker compose -f deploy/docker/docker-compose.yml ps`          |
| Worker doesn't pick up events                 | Worker not running, or pointing at a different Redis. `redis-cli KEYS 'asynq:*'` |
| `dstream cli listen` says "no source named X" | API key belongs to a different project, or typo in source name                   |
| Magic-link email never arrives                | SMTP not configured. Dev mode logs the link to `dstream server` stdout           |
| `/admin/queues` returns 403                   | User isn't super-admin yet. Run `dstream admin promote <email>` and re-login     |

---

## License

TBD before first public release. Current intent: AGPL-3.0 with a separate commercial license for hosted SaaS.
