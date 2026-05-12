#!/usr/bin/env bash
# Block until the teleport proxy responds to /webapi/ping.
set -euo pipefail

# Hit the host's port-forwarded 3080 directly — works without an /etc/hosts
# entry on the macOS host. -k skips TLS verification (just a readiness probe).
URL="https://127.0.0.1:3080/webapi/ping"
echo -n "waiting for teleport proxy "
for i in $(seq 1 60); do
  if curl -skf -o /dev/null "$URL"; then
    echo "ready"
    exit 0
  fi
  echo -n "."
  sleep 2
done
echo
echo "ERROR: teleport did not become ready in 120s; check 'make tlogs'" >&2
exit 1
