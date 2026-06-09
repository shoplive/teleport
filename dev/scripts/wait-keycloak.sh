#!/usr/bin/env bash
# Block until the Shoplive realm's discovery doc is reachable.
set -euo pipefail

# Hit the host's port-forwarded 8443 directly — works without an /etc/hosts
# entry. -k skips TLS verification (the script is just polling readiness).
URL="https://127.0.0.1:8443/realms/shoplive/.well-known/openid-configuration"
echo -n "waiting for keycloak realm 'shoplive' "
for i in $(seq 1 60); do
  if curl -skf -o /dev/null --resolve "keycloak.shoplive.local:8443:127.0.0.1" "$URL"; then
    echo "ready"
    exit 0
  fi
  echo -n "."
  sleep 2
done
echo
echo "ERROR: keycloak did not become ready in 120s; check 'docker compose logs keycloak'" >&2
exit 1
