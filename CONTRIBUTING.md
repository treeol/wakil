# Contributing to Wakil

Thank you for your interest in contributing! This guide covers the essentials
for getting a patch ready for review.

## Prerequisites

- **Go 1.26+** (see `go.mod` for the canonical minimum; the Dockerfile may use
  a newer builder toolchain)
- hisn 0.4.1 or newer for real-hisn tests (optional — they skip without it)
- A working `docker` setup only if you test the Docker backend (opt-in; hisn
  is the default when usable, and `--sandbox direct` needs neither)

## Build & Test

```sh
go build -o wakil ./cmd/wakil          # build
go vet ./...                            # vet
golangci-lint run                       # lint (0 issues; see .golangci.yml)
deadcode -test ./...                    # dead code, test files as roots (go install golang.org/x/tools/cmd/deadcode@v0.52.0)
gofmt -l .                              # must print nothing
go test -count=1 -cover ./...           # tests with coverage, like CI
LC_ALL=C go test -count=1 ./...         # locale-sensitive path, like CI
go test -race -count=1 ./internal/agent/ ./internal/exec/ \
  ./internal/sandbox/hisn/ ./internal/wiring/ ./internal/tui/ ./cmd/wakil/
scripts/check_coverage.sh               # coverage floors (reads the cover output)
scripts/check_config_docs.sh            # every config.example.json key documented
```

The `-race` run covers the packages that own concurrency. CI runs `-race` over
the whole tree; run the full set before a release.

### Real-hisn integration tests

The hisn tests need a real hisn binary and skip without one. To run them:

```sh
hisn --version                          # must report 0.4.1 or newer
hisn check                              # must pass on this host
go test -count=1 -v -timeout 10m -run TestIntegration ./internal/sandbox/hisn/
```

They also need a `wakil-dev` image pinned as in `Dockerfile`. The CI job
`hisn-integration` runs them only when hisn is on `PATH` and `hisn check`
passes; otherwise it logs why and exits clean.

### Host-log rule

Tests that write to a user's real data directory fail. `testutil.Main` points
HOME and the XDG dirs at a temp tree and fails the package if anything lands
under the real `~/.local/share/wakil` or `~/.config/wakil`. If you run the
suite while a real Wakil chat is open on the same machine, set
`WAKIL_TEST_ALLOW_REAL_DIR_CHANGES=1` for that run only.

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
- Linting uses `golangci-lint` v2.11.1 (see `.golangci.yml`); CI runs it
  automatically, and the same binary is installed in the sandbox for local
  pre-push runs: `golangci-lint run`.

## Pull Request Checklist

Before opening a PR, verify each item:

- [ ] `go build ./...` passes
- [ ] `go test -count=1 -cover ./...` passes and `scripts/check_coverage.sh` is green
- [ ] `LC_ALL=C go test -count=1 ./...` passes
- [ ] `go test -race -count=1` passes on the packages listed in Build & Test
- [ ] `go vet ./...` passes
- [ ] `golangci-lint run` passes (0 issues) and `deadcode -test ./...` is clean
- [ ] `gofmt -l .` returns nothing
- [ ] Commit messages are descriptive and reference the relevant WP/issue if applicable
- [ ] No secrets, API keys, or credentials in the diff
- [ ] No new unconfirmed write/execute paths — every destructive action goes
      through the confirmation gate

## Architecture Overview

The codebase is organized into focused packages under `internal/`:

```
cmd/wakil/         main package — entry point, CLI, TUI wiring
internal/
  agent/           the agent loop, turn phases, tool dispatch, subagents
  auth/            peer-credential checks for local socket use
  browser/         headless browser integration
  config/          flag/env/file config resolution
  core/            session host and domain types
  counsel/         Mashūra — external-model counsel (review/debug/decide/check)
  exec/            sandbox executors (hisn, docker, direct) + cwd tracking
  lsp/             language-server client
  memory/          durable memory store — SQLite, two tiers, FTS5
  sandbox/         backend-neutral sandbox contract; sandbox/hisn is the hisn backend
  staging/         kvr client — in-sandbox ephemeral KV store
  tools/           the tool set (run_shell, read_file, edit_file, …)
  tui/             terminal UI (Bubble Tea)
  wiring/          bootstrap, conversation manager, host turn
  workflow/        /plan gather→plan→review→implement state machine
Dockerfile         sandbox image; the hisn version pin lives here (single place)
```

Design notes: `docs/sandbox-backends.md`, `docs/features.md`.

## Decisions (ADRs)

Architecture decisions live in `docs/adr/`, one Markdown file per decision,
numbered sequentially. Format and the convention are in
`docs/adr/0000-adr-format.md`; the index is `docs/adr/README.md`. Add a row to
the index when you add an ADR. Do not record decisions only in commit
messages or in process logs.

## Single source of the image version

`Dockerfile` is the only place the sandbox image's hisn version and its SHA256
checksums live. Bump `HISN_VERSION` and both arch checksums together, in that
file, and nowhere else.

## Security

If you found a security issue, please see [SECURITY.md](SECURITY.md) for
disclosure contact and the threat model. Do **not** open a public issue for
security-sensitive bugs.
