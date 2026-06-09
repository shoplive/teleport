#!/usr/bin/env bash
# Tear down the shoplive-kind kind cluster.
set -euo pipefail

CLUSTER=shoplive-kind

if ! command -v kind >/dev/null; then
  echo "kind not installed; nothing to do."
  exit 0
fi

if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  echo "==> Deleting kind cluster '$CLUSTER'..."
  kind delete cluster --name "$CLUSTER"
else
  echo "kind cluster '$CLUSTER' not found."
fi

# The teleport kube_server resource lingers in the teleport backend with a TTL;
# manually clean up to make `tsh kube ls` reflect reality immediately.
if docker ps --format '{{.Names}}' | grep -qx shoplive-teleport; then
  docker compose -f "$(dirname "$0")/../docker-compose.yml" exec -T teleport \
    tctl -c /etc/teleport/teleport.yaml rm kube_cluster/shoplive-kind 2>/dev/null || true
fi

echo "kind teardown done."
