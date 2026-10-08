# dstream

> **The open-source dev IDE for webhooks.**
> Receive webhooks, store every request, and deliver them reliably — with a local tunnel, automatic retries, and a dashboard that shows every attempt.

dstream sits between webhook senders (Stripe, GitHub, Shopify, your own services)
and your app. It accepts inbound webhooks, saves every request, applies
per-connection retry + rate-limit policy, and forwards them to your endpoints —
while you watch each attempt in a dashboard.

**Status:** Phases 1–5 shipped — inbound gateway, outbound webhooks (Svix-style
publish + signed fan-out), transforms + filters, record/replay, and multi-tenant
hardening (RBAC, OIDC single sign-on, usage metering + quotas — no payment
provider). Phase 6 shipped its deployment half (Helm chart + Kubernetes
manifests). Inbound/outbound webhook **auth is deliberately deferred** to
post-release. Full detail lives in [`PLAN.md`](PLAN.md).

---

## Quick start

You need **Docker** (Desktop / OrbStack / Colima). Nothing else.

```bash
cp .env.example .env    # one-time; sane local defaults, dev login on
docker compose -f deploy/docker/docker-compose.yml up -d --build
```

That builds and starts everything — dashboard, API, worker, Postgres, Redis, and
a Jaeger trace UI. Migrations run automatically; you never run them by hand.

| Service | What it does | Address |
| ------- | ------------ | ------- |
| web     | Dashboard    | http://localhost:3000 |
| server  | API + ingest endpoint | http://localhost:8080 |
| worker  | Delivery + retries | — |
| postgres / redis | State + queue | — |
| jaeger  | Traces (optional) | http://localhost:16686 |

**Sign in:** open http://localhost:3000, enter any email. In dev mode the
magic-link URL is printed in the server logs — grab it from
`docker compose -f deploy/docker/docker-compose.yml logs -f server` and open it.
A personal workspace is created on first login.

**Stop:** `docker compose -f deploy/docker/docker-compose.yml down` (add `-v` to
wipe data).

---

## Try it: webhook in → delivered

Create an **API key** in the dashboard (Settings → API Keys), then:

```bash
export DSTREAM_API_KEY=dsk_...      # from the dashboard
API=http://localhost:8080

# 1. a source (gives you an ingest token)
SRC=$(curl -sX POST $API/api/sources -H "Authorization: Bearer $DSTREAM_API_KEY" \
  -H "Content-Type: application/json" -d '{"name":"stripe-prod","type":"stripe"}')
SRC_ID=$(echo "$SRC" | jq -r .id); TOKEN=$(echo "$SRC" | jq -r .ingest_token)

# 2. a destination (where events go)
DEST=$(curl -sX POST $API/api/destinations -H "Authorization: Bearer $DSTREAM_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name":"echo","type":"http","url":"https://httpbin.org/anything"}')
DEST_ID=$(echo "$DEST" | jq -r .id)

# 3. connect them
curl -sX POST $API/api/connections -H "Authorization: Bearer $DSTREAM_API_KEY" \
  -H "Content-Type: application/json" \
  -d "{\"source_id\":\"$SRC_ID\",\"destination_id\":\"$DEST_ID\"}"

# 4. send a webhook
curl -sX POST $API/e/$TOKEN -H "Content-Type: application/json" -d '{"hello":"world"}'
```

Open http://localhost:3000/events — the event is there, delivered, with every
attempt recorded.

---

## Forward to your laptop (CLI tunnel)

Pipe live webhooks straight to a local server — no ngrok. The `dstream` CLI runs
on **your machine** (needs Go to build):

```bash
go build -o dstream ./cmd/dstream

# make a cli destination + connection once, then:
export DSTREAM_API_KEY=dsk_...
dstream cli listen --source stripe-prod --forward http://localhost:3001
```

Every webhook to that source is forwarded to `localhost:3001`, and the response
is captured as the delivery attempt.

---

## How it works

One Go binary with subcommands (`server`, `worker`, `cli`, `migrate`, `admin`,
`maintenance`) — a **modular monolith**. Inbound webhooks are stored in Postgres,
queued in a custom per-org fair scheduler on Redis, then delivered by the worker
(rate-limited, retried, SSRF-guarded) to an HTTP endpoint or down a CLI tunnel.
Scale by running more `server` / `worker` replicas of the same image.

[![dstream architecture](brag-output/diagram.png)](brag-output/diagram.png)

- **Backend:** Go, chi, sqlc + Postgres 18, Atlas migrations.
- **Queue:** Redis-backed per-org fair scheduler (`internal/dqueue`) — at-least-once via a processing lease + recoverer, own retry/backoff + dead-letter.
- **Frontend:** TanStack Start (React 19, Vite, Tailwind).

---

## Deploy to Kubernetes

One Helm chart deploys the whole stack, with **bundled Postgres + Redis on by
default** so it runs standalone:

```bash
helm install dstream deploy/helm/dstream \
  --set ingress.host=dstream.example.com \
  --wait --wait-for-jobs
```

For production, point at managed datastores and supply your own Secret
(`postgresql.enabled=false`, `redis.enabled=false`, `secrets.existingSecret=...`
carrying `DSTREAM_DB_URL`, `DSTREAM_SESSION_SECRET`, `DSTREAM_REDIS_PASSWORD`,
`DSTREAM_SMTP_PASS`, `DSTREAM_OIDC_CLIENT_SECRET`). Bundled datastores are
single-node (no HA/backups) — fine for trials, not production.

No Helm? The same manifests are checked in as plain YAML:

```bash
kubectl apply -k deploy/k8s    # replace the REPLACE_ME_* secrets + ingress host first
```

`deploy/k8s/` is generated from the chart (`deploy/k8s/regen.sh`) — edit the
chart, not the raw files.

---

## Configuration

All config is environment variables in `.env` (copied from `.env.example`);
defaults are safe for local dev. The ones that matter for production:

| Var | For production |
| --- | -------------- |
| `DSTREAM_SESSION_SECRET` | set a real one — `openssl rand -hex 32` |
| `DSTREAM_DEV_MODE` | `false` (server refuses dev-mode on a non-localhost URL) |
| `DSTREAM_COOKIE_SECURE` | `true` behind TLS |
| `DSTREAM_PUBLIC_BASE_URL` | your public HTTPS URL |

OIDC single sign-on, usage quotas, worker scaling, and the full variable list are
documented in [`PLAN.md`](PLAN.md) and `.env.example`.

> **Webhook auth is not implemented yet** (deferred post-release): dstream does
> plain forwarding — it does not verify inbound signatures or authenticate
> outbound delivery. Don't rely on it to authenticate a sender.

---

## Roadmap

| # | Phase | Status |
| - | ----- | ------ |
| 1 | Core inbound gateway | ✅ shipped |
| 2 | Outbound webhooks (Svix model) | ✅ shipped |
| 3 | Transforms (goja) + filters (CEL) | ✅ shipped |
| 4 | Record / replay + fixtures | ✅ shipped |
| 5 | Multi-tenant hardening — RBAC, OIDC SSO, usage metering + quotas | ✅ shipped |
| 6 | Self-host packaging — Helm, single-binary release | 🟡 Helm + k8s shipped; binary release planned |

---

## Repo layout

```
cmd/dstream/    CLI entry — server | worker | cli | migrate | admin | maintenance
internal/       ingest, dqueue, deliver, webhook, filter, transform, bookmark,
                api, admin, auth, store, audit, config/logging/metrics/tracing/…
db/             schema.sql (source of truth), Atlas migrations, sqlc queries
deploy/docker/  Dockerfile, web.prod.Dockerfile (nginx SPA), docker-compose.yml
deploy/helm/    dstream Helm chart (bundled Postgres/Redis, toggleable)
deploy/k8s/     raw manifests generated from the chart (kubectl apply -k)
web/            TanStack Start dashboard
PLAN.md         live design doc — single source of truth
```

---

## Troubleshooting

| Symptom | Fix |
| ------- | --- |
| `up` fails on a stale Postgres volume | `docker compose -f deploy/docker/docker-compose.yml down -v`, then `up` (dev data is disposable). |
| No magic link to sign in | Ensure `DSTREAM_DEV_MODE=true`, then read the link from `logs -f server`. |
| Port already in use | Free 3000 (web), 8080 (server), 6379 (redis), 5433 (postgres) — a local Redis on 6379 is the usual clash. |
| Delivery to a localhost destination fails | The SSRF guard blocks private IPs — use the CLI tunnel, or set `DSTREAM_ALLOW_PRIVATE_DESTINATIONS=true` for trusted local testing. |
| `/admin/*` returns 403 | Promote yourself: `docker compose -f deploy/docker/docker-compose.yml exec server dstream admin promote you@example.com`, then re-login. |

---

## License

[Apache License 2.0](LICENSE).
