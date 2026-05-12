#!/usr/bin/env bash
# Idempotent bootstrap: applies the admin role and creates an `admin` user
# inside the running teleport container. Prints the password-set URL.
set -euo pipefail

cd "$(dirname "$0")/.."

TCTL=(docker compose exec -T teleport tctl -c /etc/teleport/teleport.yaml)

# Apply both Shoplive roles (idempotent — tctl create -f --force overwrites).
"${TCTL[@]}" create -f /etc/teleport/bootstrap/admin-role.yaml --force >/dev/null
"${TCTL[@]}" create -f /etc/teleport/bootstrap/dev-role.yaml   --force >/dev/null

# Create admin user only if missing.
if "${TCTL[@]}" get user/admin >/dev/null 2>&1; then
  echo "admin user already exists; skipping"
  exit 0
fi

# Allow logging in as the host user too (so 'tsh ssh' to the local node works).
host_login="${SUDO_USER:-${USER:-root}}"

echo
echo "creating admin user (logins: root, $host_login)..."
"${TCTL[@]}" users add admin \
  --roles=shoplive-admin \
  --logins="root,$host_login" \
  --ttl=24h
