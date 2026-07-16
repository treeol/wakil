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

# Run all tests
go test ./...

# Run the race detector on concurrent packages
go test -race ./internal/agent/... ./internal/proxy/... ./internal/tui/... ./internal/counsel/...

# Check formatting
gofmt -l cmd/ internal/

# Vet
go vet ./...
```

All of the above must pass before you send a patch.

## Code Style

- `gofmt` is authoritative — no unformatted code is merged.
- Follow standard Go conventions: effective Go, package naming, etc.
- Linting uses `golangci-lint` (see `.golangci.yml`); CI runs it automatically.

## Pull Request Checklist

Before opening a PR, verify each item:

- [ ] `go build ./...` passes
- [ ] `go test ./...` passes
- [ ] `go test -race ./internal/agent/... ./internal/proxy/... ./internal/tui/... ./internal/counsel/...` passes
- [ ] `go vet ./...` passes
- [ ] `gofmt -l cmd/ internal/` returns nothing
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
