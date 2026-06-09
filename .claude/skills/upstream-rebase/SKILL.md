---
name: upstream-rebase
description: Use when the operator wants to rebase the Shoplive fork onto a new upstream `gravitational/teleport` release — triggers like "rebase against v19.0", "pull in upstream", "upgrade teleport version", "vendor upstream X.Y", or any version-bump request. Walks through the playbook in `UPSTREAM.md`, surfaces the 12 modified-file conflict surface, and runs the SSO verification gate so the operator knows when it's actually done.
---

# Upstream rebase — Shoplive fork

The full source-of-truth playbook lives in **`UPSTREAM.md`** at the repo root. This skill is the operational walk-through — read `UPSTREAM.md` first; come back here for the runbook flow.

## Pre-flight

```sh
# 1. Make sure upstream remote is set
git remote -v | grep upstream || git remote add upstream https://github.com/gravitational/teleport.git
git fetch upstream --tags

# 2. Pick the target (latest stable minor; avoid rc/prealpha unless asked)
git tag -l 'v*' --sort=-v:refname | grep -v -E 'rc|alpha|beta' | head -5

# 3. Confirm where we last rebased from
git tag -l 'shoplive/rebased-*' --sort=-v:refname | head -3
```

Pick `<TARGET>` (e.g. `v19.0.0`) and `<PREVIOUS>` (the last `shoplive/rebased-*` or initial fork point). Use these throughout.

## Worktree the round

We never rebase in-place in the main checkout.

```sh
TARGET=v19.0.0   # adjust
git worktree add ~/Workspace/shoplive/teleport-worktrees/rebase-$TARGET -b vendor/$TARGET upstream/$TARGET
cd ~/Workspace/shoplive/teleport-worktrees/rebase-$TARGET

# Enable rerere across rounds
git config rerere.enabled true
git config rerere.autoUpdate true
```

## Bring our changes over

Two strategies — `UPSTREAM.md` describes both. **Strategy A (rebase commits)** is preferred when our history is clean. **Strategy B (cherry-pick by file)** is the fallback.

**Strategy A:**
```sh
PREVIOUS=shoplive/rebased-v18.5.0   # or wherever the last vendor sync was
git rebase --onto vendor/$TARGET $PREVIOUS master
```

**Strategy B (file-explicit, lower-fidelity but bulletproof):**
```sh
# from the new vendor/$TARGET branch, pull just our diff
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

If using B, `git status` will show the modifications and you commit them as one "Shoplive vendor sync" commit. History is cleaner but you lose intermediate intent.

## Resolve conflicts — the shortlist

Conflicts only happen in the 12 modified files. Anywhere else means upstream moved something we depend on. Per-file strategies are in `UPSTREAM.md`. Quick map:

- **modules.go** — re-add the OIDC entitlement entry to whatever map literal upstream now has.
- **service.go** — keep the `authServer.SetOIDCService(authoidc.New(...))` block right after `auth.Init`. Re-add the `authoidc` import.
- **lib/auth/apiserver.go** — keep our `oidc/requests/validate` route + handler.
- **lib/web/apiserver.go** — keep our three `/webapi/oidc/...` routes; if upstream moved github routes, our routes follow.
- **lib/web/resources.go** + **ui/resource.go** — re-add `getOIDCConnectorsHandle`, `NewOIDCConnectors`, interface entries.
- **5 Web UI files** — re-apply our diff piece by piece. If upstream rewrote `AuthConnectors.tsx`, port the github+oidc merge-fetch into the new shape.
- **AGENTS.md** — purely additive; concat.

If a conflict is in a file NOT on this list, stop and tell the operator. Either upstream restructured something we depend on, or our diff drifted. Either way it warrants a moment of attention, not a reflex resolution.

## Verify (the gate that matters)

This is the only check that proves the rebase actually works. Compile success is necessary but not sufficient.

```sh
cd dev

# Regenerate dev PKI (if it was deleted in the worktree).
make pki

# Cold rebuild — minutes if go.mod / package.json moved.
make rebuild

# Clean state
make destroy && make up

# Bootstrap roles + connector
make bootstrap-roles
make bootstrap-oidc

# SSO smoke test
make oidc-login
# → browser to Keycloak → login dev/dev + TOTP → callback → cert issued
make tsh ARGS="ls"
make tsh ARGS="ssh root@node-1"

# Unit tests
make test PKG=./lib/auth/oidc/...

# Web UI sanity
# → https://teleport.shoplive.local:3080 → "Auth Connectors" page → keycloak tile present, no errors
```

Pass criteria — all of these:
- `make test` green
- Web UI loads without TLS warnings (CA in keychain), shows OIDC connector tile
- `tsh login --auth=keycloak` round-trips end to end
- `tctl get user/dev` shows correct `created_by.connector.type=oidc` and roles from claims_to_roles
- `tsh ssh` works on node-1

If any of these fail, the rebase is NOT done. Tell the operator what failed; do not advance.

## Promote

```sh
# Push the vendor branch
git push origin vendor/$TARGET

# After review/merge to master, tag for the next round
git tag shoplive/rebased-$TARGET
git push origin shoplive/rebased-$TARGET
```

## Update UPSTREAM.md

If the rebase added new modifications (e.g. you had to patch a thirteenth file because upstream changed an interface), update the file manifest in `UPSTREAM.md`. The manifest is only useful if accurate. Forgetting this is the #1 failure mode of the doc.

## Tear down the worktree

```sh
cd ~/Workspace/shoplive/teleport
git worktree remove ~/Workspace/shoplive/teleport-worktrees/rebase-$TARGET
```

## Don't

- Don't merge `upstream/master` casually — pick tagged releases.
- Don't skip the SSO smoke test. The five minutes pay for themselves the first time you would have shipped a broken cert flow.
- Don't try to make our changes "upstream-mergeable" along the way. Private fork. Read `feedback_no_upstream_pr.md`.
- Don't rebase in-place in the main checkout. Use the worktree.
- Don't update `UPSTREAM.md`'s manifest if you didn't actually add or remove a modified file. Drift is worse than skew.
