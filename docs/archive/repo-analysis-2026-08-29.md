# Wakil Repo — General Analysis & Improvement Proposals
*Date: 2026-08-29 · Branch: master (clean vs origin except for one in-flight change set) · Analysis only — no changes made.*

## 1. Snapshot

- **Module:** `github.com/treeol/wakil`, Go 1.26.6, ~63k lines of non-test Go + ~79k lines of test Go — a healthy ~1.25:1 test:code ratio.
- **Structure:** clean `cmd/wakil` + `internal/{agent,tui,core,proxy,config,exec,tools,…}` layout, generated Connect/proto code isolated in `api/gen`, embedded web UI in `web/`, embedded system prompt in `prompts/`.
- **CI:** single `ci.yml` with test (+coverage floors), race, lint (golangci-lint v2, pinned by SHA), darwin cross-compile, and proto-breaking jobs. Docker images pinned by digest. Overall hygiene is *good* — findings below are mostly polish, cleanup, and left-over work.

## 2. In-flight / uncommitted work (finish or commit)

The working tree carries one coherent, apparently **complete** change set: removal of
`app_referer` / `app_categories` as user-configurable options (HTTP-Referer and
X-OpenRouter-Categories become hardcoded project defaults; only `app_title` stays
configurable). Touches:

- `internal/config/config.go`, `internal/proxy/client.go`, `internal/agent/{side_question,subagent}.go`, `internal/wiring/bootstrap.go`
- Tests updated consistently: `internal/config/endpoint_test.go`, `internal/proxy/endpoint_shape_test.go`
- Docs updated: `docs/configuration.md`, `config.example.json`

**Proposals:**
- ✅ The diff looks internally consistent (code, tests, docs all agree). Run the targeted tests (`go test ./internal/config ./internal/proxy ./internal/agent`) and commit — don't let this sit uncommitted.
- ⚠️ Test naming drift: `TestOpenAIAttributionHeadersRefererOptOut` now actually tests *title* opt-out; consider renaming to `…TitleOptOut` for clarity.
- ⚠️ There is also a **stash** (`stash@{0}: WIP on feature/wakild-daemon`) from the daemon branch. Triage it: apply, cherry-pick, or drop — stashes silently rot.

## 3. Repo hygiene / root-level clutter

All of the following are correctly **gitignored** (verified: none are tracked), but they clutter the working directory and some are large:

| Item | Size | Note |
|---|---|---|
| `.gocache/` | 1.4 GB | build cache — fine, but consider moving outside the repo (`GOCACHE` env) |
| `.tmp/` | 352 MB | local temp docs — periodically prune |
| `.wakil/` | 346 MB | runtime artifacts; contains old design docs (`card-121-impl-design.md`, `cleanup-review-2026-07-24.md`, `consolidated-improvement-plan.md`, …) that may be worth promoting to `docs/` or deleting |
| `.tmp-test/`, `.tmp-gocache/`, `.tmp-gotest/` | 99 MB / 91 MB / 1.2 MB | four differently-named temp-cache conventions — consolidate to one |
| `runs/` | 8.6 MB | June experiment runs — likely deletable |
| `wakil` (binary) | 46 MB | stale local build (Aug 28) |
| `b2-b5-plan*.md` (×3), `branch-audit.md` | ~74 KB | session plan artifacts, ignored — delete when the work is merged |
| `__pycache__/`, `tb_adapter/` | — | `tb_adapter` contains **only** `__pycache__` (no source; .gitignore itself calls it "orphaned adapter (stale)") — delete the directory entirely |
| `master.key` | 45 B, mode 600 | ignored & untracked (good). Consider relocating out of the repo root to the OS keystore/data dir so a future `.gitignore` regression can't expose it |

**Proposals:**
- Add a `make clean` / `scripts/clean.sh` target that removes the known temp dirs and the stale binary in one step.
- Unify the four gocache/temp conventions (`.gocache`, `.tmp-gocache`, `.gotmp`, `.tmp-gobuild` are all in .gitignore) to a single documented one.
- Delete `tb_adapter/` and drop its .gitignore entries once confirmed dead.

## 4. Left-over TODOs

Go source is remarkably clean — **zero** TODO/FIXME/HACK/XXX markers in `internal/` and `cmd/`. Remaining explicit TODOs:

1. `scripts/check_coverage.sh:76` — *"TODO: add coverage floors for internal/server/*, internal/remote, and other"*. The daemon work (PR #12) landed large new surface (`internal/remote/facade.go` 1.5k lines, `internal/wiring/hostturn.go`/`facade.go` ~1k each, `internal/core/sessionhost/host.go` 1.8k) with **no coverage floors**. This is the most valuable follow-up in the repo.
2. `docs/mashura-context-limit.md:181` — structured `Note` field integration marked TODO; verify whether it shipped and update the doc either way.

## 5. Testing gaps

- **Packages with no `_test.go` at all:**
  - `internal/store/agentstore`
  - `internal/store/backendstore`
  - `internal/store/workspacestore`
  - (`internal/core/sessionhost/storetest` is a test helper package — fine.)
  The three `store` packages are persistence code — exactly the kind of code the repo's own coverage-floor philosophy ("packages where a regression can cause damage") says should be gated. Add tests + floors.
- **Coverage floors exist** for agent/tools/exec/proxy/counsel/protoconv/tokenstore but not for the new daemon-era packages (`internal/server/*`, `internal/remote`, `internal/wiring`, `internal/core/sessionhost`) — see TODO above.
- **Environment-dependent skips** are handled gracefully (kvr-server binary, Docker, shallow clones), but that means CI silently skips staging-tool tests if the kvr binary is missing — consider a CI assertion that the skip does *not* trigger in the full CI job, so coverage can't silently degrade.
- `cmd/wakil/workflow_test.go` is 3,670 lines — split by feature area for maintainability.

## 6. Large files / refactoring candidates

Not urgent, but these files concentrate a lot of behavior and show up in every diff:

| File | Lines | Suggestion |
|---|---|---|
| `internal/agent/app.go` | 2,557 | split state/lifecycle vs turn-loop vs accessors |
| `internal/tui/tui.go` | 1,991 | extract update/view per major mode |
| `internal/core/sessionhost/host.go` | 1,770 | new (daemon work) — consider carving out snapshot/restore |
| `internal/agent/tool_handlers.go` | 1,562 | group handlers by tool family into files |
| `internal/remote/facade.go` | 1,544 | new — same treatment |
| `internal/config/config.go` | 1,496 | move validation & defaults into separate files |

`internal/wiring/facade.go` + `internal/remote/facade.go` + `internal/wiring/hostturn.go` were all added in the daemon merge — worth a deliberate post-merge pass for duplicated glue logic between the local and remote facades.

## 7. Docs

- `docs/` mixes **real documentation** (configuration.md, features.md, tools.md, endpoints.md, memory.md, staging.md, tui.md, workflows.md, remote-provisioning.md) with **working notes**: 25 files in `docs/cards/`, five `proposal-*.md`, `plan-extend-handoff.md`, `discovery-anthropic-cache-control.md`, `mashura-context-limit.md`. Several proposals are stale (session-resume-ux Jul 12, ssh-signing Jul 6, subagent-budget Jul 15) and only `commit-signing-design.md` is cross-referenced anywhere.
  - **Proposal:** move `docs/cards/`, `docs/proposal-*`, and plan docs into a `docs/archive/` (or a `notes/` dir excluded from the doc index), so `docs/` = user-facing documentation only. Close out proposals that shipped or died.
- `docs/configuration.md` is the doc most at risk of drift: `config.example.json` has ~60 top-level keys; spot-check found it current *only because* the in-flight diff updates it. After the attribution change lands, do one sweep verifying every key in `config.example.json` appears in configuration.md (a tiny script could gate this in CI).
- README is slim and link-based (good, post-refactor). No issues found.

## 8. CI / build

- Docker base images and the golangci-lint action are **digest-pinned** — good.
- `Dockerfile` uses `golang:1.26-bookworm` while `Dockerfile.daemon` uses `golang:1.26.6-bookworm` — align the pinning convention (prefer the fully-qualified one).
- `docker-compose.yml` builds `wakil-daemon:latest` — consider tagging with git SHA for reproducibility.
- CI has no job that builds the Docker images; a weekly or on-Dockerfile-change image build would catch bit-rot (e.g., the kvr-server clone-at-build step breaking).
- Darwin cross-compile job exists; consider adding `GOOS=windows` compile check if Windows is ever a target (skip if explicitly out of scope).

## 9. Minor observations

- `web/` (911-line vanilla `app.js`) is fine at this size; if it grows past ~1.5k lines, introduce a build step or split modules. No framework needed yet.
- `panic(` appears in only 3 non-test files (`core/event/ids.go`, `core/sessionhost/host.go`, `server/connect/server.go`) — verify each is an invariant-violation panic, not a reachable error path (daemon context: a panic in `server/connect` kills a request or the process).
- Only 5 `nolint` markers repo-wide — excellent lint discipline.
- `.golangci.yml` disables `-QF*`/`-ST*` staticcheck classes wholesale; periodically re-run with them enabled to harvest cheap wins.

## 10. Prioritized shortlist

1. **Commit the in-flight attribution-header change** (verified-looking, tests updated) and triage the daemon stash.
2. **Add tests + coverage floors** for `internal/store/{agentstore,backendstore,workspacestore}` and the daemon packages (`internal/remote`, `internal/server/*`, `internal/core/sessionhost`) — resolves the one real TODO in the repo.
3. **Delete dead weight:** `tb_adapter/`, `runs/`, stale root binary, `b2-b5-plan*.md`/`branch-audit.md` once merged; add a `make clean`.
4. **Docs triage:** archive `docs/cards/` + stale proposals; add a config-key ↔ configuration.md consistency check.
5. **Post-daemon-merge refactor pass** on the new 1.5k+ line facade/host files while the code is still fresh.
6. Align Dockerfile Go-image pinning; consider a CI image-build job.

---
*Method note: findings are from direct inspection (grep/wc/git/jq over the working tree at commit f97245a) — file sizes, TODO locations, and untracked-status claims were each verified by command output. Subjective refactoring suggestions in §6 and §9 are judgment calls, not defects.*
