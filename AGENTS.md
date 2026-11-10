# AGENTS.md

## Language

- Go (module: `github.com/treeol/wakil`, Go 1.26)

## Build

- `go build ./...`
- `make clean` — removes local temp/cache dirs and stale build artifacts (gitignored)

## Test

- `go test ./...`
- `go test -race ./...` — race detector (CI runs this on the full suite)
- hisn backend integration tests (`go test -count=1 -v -timeout 10m -run TestIntegration ./internal/sandbox/hisn/`) skip unless `hisn` is installed and `hisn check` passes; the backend itself requires hisn >= 0.4.2 (`config.MinHisnVersion`); set `WAKIL_HISN_BIN` to pick a binary
- `go vet ./...`
- Test isolation: packages whose tests touch Wakil data/config call `testutil.Main` from `TestMain` (`internal/testutil`): HOME and the XDG dirs point at a temp tree, and the package FAILS if anything was created or appended under the real `~/.local/share/wakil` / `~/.config/wakil`. Set `WAKIL_TEST_ALLOW_REAL_DIR_CHANGES=1` to skip that check while a real chat is running on the same machine

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
- **deadcode** — `deadcode -test ./...`
- **Dockerfile Go version pins** — `scripts/check_go_version_pins.sh`
- **darwin cross-compile** — GOOS=darwin amd64 + arm64 build & vet
- **docker build** — `Dockerfile` build gate
- **goreleaser snapshot** — `goreleaser build --snapshot --clean` (no publish/sign)
- **sandbox image multi-arch** — buildx linux/amd64+arm64 of `Dockerfile`, `push: false`
- **installer** — shellcheck of `install.sh`, then `sh install.sh --version v0.6.0 --no-config` in ubuntu:22.04, debian:12, fedora:latest, alpine:latest (hisn variant, `wakil version`, uninstall, tampered-download abort). `HISN_VERSION` in `install.sh` must equal `MinVersion` in `internal/sandbox/hisn/backend.go` (`go test ./internal/sandbox/hisn/`)

## Architecture

Entry point:
- `cmd/wakil/` — main package

Core packages (`internal/`):
- `agent/` — agent loop, turn phases, tool dispatch, subagents, Mashūra counsel tools
- `browser/` — headless browser integration
- `buildinfo/` — build-time metadata injected via ldflags
- `config/` — configuration structures and loading
- `core/` — core domain types and interfaces
- `counsel/` — external AI counsel (oracle) HTTP calls, panel execution, response parsing
- `diag/` — diagnostics
- `initcmd/` — `wakil init` (interactive/flag setup, test call, minimal config writer with 0600 backup); `cmd/wakil/configshow.go` is `wakil config show`
- `exec/` — sandboxed execution (hisn, Docker, and direct modes; `BackendExecutor` for pluggable sandbox backends; optional capability interfaces in `capabilities.go`)
- `sandbox/` — backend-neutral sandbox contract (`Backend`, `PathMap`, `GrantManager`); `sandbox/hisn/` is the hisn backend, `sandbox/project/` loads `.wakil/sandbox.json` and the user's approval of it, `sandbox/fake/` a test double. The tool layer (agent, tools, lsp, browser, tui) must not import a concrete backend — enforced by `internal/agent/no_backend_import_test.go`. Design: `docs/sandbox-backends.md`
- `lsp/` — language server protocol integration
- `memory/` — durable memory store
- `orregistry/` — OpenRouter registry
- `policy/` — policy enforcement
- `proxy/` — proxy client (Anthropic, OpenRouter)
- `safe/` — safety utilities
- `scrub/` — secret scrubbing
- `sessionhistory/` — session history persistence
- `staging/` — ephemeral KV store
- `store/` — storage layer
- `styleguide/` — embedded language style guides
- `testutil/` — test isolation from the real Wakil data/config dirs
- `timeline/` — phase timeline for start, `/new`, `/resume` and exit (`--trace`)
- `tools/` — built-in tool definitions
- `trace/` — tracing
- `tui/` — terminal UI (Bubble Tea)
- `verify/` — verification utilities
- `wiring/` — dependency wiring
- `workflow/` — workflow engine

## Infrastructure

- Docker (`Dockerfile` — sandbox image)
- GitHub Actions CI (`.github/workflows/ci.yml`)
- Release (`.github/workflows/release.yml`, `.goreleaser.yaml`) — tag `v*` only: sandbox image → ghcr.io (digest) → goreleaser (digest injected via `SANDBOX_IMAGE`) → package smoke test. Local dry run: `goreleaser release --snapshot --clean --skip=sign,publish`
- Dependabot (gomod + github-actions, weekly)
- `scripts/` — `check_coverage.sh`, `check_config_docs.sh`, `check_go_version_pins.sh`
- `install.sh`, `config.quickstart.json` — quick installer (writes no config; runs `wakil init`) and the OpenRouter starter config, which is embedded in the binary (`quickstart.go`) as the single source of the default model; both attached to releases via goreleaser `extra_files` (`install.sh` is POSIX sh, `shellcheck -s sh install.sh` must be clean; test-only env `WAKIL_INSTALL_TEST_CORRUPT=1` corrupts downloads to prove checksum aborts)
- `docs/` — project documentation

## Conventions

- Commit messages: lowercase prefix (`feat:`, `fix:`, `perf:`, `deps:`, `docs:`) — no period at end
- Code comments: sentence case, period at end, no card-number references in committed code (use commit messages for traceability)
- TUI: Bubble Tea architecture — `View()` must be cheap; expensive computations deduped or throttled
- Secrets: never hardcode, never pass to external tools; `.env` and credential files are toxic payload
- Config: every key in `config.example.json` must be documented in `docs/configuration.md` (CI-enforced)
- Dependencies: Dependabot opens weekly PRs with `deps:` prefix; SHAs are pinned in CI workflows
- Release tags: annotated (`git tag -a vX.Y.Z -m "wakil X.Y.Z" <sha>`), created only on a green master commit, and pushed individually by name (`git push origin vX.Y.Z`). Never use `git push --tags`: GitHub creates no push event when more than three tags are pushed at once, so the Release workflow silently does not run
- Snapshot/backup refs go under `refs/archive/`, not `refs/tags/`. Never push `refs/archive/*` to `origin`; back it up with `git bundle create <file> --glob='refs/archive/*'`
