#!/usr/bin/env bash
# Redeploy Phoenix split-mode stack to local Colima k8s (profile: k8s).
#
# Usage (from repo root):
#   ./deploy/k8s-local/redeploy.sh          # rebuild images + helm upgrade + rollout
#   ./deploy/k8s-local/redeploy.sh --fast   # skip image rebuild (config-only helm upgrade)
#
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
NAMESPACE=phoenix
RELEASE=phoenix
VALUES="$ROOT/deploy/k8s-local/values-split.yaml"
CHART="$ROOT/charts/uptime-phoenix"
FAST=false

for arg in "$@"; do
  case "$arg" in
    --fast) FAST=true ;;
    -h|--help)
      echo "Usage: $0 [--fast]"
      echo "  --fast  Helm upgrade only (no image rebuild)"
      exit 0
      ;;
    *) echo "Unknown arg: $arg"; exit 1 ;;
  esac
done

kubectl config use-context colima-k8s

if [[ "$FAST" == false ]]; then
  echo "==> Building frontend (host)..."
  (cd "$ROOT/web" && bun run build)

  echo "==> Building phoenix:local (api + worker)..."
  docker context use colima-k8s
  docker build -t phoenix:local "$ROOT"

  echo "==> Building phoenix-web:local (nginx + SPA)..."
  docker buildx build \
    --build-context webdist="$ROOT/web/dist" \
    -f "$ROOT/deploy/k8s-local/Dockerfile.web" \
    -t phoenix-web:local \
    --load "$ROOT"

  echo "==> Loading images into colima-k8s..."
  docker save phoenix:local phoenix-web:local -o /tmp/phoenix-split-images.tar
  docker context use colima-k8s
  docker load -i /tmp/phoenix-split-images.tar
  rm -f /tmp/phoenix-split-images.tar
fi

echo "==> Helm upgrade..."
helm upgrade "$RELEASE" "$CHART" -n "$NAMESPACE" -f "$VALUES"

echo "==> Rolling out new images (same :local tag requires restart)..."
kubectl rollout restart deployment/phoenix-api deployment/phoenix-worker deployment/phoenix-web -n "$NAMESPACE"
kubectl rollout status deployment/phoenix-api deployment/phoenix-worker deployment/phoenix-web -n "$NAMESPACE" --timeout=180s

echo ""
echo "Done. Access the UI:"
echo "  kubectl port-forward -n $NAMESPACE svc/phoenix-web 8080:80"
echo "  open http://localhost:8080   (admin / ChangeMe123!)"