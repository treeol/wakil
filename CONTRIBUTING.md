# Contributing to Wakil

Thank you for your interest in contributing! This guide covers the essentials
for getting a patch ready for review.

## Prerequisites

- **Go 1.25+** (see `go.mod` for the canonical minimum; the Dockerfile may use
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
```

All of the above must pass before you send a patch.

## Sandboxed environments with a tiny `/tmp`

Some sandboxes (including the wakil dev sandbox itself) mount `/tmp` as a
small tmpfs (e.g. 100 MB). Go's build cache, cgo temp files, `go run` of
heavy tools (golangci-lint), and even `git commit` (SSH signing writes its
key to `/tmp`) will fail with `no space left on device` or
`permission denied` on the test binary.

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
- Linting uses `golangci-lint` (see `.golangci.yml`); CI runs it automatically.

## Pull Request Checklist

Before opening a PR, verify each item:

- [ ] `go build ./...` passes
- [ ] `go test -count=1 -cover ./...` passes and `scripts/check_coverage.sh` is green
- [ ] `go test -race -count=1 $(go list ./... | grep -v /internal/lsp)` passes
- [ ] `go vet ./...` passes
- [ ] `gofmt -l .` returns nothing
- [ ] Commit messages are descriptive and reference the relevant WP/issue if applicable
- [ ] No secrets, API keys, or credentials in the diff
- [ ] No new unconfirmed write/execute paths — every destructive action goes
      through the confirmation gate

## Architecture Overview

The codebase is organized into focused packages under `internal/`:

```
agent/       core agent loop, tool dispatch, turn management
config/      configuration loading (JSON + env + flags)
counsel/     Mashūra panel counsel (multi-model review/debug/decide/check)
exec/         executor interface (docker, direct, fake)
lsp/          LSP code-intelligence server manager
memory/       durable cross-session memory store
orregistry/   OpenRouter model metadata cache
proxy/        chat endpoint HTTP client (openai + ilm-proxy kinds)
staging/      kvr client — ephemeral KV store
tools/        the tool set (run_shell, read_file, edit_file, …)
trace/        execution tracing (JSONL per session)
tui/          terminal UI
workflow/     /plan gather→plan→review→implement state machine
```

See `README.md` for the full project layout and feature documentation.

## Security

If you found a security issue, please see [SECURITY.md](SECURITY.md) for
disclosure contact and the threat model. Do **not** open a public issue for
security-sensitive bugs.
