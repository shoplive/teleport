# Shoplive Teleport — local dev cluster (containers only).
#
# No Go toolchain is required on the host. The teleport binary is built inside
# a golang container (see ../dev/Dockerfile) and the whole stack runs via
# docker compose.
#
#   make up        first-time bring-up (build image + start everything)
#   make rebuild   edit Go code -> rebuild image -> restart teleport (data preserved)
#   make hot-reload  rebuild on every change via Compose's watch + auto-restart
#   make down      stop everything (volumes wiped)
#   make tctl ARGS="..."   run tctl inside the teleport container
#   make tsh  ARGS="..."   run tsh  inside the teleport container

.DEFAULT_GOAL := help

REPO_ROOT := $(abspath $(CURDIR)/..)
PROXY     := teleport.shoplive.local:3080
KC        := keycloak.shoplive.local:8443

# tsh inside the container resolves teleport.shoplive.local via Docker DNS to
# the teleport service itself; cert SAN includes that name so TLS validates.
TSH_PROXY := teleport.shoplive.local:3080

# Pinned Go image; mirrors build.assets/versions.mk's GOLANG_VERSION.
GO_IMAGE  := golang:1.25-bookworm

# Persistent caches for `make test` so subsequent runs reuse Go's build/mod cache.
GO_BUILD_CACHE := shoplive-go-build-cache
GO_MOD_CACHE   := shoplive-go-mod-cache

DC := docker compose

.PHONY: help
help:
	@awk 'BEGIN{FS=":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: up
up: preflight pki build compose-up wait-keycloak wait-teleport bootstrap-admin ## Build + bring up the whole stack
	@echo
	@echo "  Teleport Web UI:    https://$(PROXY)"
	@echo "  Keycloak admin:     https://$(KC)        (admin / admin)"
	@echo "  Keycloak end-user:  https://$(KC)/realms/shoplive/account  (dev / dev, alice / alice)"
	@echo
	@echo "  /etc/hosts must contain:  127.0.0.1   keycloak.shoplive.local  teleport.shoplive.local"
	@echo "  Trust the dev root CA in macOS keychain so the browser stops warning:"
	@echo "    sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain $(CURDIR)/pki/ca/ca.crt"
	@echo
	@echo "  Set the admin password via the URL printed above, then log in:"
	@echo "    make login"
	@echo "    make tsh ARGS=\"ls\""
	@echo
	@echo "  Iterate on teleport: edit Go code, then 'make rebuild' (or 'make hot-reload')."

.PHONY: rebuild
rebuild: build ## Rebuild teleport image and restart teleport service (state in named volume preserved)
	$(DC) up -d --no-deps --force-recreate teleport
	@./scripts/wait-teleport.sh
	@echo "rebuild done — proxy at https://$(PROXY)"

.PHONY: down
down: ## Stop everything (state PRESERVED — admin password/TOTP, KC users survive)
	$(DC) down

.PHONY: destroy
destroy: ## Stop and wipe all state (Teleport DB + Keycloak DB)
	$(DC) down -v

.PHONY: reset
reset: destroy ## Wipe everything and bring up fresh
	@$(MAKE) --no-print-directory up

.PHONY: status
status: ## Show component status
	@printf "Keycloak:  "; curl -skf -o /dev/null https://$(KC)/realms/shoplive/.well-known/openid-configuration && echo "ready" || echo "down"
	@printf "Teleport:  "; curl -skf -o /dev/null https://$(PROXY)/webapi/ping && echo "ready" || echo "down"
	@$(DC) ps

.PHONY: logs
logs: ## Tail teleport logs
	$(DC) logs -f teleport

.PHONY: kc-logs
kc-logs: ## Tail Keycloak logs
	$(DC) logs -f keycloak

.PHONY: hot-reload
hot-reload: build compose-up wait-keycloak ## Rebuild + restart on every edit (BuildKit cache makes loops ~10–30s)
	$(DC) watch teleport

.PHONY: pki
pki: ## Generate dev root CA + per-service TLS certs (idempotent)
	@./scripts/make-pki.sh

.PHONY: trust-ca
trust-ca: ## Add the dev root CA to the macOS system keychain (sudo prompt)
	sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain $(CURDIR)/pki/ca/ca.crt

.PHONY: untrust-ca
untrust-ca: ## Remove the dev root CA from the macOS system keychain
	sudo security delete-certificate -c "Shoplive Dev Root CA" /Library/Keychains/System.keychain

.PHONY: build
build: pki ## Build the teleport image (BuildKit cache mounts speed up incrementals)
	$(DC) build teleport

.PHONY: compose-up
compose-up:
	$(DC) up -d

.PHONY: wait-keycloak
wait-keycloak:
	@./scripts/wait-keycloak.sh

.PHONY: wait-teleport
wait-teleport:
	@./scripts/wait-teleport.sh

.PHONY: bootstrap-admin
bootstrap-admin:
	@./scripts/bootstrap-admin.sh

.PHONY: bootstrap-oidc
bootstrap-oidc: ## Apply / update the Keycloak OIDC connector (idempotent via --force)
	$(DC) exec -T teleport tctl -c /etc/teleport/teleport.yaml \
		create -f /etc/teleport/bootstrap/oidc-connector.yaml --force

.PHONY: bootstrap-roles
bootstrap-roles: ## Apply / update the Shoplive admin + dev roles
	$(DC) exec -T teleport tctl -c /etc/teleport/teleport.yaml \
		create -f /etc/teleport/bootstrap/admin-role.yaml --force
	$(DC) exec -T teleport tctl -c /etc/teleport/teleport.yaml \
		create -f /etc/teleport/bootstrap/dev-role.yaml --force

.PHONY: kind-up
kind-up: ## Spin up a kind k8s cluster + teleport-kube-agent pod (registers as 'shoplive-kind')
	@./scripts/kind-up.sh

.PHONY: kind-down
kind-down: ## Tear down the kind cluster (cleans up the kube_cluster resource too)
	@./scripts/kind-down.sh

.PHONY: kube-ls
kube-ls: ## tsh kube ls
	$(DC) exec teleport tsh kube ls

.PHONY: kube-login
kube-login: ## tsh kube login shoplive-kind
	$(DC) exec teleport tsh kube login shoplive-kind

.PHONY: tctl
tctl: ## Run tctl inside the teleport container, e.g. `make tctl ARGS="get users"`
	@$(DC) exec teleport tctl -c /etc/teleport/teleport.yaml $(ARGS)

.PHONY: tsh
tsh: ## Run tsh inside the teleport container, e.g. `make tsh ARGS="ls"`
	@$(DC) exec teleport tsh $(ARGS)

.PHONY: login
login: ## tsh login as admin (after admin password is set in the bootstrap URL)
	$(DC) exec teleport tsh login --proxy=$(TSH_PROXY) --user=admin

.PHONY: oidc-login
oidc-login: ## tsh login via the Keycloak OIDC connector
	$(DC) exec teleport tsh login \
		--proxy=$(TSH_PROXY) --auth=keycloak \
		--bind-addr=0.0.0.0:38000 \
		--callback=http://localhost:38000

.PHONY: shell
shell: ## Drop into a bash shell inside the teleport container
	$(DC) exec teleport bash

.PHONY: ls
ls: ## tsh ls — list registered nodes (run after make login / make oidc-login)
	$(DC) exec teleport tsh ls

.PHONY: ssh-node-1
ssh-node-1: ## tsh ssh root@node-1 — opens a shell in the node-1 container
	$(DC) exec teleport tsh ssh root@node-1

.PHONY: ssh-node-2
ssh-node-2: ## tsh ssh root@node-2 — opens a shell in the node-2 container
	$(DC) exec teleport tsh ssh root@node-2

.PHONY: node-logs
node-logs: ## Tail logs for node-1 and node-2
	$(DC) logs -f node-1 node-2

.PHONY: hostbin
hostbin: ## Print shell snippets to expose `tsh`/`tctl` on the host (alias or PATH)
	@echo "# Add ./bin to PATH (recommended):"
	@echo "  export PATH=\"$(CURDIR)/bin:\$$PATH\""
	@echo
	@echo "# Or alias them individually:"
	@echo "  alias tsh=\"$(CURDIR)/bin/tsh\""
	@echo "  alias tctl=\"$(CURDIR)/bin/tctl\""
	@echo
	@echo "Append to ~/.zshrc to make persistent."

# ----- Test targets (run go test inside a container; no host Go needed) -----

GOFLAGS ?= -count=1
PKG     ?= ./lib/auth/oidc/...

.PHONY: test
test: ## Run `go test` inside a builder container. Override: `make test PKG=./lib/auth/...` `GOFLAGS="-run TestX -v"`
	docker run --rm -t \
		-v $(REPO_ROOT):/src -w /src \
		-v $(GO_BUILD_CACHE):/root/.cache/go-build \
		-v $(GO_MOD_CACHE):/go/pkg/mod \
		$(GO_IMAGE) \
		go test $(GOFLAGS) $(PKG)

.PHONY: vet
vet: ## go vet the OIDC packages (override PKG to widen)
	docker run --rm -t \
		-v $(REPO_ROOT):/src -w /src \
		-v $(GO_BUILD_CACHE):/root/.cache/go-build \
		-v $(GO_MOD_CACHE):/go/pkg/mod \
		$(GO_IMAGE) \
		go vet $(PKG)

.PHONY: go
go: ## Run an arbitrary go command in the builder image, e.g. `make go ARGS="mod tidy"`
	docker run --rm -t \
		-v $(REPO_ROOT):/src -w /src \
		-v $(GO_BUILD_CACHE):/root/.cache/go-build \
		-v $(GO_MOD_CACHE):/go/pkg/mod \
		$(GO_IMAGE) \
		go $(ARGS)

.PHONY: preflight
preflight:
	@command -v docker >/dev/null  || { echo "ERROR: docker not found"; exit 1; }
	@docker compose version >/dev/null 2>&1 || { echo "ERROR: 'docker compose' v2 not available"; exit 1; }
