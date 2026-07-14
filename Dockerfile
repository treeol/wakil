# syntax=docker/dockerfile:1

# Stage 1a: Build kvr-server from the kvrust repository (cloned at build time).
FROM rust:1-bookworm AS kvr-builder
WORKDIR /build
RUN git clone --depth 1 https://github.com/treeol/kvrust.git .
RUN cargo build --release --bin server

# Borrow the Go toolchain from the official image (same Debian base = no glibc mismatch).
FROM golang:1.26-bookworm AS go-toolchain

FROM debian:bookworm-slim@sha256:96e378d7e6531ac9a15ad505478fcc2e69f371b10f5cdf87857c4b8188404716

# Copy the Go toolchain into /usr/local/go (same location as the official image).
COPY --from=go-toolchain /usr/local/go /usr/local/go

# Copy the pre-built kvr-server binary.
COPY --from=kvr-builder /build/target/release/server /usr/local/bin/kvr-server

# Single apt layer: bootstrap curl/gnupg, add NodeSource 20 LTS repo, install
# all dev tools, clean apt caches.
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
           openssh-client \
           procps \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

# Rust: install rustup and the stable toolchain into /usr/local so the
# binaries are never shadowed by a workspace volume mount on /root.
ENV RUSTUP_HOME=/usr/local/rustup \
    CARGO_HOME=/usr/local/cargo
RUN curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs \
    | sh -s -- -y --default-toolchain stable --profile minimal --no-modify-path

# Go workspace also lives outside /root for the same reason.
ENV GOPATH=/usr/local/go-workspace

ENV PATH="/usr/local/go/bin:/usr/local/go-workspace/bin:/usr/local/cargo/bin:${PATH}"

# gopls — pinned to v0.22.0. protocol.go in internal/lsp/ is transcribed from
# gopls v0.22.0 internal/protocol (tsprotocol.go). A version bump here MUST be
# accompanied by a re-diff of the union types (esp. DocumentChange) in protocol.go.
RUN go install golang.org/x/tools/gopls@v0.22.0

# Entrypoint script: starts kvr-server in the background, then execs the main
# command. Traps SIGTERM to gracefully stop kvr (shutdown snapshot) before
# stopping the main process.
COPY docker/entrypoint.sh /usr/local/bin/wakil-entrypoint.sh
RUN chmod +x /usr/local/bin/wakil-entrypoint.sh
