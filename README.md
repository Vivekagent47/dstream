# dstream

Open-source webhook management, monitoring, and testing platform. The dev IDE for webhooks.

> Status: Phase 1 in development — core inbound gateway. See `PLAN.md` for the live design doc and phased roadmap.

## What it does

dstream sits between webhook senders (Stripe, GitHub, Shopify, your own services) and your application. It receives webhook traffic, durably stores it, applies routing and delivery policy, and forwards to your endpoints with retries — observable end-to-end.

Three differentiator bets:

1. **Best local dev loop** — first-class CLI: tunnel + replay + fixture library.
2. **Visual workflow builder** — node-based UI for source → filter → transform → destination.
3. **Record/replay 3rd-party providers** — VCR-style fixtures for deterministic CI tests.

## Stack

- Go backend (modular monolith, single binary with subcommands)
- Tanstack Start frontend
- Postgres (state) + Redis (queue, rate limit, cache)
- `hibiken/asynq` task queue with `asynqmon` ops UI

## Quick start (dev)

```bash
# bring up postgres + redis + minio
docker compose -f deploy/docker/docker-compose.yml up -d

# run migrations
go run ./cmd/dstream migrate

# start API server
go run ./cmd/dstream server

# in another terminal: start delivery worker
go run ./cmd/dstream worker
```

## Repo layout

See `PLAN.md` for the full layout and rationale.

## License

TBD (will pick before first public release; current intent: AGPL-3.0 with commercial license offering for hosted SaaS).
