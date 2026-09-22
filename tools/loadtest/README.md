# loadtest — ingest load-test harness

Standalone tool (`package main`, not part of the dstream binary). Fires ingest
POSTs at a target rate for a fixed duration, reports ingest latency
percentiles, and optionally queries the `attempts` table for the delivery-start
p99. Stdlib + `pgx` only.

## Flags

| flag | default | meaning |
|------|---------|---------|
| `-url` | *(required)* | full ingest URL incl. token, e.g. `http://localhost:8080/e/<token>` |
| `-rate` | `100` | target requests/sec |
| `-dur` | `60s` | how long to send |
| `-conc` | `50` | max concurrent in-flight requests (semaphore) |
| `-db` | `""` | Postgres URL; if set, query delivery-start p99 after the run |
| `-sink` | `false` | start a local HTTP server that returns 200 OK for any request |
| `-sink-addr` | `:9099` | sink listen address |
| `-body` | `""` | path to a JSON body file; empty = a unique small JSON per request |

The default body is unique per request (`{"loadtest":true,"run":<nonce>,"n":<seq>}`)
so it does **not** hit dstream's per-source ingest dedup (60s window), which
would otherwise collapse the whole run into a single delivered event. A fixed
`-body` file may be deduped — use it only when that's intended.

Ingest latency percentiles use the **nearest-rank** method
(`rank = ceil(q*n)`, value = `sorted[rank-1]`) over the 2xx-response latencies.

## One-time setup

The harness only sends traffic; you need a source, a destination pointing at
the sink, and a connection between them, plus the server and worker running.

1. Start infra and the app:
   ```
   make compose-up      # postgres + redis + minio
   make migrate-up      # apply schema
   make server          # terminal 1
   make worker          # terminal 2
   ```
2. Start the harness sink so the destination has somewhere to deliver to:
   ```
   go run ./tools/loadtest -url http://localhost:8080/e/PLACEHOLDER -sink -dur 1s
   ```
   (or just run the real load command below — it starts the sink itself).
3. In dstream, create:
   - a **source** — note its `ingest_token` (the `<token>` in the ingest URL).
   - an **HTTP destination** whose URL is the sink, e.g. `http://localhost:9099`.
   - a **connection** source → destination (enabled).

## Run

```
make load URL=http://localhost:8080/e/<token>
```

or directly:

```
go run ./tools/loadtest \
  -url http://localhost:8080/e/<token> \
  -rate 100 -dur 60s -conc 50 \
  -db "postgres://dstream:dstream@127.0.0.1:5433/dstream?sslmode=disable" \
  -sink
```

Output: total sent, 2xx / non-2xx / error counts, achieved throughput, ingest
latency p50/p95/p99/max, and (with `-db`) attempts recorded in-window,
delivered count, and delivery-start p99 (`queued_in_ms`).

## Results

Local dev run (compose Postgres+Redis on `:5433`/`:6379`, server + worker on host,
built-in `:9099` sink, ingest rate limit raised). **Not** a clean cloud
single-node baseline — hardware-dependent, illustrative.

| date | hardware | rate (req/s) | ingest p99 (ms) | delivery-start p99 (ms) |
|------|----------|--------------|-----------------|-------------------------|
| 2026-09-22 | Apple Silicon laptop (Docker Desktop) | 100 × 60s (5,989 events, 0 errors) | **14.1** | **267** |

Targets (PLAN.md §13): ingest p99 < 100 ms ✓, delivery-start p99 < 500 ms ✓.
Full ingest distribution: p50 3.9 / p95 9.1 / p99 14.1 / max 67 ms. Delivery-start:
p50 1 / p95 3 / p99 267 ms (a thin tail to ~36 s under per-org in-flight backpressure).
