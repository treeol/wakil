# syntax=docker/dockerfile:1

# Stage 1a: Build kvr-server from the kvrust repository (pinned to a specific
# commit for supply-chain integrity — a compromised upstream repo would be
# built into every image. Bump the commit when updating kvr-server).
# Last verified: 2026-07-22, commit 72059cdb78b71a4408e5ef57aec2d30d1e12de8a
FROM rust:1-bookworm@sha256:77fac8b98f9f46062bb680b6d25d5bcaabfc400143952ebc572e924bcbedc3fa AS kvr-builder
WORKDIR /build
RUN git clone https://github.com/treeol/kvrust.git . \
    && git checkout 72059cdb78b71a4408e5ef57aec2d30d1e12de8a
RUN cargo build --release --bin server

# Sandbox runtime Go toolchain. go.mod (minimum 1.25.0) governs building
# wakil on the host/CI; this image provides the Go toolchain used inside the
# sandbox container by the agent (building user projects, gopls, etc.). It may
# be newer than the go.mod minimum — the Go toolchain is forward-compatible.
# See README "Requirements".
FROM golang:1.26-bookworm@sha256:1ecb7edf62a0408027bd5729dfd6b1b8766e578e8df93995b225dfd0944eb651 AS go-toolchain

FROM debian:bookworm-slim@sha256:96e378d7e6531ac9a15ad505478fcc2e69f371b10f5cdf87857c4b8188404716

# Copy the Go toolchain into /usr/local/go (same location as the official image).
COPY --from=go-toolchain /usr/local/go /usr/local/go

# Copy the pre-built kvr-server binary.
COPY --from=kvr-builder /build/target/release/server /usr/local/bin/kvr-server

# Single apt layer: bootstrap curl/gnupg, add NodeSource 20 LTS repo, the
# Docker apt repo (docker-cli + compose plugin), and the GitHub CLI apt repo,
# then install all dev tools, clean apt caches.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
           ca-certificates \
           curl \
           gnupg \
    && mkdir -p /etc/apt/keyrings \
    && curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key \
       | gpg --dearmor -o /etc/apt/keyrings/nodesource.gpg \
    && echo "deb [signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_20.x nodistro main" \
       > /etc/apt/sources.list.d/nodesource.list \
    && curl -fsSL https://download.docker.com/linux/debian/gpg \
       | gpg --dearmor -o /etc/apt/keyrings/docker.gpg \
    && echo "deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/debian bookworm stable" \
       > /etc/apt/sources.list.d/docker.list \
    && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
       -o /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && chmod go+r /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=amd64 signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
       > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
           git \
           jq \
           make \
           build-essential \
           python3 \
           python3-pip \
           python3-venv \
           nodejs \
           docker-ce-cli \
           docker-compose-plugin \
           openssh-client \
           procps \
           chromium \
           fonts-liberation \
           libasound2 \
           libatk-bridge2.0-0 \
           libatk1.0-0 \
           libatspi2.0-0 \
           libcups2 \
           libdbus-1-3 \
           libdrm2 \
           libgbm1 \
           libgtk-3-0 \
           libnss3 \
           libpango-1.0-0 \
           libx11-6 \
           libxcomposite1 \
           libxdamage1 \
           libxext6 \
           libxfixes3 \
           libxrandr2 \
           libxkbcommon0 \
           clangd \
           gh \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

# Backup copy of files needed post-tmpfs-mount, kept outside /etc. The
# sandbox hardening mounts --tmpfs=/etc at container start (see
# internal/exec/exec.go dockerHardeningArgs) for a writable /etc/passwd —
# but a tmpfs mount replaces the ENTIRE /etc directory with an empty
# filesystem, silently wiping everything apt installed there at build time:
#   - /etc/ssl/certs/ca-certificates.crt (apt-get install ca-certificates):
#     every TLS client (curl, git, the headless browser) fails cert
#     verification post-start — curl "error setting certificate file",
#     chromedp "context canceled" on navigation.
#   - /etc/chromium.d/* (apt-get install chromium): the chromium launcher
#     script sources every file in this directory; on an empty tmpfs the
#     glob doesn't expand and `.` (source) fails on the literal string,
#     aborting the launcher before Chromium even starts.
#   - /etc/alternatives/* (dpkg alternatives): Debian manages compiler and
#     utility symlinks (/usr/bin/cc, /usr/bin/c++, /usr/bin/awk, c89, c99)
#     via /usr/bin/<name> -> /etc/alternatives/<name> -> real binary. On an
#     empty tmpfs these all dangle, breaking cc/c++/awk and any build system
#     (cargo, node-gyp, configure) that defaults to cc.
# These backups are restored into the fresh tmpfs by restoreEtcBackups
# (host-side, right after container start) — same pattern as
# ensurePasswdEntry for /etc/passwd.
RUN mkdir -p /usr/local/share/wakil-etc-backup/ssl-certs \
             /usr/local/share/wakil-etc-backup/chromium.d \
             /usr/local/share/wakil-etc-backup/alternatives \
    && cp /etc/ssl/certs/ca-certificates.crt /usr/local/share/wakil-etc-backup/ssl-certs/ \
    && cp -a /etc/chromium.d/. /usr/local/share/wakil-etc-backup/chromium.d/ \
    && cp -a /etc/alternatives/. /usr/local/share/wakil-etc-backup/alternatives/

# Rust: install rustup and the stable toolchain into /usr/local so the
# binaries are never shadowed by a workspace volume mount on /root.
#
# Supply-chain note: rustup is installed via the standard `curl | sh` method
# from https://sh.rustup.rs. This is the officially recommended installation
# method (https://rustup.rs/) and uses HTTPS with TLS 1.2+. The script is not
# checksum-verified at install time — this is a known supply-chain risk shared
# by all rustup users, mitigated by the HTTPS transport and the fact that
# the rustup project is maintained by the Rust Foundation. A more paranoid
# approach would download a pinned rustup-init binary with a verified
# checksum, but this adds maintenance burden for marginal security gain over
# HTTPS. If this becomes a concern, switch to a pinned rust:bookworm base
# image that already has rustup installed.
ENV RUSTUP_HOME=/usr/local/rustup \
    CARGO_HOME=/usr/local/cargo
RUN curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs \
    | sh -s -- -y --default-toolchain stable --profile minimal --no-modify-path \
    && /usr/local/cargo/bin/rustup component add rust-analyzer

# Go workspace also lives outside /root for the same reason.
ENV GOPATH=/usr/local/go-workspace

ENV PATH="/usr/local/go/bin:/usr/local/go-workspace/bin:/usr/local/cargo/bin:${PATH}"

# golangci-lint — pinned to v2.10.0, matching CI (.github/workflows/ci.yml).
# Installed via digest-pinned multi-stage COPY from the official image rather
# than curl|sh: this matches the Dockerfile's supply-chain posture (base images
# are digest-pinned, kvrust is commit-pinned) and avoids executing a remote
# script at build time. The version MUST match CI to keep local pre-push lint
# parity — bump both together.
COPY --from=golangci/golangci-lint:v2.10.0@sha256:bdd784f3b55fc235da94a2afe8d37f14932f7d6d3a8b7e418588aeb4240ef58d /usr/bin/golangci-lint /usr/local/bin/golangci-lint

# gopls — pinned to v0.22.0. protocol.go in internal/lsp/ is transcribed from
# gopls v0.22.0 internal/protocol (tsprotocol.go). A version bump here MUST be
# accompanied by a re-diff of the union types (esp. DocumentChange) in protocol.go.
RUN go install golang.org/x/tools/gopls@v0.22.0

# Language servers for LSP code intelligence (match defaultLSPServer mappings):
#   - pyright-langserver: Python type checker + LSP server (npm package)
#   - typescript-language-server: TypeScript/JavaScript LSP wrapper (npm package)
# Both require TypeScript for semantic analysis.
RUN npm install -g pyright typescript-language-server typescript

# Entrypoint script: starts kvr-server in the background, then runs the main
# command. Traps SIGTERM to gracefully stop kvr (shutdown snapshot) before
# stopping the main process.
#
# Root rationale: the sandbox runs as root by design. The sandbox must:
#   - Create /etc/passwd entries for workspace users (ensurePasswdEntry)
#   - Manage Docker containers (docker exec, docker cp)
#   - Write to /usr/local/bin and /usr/local/share
#   - Kill processes by PGID (KillPgid)
# A non-root user would require granting specific Linux capabilities
# (CAP_SYS_ADMIN, CAP_KILL, CAP_SETUID) that are effectively equivalent to
# root for this use case. The sandbox is isolated via Docker's own security
# mechanisms (--tmpfs=/etc, read-only mounts, dropped capabilities on child
# containers) — the root user inside the sandbox cannot escalate to the host.
# See SECURITY.md for the full threat model.
#
# Signal handling: entrypoint.sh uses a trap-based approach instead of exec
# because it must run kvr-server in the background AND clean it up on exit.
# The trap catches SIGTERM, signals kvr for graceful shutdown, waits, then
# stops the main process. This is a standard pattern for multi-process
# containers without an init system. If signal forwarding issues arise,
# consider adding tini as PID 1.
COPY docker/entrypoint.sh /usr/local/bin/wakil-entrypoint.sh
RUN chmod +x /usr/local/bin/wakil-entrypoint.sh
