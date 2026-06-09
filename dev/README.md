# Shoplive Teleport — local dev cluster

Self-contained dev environment for the in-house OIDC SSO build. **Containers only — no Go toolchain required on the host.** Single host. Tear down with `make down`.

## What runs

| Component | Image | Endpoint |
|---|---|---|
| Postgres (Keycloak backend) | `postgres:16-alpine` | (internal only) |
| Keycloak (Shoplive realm pre-seeded) | `quay.io/keycloak/keycloak:26.0` | `https://keycloak.shoplive.local:8443` |
| Teleport (auth + proxy + ssh + db_service) | built from `../` via `dev/Dockerfile` | `https://teleport.shoplive.local:3080` (+ `:3023/3024/3025/38000`) |
| `node-1`, `node-2` | same teleport image, ssh-only roles | reverse-tunnel to teleport |
| (optional) kind k8s + teleport-kube-agent | `kind` + same teleport image inside | `make kind-up` |

The two service hostnames resolve to the matching container both inside the docker network (via `networks.aliases`) and on the macOS host (via the `/etc/hosts` entry above). That's required to make the OIDC `iss` claim, the IdP `redirect_uri`, and `tsh login --proxy=...` all agree on a single URL.

State persists across `make down` via two named volumes (`teleport_data`, `postgres_data`). Keycloak is configured with `KC_DB=postgres` (same backend type we'll run in EKS).

The teleport image is multi-stage: a `golang:1.25` builder (with Node 24 + pnpm via corepack) compiles `teleport`, `tctl`, `tsh` from the current source tree, including the Web UI bundle (`pnpm build-ui-oss`). The runtime image carries just the three binaries on `debian-slim`. BuildKit cache mounts are wired for the Go build/mod caches *and* the pnpm store, so iterative rebuilds are fast.

Build skips: `RDPCLIENT_SKIP_BUILD=1` (Rust RDP client — not relevant to OIDC) and `FIDO2=off` (TOTP via Keycloak is enough for dev). First cold build is ~5–8 min (Go modules + pnpm install + bundle); subsequent rebuilds with no source change are seconds, source changes are typically 30–60 s.

The realm in `keycloak/realm-shoplive.json` ships with:
- Client `teleport-proxy` (confidential, PKCE S256, secret `teleport-dev-secret`)
- Groups `teleport-admins`, `teleport-devs`
- Test users `dev / dev` (admin group), `alice / alice` (dev group) — both forced to set up TOTP on first login
- TOTP enforced. Google IdP federation is **not** wired (needs real Google OAuth client; configure manually in Keycloak when you need it).

`teleport.yaml` defaults to `type: local` (username + OTP) so the cluster is usable **before** the `OIDCService` implementation lands. Switch to `type: oidc` after Phase 5 of the SSO plan.

## Prerequisites

- Docker Desktop or Docker Engine + Compose v2.
- One-time `/etc/hosts` entry on the macOS host:
  ```
  127.0.0.1   keycloak.shoplive.local  teleport.shoplive.local
  ```
  Both names need to resolve consistently from the macOS browser AND from inside the docker network so the OIDC `iss` claim and redirect URLs match.

That's it. No Go, no Node, no Rust on the host.

## TLS / root CA

`make pki` (auto-invoked by `make up`) runs a one-shot `alpine + openssl` container that produces:

```
dev/pki/
├── ca/
│   ├── ca.crt   # Shoplive Dev Root CA
│   └── ca.key
├── keycloak/    # SAN: keycloak.shoplive.local, keycloak, localhost, 127.0.0.1
│   ├── tls.crt
│   └── tls.key
└── teleport/    # SAN: teleport.shoplive.local, teleport, localhost, 127.0.0.1
    ├── tls.crt
    └── tls.key
```

The root CA is baked into the teleport runtime image (`update-ca-certificates`), so the in-container OIDC client trusts Keycloak. To make the macOS browser trust both endpoints without click-through warnings:

```sh
make trust-ca       # one-time, prompts for sudo
# later, to clean up:
make untrust-ca
```

`pki/` is gitignored — regenerate by `rm -rf dev/pki && make pki` (and rebuild the image).

## First run

```sh
cd dev
make up
```

This will:
1. `docker compose build teleport` — builds the multi-stage image (cold first build is ~3 min).
2. `docker compose up -d` — starts Keycloak + teleport.
3. Wait for the Shoplive realm and `/webapi/ping` to be live.
4. Apply the `shoplive-admin` role and create an `admin` user, printing a password-set URL.

Open the URL printed at the end, set a password + register TOTP, then:

```sh
make login                 # tsh login --proxy=localhost:3080 --user=admin --insecure
make tsh ARGS="ls"         # see the teleport-dev node
make tsh ARGS="ssh root@teleport-dev"
```

Web UI: `https://localhost:3080` (accept the self-signed cert).

## Iteration loop

After editing Go code:

```sh
make rebuild   # docker compose build teleport && force-recreate teleport
```

Fast (~10–30s) because the BuildKit cache preserves the Go build cache across image builds. The `teleport_data` named volume is reused, so users / roles / connectors persist across rebuilds.

For continuous rebuilds on every save:

```sh
make hot-reload   # docker compose watch teleport
```

`compose watch` monitors `api/`, `lib/`, `tool/`, `proto/`, `gen/`, `session/`, `go.mod`, `go.sum`, and `teleport.yaml`; any change triggers an image rebuild + container restart. Logs stream to stdout — Ctrl+C to stop watching (containers keep running; `make down` to actually stop them).

## Testing the OIDC build

### Unit tests (no host Go required)

```sh
make test                              # ./lib/auth/oidc/... by default
make test PKG=./lib/auth/...           # widen scope
make test PKG=./lib/auth/oidc/... GOFLAGS="-run TestCallbackValidation -v"
make vet PKG=./lib/auth/...            # go vet
make go ARGS="mod tidy"                # any other go command
```

These run `go test` inside a fresh `golang:1.25-bookworm` container; the build/mod caches are persisted in two named Docker volumes (`shoplive-go-build-cache`, `shoplive-go-mod-cache`), so subsequent runs are fast.

### End-to-end SSO round-trip

The in-house OIDC RP at `lib/auth/oidc/` is complete: `tsh login --auth=keycloak` builds a real Keycloak auth URL, walks the user through Keycloak (login + TOTP), and the callback handler exchanges the code, verifies the ID token (incl. nonce + PKCE), maps claims to a Teleport user, upserts it, and issues a session + SSH cert. This is **not** the original "OIDC is only available in Teleport Enterprise" upsell — that path is intentionally short-circuited by the in-house service.

1. Apply the connector once: `make bootstrap-oidc`
2. Trigger SSO login:

   ```sh
   make oidc-login
   ```

   This invokes `tsh login --auth=keycloak --bind-addr=0.0.0.0:38000 --callback=http://localhost:38000`. tsh's callback listener binds inside the container on port 38000, which is forwarded to the macOS host (see `docker-compose.yml`).

   Expected flow:

   ```
   tsh prints  http://localhost:38000  →  open in macOS browser
   browser → https://localhost:3080            (proxy redirects to Keycloak)
   browser → http://localhost:8080/realms/shoplive/...   (Keycloak login + TOTP)
   browser → https://localhost:3080/v1/webapi/oidc/callback
            └─ proxy calls Service.ValidateOIDCAuthCallback
               (token exchange → ID-token verify → claim mapping → user upsert)
            └─ proxy posts session + SSH cert back to tsh's local listener
   tsh    →  writes the new identity into ~/.tsh and exits 0
   ```

   To inspect what arrived from Keycloak, tail teleport logs (`make logs`) — the callback handler logs presence flags (`has_state`, `has_code`) and any IdP-side `error` / `error_description` query parameters. Raw `state`/`code` values are intentionally not logged.

   For a quick "did the URL build correctly?" check without going through the browser: `make tsh ARGS="login --proxy=localhost:3080 --insecure --auth=keycloak --browser=off"` prints the Keycloak authz URL. Inspect the URL — it should target `/realms/shoplive/protocol/openid-connect/auth` with `client_id=teleport-proxy`, `response_type=code`, the `state` token, and the configured `scope`.

### Host-side `tsh` / `tctl` (alias)

`dev/bin/{tsh,tctl}` are thin wrappers that exec into the running teleport container. They're indistinguishable from a host-installed CLI for everyday use.

```sh
make hostbin   # prints the snippet to copy into ~/.zshrc

# Either: prepend bin/ to PATH
export PATH="$(pwd)/dev/bin:$PATH"
tsh login --proxy=localhost:3080 --user=admin --insecure
tctl get users

# Or alias each
alias tsh="$(pwd)/dev/bin/tsh"
alias tctl="$(pwd)/dev/bin/tctl"
```

Caveats:
- The wrappers refuse to run if the teleport container isn't up.
- tsh state (certs, current cluster) lives at `/root/.tsh` inside the container — wiped by `make destroy`, preserved by `make down`/`make up`.
- For OIDC login from the alias, you still need the SSO-specific flags so the callback round-trips through forwarded port 38000:
  ```sh
  tsh login --proxy=localhost:3080 --insecure --auth=keycloak \
    --bind-addr=0.0.0.0:38000 --callback=http://localhost:38000
  ```
  (Or just keep using `make oidc-login` — same flags, less typing.)

## Realm changes

The realm JSON is imported on first boot only. To re-import after edits:

```sh
make down   # also wipes Keycloak's H2 DB
make up
```

For one-off changes (testing claim mappers, federation), use the Keycloak admin console at `http://localhost:8080` (admin / admin). Anything you want persisted must go back into `keycloak/realm-shoplive.json`.

To add Google IdP federation locally, you need a real Google Cloud OAuth client (Web application, redirect URI `http://localhost:8080/realms/shoplive/broker/google/endpoint`). Configure it in Keycloak's "Identity Providers" tab — not committed since it requires per-developer credentials.

## Files

```
dev/
├── Makefile                       # entry point
├── Dockerfile                     # multi-stage teleport build
├── Dockerfile.dockerignore        # trim build context
├── docker-compose.yml             # keycloak + teleport
├── teleport.yaml                  # local cluster config (paths inside container)
├── keycloak/
│   └── realm-shoplive.json        # auto-imported realm
├── bootstrap/
│   ├── admin-role.yaml            # full-access role for dev
│   └── oidc-connector.yaml        # Keycloak connector
└── scripts/
    ├── wait-keycloak.sh
    ├── wait-teleport.sh
    └── bootstrap-admin.sh
```

## Common commands

```sh
make                       # show targets
make up                    # build + bring everything up + bootstrap admin
make rebuild               # rebuild teleport image, restart teleport, keep state
make hot-reload            # compose watch — rebuild on every save
make down                  # stop everything (state PRESERVED)
make destroy               # stop + wipe all state volumes
make reset                 # destroy + up (clean slate)
make status                # is everything ready?
make logs / make kc-logs   # tail teleport / keycloak
make tctl ARGS="get users"
make tsh  ARGS="ls"
make shell                 # bash inside the teleport container
make test                  # go test inside a builder container
make trust-ca              # add dev CA to macOS keychain (sudo)

# SSO + tsh
make login                 # local admin login
make oidc-login            # tsh login --auth=keycloak (PKCE, callback wired)
make bootstrap-oidc        # apply / update OIDC connector
make bootstrap-roles       # apply / update shoplive-admin + shoplive-dev roles

# SSH / k8s targets via Teleport
make ls                    # tsh ls — registered nodes
make ssh-node-1            # tsh ssh root@node-1
make kind-up               # spin up kind cluster + teleport-kube-agent
make kube-ls               # tsh kube ls
make kube-login            # tsh kube login shoplive-kind
make kind-down             # tear down kind
```

## Caveats

- `--insecure` on tsh is required because the proxy uses self-signed certs. Don't carry this flag into anything that talks to the real cluster.
- `make down` is **non-destructive** — it stops containers but preserves the `teleport_data` and `keycloak_data` named volumes, so the admin user's password + TOTP and the Keycloak end-users' TOTP secrets all survive across down/up cycles.
- `make destroy` is the wipe path (`docker compose down -v`). Use it when you want a clean slate (e.g. after editing the realm JSON, since realms are only imported on first boot).
- `make reset` = `destroy` + `up`.
- The admin user is local (not OIDC). Once OIDC works end-to-end, demote it to a break-glass account or delete it.
- `tsh` runs inside the teleport container in this setup. That means the OAuth loopback used during SSO login binds inside the container, not on the macOS host — solving that for OIDC is part of Phase 7.
