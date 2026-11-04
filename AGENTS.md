# AGENTS.md

## Language

- Go (module: `github.com/treeol/wakil`, Go 1.26)

## Build

- `go build ./...`
- `make clean` — removes local temp/cache dirs and stale build artifacts (gitignored)

## Test

- `go test ./...`
- `go test -race ./...` — race detector (CI runs this on the full suite)
- hisn backend integration tests (`go test -count=1 -v -timeout 10m -run TestIntegration ./internal/sandbox/hisn/`) skip unless `hisn` is installed and `hisn check` passes (profile/risk/kvr/override tests additionally need >= 0.3.1); set `WAKIL_HISN_BIN` to pick a binary
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
- **goreleaser snapshot** — `goreleaser build --snapshot --clean` (no publish/sign)
- **sandbox image multi-arch** — buildx linux/amd64+arm64 of `Dockerfile`, `push: false`

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
- `exec/` — sandboxed execution (Docker + direct modes; `BackendExecutor` for pluggable sandbox backends; optional capability interfaces in `capabilities.go`)
- `sandbox/` — backend-neutral sandbox contract (`Backend`, `PathMap`, `GrantManager`); `sandbox/hisn/` is the hisn backend, `sandbox/project/` loads `.wakil/sandbox.json` and the user's approval of it, `sandbox/fake/` a test double. The tool layer (agent, tools, lsp, browser, tui) must not import a concrete backend — enforced by `internal/agent/no_backend_import_test.go`. Design: `docs/sandbox-backends.md`
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
- Release (`.github/workflows/release.yml`, `.goreleaser.yaml`) — tag `v*` only: sandbox image → ghcr.io (digest) → goreleaser (digest injected via `SANDBOX_IMAGE`) → package smoke test. Local dry run: `goreleaser release --snapshot --clean --skip=sign,publish`
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
- Release tags: annotated (`git tag -a vX.Y.Z -m "wakil X.Y.Z" <sha>`), created only on a green master commit, and pushed individually by name (`git push origin vX.Y.Z`). Never use `git push --tags`: GitHub creates no push event when more than three tags are pushed at once, so the Release workflow silently does not run
- Snapshot/backup refs go under `refs/archive/`, not `refs/tags/`. Never push `refs/archive/*` to `origin`; back it up with `git bundle create <file> --glob='refs/archive/*'`
