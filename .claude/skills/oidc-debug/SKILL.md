---
name: oidc-debug
description: Use when SSO login is failing in the local dev cluster — error messages like "OIDC discovery failed", "tls: failed to verify certificate", "invalid_scope", "Missing parameter: code_challenge_method", "404 page not found / object not found", or generic "Unable to log in". Walks the operator through layered diagnostics in the right order so they don't waste time on the wrong layer.
---

# OIDC SSO failure debugging — Shoplive dev cluster

The SSO flow has eight layers, each with a typical failure mode. Don't reorder these — earlier layers gate later ones, and chasing a Phase-7 symptom while a Phase-2 thing is broken wastes time.

## 0. Always do first

```sh
cd dev
make status                              # are containers up?
make logs | tail -200 | grep -iE "oidc|sso|error|fail"
```

If `make status` shows anything down, fix that before going further.

## 1. /etc/hosts + CA trust

The OIDC `iss` claim must match between the IdP, the Teleport proxy, and the macOS browser. We use `keycloak.shoplive.local` and `teleport.shoplive.local` as common hostnames.

Verify:
```sh
grep "shoplive.local" /etc/hosts
# expected: 127.0.0.1   keycloak.shoplive.local  teleport.shoplive.local

# host can reach (port 443/8443 forwarded)
curl -kI https://keycloak.shoplive.local:8443/realms/shoplive/.well-known/openid-configuration
curl -kI https://teleport.shoplive.local:3080/webapi/ping
```

For host tsh / browser without `--insecure`:
```sh
make trust-ca       # one-time sudo prompt
```

If you regenerated `pki/`, you must `make destroy && make up` so KC reloads its server cert from the new CA. (Or at minimum `docker compose up -d --force-recreate keycloak`.)

## 2. Realm + client config in Keycloak

```sh
# Hit discovery, look at registered scopes
curl -sk https://keycloak.shoplive.local:8443/realms/shoplive/.well-known/openid-configuration | jq '.scopes_supported, .issuer'
```

If "scopes_supported" doesn't include the scopes our connector requests (`email profile groups`), the realm wasn't imported correctly. Re-import:

```sh
make destroy   # wipes postgres → realm re-import on next up
make up
```

KC realm JSON: `dev/keycloak/realm-shoplive.json`. Standard scopes are explicitly defined there (profile/email/roles/web-origins/groups/acr/basic). Don't trim.

## 3. Connector resource

```sh
make tctl ARGS="get oidc"
make tctl ARGS="get oidc/keycloak --with-secrets"
```

Re-apply with:
```sh
make bootstrap-oidc   # tctl create -f --force
```

Common gotchas:
- `issuer_url` must EXACTLY match KC's advertised `iss` — including scheme, port, trailing path. Mismatch → "iss claim mismatch" or signature failures.
- `redirect_url` must match a registered redirect URI in the KC client (the realm JSON has `https://teleport.shoplive.local:3080/v1/webapi/oidc/callback`).
- `pkce_mode: enabled` is required when KC client has `pkce.code.challenge.method=S256` set. Otherwise KC returns `Missing parameter: code_challenge_method`.

## 4. Auth-side route

`POST /v1/oidc/requests/validate` must be registered on the auth server. If the proxy logs say `404 page not found / object not found` from `ValidateOIDCAuthCallback`, this route is missing. Live in `lib/auth/apiserver.go`. Check it didn't get reverted.

## 5. Proxy-side routes

`/webapi/oidc/{login/web,login/console,callback}` must exist. If the browser hits `/v1/webapi/oidc/login/web` and gets `path not found`, these routes are missing. Live in `lib/web/oidc.go` registered from `lib/web/apiserver.go`.

## 6. modules entitlement

```sh
make logs | grep -i 'entitlements:<key:"OIDC"'
# expect: ... key:"OIDC" value:<enabled:true > ...
```

If `enabled:false`, `lib/modules/modules.go`'s `defaultModules.Features()` lost the OIDC entry. Without it the connector CRUD calls fail with "OIDC is only available in Teleport Enterprise".

## 7. The actual auth flow

Watch the logs in real time while you click "Login with Keycloak":
```sh
make logs | grep -iE "OIDC|SSO|user.login"
```

Expect the sequence (success path):
```
OIDC auth request created   ← service.go logs this with state, issuer, pkce
emitting audit event ... user.login ... method:oidc success:true
Redirecting to web browser  ← lib/web/oidc.go:142
```

Failure breakpoints by symptom:

- **"OIDC discovery failed"** → containers can't reach the issuer URL. Usually a hostname / network problem (revisit step 1).
- **"x509: certificate signed by unknown authority"** → containers don't trust the dev CA. The teleport image must include `dev/pki/ca/ca.crt`. `make rebuild` after CA regen.
- **"Invalid scopes: ..."** → realm doesn't have those scopes defined (revisit step 2).
- **"Missing parameter: code_challenge_method"** → connector `pkce_mode` not enabled but client requires PKCE.
- **"id_token signature failure"** / "iss claim mismatch" → KC's actual issuer (`KC_HOSTNAME` env) differs from our connector's `issuer_url`. Both must be `https://keycloak.shoplive.local:8443/realms/shoplive`.
- **"OIDC nonce mismatch"** → unlikely; would mean replay or a bug in `extractNonce` / `req.Type` handling.
- **"OIDC user mismatch"** (MFA) — IdP-authenticated user doesn't match the user who initiated MFA. Usually means the user logged in as the wrong identity in Keycloak.

## 8. Post-login state

```sh
make tctl ARGS="get user/<oidc-username>"
# spec.created_by.connector.type:oidc, oidc_identities present, roles assigned via claims_to_roles
make tctl ARGS="get audit-events" | tail
```

If user appears with empty roles, claims didn't match `claims_to_roles`. Check the user's groups in Keycloak admin console (`https://keycloak.shoplive.local:8443`, admin/admin) — they need to be in `teleport-admins` or `teleport-devs`.

## When all else fails

```sh
make destroy && make up && make bootstrap-oidc && make bootstrap-roles
make trust-ca   # if browser still complains
```

Most "weird" SSO breakage is downstream of stale state. A clean wipe is faster than chasing it. Just remember it nukes admin TOTP — re-register on next login.

## Don't do these

- Don't add `--insecure` flags everywhere — once it works without, leave it. `--insecure` masks cert issues and is a habit you don't want carrying into prod.
- Don't propose "use Teleport Enterprise" as a fix path. The whole fork exists to avoid that.
- Don't edit `lib/auth/oidc.go` (the interface stub). The implementation is in `lib/auth/oidc/` (the package).
