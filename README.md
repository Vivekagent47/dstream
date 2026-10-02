# dstream

> **The open-source dev IDE for webhooks.**
> Receive webhooks, store every request durably, and deliver them reliably — with a local tunnel, automatic retries, and full observability.

dstream sits between webhook senders (Stripe, GitHub, Shopify, your own services) and your app. It accepts inbound webhooks, persists every request, applies per-connection delivery + retry policy, and forwards to your endpoints — while you watch every attempt in a dashboard.

**Status:** Phases 1–4 shipped — core inbound gateway (security-hardened), outbound webhooks (Svix-style publish + signed fan-out + App Portal), transforms + filters (CEL filter + sandboxed `goja` transform on both pipelines), and record/replay (fixtures, import/export, auto-capture rules, ordered scenarios). Phase 5a also shipped: role-based access control across the API, with per-key roles. Phase 5b shipped **instance-level OIDC single sign-on** — one IdP per deployment, not yet exercised against a real IdP ([see below](#single-sign-on-oidc)). Remaining: billing hooks and self-host packaging — see the roadmap below. **Webhook auth (inbound signature verification, outbound delivery auth) is deliberately deferred to post-release.** `PLAN.md` is the live design doc.

---

## Demo

https://github.com/Vivekagent47/dstream/raw/main/brag-output/brag.mp4

<video src="brag-output/brag.mp4" controls width="100%"></video>

[▶ Watch the launch video](brag-output/brag.mp4)

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

Running against a company IdP instead? See [Single sign-on](#single-sign-on-oidc) — the login page renders a "Sign in with SSO" button once the server advertises an issuer.

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

### The two bets

| Bet | What it is | Status |
| --- | ---------- | ------ |
| **Best local dev loop** | First-class CLI: tunnel, replay, fixture library, ordered scenarios. Test webhook handlers like unit tests. | ✅ shipped |
| **Record / replay providers** | VCR-style capture of live provider traffic for deterministic CI. | ✅ shipped |

Combined positioning: **the dev IDE for webhooks** — OSS-first, self-hostable from one binary, SaaS-able from the same codebase.

### How it compares

|                                   | dstream       | Hookdeck | Convoy | Svix    | webhook.site |
| --------------------------------- | ------------- | -------- | ------ | ------- | ------------ |
| Inbound gateway                   | ✅ shipped    | ✅       | ✅     | partial | partial      |
| Per-connection retry + RPS policy | ✅ shipped    | ✅       | ✅     | ✅      | ❌           |
| CLI tunnel                        | ✅ shipped    | basic    | ❌     | ❌      | view only    |
| OSS + self-host                   | ✅            | ❌       | ✅     | partial | ❌           |
| Outbound (publish)                | ✅ shipped    | ✅       | ✅     | ✅      | ❌           |
| Transforms + filters (per-edge)   | ✅ shipped    | ✅       | partial| ❌      | ❌           |
| Record / replay fixtures          | ✅ shipped    | ❌       | ❌     | ❌      | ❌           |

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

<details>
<summary><b>Full component map</b> — every package and how it wires together (click to expand)</summary>

<br>

[![dstream architecture](brag-output/diagram.png)](brag-output/diagram.png)

Dashboard and CLI on top, the API and identity layer in the middle, and three
runtimes below it: the inbound pipeline (ingest → event store), outbound
webhooks (publish → fan-out), and the delivery runtime (worker → fair queue →
filter/transform → your endpoint). Generated with
[gitdiagram](https://gitdiagram.com/vivekagent47/dstream).

</details>

One Go binary, several subcommands (`server`, `worker`, `cli`, `migrate`, `admin`, `maintenance`) — a **modular monolith**. Self-hosters run one container set; scale by running more `server`/`worker` replicas of the same image.

- **Backend:** Go, chi router, sqlc-generated Postgres access, Atlas-managed migrations.
- **Queue:** a custom Redis-backed per-org fair scheduler (`internal/dqueue`) — round-robin across orgs, at-least-once via a processing lease + recoverer, own retry/backoff + dead-letter. Queue stats at `/admin/queues`, plus a super-admin ops console at `/console/queues` (per-lane drill-down; requeue a dead event, promote a scheduled one, drain the dead list).
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
| `DSTREAM_MAGIC_LINK_TTL` | `15m` | Validity of a link minted by `dstream admin magic-link` — **and only that one today.** Both HTTP magic-link paths (`/api/auth/magic-link/request` and the signed-out invite-accept fallback) hardcode 15 minutes; the CLI is this value's first and only reader. Changing it does not yet move the dashboard's link expiry. |
| `DSTREAM_DB_URL`, `DSTREAM_REDIS_ADDR` | local defaults | Overridden automatically inside Docker to the in-network services. |

For production: set a real `DSTREAM_SESSION_SECRET`, `DSTREAM_DEV_MODE=false`, `DSTREAM_COOKIE_SECURE=true`, and a non-localhost `DSTREAM_PUBLIC_BASE_URL` (served over TLS).

### Single sign-on (OIDC)

> **⚠️ Not yet exercised against a real IdP.** The protocol layer is tested
> against a locally-signing fake IdP (wrong audience, unpublished signing key,
> and absent vs. false `email_verified` are all covered) and the login page was
> driven in a real browser against a stubbed methods endpoint — but **no
> Okta / Entra ID / Google Workspace round-trip has ever been run.** If you
> turn this on, you are the first person to exercise the real handshake.
> Set it up on a staging deployment first, and keep the break-glass below to
> hand before you set `DSTREAM_OIDC_ENFORCE=true`.

dstream supports **instance-level** OIDC: **one IdP for the whole
deployment**, enabled by setting `DSTREAM_OIDC_ISSUER`. Set nothing and
behaviour is unchanged — magic links only, no SSO button. There is no per-org
or per-tenant IdP configuration, no SAML, no SCIM, and no group-to-role
mapping; see `PLAN.md` §8 for why each is deferred.

| Var | Default | Purpose |
| --- | ------- | ------- |
| `DSTREAM_OIDC_ISSUER` | `""` | Issuer URL (e.g. `https://login.example.okta.com`). **Non-empty enables SSO**; there is no separate on/off flag. Discovery runs at boot, so a wrong issuer fails the boot rather than every login. |
| `DSTREAM_OIDC_CLIENT_ID` | `""` | Required when the issuer is set (the server refuses to boot without it). |
| `DSTREAM_OIDC_CLIENT_SECRET` | `""` | Required when the issuer is set. |
| `DSTREAM_OIDC_SCOPES` | `openid,email,profile` | Extra scopes if your IdP needs them. `openid` is always requested whether or not you list it. |
| `DSTREAM_OIDC_DEFAULT_ORG` | `""` | Org **slug** that first-time SSO users join. Blank = each gets a personal workspace, matching magic-link behaviour. |
| `DSTREAM_OIDC_DEFAULT_ROLE` | `member` | Role for that auto-join: `member` or `admin`. **`owner` is refused at boot.** An existing membership always wins — changing this never re-grades anyone who is already a member. |
| `DSTREAM_OIDC_ENFORCE` | `false` | Refuse magic-link **minting** so it can't bypass IdP policy. Redemption of an already-minted token stays open — that's what keeps the break-glass below usable. Refused at boot if no issuer is set (that would disable every way to log in). |

**Register this redirect URI at your IdP:**

```
<DSTREAM_PUBLIC_BASE_URL>/api/auth/sso/callback
```

It is derived, not configured — one less value to get wrong — and the server
prints the exact string in its `sso enabled` startup log line, so copy it from
there. A mismatch is the most common OIDC setup failure.

Two things to know before you enable it:

- **Your IdP must send `email_verified`.** dstream matches an SSO login to an
  account by the IdP's verified email address, so a missing or false
  `email_verified` claim is **refused** — otherwise an IdP could assert an
  address it doesn't control and sign into the matching account. If your IdP
  omits the claim, add it at the IdP; that's the correct place to fix it.
- **`DSTREAM_OIDC_DEFAULT_ORG` is not checked at boot.** A slug matching no
  org fails at the *first SSO login*, with a `500` naming the variable and a
  server log line. This is deliberate: validating it at startup would brick a
  deployment whose default org is created by a later seed step.

#### Break-glass (keep this to hand)

`DSTREAM_OIDC_ENFORCE=true` refuses magic-link **minting** —
`POST /api/auth/magic-link/request` and the signed-out invite-accept fallback
both return `403`. Redemption stays open, which is what makes the escape hatch
below work. **API keys are unaffected**: machines don't do SSO, so CI keeps
working.

A misconfigured IdP (expired client secret, rotated signing keys, discovery
down) would otherwise lock every human out — including whoever would fix it.
So:

```bash
docker compose -f deploy/docker/docker-compose.yml exec server \
  dstream admin magic-link you@example.com
```

It prints a single-use sign-in link for an **existing** user, bypassing
enforcement. The user must already exist and already belong to at least one
org — it refuses otherwise, because the audit row below has nowhere to be
filed. For a brand-new deployment with nobody in it, use
`dstream admin bootstrap` instead.

**This command is as powerful as host shell access** — it mints a working
login for any existing user, with no IdP involved. That is intentional: shell
access to the host is a stronger factor than any identity provider. Treat
access to the host, the container, and `docker compose exec` as equivalent to
access to every account on the deployment.

Each use writes an audit row (`auth.break_glass_magic_link`). Two known
limitations of that row, so you can read the trail correctly:

- **Its actor columns name the *target* user, not the operator.** The audit
  table's `CHECK` requires a non-null actor, and the real actor is whoever
  holds the shell — so the operator's identity lives in the row's metadata
  instead (`host`, `os_user`, `actor: "cli"`). Read those, not the actor
  column.
- **It's filed in the user's oldest-membership org only.** A user who belongs
  to several orgs gets one row, in the org they joined first; their
  break-glass is **invisible** in the other orgs' audit trails.

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

- **Role-based access control** — members read, create and edit an org's sources, connections, destinations and endpoints; admins additionally delete, read and rotate endpoint secrets, publish outbound messages, and mint or revoke App Portal access; owners additionally delete the org and transfer ownership. Members keep full create and edit rights over routing configuration, including destination and endpoint URLs. API keys carry their own role (default `admin`, so existing keys are unaffected). **Upgrading from an earlier version:** existing `member` users lose delete, secret, publish and App Portal mint/revoke access — promote anyone who needs it to `admin`.
- **SSRF-guarded delivery** — the worker refuses to POST to loopback/private/link-local (cloud-metadata) addresses; checked at dial time to defeat DNS rebinding.
- **Session revocation** — signed cookies carry an epoch; logout invalidates all of a user's sessions.
- **Single sign-on shares the session model** — an OIDC login goes through the same signer and the same account bootstrap as a magic-link login, so epoch revocation ("log out everywhere") covers SSO users too, and neither method can escalate a role an existing member already holds. The callback has to be a `GET` (an IdP redirects the browser back by navigation), so its state is **bound to the browser** by a short-lived `dstream_sso_state` cookie on top of being single-use in Redis — without that binding the callback would be a session-fixation primitive, since anyone can mint a state at the unauthenticated start endpoint. **Named residual:** that cookie's whole security property is that its value can't be injected, and it is not `__Host-` prefixed (which would require `Secure`, false in local HTTP dev) — so unlike `dstream_session` (HMAC-signed) and `dstream_csrf` (bound to the session value), a cookie tossed from a **compromised sibling subdomain** is directly exploitable against this one. If you serve dstream on a shared parent domain, treat every sibling subdomain as part of its trust boundary. SSO changes nothing about API-key auth or the App Portal.
- **CSRF** double-submit on the dashboard; API keys are exempt by construction.
- **Rate limits** on ingest, magic-link issuance and SSO start (300/hour per client IP); **per-destination** rate + in-flight caps on delivery.
- Sensitive inbound headers (`Authorization`, `Cookie`) are stripped before forwarding to destinations.

**Not yet implemented — deliberately deferred to post-release:** inbound webhook **signature verification** and outbound **delivery auth**. dstream does plain forwarding today: `requests.sig_verified` is always false, and a destination's `auth_config` is stored but unused. The columns and API fields exist so auth can land without a migration. Don't rely on dstream to authenticate a sender yet.

---

## Roadmap

| # | Phase | Status |
| - | ----- | ------ |
| 1 | **Core inbound gateway** — ingest → dedup → deliver → retry → dashboard | ✅ shipped + hardened |
| 2 | **Outbound webhooks** — Svix-style publish + signed subscriber fan-out, endpoint lifecycle, App Portal, delivery controls, operational webhooks | ✅ shipped |
| 3 | **Transformations + filters** — CEL filter + sandboxed `goja` transform per connection/endpoint (both pipelines), `filtered` status, preview endpoints; tracing completion + load-test harness | ✅ shipped |
| 4 | **Record / replay + fixtures** — retention-pinned fixtures, reinject/replay-to-URL/export, import, CEL auto-capture rules, ordered scenarios; CLI + dashboard | ✅ shipped |
| 5 | Multi-tenant hardening — full RBAC, SSO, billing hooks | 🚧 5a + 5b shipped (RBAC, key roles, instance-level OIDC SSO); 5c billing planned |
| 6 | Self-host packaging — Helm, single-binary release | planned |

A visual workflow builder held the fifth slot until 2026-09-29: it was built, then dropped — the connections page already reads the topology and builds it, so a node canvas was a third way to do the same thing. The phases after it moved up. See `PLAN.md` §7 for the reasoning.

---

## Repo layout

```
cmd/dstream/      CLI entry — server | worker | cli | migrate | admin | maintenance
internal/
  ingest/         HTTP receiver, dedup, body store, fan-out, auto-capture hook
  dqueue/         Redis per-org fair-scheduling delivery queue (Lua + client)
  deliver/        HTTP delivery, retry policy, rate limit, SSRF guard, reaper
  webhook/        outbound publish + signed subscriber fan-out (Svix model)
  opevents/       operational webhooks (per-org app, lifecycle triggers)
  filter/         CEL filter evaluation (cost-bounded)
  transform/      goja JS sandbox for per-connection/endpoint transforms
  bookmark/       fixture capture, reinject, replay-to-URL, export
  api/            REST API (pipeline, outbound, identity, portal, CLI tunnel)
  admin/          /admin/* routes (overview, orgs, queue stats + ops)
  auth/           API keys, signed sessions, magic links, OIDC SSO, portal tokens, CSRF, RBAC
  store/          sqlc-generated Postgres access
  audit/          audit-log writes
  config/ logging/ metrics/ tracing/ middleware/ mailer/
db/
  schema/         schema.sql — the source of truth
  migrations/     Atlas migrations (embedded in the binary, auto-applied)
  queries/        sqlc query inputs
deploy/docker/    Dockerfile, web.Dockerfile, docker-compose.yml
deploy/helm/      empty — Phase 6
web/              TanStack Start dashboard (+ customer-facing App Portal)
tools/loadtest/   ingest load harness (`make load`)
PLAN.md           live design doc — single source of truth
```

---

## Troubleshooting

| Symptom | Fix |
| ------- | --- |
| `up` fails on a stale Postgres volume | PG18 won't start on older data. `docker compose -f deploy/docker/docker-compose.yml down -v` then `up` again (dev data is disposable). |
| Can't sign in / no magic link | Ensure `DSTREAM_DEV_MODE=true` in `.env`, then read the link from `logs -f server`. |
| Magic-link sign-in returns `403` | `DSTREAM_OIDC_ENFORCE=true` — use the SSO button instead. If the IdP itself is broken and nobody can get in, the break-glass is `dstream admin magic-link you@example.com` (see [Single sign-on](#single-sign-on-oidc)). |
| SSO fails: "email not verified by the identity provider" | Your IdP isn't sending `email_verified: true`. Add the claim at the IdP — dstream will not match an unverified address to an account. |
| SSO fails: "DSTREAM_OIDC_DEFAULT_ORG does not match any organization" | The variable wants an org **slug**. `dstream admin org create <name> <owner_email>` derives the slug from the name and prints it — copy that. Or unset the variable, in which case first-time SSO users each get a personal workspace. The slug is intentionally not checked at boot. |
| `sso not configured` (404) on the SSO button | `DSTREAM_OIDC_ISSUER` is empty on the server. The login page also offers both sign-in controls when it can't reach `/api/auth/methods`, so this 404 can mean the probe failed rather than that SSO is missing. |
| Port already in use | Free host ports 3000 (web), 8080 (server), 6379 (redis), 5433 (postgres) — a local Redis on 6379 is the usual clash. |
| `/admin/queues` returns 403 | Promote yourself: `docker compose -f deploy/docker/docker-compose.yml exec server dstream admin promote you@example.com`, then re-login. |
| Delivery to a localhost destination fails | The SSRF guard blocks private IPs. Use the **CLI tunnel** for local forwarding, or set `DSTREAM_ALLOW_PRIVATE_DESTINATIONS=true` for trusted local testing. |

---

## License

TBD before first public release. Current intent: AGPL-3.0, with a separate commercial license for hosted SaaS.
