# Contributing to Wakil

Thank you for your interest in contributing! This guide covers the essentials
for getting a patch ready for review.

## Prerequisites

- **Go 1.26+** (see `go.mod` for the canonical minimum; the Dockerfile may use
  a newer builder toolchain)
- A working `docker` setup if you want to test the sandbox mode (optional —
  `direct` mode works without Docker)

## Build & Test

```sh
# Build the binary
go build -o wakil ./cmd/wakil

# Run all tests (with coverage, like CI)
go test -count=1 -cover ./...

# Check coverage floors on damage-critical packages
go test -count=1 -cover ./... | scripts/check_coverage.sh

# Run the race detector (all packages except internal/lsp, which has a known
# pre-existing race — see .github/workflows/ci.yml)
go test -race -count=1 $(go list ./... | grep -v /internal/lsp)

# Check formatting (whole tree, like CI)
gofmt -l .

# Vet
go vet ./...

# Lint (matches CI — same golangci-lint v2.10.0, same .golangci.yml config)
golangci-lint run
```

All of the above must pass before you send a patch.

## Sandboxed environments with a small `/tmp`

New sandbox containers default to a 4 GB `/tmp` tmpfs, but older containers
or a custom `docker_tmpfs_size` may still be too small. Go's build cache,
cgo temp files, `go run` of heavy tools (golangci-lint), and even
`git commit` (SSH signing writes its key to `/tmp`) will fail with
`no space left on device` or `permission denied` on the test binary.

Fix: point the Go toolchain and the system temp dir at the workspace disk:

```sh
mkdir -p .tmp-gocache
export TMPDIR=$PWD/.tmp-gocache \
       GOTMPDIR=$PWD/.tmp-gocache \
       GOCACHE=$PWD/.tmp-gocache
```

- `TMPDIR` — cgo temp files, `git` SSH signing keys, everything that honors
  the POSIX temp-dir variable
- `GOTMPDIR` — Go's own work directory (`go run`, `go test` binaries)
- `GOCACHE` — the build cache (the big one)

`.tmp-gocache/` is already in `.gitignore`. Set these once per shell session
(or add to your shell rc / direnv) and the full gate above works.

## Code Style

- `gofmt` is authoritative — no unformatted code is merged.
- Follow standard Go conventions: effective Go, package naming, etc.
- Linting uses `golangci-lint` v2.10.0 (see `.golangci.yml`); CI runs it
  automatically, and the same binary is installed in the sandbox for local
  pre-push runs: `golangci-lint run`.

## Pull Request Checklist

Before opening a PR, verify each item:

- [ ] `go build ./...` passes
- [ ] `go test -count=1 -cover ./...` passes and `scripts/check_coverage.sh` is green
- [ ] `go test -race -count=1 $(go list ./... | grep -v /internal/lsp)` passes
- [ ] `go vet ./...` passes
- [ ] `golangci-lint run` passes (see `.golangci.yml`; CI runs the same)
- [ ] `gofmt -l .` returns nothing
- [ ] Commit messages are descriptive and reference the relevant WP/issue if applicable
- [ ] No secrets, API keys, or credentials in the diff
- [ ] No new unconfirmed write/execute paths — every destructive action goes
      through the confirmation gate

## Architecture Overview

The codebase is organized into focused packages under `internal/`:

```
cmd/wakil/         main package — entry point, CLI, TUI wiring, daemon subcommand
api/proto/         Connect/gRPC service definitions (session, event, auth, backend, …)
api/gen/           generated Go from proto (connectrpc)
internal/
  agent/           the agent loop and tool-call assembly
  auth/            authentication — join tokens, web sessions, API tokens, OIDC, peer creds
  browser/         headless browser integration
  config/          flag/env/file config resolution
  core/            transport-free domain core — session service, event model, session host
  counsel/         mashūra — external-model counsel (review/debug/decide/check)
  crypto/          envelope encryption for secrets at rest (AES-256-GCM)
  diag/            diagnostic output seam (prevents TUI garble from raw stderr writes)
  exec/            executor backends (docker, direct) + cwd tracking
  lsp/             language-server client — manager, JSON-RPC transport, tools
  memory/          durable memory store — SQLite, two tiers, FTS5, provenance
  orregistry/      OpenRouter model registry fetch + cache (context lengths)
  policy/          policy evaluation for auto-approve and consent gating
  protoconv/       shared proto↔domain event conversion (32-kind switch)
  proxy/           chat endpoint HTTP client (openai + ilm-proxy kinds)
  remote/          remote facade — Connect RPC client for daemon mode
  safe/            path confinement and safety checks
  scrub/           secret scrubbing for tool output and traces
  server/connect/  Connect HTTP server — handlers, auth interceptor, origin validation
  sessionhistory/  searchable session transcript index (SQLite + FTS5)
  staging/         kvr client — in-sandbox ephemeral KV store (UDS wire protocol)
  store/           SQLite migrations and per-domain stores (agent, backend, workspace)
  tools/           the tool set (run_shell, read_file, edit_file, …)
  trace/           execution tracing
  tui/             terminal UI
  verify/          deterministic workflow verification (test/build/lint detection)
  wiring/          bootstrap, conversation manager, facade, host turn, headless runner
  workflow/        /plan gather→plan→review→implement state machine
web/               embedded web console (vanilla JS, served by the daemon)
Dockerfile         sandbox image — Go, Node, Rust, Python toolchains, gopls, docker CLI + compose, gh, golangci-lint
Dockerfile.daemon  minimal daemon image (distroless, non-root, pure-Go SQLite)
docker-compose.yml daemon as a Docker service with volume mounts for data/config/socket
```

See `docs/features.md` for feature documentation.

## Security

If you found a security issue, please see [SECURITY.md](SECURITY.md) for
disclosure contact and the threat model. Do **not** open a public issue for
security-sensitive bugs.
