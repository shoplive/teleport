---
name: add-test-resource
description: Use when the operator wants to add a new resource to the local dev cluster for testing — additional SSH node, a database, an app, or a new label/zone. Lays out the minimum file changes per resource type so testing reflects realistic shapes (RBAC labels, multi-zone, mixed roles).
---

# Adding a test resource to the dev cluster

The dev cluster has SSH nodes (`teleport-dev`, `node-1`, `node-2`) + the kind k8s cluster registered as a kube resource. Add new resources by following the matching template below.

## SSH node (most common)

**Use case:** test RBAC label scoping, multi-zone setups, alternative shells.

1. Create `dev/bootstrap/node-3.yaml`:

```yaml
version: v3
teleport:
  nodename: node-3
  data_dir: /var/lib/teleport
  auth_token: dev-static-shoplive-token
  proxy_server: teleport.shoplive.local:3080
  log:
    severity: INFO
    output: stderr

auth_service:
  enabled: no
proxy_service:
  enabled: no

ssh_service:
  enabled: yes
  labels:
    env: prod          # ← deliberately different so dev role doesn't see it
    role: web
    zone: c
```

2. Add the service to `dev/docker-compose.yml`, mirroring `node-1`:

```yaml
  node-3:
    image: shoplive/teleport-dev:latest
    container_name: shoplive-node-3
    command: ["start", "--config=/etc/teleport/node.yaml"]
    volumes:
      - ./bootstrap/node-3.yaml:/etc/teleport/node.yaml:ro
      - node3_data:/var/lib/teleport
    depends_on:
      - teleport
    networks:
      shoplive:
        aliases:
          - node-3.shoplive.local
    restart: unless-stopped
```

Plus the volume:
```yaml
volumes:
  ...
  node3_data:
```

3. Bring it up:

```sh
make up                       # picks up new service
make tsh ARGS="ls"            # node-3 should appear
make tsh ARGS="ssh root@node-3"
```

4. (optional) Add a make target in `dev/Makefile`:

```make
.PHONY: ssh-node-3
ssh-node-3:
	$(DC) exec teleport tsh ssh root@node-3
```

## Test the RBAC by label

After adding `node-3` with `env: prod`:
- Login as `dev` (admin) → `tsh ls` shows it (admin has `node_labels: '*': '*'`).
- Login as `alice` (shoplive-dev role) → `tsh ls` does NOT show it (dev role has `node_labels: env: dev`).

This is the most useful thing this resource gives you — proof that label-based RBAC works as configured.

## Database (postgres)

**Use case:** test `tsh db ls` / `tsh db connect`. Heavier than nodes because Teleport requires TLS to the DB.

The setup is sketched in earlier conversations but not landed yet. Ping `shoplive-architect` for the full plan if needed. Outline:

1. Add a `postgres-test` container to `docker-compose.yml`.
2. Bake server cert + custom `pg_hba.conf` (TLS-only, `trust` auth) into a `Dockerfile.postgres`.
3. Generate a postgres-test cert in `make-pki.sh` (sign with the existing dev CA).
4. Add `db_service` block to `dev/teleport.yaml`.
5. Add `dev/bootstrap/db-shoplive.yaml` (kind: db, protocol: postgres, uri: `postgres-test.shoplive.local:5432`, `tls.mode: verify-ca`).
6. Update roles with `db_labels`, `db_users`, `db_names`.
7. Bootstrap with `make tctl ARGS="create -f /etc/teleport/bootstrap/db-shoplive.yaml --force"`.

Verify: `make tsh ARGS="db ls"` → `make tsh ARGS="db connect shoplive-postgres --db-user=dev --db-name=shoplive"`.

## Web app

**Use case:** test the application access proxy.

1. Add a service to `docker-compose.yml` that runs a tiny HTTP echo server (e.g. `nginx:alpine` with a trivial config).
2. Add `app_service` to `dev/teleport.yaml`:

```yaml
app_service:
  enabled: yes
  apps:
    - name: shoplive-echo
      uri: http://echo.shoplive.local:8080
      labels:
        env: dev
```

3. `make rebuild`.

`make tsh ARGS="app login shoplive-echo"` then `tsh app connect shoplive-echo` (or hit it via the Web UI).

## Kubernetes cluster

Already wired — see `dev/scripts/kind-up.sh`. To add a SECOND kind cluster:

1. Copy `dev/kind/cluster.yaml` → `cluster-2.yaml`, change `name: shoplive-kind-2`.
2. Copy `dev/kind/teleport-agent.yaml.tmpl` → `teleport-agent-2.yaml.tmpl`, change `kube_cluster_name: shoplive-kind-2` and adjust labels.
3. Adapt `dev/scripts/kind-up.sh` to take a cluster name argument, OR add a `kind-up-2.sh`.

Effort: ~30 min.

## Don't

- Don't reuse a resource name (`node-1`) and expect it to work — Teleport's identity is rooted in nodename + UUID, and reusing names confuses the inventory.
- Don't skip the labels — bare resources are useless for testing RBAC.
- Don't put `env: dev` on every new resource. The whole point of adding more is having mixed labels so `shoplive-dev` role can be tested for what it CAN'T see.
- Don't expose new ports to the macOS host unless you actually need to. Teleport routes through the proxy; direct access is a leak.
- Don't forget the volume entry in compose. Without it, the named volume isn't created and the container fails on first boot with a permissions/state error.

## After adding a resource

```sh
make tctl ARGS="get nodes"           # or kube_clusters / databases / apps
make logs | grep -i "registered"     # confirms the agent joined
make oidc-login                      # refresh cert with new RBAC reach
make tsh ARGS="ls"                   # see what your role exposes
```
