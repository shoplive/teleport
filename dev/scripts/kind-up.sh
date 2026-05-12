#!/usr/bin/env bash
# Spin up a kind k8s cluster and register it with the Shoplive teleport
# cluster via a teleport-kube-agent pod.
#
# After this runs:
#   make tsh ARGS="kube ls"      → "shoplive-kind" listed
#   make tsh ARGS="kube login shoplive-kind"
#   make tsh ARGS="kubectl get nodes"
set -euo pipefail

cd "$(dirname "$0")/.."

CLUSTER=shoplive-kind
KIND_NODE="${CLUSTER}-control-plane"

if ! command -v kind >/dev/null; then
  echo "ERROR: kind not installed. macOS: brew install kind" >&2
  exit 1
fi
if ! command -v kubectl >/dev/null; then
  echo "ERROR: kubectl not installed. macOS: brew install kubectl" >&2
  exit 1
fi

# 1. Create cluster if missing.
if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  echo "==> Creating kind cluster '$CLUSTER'..."
  kind create cluster --name "$CLUSTER" --config kind/cluster.yaml
else
  echo "==> kind cluster '$CLUSTER' already exists."
fi

# 2. Connect kind control-plane container to the shoplive docker network so
#    teleport.shoplive.local / keycloak.shoplive.local resolve via docker DNS
#    from the kind container itself.
if ! docker network inspect shoplive -f '{{range .Containers}}{{println .Name}}{{end}}' \
        | grep -qx "$KIND_NODE"; then
  echo "==> Connecting $KIND_NODE to docker network 'shoplive'..."
  docker network connect shoplive "$KIND_NODE"
fi

# 3. Resolve teleport / keycloak container IPs on the shoplive network.
TELEPORT_IP=$(docker inspect shoplive-teleport \
  -f '{{range $k, $v := .NetworkSettings.Networks}}{{if eq $k "shoplive"}}{{$v.IPAddress}}{{end}}{{end}}')
KEYCLOAK_IP=$(docker inspect shoplive-keycloak \
  -f '{{range $k, $v := .NetworkSettings.Networks}}{{if eq $k "shoplive"}}{{$v.IPAddress}}{{end}}{{end}}')

if [[ -z "$TELEPORT_IP" || -z "$KEYCLOAK_IP" ]]; then
  echo "ERROR: shoplive-teleport or shoplive-keycloak IP empty. Is 'make up' done?" >&2
  exit 1
fi

echo "==> Resolved IPs:  teleport=$TELEPORT_IP  keycloak=$KEYCLOAK_IP"

# 4. Load the locally-built shoplive/teleport-dev image into kind so the
#    agent pod can find it without going to a registry.
echo "==> Loading shoplive/teleport-dev:latest into kind..."
kind load docker-image shoplive/teleport-dev:latest --name "$CLUSTER"

# 5. Render and apply the agent manifest.
echo "==> Applying teleport-kube-agent manifest..."
sed -e "s|@TELEPORT_IP@|$TELEPORT_IP|g" \
    -e "s|@KEYCLOAK_IP@|$KEYCLOAK_IP|g" \
    kind/teleport-agent.yaml.tmpl \
  | kubectl --context "kind-$CLUSTER" apply -f -

# 6. Wait for the rollout.
kubectl --context "kind-$CLUSTER" -n teleport-agent rollout status \
  deployment/teleport-kube-agent --timeout=120s

echo
echo "  ✓ kind cluster ready, teleport-kube-agent running."
echo
echo "  Verify the kube resource is registered:"
echo "    make tsh ARGS=\"kube ls\""
echo "    make tsh ARGS=\"kube login shoplive-kind\""
echo "    make tsh ARGS=\"kubectl get nodes\""
echo
echo "  Tear down with:  make kind-down"
