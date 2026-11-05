# Cleanup pass — 2026-11

Plan and results for the consolidation/dead-code pass. The plan sections below
are kept as written for the record; the **Final results** section at the end
supersedes them wherever they differ. Baseline: `master`
at `15868f6`, clean tree. Scope and non-goals are as set in the task; no
version bump, tag or push. Companion: [DECISIONS-2026-11.md](DECISIONS-2026-11.md).

Evidence tags: **[V]** measured/read by me; **[S]** subagent-reported, not
re-verified; **[inferred — confirm]**.

## Baseline metrics (before any change) [V]

| Metric | Value |
|---|---|
| Non-test `.go` files (tracked, excl. `api/gen`) | 306 |
| Test `.go` files | 417 |
| Code LOC (non-test, excl. `api/gen`) | 101,030 |
| Test LOC | 127,966 |
| `func Test*` count | 4,258 |
| Exported top-level symbols (non-test, excl. `api/gen`) | 1,873 |
| `go build ./cmd/wakil` (warm cache) | 1.22 s |
| Binary size `wakil` (linux/amd64) | 50,746,487 bytes |
| `go test -p 2 ./...` | exit 0, 51 packages `ok` |

Toolchain: go 1.26.10 (`go.mod`), golangci-lint v2.11.1 (AGENTS.md).

## Usage evidence source — now readable

The 2026-11-04 inventory said `~/.local/share/wakil` was not visible. In this
session it **is** readable (`ls ~/.local/share/wakil` lists `sessionhost`,
`sessionhistory`, `memory`, `sessions`, `toolcache`, `input_history`). Part C
uses counts from these stores, with the queries recorded in the decision sheet.
Copies were taken to `/tmp/wk-usage` so the live WAL databases were not touched.

## Part A — consolidate

### Mashūra round 1 — result (2 readers: gpt-6-astra, claude-fable-5.1; both complete)

Triage and what changed in the plan:

- **BLOCKING (accepted): A1 would orphan existing stores.** Store keys are
  `workspaceKey(WorkspacePath())` (`MemoryDBPath`, `SessionHostDBPath` via
  `stagingKey`), and the daemon session-host ID is `sha256(WorkspacePath())`
  (`wiring/conversation_manager.go:492`). Moving those off `WAKIL_WORKSPACE_PATH`
  changes keys for every existing user. Frozen value at baseline for `/mnt/wakil`:
  memory key `f3fa3243c6fc07e5`, sessionhost key `f3fa3243c6fc07e5`, workspace
  key sha256 `f3fa3243…11ffb`. **Revised A1: no store-identity change.** Identity
  derivation stays as is; the only deliverable is one `ResolveProject` that every
  caller goes through, returning exactly today's values, plus a golden test that
  asserts the frozen keys above and the `plan gc` path. Any real key change is a
  separate behaviour release, not part of this pass.
- **BLOCKING (accepted): A2 would weaken containment.** Five mechanisms have
  different semantics: `DockerExecutor.ConfinePath` (in-container `readlink -f`),
  `toolcap` (`os.Root`, rooted open), `sandbox.Within`/`PathMap` (namespace
  mapping, AgentRoot deliberately not host-resolved), `memory.isWithinWorkspace`
  (host, symlinked-parent hole), `exec.isInsideWorkspace`. **Revised A2: share only
  the identical lexical prefix check** (`exec.isInsideWorkspace`, `sandbox.Within`,
  `tools.underRoot` are textually the same). Keep resolution strategies per layer.
  `memory` gets the `exec.resolveExistingAncestor` treatment (a strengthening) with a
  new test for a symlinked missing parent. `config.go:2453` is workspace
  resolution, not containment; it moves to A1's list.
- **BLOCKING (accepted): A3 "create no file" is not enforced by an existence check.**
  `memory.Open` does MkdirAll, then `sql.Open` (lazy), then schema/migration writes.
  `OpenExisting` refuses a missing store before any of those. For an existing store
  it still sets WAL and runs migration, so it may write; it is NOT a read-only open.
  `plan gc` uses it (via `PlanGCStore`). `LoadConfig` in `plan gc` is kept: removing
  it changes the broken-config exit, which is a separate decided item.
- **Rejected:** "Delete the validators" vs "wire them" is a false dichotomy for the
  reviewers: the inline `validateEnums` switches duplicate the sets. Decision:
  **wire** `OutputModeIsValid`/`ClipboardModeIsValid` into `validateEnums` (same
  accepted set, same error text, zero behaviour change), then the comments are true.
- **WithNow/WithIDGenerator:** deleted, consistent with the rule below.
- **Rule for the test-only items (both reviewers):** a symbol with no non-test
  caller AND no test caller is deleted. Test-only seams that tests use are kept.
- **WAKIL_WORKSPACE_PATH role:** the reviewer showed no file-operation-only reader.
  Its readers are all identity uses (listed above). So "file operations only" is
  not established and is dropped. It keeps its current behaviour in this pass. Its
  fate (keep vs. replace with `WAKIL_WORKSPACE_KEY`) is a decision-sheet item for
  the user, not implemented here.
- **Toolcache** is keyed by chat ID (`toolCacheDir(chatID)`), not by project. It is
  excluded from the per-project identity test and not rekeyed.
- **B1 evidence:** deadcode reports functions only; `staging.MaxKeyLen/MaxMGetKeys/
  MaxFrameSize` are constants/funcs reported as unreachable functions. Each is
  deleted only if grep finds no non-declaration refs (done above: 0 refs). The
  `hisn.DefaultStateDir` reader check (documented `home_root` default) is run before
  deletion; `NewAuthHandlerWithOIDCAndSecureCookies` is deleted on deadness only
  (rationale from Part C dropped).

Round 2 ran on the committed diffs; see "Final results" at the end.

### A1 One workspace/project resolver — PLAN (superseded by the round-1 result above)
Current derivations found [V]:
1. `agent.canonicalWorkspace` / `workspaceKey` (`internal/agent/workspace.go`): Abs + EvalSymlinks → sha256; honours `WAKIL_WORKSPACE_KEY`.
2. `config.Config.WorkspacePath()` (`internal/config/config.go:1908`): `WAKIL_WORKSPACE_PATH` → `WorkDir` (direct) / `HostWorkDir`.
3. `cmd/wakil/plan_gc.go:agentWorkspace`: cwd, Abs + EvalSymlinks, explicitly ignores (2).
4. `wiring/sandbox_project.go:54`: its own EvalSymlinks root.
5. `agent/session.go:145` and `repostate.go:104`: use (1).

Proposed single resolver: `agent.ResolveProject(flagDir string) (canonicalPath, projectID string, err error)` with precedence **explicit `--project`/flag > cwd**; `WAKIL_WORKSPACE_PATH` and config host workdir are **not** consulted for store identity. Rationale: the 0.4.3 bug (gc opened `/mnt/<name>`) came from (2). Whether `WAKIL_WORKSPACE_PATH` keeps a role (container bind-mount file ops only, per `docker-compose.yml:68`) is a decision that I will state in `docs/configuration.md` (A4) and keep only for file-operation paths, not for store identity. **Open point for Mashūra round 1.**

Deletions: the duplicate derivations in plan gc and `WorkspacePath()` callers that feed store paths. Test: for memory, sessionhistory, sessionhost, hisn-home, skills, toolcache, a chat started in cwd C and `wakil plan gc` run in C yield the same path, including when C is a symlink and `WAKIL_WORKSPACE_PATH` is set elsewhere.

### A2 One path-containment helper — PLAN
Five implementations [V]:
- `exec.isInsideWorkspace` (`exec_ops.go:167`)
- `memory.isWithinWorkspace` (`memory/store.go:448`)
- `tools/toolcap.go` confinement (`:213`, `:230`)
- `sandbox/paths.go:54`
- `config.go:2453` (EvalSymlinks on a config path)

Target: one helper in a leaf package (`internal/safe` already exists with no deps, so the containment helper lives there) that resolves the root once and the candidate via its deepest existing ancestor. Delete the others and call the shared one. Existing escape tests stay as-is and must stay green.

### A3 Store open never creates by accident — PLAN
`memory.Open` creates a missing DB; `sessionhistory.Open` likewise. Split into `OpenExisting` (error on a missing store, creates nothing) and `Create`. `OpenExisting` is NOT a read-only open: for an existing store it still sets WAL and runs migration. `plan gc` (via `PlanGCStore`) switches to `OpenExisting`. Test: a missing store returns an error and creates no file or directory. A true read-only open (mode=ro, no pragma, no migration) is an open item, not part of this step.

### A4 Env/config override inventory — PLAN
Inventory is a grep of `WAKIL_*` / `ILM_*` in Go code and docs. Remove env vars and config keys with no reader; document the rest with precedence in `docs/configuration.md`. Known candidate from the inventory: `memory_preflight_recall` (read nowhere; docs claim it disables a feature) → **B2**.

### A5 Docs drift — PLAN
- CHANGELOG: `0.4.0` heading says "Unreleased"; restore the bracketed dated format used by other headings; keep the "0.4.2 folded into 0.4.3" wording.
- Pre-push checklist (repo copy and memory entry `env/prepush-checklist`): replace the pinned Go literal with a derivation from `go.mod` (`go env GOVERSION`).

## Part B — remove dead code

### B1 Dead-code table

Tool: `golang.org/x/tools/cmd/deadcode@v0.30.0 -test ./...` (pinned, not `@latest`; the inventory's panel note asked for a pinned version). Result: 27 unreachable, identical to the inventory list [V, `/tmp/deadcode-test.txt`].

Each candidate checked for non-declaration references in tracked Go files (comments excluded), registration/reflection, build tags.

| Symbol | Location | Why dead | Decision |
|---|---|---|---|
| `tuiConfirmer` | agent/commands.go:93 | only caller is `RunTurn` (dead, below) | **delete** |
| `RunTurn` | agent/tui_cmds.go:50 | no caller; the TUI reaches turns another way | **delete** |
| `RestoreRepoStateResume` | agent/repostate.go:344 | no caller; `internal/remote` `RestoreRepoStateResume` is a **different** RPC method (`remote/facade.go:715`, `bootstrap.go:66`), kept | **delete** (agent func only) |
| `StagingSocketPath` | agent/staging.go:75 | zero refs outside its doc comment | **delete** |
| `apitoken.IsNotFound` | auth/apitoken/apitoken.go:209 | zero refs (the grep hit is `jointoken.IsNotFound`, different package) | **delete** |
| `config.OutputModeIsValid` | config/config.go:105 | zero callers; comment at :1730 claims validateEnums guards with it — it does not | **keep, flagged** (behaviour question, see open points) |
| `config.ClipboardModeIsValid` | config/config.go:132 | same as above | **keep, flagged** |
| `core/id` `NewFromReader` | core/id/id.go:39 | zero refs | **delete** |
| `core/id` `NewTenantID`, `NewUserID`, `NewWorkspaceID`, `NewTurnID`, `NewToolCallID`, `NewSubagentID` | core/id/id.go:138–159 | wrappers over `event.New*`; the package's own tests call `event.`; no non-test caller | **delete** (verify test refs before commit) |
| `sessionhost.WithNow` | core/sessionhost/host.go:297 | test seam, only referenced in comments | **delete** — see open point |
| `sessionhost.WithIDGenerator` | core/sessionhost/host.go:300 | same | **delete** — see open point |
| `sandbox.Run` | sandbox/cmdhandle.go:108 | zero callers | **delete** |
| `fake.Echo` | sandbox/fake/fake.go:128 | test double, zero callers | **delete** |
| `hisn.DefaultStateDir` | sandbox/hisn/exec.go:210 | zero callers | **delete** |
| `connect.NewAuthHandlerWithOIDCAndSecureCookies` | server/connect/auth_handler.go:83 | zero callers; OIDC — see D-sheet S12a | **delete** (decision sheet recommends OIDC removal) |
| `connect.NewServerFromInterfaces`, `…WithAuth`, `…WithAuthAndOIDC` | server/connect/server.go:138,180,198 | zero callers (only panic-message strings) | **delete** |
| `Server.SessionState` | server/connect/server.go:166 | zero callers | **delete** |
| `staging.MaxKeyLen`, `MaxMGetKeys`, `MaxFrameSize` | staging/client.go:709–715 | zero callers | **delete** |

Result of checks on the deleted set: none is referenced by reflection, a registration table, an MCP/daemon handler, or a build-tagged file (18 tagged tracked files exist; none of the symbols is in them [V]). Full `go test` must stay green after deletion.

**Open point (not deleted):** the `WithNow`/`WithIDGenerator` option seams are test-only. Deleting them removes the ability to inject a clock/ID generator into `sessionhost` tests; if any test needs determinism it would have to use another path. Mashūra round 1 to confirm.

Note: the inventory's "102 without `-test`" list is not re-run here; test-only seams flagged there are not deleted.

### B2 Config keys with no reader — PLAN
Candidate `memory_preflight_recall` (read nowhere; `docs/configuration.md:143` and `config.example.json:133` promise behaviour that does not exist). Remove key, docs entry, example entry, CHANGELOG line under **Removed**. Per the no-behaviour-change rule, removing a no-op key is listed as a removal, not a behaviour change; Mashūra to confirm this classification.

### B3 Unused files/scripts/prompts — PLAN
Grep-based reference check plus a `go build` of all entry points. Anything with unclear owner is listed, not deleted. **Out of scope for B3:** the locale fix for `apply_skill_template` (B3 in the task refers to the locale/helper handling in the out-of-scope list, so it is not touched here; reported only). Clutter at repo root (`ilmql/` 5.8 GB target dir, `tmp/`, `dist/`, `wakil` binary) is untracked/ignored; listed, not deleted.

## Part C — decision sheet
Written to `docs/DECISIONS-2026-11.md` after usage counts are gathered. Implement nothing there.

## Open points (not blocking, for the report)
1. `OutputModeIsValid` / `ClipboardModeIsValid`: validation claimed by comments is not wired. Either wire it (behaviour change) or fix the comment. Kept.
2. `WithNow`/`WithIDGenerator` test seams removed (see B1).
3. `go mod tidy` not run in this pass (`BurntSushi/toml` marked indirect) — separate hygiene commit if desired.

### A4 results (step 2, executed)

| Var | Result | Action |
|---|---|---|
| `ILM_ASSIST_DECIDER_ENDPOINT`, `ILM_ASSIST_ENABLED`, `ILM_ASSIST_SHADOW_SAMPLE`, `ILM_ASSIST_TIMEOUT_S`, `ILM_ECON_ENDPOINT`, `ILM_FLUSH_S` | External: owned by ilm-stack, not read by Wakil Go code | Kept in `docs/ilm-stack/`; excluded from the Wakil inventory |
| `WAKIL_STAGING_DIR` | Documented but unread (no Go, script, or compose reader; no runtime-built name; no env-binding library) | Operator comment removed from `Dockerfile.daemon`; documented but unread, comment removed |
| `WAKIL_DAEMON_SOCKET` | Documented but unread (same checks) | Operator comment removed from `docker-compose.yml`; `--socket` flag kept (`internal/config/config.go:1515`); documented but unread, comment removed |
| `WAKIL_DAEMON_TAG`, `WAKIL_GID`, `WAKIL_UID` | Read by `docker-compose.yml` | Kept |
| `WAKIL_HISN_BIN` | Read by integration tests | Kept |
| `memory_preflight_recall` (B2) | Already removed in `77f4235` (0.4.2) | Nothing to commit |

Tool-output incidents (0.4.4 bug list): (1) async envelope for op-7 returned only `lines=16` with no body; (2) `read_process_log("bg17")` returned "no background process" for a finished `notify_on_exit` job; (3) `read_file` rejected `/tmp/...` as outside the workspace. Each was recovered by writing to `/tmp` and reading via `cat` in the shell.

---

# Final results

Range `15868f6..HEAD` (16 commits, all listed below; `git log --oneline 15868f6..HEAD`). No version bump, tag or push.
Evidence tags as above. Gate after every commit: `go build ./...`, `go vet ./...`,
`golangci-lint run ./...` (0 issues), `LC_ALL=C go test -p 2 ./...`
(exit 0, 51 `ok`, 0 FAIL). Final HEAD also passes `go vet -tags leanfake|browser|race`.

## Commits

| Commit | Item |
|---|---|
| `3a633c2` | A5 CHANGELOG heading format |
| `d0d3441` | A5 `scripts/check_go_version_pins.sh` + CI step |
| `b3b4f37` | A4 operator comments for unread `WAKIL_STAGING_DIR` / `WAKIL_DAEMON_SOCKET` removed |
| `54846c7` | validators wired into `validateEnums` |
| `9d56b1d` | A1 `ResolveProject` + golden-key + chat/gc invariant tests |
| `dab7ecf` | A2 shared lexical helper; `memory.isWithinWorkspace` symlinked-parent fix |
| `5bf7ade` | A3 `memory.OpenExisting`; `plan gc` via `PlanGCStore` |
| `78cb375` | B1 `RunTurn`, `tuiConfirmer` |
| `8209c9f` | B1 zero-reference helpers |
| `fb6b18f` | B1 `agent.RestoreRepoStateResume` |
| `ec3f3cf` | B1 `sessionhost.WithNow`/`WithIDGenerator`, `id.NewFromReader` |
| `06f69b1` | B1 `apitoken.IsNotFound` |
| `ef594a8` | B1 `connect.NewServerFromInterfaces*` |
| `03d8af0` | gofmt: trailing blank lines left by B1 (found by round 2, see below) |
| `bc0183f` | test: dangling-symlink rejection in `isWithinWorkspace` (round 2) |
| `54bc114` | B1 `id.NewToolCallID`/`NewSubagentID` (deadcode leftovers); `ResolveProject` doc fix |

B2 (`memory_preflight_recall`) was already removed in `77f4235` (0.4.2); nothing
to commit.

## Metrics before / after [V]

Same script (`/tmp/metrics.sh`) on a `git archive 15868f6` export and on HEAD.

| Metric | Before | After |
|---|---|---|
| Non-test `.go` files (excl. `api/gen`) | 284 | 285 |
| Test `.go` files | 417 | 420 |
| Code LOC (non-test, excl. `api/gen`) | 101,030 | 100,752 |
| Test LOC | 127,966 | 128,132 |
| `func Test*` | 4,258 | 4,265 |
| Exported top-level symbols | 1,873 | 1,855 |
| Binary size (`go build ./cmd/wakil`, linux/amd64) | 50,742,087 | 50,747,339 |
| Warm `go build -o /dev/null ./cmd/wakil` | 1.22 s (baseline note) | 1.24 s |
| `deadcode@v0.30.0 -test ./...` unreachable funcs | 27 | 0 (2 after `ec3f3cf`…`ef594a8`, 0 after `54bc114`) |
| `go test -p 2 ./...` | 51 ok | 51 ok |

Net: −278 code LOC, +166 test LOC, −18 exported symbols. The baseline table at
the top says 306 non-test files; this script counts 284 at the same commit
(`grep -v '_test.go'` and `grep -v '^api/gen'` on `git ls-files`). The two counts
use different file filters and I did not reconcile them, so compare only the
Before/After columns of this table with each other. LOC, test and symbol rows
matched the top table exactly at baseline (101,030 / 127,966 / 4,258 / 1,873). The binary grew by 5 KB: the added code (new helper, tests'
production hooks) outweighs the removed functions, which the linker had already
dropped as unreachable. Dead-code removal does not shrink the binary here.

The final deadcode result (0) was measured on HEAD after `54bc114` (`/tmp/deadcode-final.txt`, rc 0, no `unreachable func` lines).

## B1 final dead-code table

| Symbol | Final decision | Commit |
|---|---|---|
| `tuiConfirmer`, `RunTurn` | deleted | `78cb375` |
| `StagingSocketPath`, `sandbox.Run`, `fake.Echo`, `hisn.DefaultStateDir`, `connect.NewAuthHandlerWithOIDCAndSecureCookies`, `Server.SessionState`, `staging.MaxKeyLen/MaxMGetKeys/MaxFrameSize`, `id.NewTenantID/NewUserID/NewWorkspaceID/NewTurnID` and siblings | deleted (zero refs, fresh grep incl. tests and tagged files) | `8209c9f` |
| `agent.RestoreRepoStateResume` | deleted; RPC and `conversation_manager` use `RestoreRepoStateRead` + `RestoreRepoStateResumeApply` directly | `fb6b18f` |
| `sessionhost.WithNow/WithIDGenerator`, `id.NewFromReader` | deleted as a chain | `ec3f3cf` |
| `apitoken.IsNotFound` | deleted | `06f69b1` |
| `connect.NewServerFromInterfaces`, `…WithAuth`, `…WithAuthAndOIDC` | deleted; package is under `internal/` | `ef594a8` |
| `id.NewToolCallID`, `id.NewSubagentID` | deleted (surfaced by the post-pass deadcode run) | `54bc114` |
| `config.OutputModeIsValid`, `ClipboardModeIsValid` | **kept, now used** by `validateEnums` | `54846c7` |

B3 (unused files): list-only; not acted on. `ilmql/` (5.8 GB), `tmp/`, `dist/`, the
`wakil` binary are untracked or ignored.

Behaviour note found while verifying `RestoreRepoStateResume`: the deleted
wrapper applied `--think` when no repo-state existed. Both live callers
(`wiring/conversation_manager.go:123`, `server/connect/session_state_handler.go:653`)
return early on `st == nil` and do **not** apply `--think` on that path. The
wrapper itself had no caller, so behaviour did not change, but if `/resume` on a
fresh workspace is meant to honour `--think`, that is already not the case today
[V: both call sites read]. Listed as an open point.

## The five containment mechanisms (A2) [V]

| Mechanism | Location | Resolution strategy | Shared now? |
|---|---|---|---|
| `DockerExecutor.ConfinePath` | `internal/exec` | in-container `readlink -f` | no — different namespace |
| `toolcap` confinement | `internal/tools/toolcap.go` | `os.Root` rooted open | no — kernel-enforced |
| `sandbox.Within` / `PathMap` | `internal/sandbox/paths.go` | namespace mapping; AgentRoot deliberately not host-resolved | lexical prefix → `sandbox.WithinLexical` |
| `exec.isInsideWorkspace` | `internal/exec/exec_ops.go` | lexical, after `resolveExistingAncestor` by the caller | lexical prefix → `sandbox.WithinLexical` |
| `memory.isWithinWorkspace` | `internal/memory/store.go` | host `EvalSymlinks`; now falls back to `resolveExistingAncestor` | own copy of the ancestor walk |

Correction to the plan text: `tools.underRoot` does not clean its inputs, so it is
a different contract and was **not** merged (the plan called it textually
identical; it is not).

`memory.resolveExistingAncestor` is a copy of `exec.resolveExistingAncestor`
with the same walk; the two differ only in error wrapping (`exec` adds
"resolving path … (dangling symlink?)"; `memory` returns the raw error) [V,
`diff` of the two bodies]. Not merged: `memory` must not import `exec`.

Behaviour change in `memory` (intended, fail-closed), documented by round 2:
when `EvalSymlinks(target)` fails, the target is now judged by its deepest
existing ancestor, and any error there (dangling symlink, `ENOTDIR`, `EACCES`,
`ELOOP`) returns `false` where it used to fall back to a lexical check.
Pinned by `TestIsWithinWorkspaceRejectsMissingFileUnderSymlinkedParent`,
`…AcceptsMissingFileInsideRoot`, `…RejectsDanglingSymlinkOnPath`.

## The two workspace sources (A1) [V]

| | Chat / daemon | `plan gc` |
|---|---|---|
| Function | `cfg.WorkspacePath()` (`internal/config/config.go:1908`), via `agent.ChatProjectPath` | `ResolveProject` (`cmd/wakil/plan_gc.go`) |
| Order | `WAKIL_WORKSPACE_PATH` → `WorkDir` (direct) / `HostWorkDir` (docker) | `--project` → cwd; `Abs` + `EvalSymlinks` |
| Reads `WAKIL_WORKSPACE_PATH` | yes | no |
| Key derivation | `agent.WorkspaceKey` (honours `WAKIL_WORKSPACE_KEY`) | same |

Invariant locked by test: with `WAKIL_WORKSPACE_PATH` unset, both give the same
store for the same cwd (plain and symlinked; direct and docker). With the
variable set to another directory they diverge by construction. Golden keys for
`/mnt/wakil`: memory `f3fa3243c6fc07e5`, workspace `f3fa3243…11ffb`. No key
derivation changed in this pass. Merge options and migration cost: C4 in
[DECISIONS-2026-11.md](DECISIONS-2026-11.md).

## Mashūra round 2 (cap reached: no round 3)

Presented per part on the committed diffs (`.tmp/r2-*.diff`, each scanned with
non-emitting `grep -c` for key/secret shapes: 0 matches). Two readers per part:
`openai/gpt-6-astra` and `anthropic/claude-fable-5.1`.

| Part | gpt-6-astra | claude-fable-5.1 |
|---|---|---|
| A1 + A3 | read | read |
| A2 | read | first attempt **VOID** (reply cited a wrong last section); re-run and read |
| A4 + A5 + validators | read | **VOID** — not obtained |
| B1 deletions | read | read |

Part A4+A5+validators was read by one reviewer only. (Two VOID replies were not
counted as reviews.)

**Triage rule from the task:** only an item that changes a test result or exit
status, or breaks the unchanged-behaviour invariant, is BLOCKING.

| Finding | Reader | Verified how | Result |
|---|---|---|---|
| `isWithinWorkspace` now rejects a path through a dangling symlink that points inside the root | both (A2) | read `memory.isWithinWorkspace`; compared with `exec.resolveExistingAncestor` | **Accepted as intended fail-closed policy**, pinned by a new test (`bc0183f`). Not a regression in any existing test (full suite green). |
| 6 files left with a trailing blank line after B1 deletions; CI runs `gofmt -l .` | fable (B1) | `gofmt -l .` listed exactly those 6; baseline files were clean | **Real; fixed** in `03d8af0`. (A would-be CI failure.) |
| `go vet` with build tags may hide callers | fable (B1) | `go vet -tags leanfake`, `browser`, `race`: all rc 0 | not a finding |
| Policy evaluation unreachable after `tuiConfirmer` deletion | both (B1) | `Evaluate` has live callers at `wiring/hostturn.go:684`, `wiring/headless.go:185` | not a finding |
| `RestoreRepoStateResume` carried the `--think` fallback | both (B1) | read both live callers | behaviour unchanged (wrapper had no caller); open point above |
| Non-NotExist stat error text for `plan gc` changed prefix | fable (A1+A3) | `grep "cannot access store"`: only `memory/store.go:204`; no test asserts the old prefix | not blocking; message is now `plan gc: memory open error: memory: cannot access store …` |
| `TestResolveProjectGoldenKeys` hard-codes `/mnt/wakil` | fable (A1+A3) | read the test | non-blocking open point (below) |
| `t.Setenv("WAKIL_WORKSPACE_KEY","")` is not "unset" | fable (A1+A3) | `workspace.go:47` uses `os.Getenv(...) != ""`; `config.go:1909` likewise | not a finding |
| Pin script misses `FROM` forms (`--platform`, no `-variant`, digest-only) and could fail open on mixed forms | both (A4/A5) | read the Dockerfiles: both have exactly one `FROM golang:1.26.10-bookworm@sha256:…` | **open point**, not blocking: today's files match; a future unrecognised form makes the script fail loudly, but a *mix* of a recognised and an unrecognised form could pass |
| Validator equivalence | both (A4/A5) | read `OutputModeIsValid` / `ClipboardModeIsValid` and the diff | accepted sets are exactly the constants the old switches listed; error text byte-identical; non-blocking: list is still duplicated in the message |
| `OpenExisting` is check-then-open (race) | gpt (A1+A3) | by reading | non-blocking; documented as not read-only and not race-proof |
| Chat/gc invariant test models "chat" by a hand-built `Config` | both (A1+A3) | by reading | non-blocking; function-level, not CLI-level |

No BLOCKING point went unresolved.

## Open points (not implemented)

1. `plan gc` loads a config it never uses (`agentWorkspace` ignored it); removing
   `LoadConfig` changes the broken-config exit status, so it needs its own decision.
2. A true read-only store open (`mode=ro`, no pragma, no migration) for future
   inspect/dry-run callers. `OpenExisting` is not that.
3. `TestResolveProjectGoldenKeys` hard-codes `/mnt/wakil`; it passes elsewhere
   because the path string is hashed even when the directory is absent, but it
   would fail if `/mnt/wakil` were a symlink to another path.
4. Pin script: reject unrecognised Go base-image references instead of skipping them.
5. `/resume` on a fresh workspace does not apply `--think` (see B1 note).
6. `docker-compose.yml:66` documents `wakil --print-workspace-key`, which no code
   implements.
7. Tests write into the user's real `~/.local/share/wakil` (C-sheet finding 1).
8. `go mod tidy` not run (`BurntSushi/toml` marked indirect).
9. `GOOS=windows go build ./...` fails in `internal/exec` (`syscall.Setpgid`,
   `syscall.Kill`); the same errors occur at baseline `15868f6`, only line
   numbers differ [V, diff of both outputs].
10. `config.go:2453` is workspace resolution, not containment (moved to A1's list in round 1).

## Tool-output incidents for the 0.4.4 bug list

Each was recovered by writing to `/tmp` and reading via the shell.
1. Async envelope for a finished `run_background` job often shows only the
   trailing `use read_process_log(...)` hint and no output. Repro: any
   `run_background` with `notify_on_exit`, then `wait_for_completion`.
2. `read_process_log(bgN)` returns "no background process" for a job that just
   finished (seen earlier in the pass).
3. `read_file` rejects `/tmp/...` as outside the workspace.
4. `run_shell` result with a bash-only construct under `sh`
   (`${PIPESTATUS[0]}`, `<(...)`): `Bad substitution` / `Syntax error: "(" unexpected`;
   the harness then reports `exit code unknown (completion marker missing)`.
   Repro: `run_shell` with `diff <(a) <(b)`.
5. A `sed` line-range edit computed from `grep -n` offsets cut the wrong lines
   (earlier in the pass; restored with `git checkout`).
6. This session: a combined gate whose log was read before `go test` finished
   showed one `ok` line and no `EXIT=`; reading it again after the completion
   envelope gave the full result.

## Skill auto-creation "app note" count (C2)

**Three** `[app note — not from the user: …]` suggestions appeared inside tool
results during this pass (two in earlier sessions, one in this one). All were
ignored. Source: `internal/agent/skill_suggestion.go:109,118`.

## Host sequence (to run on the host)

```bash
cd ~/coding/wakil
go build ./... && go build -o wakil ./cmd/wakil
LC_ALL=C go test -p 2 ./...
golangci-lint run ./...
./wakil plan gc            # must print: store: …/memory/5d8efbe880d17149/memory.db
# short real chat as smoke test: start ./wakil, send one message, /new, exit
```

Not verified by me: the host `plan gc` output (store `5d8efbe880d17149` does not
exist in this environment), a real chat, and the new CI step on GitHub.
