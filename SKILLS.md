# SKILLS.md

Operator profile, target deployment, and auth-stack context for this fork. Read this before suggesting architecture-level changes — it tells you the assumptions the operator is working under.

## Operator profile

- 10-year Go developer. Default to advanced explanations: skip language basics, idiomatic-Go preambles, and "what is a goroutine"-level commentary. Talk about the seam, the contract, the failure mode.
- Comfortable with Teleport conceptually but is **building OIDC SSO in this fork from scratch** — assume context for upstream's enterprise OIDC implementation is missing.
- Korean-language collaboration. Code, identifiers, and committed text remain English; conversation can be Korean. Keep responses terse.

## Target deployment

```
            ┌──────────────────────────────────────────┐
            │              EKS (Kubernetes)            │
            │   teleport auth (HA) ──┐                 │
            │   teleport proxy (HA) ─┤  same cluster   │
            └──────────────────────┬─┴─────────────────┘
                                   │  reverse-tunnel /
                                   │  TLS routing
                                   ▼
                    ┌──────────────────────────┐
                    │   EC2 hosts (selective)  │
                    │   teleport agent installs │
                    └──────────────────────────┘

  Login flow:  user / tsh ──► proxy ──► auth ──► Keycloak ──► Google IdP
                                                        └── TOTP enforced
```

- **Teleport control plane on EKS.** Auth + proxy services. Multi-replica; persistence and locking go through Teleport's existing backends (`lib/backend/...`). Don't propose designs that assume single-writer auth.
- **Agents on EC2.** Only the hosts that need Teleport-mediated access run the agent. They join the cluster via Teleport join tokens — for AWS the natural fit is the IAM join method (`lib/auth/join_iam.go`). Static tokens are a fallback only.
- **No e/ submodule.** `e/` is empty in this checkout. Anything the OSS code stubs as "enterprise only" is in scope to implement here. See `CLAUDE.md` for the OIDC seam.

## Auth chain

```
Teleport (proxy/auth)
    │
    └─► OIDC: Keycloak  (sole RP that Teleport sees)
              │
              ├─► Federation: Google Identity Provider
              │     (primary user directory)
              │
              └─► Second factor: TOTP enforced by Keycloak
                    + Teleport per-session MFA on top
```

- **Keycloak is the only OIDC provider Teleport talks to.** Google IdP is federated *into* Keycloak, not registered as a second connector. This means the `OIDCConnector` resource has exactly one entry pointing at the Keycloak realm.
- **TOTP is enforced at Keycloak**, but Teleport's `cluster_auth_preference` still requires its own second factor for sensitive operations (`per_session_mfa: true`). Don't conflate the two — the IdP's `amr=otp` claim is *not* a substitute for Teleport's challenge round-trip.
- **`tsh` must use the same SSO.** The CLI flow is `tsh login --auth=<connector> --proxy=<proxy>` → opens browser → Keycloak → Google → TOTP → callback to `tsh`'s loopback listener → short-lived cert issued. Ensure any change to the OIDC implementation keeps this CLI path working (it shares the auth flow with the Web UI).
- **No long-lived credentials.** The whole point: certificates expire, sessions are recorded, MFA is required. Reject any suggestion that re-introduces password auth, long-TTL tokens, or shared SSH keys.

## Out-of-scope features (for now)

The fork is intentionally minimal. The following enterprise features stay disabled / unimplemented unless explicitly scoped in:

- SAML SSO (`lib/auth/saml.go` is the same kind of stub as OIDC — leave it stubbed).
- Access requests / Just-In-Time elevation workflows beyond what OSS already supports.
- Device Trust, Identity Governance, Access Monitoring, Identity Activity Center, Access Lists review.
- Hosted plugins, Okta integration, Identity Center.

If a task hints at these, pause and confirm scope before implementing.

## Working norms

- **Private fork, no upstream PRs.** Never propose sending changes back to `gravitational/teleport`. The fork diverges intentionally; don't constrain designs to be upstream-mergeable.
- **Periodic rebases against upstream tags.** Read `UPSTREAM.md` before doing or recommending anything rebase-related — it has the playbook and the 12-file manifest of modified upstream files.
- **Worktree workflow.** Non-trivial work happens in `git worktree` checkouts under `~/Workspace/shoplive/teleport-worktrees/`. Sub-agents that mutate multi-file changes prefer `isolation: "worktree"`.
- **Don't propose "use Teleport Enterprise"** as a fix path. The cost decision is upstream of any technical recommendation.
- **Stay in OSS surface.** When extending, add to `lib/...` and register via the existing `Set<X>Service` seams. Don't reintroduce a private submodule.
- **Touch generated code only via `make grpc` / `make grpc/host`.** Hand edits to `*.pb.go` get reverted by the next regen.
- **Match upstream conventions** (trace.Wrap, slog, audit-event emission, AGPL header) — see `CLAUDE.md`. Done for internal consistency, not for mergeability.
- **Brief is better than thorough.** Operator reads diffs, not paragraphs.
