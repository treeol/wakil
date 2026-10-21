# AGENTS.md

## Language

- Go (module: `github.com/treeol/wakil`, Go 1.26)

## Build

- `go build ./...`
- `make clean` — removes local temp/cache dirs and stale build artifacts (gitignored)

## Test

- `go test ./...`
- `go test -race ./...` — race detector (CI runs this on the full suite)
- `go vet ./...`

## Lint

- `golangci-lint run` (v2.11.1) — CI gate
- `gofmt -l .` — CI fails on unformatted files
- `govulncheck ./...` — vulnerability scan (dedicated workflow: .github/workflows/govulncheck.yml)

## CI Checks (.github/workflows/ci.yml)

Beyond build/test/lint, CI enforces:

- **gofmt** — all files must be formatted
- **go vet** — full repo
- **race detector** — `go test -race ./...`
- **coverage floors** — `scripts/check_coverage.sh` (agent/tools/exec/proxy)
- **config-doc consistency** — `scripts/check_config_docs.sh` (config.example.json ↔ docs/configuration.md)
- **buf breaking** — proto schema breaking-change detection against base branch
- **darwin cross-compile** — GOOS=darwin amd64 + arm64 build & vet
- **docker build** — Dockerfile + Dockerfile.daemon build gates

## Architecture

Entry point:
- `cmd/wakil/` — main package

Core packages (`internal/`):
- `agent/` — agent loop, turn phases, tool dispatch, subagents, Mashūra counsel tools
- `auth/` — authentication
- `browser/` — headless browser integration
- `config/` — configuration structures and loading
- `core/` — core domain types and interfaces
- `counsel/` — external AI counsel (oracle) HTTP calls, panel execution, response parsing
- `crypto/` — cryptographic utilities
- `diag/` — diagnostics
- `exec/` — sandboxed execution (Docker + direct modes)
- `ilm/` — ILM (in-context learning memory) stack
- `lsp/` — language server protocol integration
- `memory/` — durable memory store
- `orregistry/` — OpenRouter registry
- `policy/` — policy enforcement
- `protoconv/` — protobuf conversion
- `proxy/` — proxy client (Anthropic, OpenRouter)
- `remote/` — remote provisioning
- `safe/` — safety utilities
- `scrub/` — secret scrubbing
- `server/` — server
- `sessionhistory/` — session history persistence
- `staging/` — ephemeral KV store
- `store/` — storage layer
- `tools/` — built-in tool definitions
- `trace/` — tracing
- `tui/` — terminal UI (Bubble Tea)
- `verify/` — verification utilities
- `wiring/` — dependency wiring
- `workflow/` — workflow engine

## Infrastructure

- Docker (`Dockerfile` — sandbox image, `Dockerfile.daemon` — daemon image)
- GitHub Actions CI (`.github/workflows/ci.yml`)
- Dependabot (gomod + github-actions, weekly)
- Proto schema (`buf.yaml`, `buf.lock`)
- `scripts/` — `check_coverage.sh`, `check_config_docs.sh`
- `docs/` — project documentation

## Conventions

- Commit messages: lowercase prefix (`feat:`, `fix:`, `perf:`, `deps:`, `docs:`) — no period at end
- Code comments: sentence case, period at end, no card-number references in committed code (use commit messages for traceability)
- TUI: Bubble Tea architecture — `View()` must be cheap; expensive computations deduped or throttled
- Secrets: never hardcode, never pass to external tools; `.env` and credential files are toxic payload
- Config: every key in `config.example.json` must be documented in `docs/configuration.md` (CI-enforced)
- Proto: wire-compatible changes only — `buf breaking` runs in CI
- Dependencies: Dependabot opens weekly PRs with `deps:` prefix; SHAs are pinned in CI workflows
