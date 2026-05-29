# syntax=docker/dockerfile:1.6
#
# Production multi-arch (amd64+arm64) image for the Shoplive Teleport fork.
#
# Builder stage installs the full toolchain (Go + Node + Rust) — identical to
# dev/Dockerfile so dev and prod builds share the same recipe. The runtime is
# a distroless nonroot image carrying only the three teleport binaries (no
# shell, no package manager, no test users, no dev CA).
#
# Built via `docker buildx build --platform linux/amd64,linux/arm64 ...`.
# Builder runs under QEMU emulation on the non-native arch; first build is
# slow, BuildKit cache mounts + GHA cache amortize subsequent rebuilds.

FROM golang:1.25-bookworm AS builder

# TARGETARCH is injected by BuildKit for each platform leg (amd64 / arm64).
# Used below to give each leg its own Go build-cache bucket so parallel
# multi-arch builds cannot corrupt each other's cache.
ARG TARGETARCH

ENV RUSTUP_HOME=/usr/local/rustup \
    CARGO_HOME=/usr/local/cargo \
    PATH=/usr/local/cargo/bin:$PATH

# Toolchain layered in one step so apt cache is purged in the same layer.
# Versions pinned to match dev/Dockerfile — keep these in sync when bumping.
RUN apt-get update && apt-get install -y --no-install-recommends \
        build-essential \
        git \
        ca-certificates \
        curl \
        gnupg \
    && curl -fsSL https://deb.nodesource.com/setup_24.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && corepack enable \
    && curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | \
        sh -s -- -y --no-modify-path --profile minimal --default-toolchain 1.94.0 \
    && rustup target add wasm32-unknown-unknown \
    && curl -L --proto '=https' --tlsv1.2 -sSf \
        https://raw.githubusercontent.com/cargo-bins/cargo-binstall/main/install-from-binstall-release.sh | bash \
    && cargo binstall --no-confirm wasm-bindgen-cli@0.2.116 wasm-opt@0.116.1 \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src

# Pre-pull Go modules so source edits don't reinvoke `go mod download`.
COPY go.mod go.sum ./
COPY api/go.mod api/go.sum ./api/
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# Build pipeline (orchestrated by the upstream Makefile):
#   1. ensure-webassets  →  pnpm install + pnpm build-ui-oss → webassets/teleport
#   2. go build with -tags webassets_embed → binaries embed the Web UI
#
# Trims:
#   RDPCLIENT_SKIP_BUILD=1 — skip building the server-side Rust RDP client.
#   FIDO2=off              — no libfido2/U2F; TOTP via Keycloak + per-session
#                            MFA at the SSO layer suffices for prod.
RUN --mount=type=cache,id=go-build-${TARGETARCH},target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.local/share/pnpm \
    --mount=type=cache,target=/root/.cache/pnpm \
    make \
        RDPCLIENT_SKIP_BUILD=1 \
        FIDO2=off \
        build/teleport \
        build/tctl \
        build/tsh

# Distroless base — glibc + ca-certificates only. Public CA bundle handles
# HTTPS to a real IdP (Keycloak with a real cert) and to AWS / cluster APIs.
# Runs as the `nonroot` user (uid 65532).
#
# Runtime debian12 (glibc 2.36) is paired with builder bookworm (also glibc
# 2.36) so the binary's symbol version requirements stay compatible. Bumping
# the builder to a newer base (trixie / glibc 2.40) without also bumping the
# runtime breaks dynamic loading at startup — keep these two pinned together.
FROM gcr.io/distroless/base-debian12:nonroot

COPY --from=builder /src/build/teleport /usr/local/bin/teleport
COPY --from=builder /src/build/tctl     /usr/local/bin/tctl
COPY --from=builder /src/build/tsh      /usr/local/bin/tsh

# 3023 SSH proxy, 3024 reverse-tunnel, 3025 auth API, 3080 proxy web.
EXPOSE 3023 3024 3025 3080

ENTRYPOINT ["/usr/local/bin/teleport"]
CMD ["start", "--config=/etc/teleport/teleport.yaml"]
