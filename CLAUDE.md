# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Fork context

This is Shoplive's **private** fork of `gravitational/teleport`. The fork exists to **self-implement OIDC SSO**, which is an enterprise-only feature upstream. Treat that as the primary motivation when reasoning about changes in this area.

**Not upstreaming.** Never propose merges, PRs, or discussions back to `gravitational/teleport`. The fork will diverge intentionally — don't preserve enterprise/OSS surface symmetry just to keep changes mergeable upstream. Watch upstream releases for security fixes and rebases; that's the only direction the relationship flows.

**Upstream rebases.** See [`UPSTREAM.md`](./UPSTREAM.md) — full playbook + a 12-file manifest of modified upstream files (the rebase-conflict surface). Everything else we add lives in new files (`lib/auth/oidc/`, `lib/web/oidc.go`, `dev/`, `.claude/`, root-level docs) which don't conflict on rebase.

**Worktree workflow.** Non-trivial feature work and rebases happen in dedicated `git worktree` checkouts under `~/Workspace/shoplive/teleport-worktrees/`. The main checkout stays clean for fast pulls. Sub-agents that mutate code prefer `isolation: "worktree"` in their Agent tool calls.

- Upstream gates SSO (OIDC, SAML) behind the closed-source `e/` submodule (`gravitational/teleport.e`). That submodule is intentionally **not** initialized in this checkout — `e/` is empty and `e_imports.go` only exists to keep `go mod tidy` from dropping enterprise transitive deps.
- The OSS side of OIDC was a stub-only seam — the Shoplive in-house implementation now lives at `lib/auth/oidc/{service,discovery,callback,claims}.go` and is registered from `lib/service/service.go` via `authServer.SetOIDCService(authoidc.New(...))`. The original interface is in `lib/auth/oidc.go`.
- HTTP routes (added in this fork):
  - Proxy side: `lib/web/oidc.go` (handlers) + `/webapi/oidc/{login/web,login/console,callback}` registered in `lib/web/apiserver.go`
  - Auth side: `validateOIDCAuthCallback` + `POST /v1/oidc/requests/validate` in `lib/auth/apiserver.go`
- `lib/modules/modules.go` flips `entitlements.OIDC: {Enabled: true}` in `defaultModules.Features()` so the four enterprise gates in `lib/auth/auth_with_roles.go` pass.
- `UpsertOIDCConnector` / `DeleteOIDCConnector` (CRUD on the connector resource) are already implemented OSS-side; the auth-flow methods (`CreateOIDCAuthRequest`, `ValidateOIDCAuthCallback`, etc.) used to return `errOIDCNotImplemented` and now route to our `Service`.
- Connector types: `api/types/oidc.go`, `api/types/oidc_external.go`.
- When reasoning about a feature, first check whether it has a similar `Set<X>Service` seam — SAML and others follow the same pattern (`lib/auth/saml.go`, `lib/auth/auth.go`).

The license/build-type plumbing is in `lib/modules/` (`BuildOSS`, `BuildEnterprise`, `BuildCommunity` — `teleportBuildType` is set via `-ldflags` in the Makefile). Don't lie about the build type to unlock features; extend the OSS interface instead.

## Repository layout

Polyglot monorepo:
- **Go**: root module `github.com/gravitational/teleport` plus a separate `api/` module (Apache 2.0; the rest is AGPL-3.0). `lib/` holds the bulk of server logic, `tool/` holds the CLI entry points (`teleport`, `tctl`, `tsh`, `tbot`, `teleport-update`, `fdpass-teleport`), `integrations/` holds out-of-tree integrations (operator, terraform provider, event-handler, access plugins) each with their own go.mod.
- **TypeScript**: pnpm workspace. UI code lives in `web/packages/teleport` (web UI) and `web/packages/teleterm` (Electron app). `web/packages/shared` and `web/packages/design` are shared libraries.
- **Rust**: `lib/srv/desktop/rdp/rdpclient` (RDP client for desktop access) and `tool/fdpass-teleport`. Cargo workspace at the repo root.
- **Protos**: `proto/` and `api/proto/`. Generated Go/TS lives under `gen/` and `api/gen/`.

## Local dev cluster

For day-to-day Shoplive work, **don't build on the host** — `dev/` runs the entire stack (teleport + Keycloak) in Docker, with the teleport binary built inside `golang:1.24`. The host needs only Docker; no Go / Node / Rust.

```sh
cd dev && make up           # build image + start cluster + bootstrap admin
cd dev && make rebuild      # after editing Go code
cd dev && make hot-reload   # compose watch — rebuild on every save
cd dev && make tctl ARGS="get users"
cd dev && make tsh  ARGS="ls"
```

See `dev/README.md` for the full flow. The raw `make` targets below describe the upstream host build path — only relevant if you actually want to install Go locally.

## Common commands

Build (Go binaries land in `build/`):
- `make` — build all OSS binaries (development).
- `make full` — production build with embedded webassets.
- `make build/teleport` / `make build/tctl` / `make build/tsh` / `make build/tbot` — single binary.
- `make teleport-hot-reload` — runs `teleport start` under `CompileDaemon`; `TELEPORT_ARGS='start --config=...'` to customize.
- `make -C build.assets build-binaries` — fully dockerized build (no host toolchain needed).
- Build tags worth knowing: `webassets_embed`, `pam`, `bpf`, `fips`, `libfido2` / `libfido2static`, `touchid`, `desktop_access_rdp`, `piv`. The Makefile auto-detects most based on installed dependencies; override with env (e.g. `FIDO2=dynamic|static|off`, `FIPS=yes`, `RDPCLIENT_SKIP_BUILD=1`, `WEBASSETS_SKIP_BUILD=1`).

Web UI:
- `pnpm install` once, then `pnpm build-ui-oss` (or `make docker-ui` for a clean container build).
- `pnpm start-teleport` — Vite dev server. Requires local HTTPS certs via `mkcert` (see `web/README.md`).
- `DEBUG=1 ./build/teleport start -d` — runs the daemon serving UI assets from `webassets/teleport/app` instead of the embedded copy.

Tests:
- `make test` — full suite (helm, sh, api, go, rust, operator, terraform). Slow.
- `make test-go-unit` — Go unit tests (excludes `e2e`, `integration`, `tool/tsh`, `integrations/...`). Use `SUBJECT=./lib/auth/...` to scope (must be a package list, not a file).
- `make test-go-tsh` — tsh tests (separate target because of build tags).
- `make test-api` — `api/` module tests.
- Single test: `go test -tags "..." -run TestName ./lib/auth/...`. Match the tags from the Makefile target (`PAM_TAG`, `RDPCLIENT_TAG`, etc.) when the test depends on tagged code.
- `pnpm test` — Jest for the JS workspaces; `pnpm test -- web/packages/teleport/src/...` to scope.
- `pnpm type-check` — TS build across all workspaces.

Lint / format:
- `make lint` — runs `lint-api`, `lint-go`, `lint-kube-agent-updater`, `lint-tools`, `lint-protos`, `lint-no-actions`. Each is also a standalone target.
- `make lint-go GO_LINT_FLAGS=--new` — only lint diff-relative changes (much faster).
- `make fix-imports/host` — runs `gci` with the project's import grouping (stdlib / default / `github.com/gravitational/teleport` / integrations).
- `pnpm lint` (oxlint + oxfmt check), `pnpm format` (oxfmt write).

Proto regeneration:
- `make grpc` — regenerates Go/TS gRPC stubs in the build container. Run this whenever any `*.proto` under `proto/` or `api/proto/` changes.
- `make grpc/host` — same, but on the host (requires the toolchain documented in `build.assets/`).
- `make protos-up-to-date` in CI verifies generated files were committed.

## Architecture notes worth knowing before editing

- **Service composition.** `lib/service/service.go` is the supervisor that boots the auth, proxy, ssh, kube, db, app, desktop, discovery, etc. services in one process based on config. Most subsystems hang off `lib/service/`. Per-protocol server logic lives in `lib/srv/...`.
- **Auth core.** `lib/auth/auth.Server` is the authority. Extension seams follow the pattern `Set<Subsystem>Service(svc <Subsystem>Service)` — used today for OIDC, SAML, release service, etc. Audit events are emitted via `a.emitter.EmitAuditEvent(ctx, ...)` with codes from `lib/events`. RBAC + token issuance go through `authz` and `lib/auth/keystore`.
- **Backend / cache.** Resources are persisted via `lib/backend/...` (etcd, dynamo, firestore, postgres, sqlite, in-memory). `lib/cache/` is the read-through cache that fans out watch events to other services. New resource types need backend marshalers, cache wiring, RBAC verbs, and proto definitions in lockstep.
- **API module.** `api/` is consumer-facing and has stricter compatibility rules than the rest of the tree. Don't reach into `lib/...` from `api/...`. The Go client (`api/client`) is what `tctl` / `tsh` / external integrations use.
- **Web HTTP layer.** `lib/web/apiserver.go` is the central router; per-feature handlers live in sibling files. The Web UI talks to it; the Web UI itself lives in `web/packages/teleport`.
- **Modules / feature gating.** `lib/modules/Modules.Features()` is the single source of truth for "is feature X enabled". Build type (`BuildOSS|BuildEnterprise|BuildCommunity`) is set at link time via `lib/modules.teleportBuildType` ldflag. The default `defaultModules` returns `IsOSSBuild() == true`.
- **`e/` submodule.** Empty in this fork. Don't try to `git submodule update` it (no access). If a Go package is missing, the symbol probably lived in `e/` and needs to be re-implemented OSS-side here.

## Conventions worth respecting

- Errors are wrapped with `github.com/gravitational/trace` (`trace.Wrap`, typed errors like `&trace.AccessDeniedError{...}`). Match this; don't switch to `fmt.Errorf` / `errors.Is` in existing files.
- Logging uses `slog` (e.g., `a.logger.WarnContext(ctx, "...", "error", err)`).
- Audit events: emit via `EmitAuditEvent` with metadata + a code from `lib/events` constants. Failure to emit is a warning, not a hard error.
- Generated code (`gen/`, `api/gen/`, `*.pb.go`, `*_pb.ts`) is committed; never hand-edit it. Regenerate with `make grpc` and commit the diff in the same PR.
- AGPL header at the top of every Go source file. New files need the same boilerplate.
