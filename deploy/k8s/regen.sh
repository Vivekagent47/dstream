#!/usr/bin/env bash
# Re-render deploy/k8s/ from the Helm chart. The chart is the source of truth;
# these raw manifests are a convenience for `kubectl apply -k` without Helm.
# Run this after any change to deploy/helm/dstream.
set -euo pipefail
cd "$(dirname "$0")"
CHART=../helm/dstream
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

helm template dstream "$CHART" \
  --namespace dstream \
  --set ingress.host=dstream.example.com \
  --set secrets.sessionSecret=REPLACE_ME_SESSION_SECRET \
  --set postgresql.password=REPLACE_ME_POSTGRES_PASSWORD \
  --output-dir "$TMP"

HEADER='# GENERATED from the Helm chart (deploy/helm/dstream) via `helm template`.
# Do not edit by hand — change the chart and re-run ./deploy/k8s/regen.sh.
# Placeholders to replace before applying: REPLACE_ME_* and dstream.example.com.'

rm -f ./*.yaml
cat > namespace.yaml <<'NS'
apiVersion: v1
kind: Namespace
metadata:
  name: dstream
NS

for f in "$TMP"/dstream/templates/*.yaml; do
  b=$(basename "$f")
  { printf '%s\n\n' "$HEADER"; cat "$f"; } > "$b"
done

{
  echo 'apiVersion: kustomize.config.k8s.io/v1beta1'
  echo 'kind: Kustomization'
  echo 'namespace: dstream'
  echo 'resources:'
  echo '  - namespace.yaml'
  for f in $(ls ./*.yaml | grep -vE 'namespace.yaml|kustomization.yaml' | sort); do echo "  - $(basename "$f")"; done
} > kustomization.yaml

echo "regenerated $(ls ./*.yaml | wc -l | tr -d ' ') manifests"
