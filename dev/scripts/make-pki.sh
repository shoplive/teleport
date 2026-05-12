#!/usr/bin/env bash
# Generate a Shoplive dev root CA + per-service TLS certs (Keycloak, Teleport).
# Idempotent — once `pki/ca/ca.crt` exists, this is a no-op. Remove `pki/` to
# regenerate.
#
# All openssl work runs in a one-shot alpine container so the host needs no
# openssl/cfssl/mkcert install.
set -euo pipefail

cd "$(dirname "$0")/.."

# Idempotent: check the LAST file generated, so partial runs (e.g. an openssl
# prompt error before this fix landed) don't get falsely treated as complete.
if [[ -f pki/ca/ca.crt && -f pki/keycloak/tls.crt && -f pki/keycloak/tls.key \
   && -f pki/teleport/tls.crt && -f pki/teleport/tls.key ]]; then
  exit 0
fi

# Wipe partial state so a rerun starts clean.
rm -rf pki
mkdir -p pki/ca pki/keycloak pki/teleport

docker run --rm -v "$(pwd)/pki:/pki" alpine:3 sh -c '
  set -e
  apk add --no-cache openssl > /dev/null

  cd /pki

  # ---------- Root CA (10 years) ----------
  openssl genrsa -out ca/ca.key 4096 > /dev/null 2>&1
  openssl req -x509 -new -nodes -key ca/ca.key -sha256 -days 3650 \
    -out ca/ca.crt \
    -subj "/CN=Shoplive Dev Root CA/O=Shoplive (dev only)"

  # ---------- Keycloak server cert ----------
  openssl genrsa -out keycloak/tls.key 2048 > /dev/null 2>&1
  cat > /tmp/kc.cnf <<EOF
[req]
prompt = no
distinguished_name = dn
req_extensions = ext
[dn]
CN = keycloak.shoplive.local
[ext]
subjectAltName = DNS:keycloak.shoplive.local,DNS:keycloak,DNS:localhost,IP:127.0.0.1
EOF
  openssl req -new -key keycloak/tls.key -out /tmp/kc.csr -config /tmp/kc.cnf
  openssl x509 -req -in /tmp/kc.csr -CA ca/ca.crt -CAkey ca/ca.key -CAcreateserial \
    -out keycloak/tls.crt -days 825 -sha256 \
    -extfile /tmp/kc.cnf -extensions ext > /dev/null 2>&1

  # ---------- Teleport proxy cert ----------
  openssl genrsa -out teleport/tls.key 2048 > /dev/null 2>&1
  cat > /tmp/tp.cnf <<EOF
[req]
prompt = no
distinguished_name = dn
req_extensions = ext
[dn]
CN = teleport.shoplive.local
[ext]
subjectAltName = DNS:teleport.shoplive.local,DNS:teleport,DNS:localhost,IP:127.0.0.1
EOF
  openssl req -new -key teleport/tls.key -out /tmp/tp.csr -config /tmp/tp.cnf
  openssl x509 -req -in /tmp/tp.csr -CA ca/ca.crt -CAkey ca/ca.key -CAcreateserial \
    -out teleport/tls.crt -days 825 -sha256 \
    -extfile /tmp/tp.cnf -extensions ext > /dev/null 2>&1

  # Keycloak runs as UID 1000 inside its image; certs need to be readable.
  chmod 644 keycloak/tls.crt keycloak/tls.key teleport/tls.crt teleport/tls.key ca/ca.crt
'

echo
echo "  ✓ PKI generated under dev/pki/"
echo
echo "  Add to /etc/hosts (one-time, host machine):"
echo "    127.0.0.1   keycloak.shoplive.local  teleport.shoplive.local"
echo
echo "  Trust the root CA in macOS keychain (recommended; otherwise click through cert warnings):"
echo "    sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain $(pwd)/pki/ca/ca.crt"
echo
echo "  Untrust later:"
echo "    sudo security delete-certificate -c 'Shoplive Dev Root CA' /Library/Keychains/System.keychain"
