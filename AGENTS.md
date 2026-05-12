# AGENTS.md

## Review Guidelines
- Focus only on critical security, reliability, performance, and scalability issues.
- Ignore style, performance micro-optimizations, and readability nits unless they are tied to a significant failure

### What to Look For
- Authentication/authorization bypasses
- Secret leakage, unsafe logging, or credential exposure
- Unsafe defaults in security-sensitive areas
- Injection risks (SQL, command, template, path traversal, SSRF)
- Insecure crypto usage or key handling
- Privilege escalation or sandbox escapes
- Data corruption, durability failures, or irreversible loss scenarios
- Concurrency hazards that can cause outages or data races
- Reliability regressions: crash loops, panics, deadlocks, unbounded retries

### Documentation

When you are looking at a given product area find the relevant documentation in the docs/ directory to ensure you understand the context in which the code is used.

---

## Shoplive fork — extra review focus

This fork's reason to exist is **self-hosted OIDC SSO** (upstream gates SSO behind the closed `e/` submodule). Keep upstream's review checklist above, then layer the items below for any change that touches auth, identity, or `tsh`.

### Threat surface specific to this fork

- **OIDC service registration.** `Server.SetOIDCService` (`lib/auth/auth.go`) is the single seam for the SSO implementation. Any code path that registers a service must:
  - Run only on the auth process (not proxy/agent), and only after `Modules` is finalized.
  - Reject empty/unsigned tokens, validate `iss`/`aud`/`exp`/`nonce`, and verify against the IdP's published JWKs (no static-key fallback).
  - Never log raw `id_token`s, refresh tokens, client secrets, or `state`/`code` query values. Treat the full `url.Values` passed to `ValidateOIDCAuthCallback` as sensitive.
- **Connector CRUD.** `UpsertOIDCConnector`/`DeleteOIDCConnector` are already OSS-side and emit audit events (`OIDCConnectorCreatedEvent`, `OIDCConnectorDeletedEvent`). New SSO-related mutations must emit equivalent events with a stable code from `lib/events`.
- **Build-type spoofing.** Don't gate Shoplive's OIDC behind `IsEnterpriseBuild()` or `BuildEnterprise` — the right gate is "is `OIDCService` registered?". Flag any PR that flips `teleportBuildType` to `ent` without a license/entitlement story.
- **`tsh` SSO login.** The CLI hits `/webapi/oidc/login/console` (or equivalent) and waits on a local callback. Review changes to:
  - The local callback listener's port range / loopback binding (don't open `0.0.0.0`).
  - The PKCE verifier and `state` randomness (must be CSPRNG-derived, ≥128 bits).
  - Browser-launch behavior — `tsh` prints the URL when the browser can't open; that fallback must remain so headless boxes still work.
- **Dual IdP chain.** Keycloak fronts Google Identity (federation) and adds TOTP. Don't conflate "Keycloak `acr`/`amr` claims" with Teleport's MFA evidence — TOTP enforced by Keycloak does not by itself satisfy Teleport's per-session MFA. Confirm `MFAVerified` is set only when an Teleport-issued challenge round-trips.

### Deployment-shape assumptions to check

- Auth + proxy run on **EKS** (multi-replica). Reject anything that assumes a single-writer auth process unless it goes through the existing leader-election / cluster-state primitives in `lib/backend` or `lib/service`.
- Agents run on **EC2 hosts** (long-lived). Joining is via Teleport join tokens / IAM joining (`lib/auth/join_*.go`) — flag any change that bypasses join-method validation or weakens token TTLs.

### What to push back on

- "Just call out to `e/`" — the submodule is unavailable; suggest the OSS extension point instead.
- "Disable MFA / TOTP for X edge case" — the cluster's auth preference is `webauthn` + `otp` with TOTP required at the IdP; never recommend setting `second_factor: off` or removing the OTP factor in `cluster_auth_preference`.
- Storing OIDC client secrets in `OIDCConnector.Spec` plaintext when the resource will be persisted via the dynamic config flow — confirm the secret is read from a `secret_ref` or env, not committed.

### Cross-references
- Engineering / build / repo layout: `CLAUDE.md`.
- Operator profile, deployment plan, auth chain: `SKILLS.md`.
