# Raw Kubernetes manifests

Plain YAML for teams that apply with `kubectl`/Kustomize instead of Helm.

**These files are generated** from the Helm chart at `../helm/dstream` by
`./regen.sh`. Edit the chart, not these — then re-run `./regen.sh`. The Helm
chart is the source of truth and supports options these static files bake in.

## Before applying — replace the placeholders
- `REPLACE_ME_SESSION_SECRET` — the session-cookie signing secret (`secret.yaml`).
- `REPLACE_ME_POSTGRES_PASSWORD` — appears in both the `pgPassword`/`DSTREAM_DB_URL`
  of `secret.yaml`; keep them identical or Postgres auth fails.
- `dstream.example.com` — your ingress host (`ingress.yaml`, and the base URLs in
  `configmap.yaml`).
- Images default to `ghcr.io/vivekagent47/dstream{,-web}:0.1.0`.

## Apply
    kubectl apply -k deploy/k8s

## What you get
server + worker + a migrate Job + an nginx-served SPA, plus bundled single-node
Postgres and Redis. The datastores are **dev/demo grade** (no HA, no backups).
For production, prefer the Helm chart with `postgresql.enabled=false` /
`redis.enabled=false` pointing at managed datastores, and supply secrets via a
real Secret rather than the placeholders here.

Migrations run as a Job; it retries until Postgres is up. Give it a moment on a
fresh apply.

The migrate Job name is pinned to a revision (e.g. `dstream-migrate-r1`) and a
Job's pod template is immutable, so re-running `kubectl apply -k` after changing
the image tag fails with `field is immutable` while the old Job still exists.
Delete the finished Job first (`kubectl delete job -l app.kubernetes.io/component=migrate`,
or regenerate from the Helm chart, which bumps the revision each upgrade).
