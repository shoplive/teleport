# UPSTREAM.md

**Fork maintenance — pulling new releases from `gravitational/teleport`.**

This is a private Shoplive fork. We don't merge back to upstream. We do consume upstream releases (security fixes, version bumps, new features) by periodically rebasing or merging upstream into our work. This document is the playbook for that.

If you're starting fresh in a session, read this before touching the rebase. The rest of the conversation history is incidental; this is the source of truth.

## What we changed vs. upstream

Two categories matter for rebase risk. **New files** are zero-risk on rebase. **Modified files** are where conflicts happen.

### NEW (zero rebase risk)

These don't exist in upstream and won't conflict. Even if upstream adds something at the same path later, that's a content conflict — caught explicitly during rebase.

```
lib/auth/oidc/                                  # in-house OIDC RP package
  ├── service.go
  ├── discovery.go
  ├── callback.go
  ├── claims.go
  └── *_test.go
lib/web/oidc.go                                 # proxy-side OIDC HTTP handlers
dev/                                            # local dev cluster (containers, PKI, kind, scripts)
.claude/                                        # Claude Code agents + skills (fork-only tooling)
CLAUDE.md                                       # fork-level engineering doc
SKILLS.md                                       # operator profile + system context
UPSTREAM.md                                     # this file
```

### MODIFIED (rebase risk surface — the only files to watch)

These already exist upstream and we patched them. Each rebase needs careful merge here.

| File | Why we patched it |
|---|---|
| `lib/modules/modules.go` | Add `entitlements.OIDC: {Enabled: true}` to `defaultModules.Features()` so OSS bypasses the four "OIDC is only available in Teleport Enterprise" gates in `lib/auth/auth_with_roles.go`. |
| `lib/service/service.go` | Call `authServer.SetOIDCService(authoidc.New(...))` immediately after `auth.Init`. One block, one extra import (`authoidc`). |
| `lib/auth/apiserver.go` | Register `POST /:version/oidc/requests/validate` + `validateOIDCAuthCallback` handler. Auth-server side of the OIDC callback dance. |
| `lib/web/apiserver.go` | Register `/webapi/oidc/{login/web,login/console,callback}` proxy routes (mirrors github SSO routes). Plus `GET /webapi/oidc` for connector listing. |
| `lib/web/resources.go` | Add `getOIDCConnectorsHandle` + `getOIDCConnectors` + extend the `resourcesAPIGetter` interface with `GetOIDCConnectors`/`ListOIDCConnectors`. |
| `lib/web/ui/resource.go` | Add `NewOIDCConnectors` helper (mirrors `NewGithubConnectors`). |
| `web/packages/teleport/src/AuthConnectors/AuthConnectors.tsx` | Fetch both github + oidc and merge into the items list. |
| `web/packages/teleport/src/AuthConnectors/ConnectorList/ConnectorList.tsx` | OIDC tiles read-only (no Edit/Delete) + GitHub placeholder when not configured. |
| `web/packages/teleport/src/AuthConnectors/ssoIcons/getSsoIcon.tsx` | Map `keycloak` connector name to the `openid` ResourceIcon. |
| `web/packages/teleport/src/config.ts` | Add `oidcConnectorsPath` + `getOIDCConnectorsUrl`. |
| `web/packages/teleport/src/services/resources/resource.ts` | Add `fetchOIDCConnectors()`. |
| `AGENTS.md` | Append Shoplive-fork-specific review checklist below upstream's. |

That's it. Twelve files. **If a rebase conflict happens outside this list, double-check — something has drifted.**

## Branch / remote convention

Set up once per checkout:

```sh
git remote add upstream https://github.com/gravitational/teleport.git
git fetch upstream --tags
```

Branch model:

- `master` — our integration branch. Tracks our origin's master, contains upstream + Shoplive patches.
- `shoplive/<feature>` — feature branches off `master` (or off the worktree's branch). Squash-merge or rebase-merge into `master`.
- `vendor-upstream` — short-lived branch used during a rebase round. Created from `master`, rebased onto `upstream/v<X.Y>`, force-pushed onto `master` after verification.

Don't push force to public remotes other than the dedicated rebase branch. Coordinate.

## Worktree workflow (always)

We do non-trivial feature work and rebases in **git worktrees**, not in-place. The main checkout stays clean for fast pulls and emergency hotfixes.

```sh
# One-time: create a sibling directory for worktrees
mkdir -p ~/Workspace/shoplive/teleport-worktrees

# For a feature
git worktree add ~/Workspace/shoplive/teleport-worktrees/feat-X -b shoplive/feat-X

# For an upstream rebase round
git worktree add ~/Workspace/shoplive/teleport-worktrees/rebase-v<X.Y> -b vendor-upstream upstream/v<X.Y>

# When done
git worktree remove ~/Workspace/shoplive/teleport-worktrees/feat-X
```

Inside Claude Code, sub-agents that make non-trivial changes should be spawned with `isolation: "worktree"` so we never have two pieces of work fighting over the working tree.

## Rebase playbook

### 1. Decide the target

Look at upstream tags and pick one (typically the latest minor; don't chase rc / prealpha unless you really want bleeding-edge):

```sh
git fetch upstream --tags
git tag -l 'v*' --sort=-v:refname | head -10
```

Pick something like `v19.0.0` and use it consistently as `<TARGET>` below.

### 2. Worktree the rebase round

```sh
TARGET=v19.0.0
git worktree add ~/Workspace/shoplive/teleport-worktrees/rebase-$TARGET -b vendor/$TARGET upstream/$TARGET
cd ~/Workspace/shoplive/teleport-worktrees/rebase-$TARGET
```

Now we have a clean checkout at upstream's tag.

### 3. Cherry-pick or rebase Shoplive commits

Two strategies, pick one:

**A. Rebase (preferred when our changes are commit-clean):**
```sh
git rebase --onto vendor/$TARGET <last-upstream-commit-we-rebased-from> master
```

**B. Cherry-pick the modified files explicitly** (use when our history is messy or the rebase has too many conflicts):
```sh
# from a clean rebase branch tracking upstream/$TARGET
git checkout master -- \
    AGENTS.md CLAUDE.md SKILLS.md UPSTREAM.md \
    .claude/ dev/ \
    lib/auth/oidc/ lib/web/oidc.go \
    lib/auth/apiserver.go \
    lib/modules/modules.go \
    lib/service/service.go \
    lib/web/apiserver.go \
    lib/web/resources.go \
    lib/web/ui/resource.go \
    web/packages/teleport/src/AuthConnectors/ \
    web/packages/teleport/src/services/resources/resource.ts \
    web/packages/teleport/src/config.ts
```

That's literally our entire diff. The new files (`lib/auth/oidc/`, `lib/web/oidc.go`, `dev/`, `.claude/`) come over verbatim. The 12 modified files are where conflicts happen.

### 4. Resolve conflicts (the 12-file shortlist)

Let `git rerere` remember resolutions across rounds:

```sh
git config rerere.enabled true
git config rerere.autoUpdate true
```

For each conflicting modified file, the strategy by file:

- **`lib/modules/modules.go`** — only adds one entry to a map literal. If upstream adds new entitlement keys nearby, just add ours back. Trivial.
- **`lib/service/service.go`** — one `authServer.SetOIDCService(...)` block + one import. The wiring point right after `auth.Init` is stable. Most conflicts here are upstream renaming `process.logger` or related — rare.
- **`lib/auth/apiserver.go`** — one route registration + one handler function. Both go in clearly-marked spots.
- **`lib/web/apiserver.go`** — three route registrations near the github routes. If upstream moved github routes, follow.
- **`lib/web/resources.go`** — interface entries + `getOIDCConnectorsHandle` near `getGithubConnectorsHandle`. Mirror upstream's pattern.
- **`lib/web/ui/resource.go`** — one helper function next to `NewGithubConnectors`. Painless.
- **Web UI files (5)** — TS conflicts most often when upstream rewrites the AuthConnectors page (rare). Keep our patches together near the GitHub equivalents.
- **`AGENTS.md`** — purely additive. Concat.

If a conflict is in something NOT in the table above, stop and investigate. Either upstream relocated something we depend on, or our diff drifted without this doc being updated.

### 5. Verify

Container-based, no host toolchain assumed:

```sh
cd dev

# 5a. PKI is per-checkout — regenerate
make pki

# 5b. Image rebuild — if go.mod / Cargo / package.json changed upstream,
#     this can take a few minutes (BuildKit caches help).
make rebuild

# 5c. Bring up clean
make destroy && make up

# 5d. SSO end-to-end smoke test
make bootstrap-oidc
make bootstrap-roles
make oidc-login           # should round-trip Keycloak → cert

# 5e. Unit tests
make test PKG=./lib/auth/oidc/...
```

Pass criteria:
- `make test` green
- Web UI loads at `https://teleport.shoplive.local:3080` without TLS warnings (CA still in trust store)
- "Login with Keycloak" button visible on `/web/login`
- After login, `make tctl ARGS="get user/dev"` shows `created_by.connector.type=oidc` and the right roles
- `make tsh ARGS="ls"` shows the dev nodes

If any step fails, treat the rebase as not done. Don't merge.

### 6. Promote

```sh
# After verification, in the rebase worktree:
git push origin vendor/$TARGET
# Open a PR (or fast-forward master) — your team's call
```

Tag the merge so the next round has a starting point:
```sh
git tag shoplive/rebased-$TARGET
git push origin shoplive/rebased-$TARGET
```

This tag is what the NEXT rebase uses as `<last-upstream-commit-we-rebased-from>` in step 3A.

### 7. Update this manifest if needed

If the rebase added new modifications to upstream files (e.g. you had to patch a third file because upstream changed an interface our code uses), update the table at the top of this file. The manifest is only useful if it's accurate.

## Conflict patterns we've seen / expect

- **Entitlements map gets reordered upstream** — they sometimes alphabetize. Just keep the OIDC entry, move it where new alphabetization wants.
- **`auth.Init` signature changes** — has happened. The fix is one or two extra args; SetOIDCService is unaffected.
- **Github SSO routes move** — when github SSO is reorganized, our OIDC routes need to follow because they sit next to them by convention.
- **Web UI file restructuring** — React refactors are the messiest. If `AuthConnectors.tsx` is rewritten, port our merge-fetch logic to wherever it landed.
- **OIDCConnector interface gets new methods** — if `api/types/oidc.go`'s interface grows, our `lib/auth/oidc/service.go` may need to satisfy it. The compile-time `var _ auth.OIDCService = (*Service)(nil)` assertion catches this immediately.

## Don't

- **Don't try to upstream our changes.** This is a private fork. Don't waste cycles making patches "upstream-mergeable."
- **Don't squash unrelated upstream commits.** Keep upstream history intact so blame works.
- **Don't rebase without testing the SSO flow end-to-end.** Compile success ≠ working SSO. The five-minute `make oidc-login` round-trip is the only honest check.
- **Don't pull in `e/` (the enterprise submodule).** It stays empty. Our OIDC implementation is the substitute. If `e_imports.go` references something we don't have, lint errors are expected — let them stay.
- **Don't merge upstream master casually.** Pick a tagged release, rebase, verify, tag the merge. Continuous merging makes provenance unreadable.

## When this doc gets out of date

If the file manifest above doesn't match `git diff --name-only` against the last `shoplive/rebased-*` tag, the manifest is wrong, not the code. Update the manifest.
