---
name: dev-cluster
description: Use when the operator wants to bring up, tear down, or troubleshoot the local dev cluster (`dev/`). Triggers include "make up isn't working", "fresh start", "the cluster is in a weird state", "I want to test SSO again from scratch", "wipe everything", or any first-time setup question. Maps the operator to the right command rather than letting them guess.
---

# Local dev cluster — bring-up, teardown, troubleshooting

The dev cluster is documented in `dev/README.md`. This skill is the lookup table for "which command should I run for X."

## First-time bring-up (host)

```sh
# 1. Hosts file (one-time)
sudo sh -c 'echo "127.0.0.1   keycloak.shoplive.local  teleport.shoplive.local" >> /etc/hosts'

# 2. Bring up the stack (~3–8 min cold; cached rebuilds 30–60s)
cd dev
make up

# 3. Trust the dev CA in macOS keychain so the browser stops warning
make trust-ca

# 4. Set the admin password using the URL printed by `make up`,
#    register TOTP, then verify
make login
make tsh ARGS="ls"

# 5. Apply the OIDC connector + roles, then SSO login as a Keycloak user
make bootstrap-oidc
make bootstrap-roles
make oidc-login   # browser opens to Keycloak; user dev/dev or alice/alice
```

## Daily flow (start of work)

```sh
cd dev
make up              # state preserved across down/up cycles
make status          # confirm everything's ready
```

## Daily flow (end of work)

```sh
make down            # stops containers, KEEPS state (admin TOTP, KC users, teleport DB)
```

## Iterating on Go code

```sh
# After editing in lib/...
make rebuild         # ~10–30s with BuildKit caches
make logs | grep -i oidc
```

For continuous rebuilds:
```sh
make hot-reload      # docker compose watch — rebuilds on every save
```

## Iterating on Web UI

The TS bundle is built INSIDE the teleport image during `make rebuild`. There's no separate dev server wired up. So:

```sh
make rebuild
# hard-refresh browser
```

## Adding test resources

| Goal | Command |
|---|---|
| Add an SSH node | Edit `dev/bootstrap/node-1.yaml` or add `node-3.yaml`, then update `docker-compose.yml`, `make rebuild` |
| Spin up kind k8s cluster | `make kind-up` (requires `brew install kind kubectl`) |
| Tear down kind | `make kind-down` |
| Test the kube path | `make kube-ls` → `make kube-login` → `make tsh ARGS="kubectl get nodes"` |

## Common breakages

### "make up" complains "go not found"

That's history. The whole thing builds in containers; the host doesn't need Go. If you see this, you're on an old commit — pull main.

### Build fails on cargo / rustup / wasm

The teleport image uses Rust for IronRDP wasm in the Web UI. `dev/Dockerfile` installs Rust 1.94.0 + cargo-binstall + prebuilt wasm-bindgen-cli/wasm-opt. If a recent docker layer change broke it, check the Dockerfile diff.

### "OIDC discovery failed: tls: failed to verify certificate"

Your CA trust desynced. Two possibilities:
- KC is running with an older cert from before pki regen → `docker compose up -d --force-recreate keycloak`.
- Teleport image was built before the latest CA → `make rebuild`.
- For a clean reset: `make destroy && make up`.

### tsh from host says "x509: unknown authority"

Either run `make trust-ca` (adds CA to keychain) or use the in-container tsh: `make tsh ARGS="..."` / `make oidc-login`.

### Web UI shows "github connector 'X' is not configured" on an OIDC connector

Don't click Edit on OIDC tiles. The OSS Web UI doesn't have an OIDC editor; we hide Edit/Delete on OIDC items in `ConnectorList.tsx` for that reason. If clicking still routes you there, the change reverted.

### Realm changes aren't picked up

KC imports the realm on first boot only (it logs `Realm 'shoplive' already exists. Import skipped`). After editing `dev/keycloak/realm-shoplive.json`:

```sh
make destroy && make up   # wipes the postgres volume → fresh import
```

### Admin password / TOTP lost

```sh
make tctl ARGS="rm user/admin"
make bootstrap-admin     # prints a new password-set URL
```

For a Keycloak end-user (dev/alice) who lost TOTP: Keycloak admin console (`https://keycloak.shoplive.local:8443`, admin/admin) → Users → select user → Credentials → delete the OTP entry → next login re-prompts setup.

## Nuke options

| Want | Command |
|---|---|
| Stop everything, KEEP state | `make down` |
| Wipe state (TOTP, users, sessions, kc data) | `make destroy` |
| Full clean slate (= destroy + up) | `make reset` |
| Wipe just the dev CA / certs | `rm -rf dev/pki && make rebuild` |
| Wipe tsh state on host | `rm -rf ~/.tsh` |

## Don't

- Don't run `make destroy` casually — admin TOTP and Keycloak end-user TOTPs go away.
- Don't add `--insecure` to fix TLS issues. The CA system is already there; trust it once.
- Don't manually mutate things via Keycloak admin console expecting them to persist past `make destroy`. Persist your changes in the realm JSON (`dev/keycloak/realm-shoplive.json`).
- Don't switch hostnames (`*.shoplive.local`) without re-cutting certs — the CA bundle baked into the teleport image won't match.
