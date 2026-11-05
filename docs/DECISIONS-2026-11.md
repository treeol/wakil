# Decision sheet — 2026-11

Evidence only. Nothing here is implemented. Companion:
[CLEANUP-2026-11.md](CLEANUP-2026-11.md). Evidence tags: **[V]** measured/read by
me in this session; **[inferred — confirm]** my reading, not checked.
Browser tools are decided (keep) and are not part of this sheet.

## How the usage numbers were gathered

Source: `~/.local/share/wakil` on the host, readable in this session. All
databases were copied to `/tmp/wk-usage` before any query, so the live WAL files
were not opened. Queries are scripts in `/tmp/wk-usage/q*.py` (reproduced below).

**Read this first — the store is dominated by test runs.** Running the
repository's own test suite changes the user's data directory [V]:

| Probe (`go test -count=1 -p 2 ./...`) | memory dirs | sessionhost dirs | sessionhistory dirs | input_history lines |
|---|---|---|---|---|
| before | 1976 | 2320 | 1976 | 20342 |
| after | 1981 | 2326 | 1981 | 20395 |
| per package (`internal/tui`) | – | – | – | +53 |
| per package (`internal/wiring`) | +5 | +6 | +5 | 0 |

So 1,981 of the store directories are largely produced by tests, not by
chat use. The top `input_history` lines are `line one`/`line two`/`line three`
(1,231 each), `/new` (1,009), `/cwd` (1,002), `hello` (975): test fixtures.
Every number below is therefore an **upper bound on real use**, and the
test leak is a finding of its own (see "Findings outside C1–C4").

## C1 — Daemon / remote stack

**What it is.** A Connect-RPC daemon (`wakil daemon`), a remote facade that lets
the TUI attach to it, token/OIDC auth, a web UI, and the persistent session host.

| | Value |
|---|---|
| Non-test code | `internal/server` + `internal/remote` + `internal/core/sessionhost` + `internal/auth` + `internal/staging`: 10,560 LOC, 38 files [V] |
| Tests | 13,011 LOC, 351 `Test*` [V] |
| `cmd/wakil` daemon files | `daemon.go`, `daemon_server.go`, `daemon_signal.go`: 858 LOC [V] |
| Web UI | `web/`: 4 files, 1,643 lines [V] |
| Generated | `api/gen`: 13,816 lines (from `api/proto`, 10 proto files) [V] |
| Importers | `internal/server`, `internal/remote`: only `cmd/wakil`. `sessionhost`: `cmd/wakil`, `server/connect`, `wiring`. `auth`: `cmd/wakil`, `server/connect`. `staging`: `agent`, `sandbox/hisn`, `wiring` [V] |
| Maintenance since 2026-09-01 | 45 commits, +2,986 −1,308 lines over these paths (`git log --since=2026-09-01`) [V] |

**Usage** (`q1.py`, `q4.py`, `q5.py`, over 2,314 sessionhost DB copies) [V]:
- 2,907 sessions, all created by `usr_local`; **0** `api_tokens`, **0** `web_sessions`,
  **1** `join_tokens`, **0** `backends`/`workspaces`/`agents`/`agent_revisions`.
- Only 2 of 2,314 DBs hold ≥5 user messages (672 and 364). All 2,290 user messages
  come from **9 distinct texts** (`hello` 836, `continue` 716, `write the file` 418,
  `task` 112, `print hello` 60, `clean tmp` 60, `go` 32, …). That is test-fixture
  text, not conversation.
- First/last session: 2026-08-20 … 2026-11-05.

**Reading.** No evidence of daemon/web/remote use by a person: no API token, no
web login, one join token. The stack is exercised by the test suite. The session
host as a *persistence layer for the local chat* is separate from the remote/auth
surface and is wired into `wiring` and the chat path [inferred — confirm].

**Removal risk.** High for `sessionhost`, `staging` (imported by `agent`,
`wiring`, hisn); lower for `server`, `remote`, `auth`, `web` (reached only from
`cmd/wakil`). Generated code and the `SessionStateService` RPC are used by the
remote facade; `internal/remote` removal would also take `internal/server`'s
`SessionStateHandler`.

**Recommendation.** Do not delete in this pass. If the stack is not in use,
remove in layers, each its own release: (1) web UI + OIDC + token auth
(`web/`, `internal/auth`, auth handlers) — no importers outside the daemon;
(2) `internal/remote` + `internal/server`; (3) keep `sessionhost`/`staging`
unless the chat path is first detached from them.

**Removal plan per option.**
- *Keep:* nothing to do; note the test leak below.
- *Remove web+auth:* drop `web/`, `internal/auth/**`, auth handlers in
  `internal/server/connect`, proto auth services, regenerate `api/gen`; remove
  `docs/remote-provisioning.md`, the compose OIDC/token settings; migrate nothing
  (0 tokens in the store).
- *Remove daemon entirely:* additionally drop `internal/server`, `internal/remote`,
  `cmd/wakil/daemon*.go`, `Dockerfile.daemon`, daemon compose service, the e2e
  daemon tests; `sessionhost` stays while `wiring` uses it.

## C2 — Skill auto-creation and skill templates

**What it is.** `/skill-creation off|on|auto|auto-mashura`, a draft pipeline,
suggestion hints appended to tool results, `/skills` review commands, and the
`apply_skill_template` / `capture_skill_template` tooling.

| | Value |
|---|---|
| Non-test code | 13 Go files; 6,212 LOC [V] |
| Tests | 4,194 LOC, 189 `Test*` [V] |
| Largest files | `skill_template_handlers.go` 1,470, `skill_creation_pipeline.go` 1,004, `skill_template.go` 930, `skill_handlers.go` 562, `skill_creation.go` 495 [V] |
| Store | `internal/memory/store_skills.go` 406 LOC; global `~/.local/share/wakil/skills/skills.db` [V] |
| Config | `skill_creation.*` keys (`docs/configuration.md:124-128`), 135 LOC `internal/config/skill_creation.go` [V] |
| Maintenance since 2026-09-01 | 19 commits, **+9,154 −241** lines — nearly all the code was written in this window [V] |

**Usage** [V]:
- `skills.db` copy: **0 entries**, 1 `meta` row (`q3.py`). No skill has ever been
  stored through this store on this host.
- `input_history`: 0 lines starting `/skill-creation`, `/skill-create`, `/skill`,
  `/workflow`, `/plan`, `/collapse`, `/browser`, `/daemon`.
- The default mode is `"on"` (`internal/config/skill_creation.go:36`,
  `docs/configuration.md:125`), so the hint path is active unless disabled.
- **`list_skills` in this session returns 16 skills** (e.g. `task-workflow`,
  `general-cleanup-pass`). They live in the main skill store the tool layer
  reads, not in the `skills.db` copy above [inferred — confirm]: the 0-entry
  count describes the on-disk `skills/skills.db` path only, not the skill set the
  agent can load.

**Incident evidence (added at the user's request).** The suggestion hint is
appended to tool results as text beginning `[app note — not from the user: …]`
(`internal/agent/skill_suggestion.go:109,118`; max 2 per session,
`skillHintMaxPerSession = 2`, line 44). During this cleanup pass it appeared
**three times** inside tool results that were otherwise ordinary command output:
twice in the earlier sessions (`edit_file` result for `ci.yml`; a later tool
result) and once in this one (a `run_shell` gate readback). Each time the note
asked the agent to draft a skill from the workflow. The wording is a nudge, but:
- it is **model-visible text injected into tool output**, so it is
  indistinguishable by position from content a tool or a fetched page could
  contain; the label says "not from the user" but relies on the model obeying it;
- it fired during a task whose own rules said to ignore unrequested actions, and
  it fires on *length of the tool sequence*, not on any sign the work is reusable;
- the `skill-creation` default is `on`, so a user who never ran `/skill-creation`
  gets it.

**Removal risk.** Medium-high: `repostate.go` persists the mode;
`skill_suggestion.go` is called from the turn loop; templates are a separate
feature (the `skill_template*` files, 3,100+ LOC) that does not depend on
auto-creation [inferred — confirm].

**Recommendation.** Split the decision. (a) Default `skill_creation.mode` to
`off` and keep the code — a one-line default change that ends the injected note
for users who did not opt in. (b) Decide separately whether the draft pipeline
(`skill_creation_pipeline.go`, `skill_create.go`, `skill_creation.go`) earns its
~1,800 LOC with zero stored skills. (c) Keep templates: different feature,
explicit tool calls only.

**Removal plan per option.**
- *Default off:* change `ModeOrDefault` and the docs row; nothing else. Existing
  repo-state overrides keep working.
- *Remove auto-creation:* delete `skill_creation*.go`, `skill_suggestion.go`,
  `skill_create.go`, `skill_secret_screen.go`, `repostate.go` `SkillCreation`
  field (keep reading old files, ignore the field), config keys, the `/skill-creation`
  command, docs; keep `store_skills.go` and `/skills`.
- *Remove templates too:* additionally delete `skill_template*.go` and the
  two template tools; the stored skills with template sections remain readable
  as plain skills.

## C3 — `/plan` vs workflow skills

**What it is.** The built-in `/plan` workflow engine (steps, oracle review,
verify, `/plan approve`) versus skills such as `task-workflow` that describe a
similar procedure in prose.

| | Value |
|---|---|
| Non-test code | `internal/agent/workflow_engine.go` 630, `internal/workflow/workflow.go` 610, `cmd/wakil/plan_gc.go` 229, `internal/agent/plan_gc_store.go` 35: 1,275 LOC across the `workflow`/`plan` non-test files [V] |
| Tests | 1,247 lines of workflow/plan tests in `internal/workflow` + `internal/agent` [V] |
| Docs | `docs/workflows.md` (40 lines) [V] |
| Maintenance since 2026-09-01 | 13 commits, +911 −209 over `internal/workflow`, `workflow_engine.go`, `plan_gc.go`, `plan_gc_store.go` [V] |
| Entry points | `/plan` handled in `internal/agent/commands.go`, `internal/tui`, `internal/wiring/hostturn.go`, `facade.go` [V, grep] |

**Usage** [V]:
- `input_history`: 0 lines starting `/plan` or `/workflow` (`grep -cE '^/(wf|workflow|plan)'` → 0).
- Sessionhost event kinds across all DBs: `workflow_turn_started` 468,
  `workflow_final_review` 152, `workflow_outcome` 128. These come from the same
  9 fixture texts, so they show the engine runs under test, not that a person
  uses it.
- 0 `plan/current` entries in any of 1,971 memory DB copies.

**Reading.** The two are not equivalent: `/plan` is an engine with state, gated
steps and a final review; a workflow skill is advice the model may or may not
follow. Nothing in the evidence shows `/plan` is used interactively on this
host, but `plan gc` exists *because* `plan/current` entries were written in
0.4.2–0.4.3 by the model following a skill, not by `/plan` [inferred — confirm].

**Removal risk.** Medium: the engine is wired through the turn path
(`hostturn.go`) and persisted state types; `plan gc` would lose its reason to
exist only if `plan/current` memory writes also go.

**Recommendation.** Keep for now; revisit when real usage data exists. Do not
replace `/plan` with a skill: a skill cannot enforce the oracle/verify gates.
If removal is wanted, remove the engine and keep `plan gc` until old
`plan/current` entries age out.

**Removal plan per option.**
- *Keep:* nothing.
- *Remove engine:* delete `internal/workflow`, `workflow_engine.go`, the
  `/plan` handlers in `commands.go`/`tui`/`wiring`, `docs/workflows.md`,
  `docs/PLAN-*.md` references; drop `workflow_*` event kinds (stored events stay
  readable only if the decoder keeps them — keep the decoder).
- *Remove `plan gc` as well:* only after 0.4.x `plan/current` entries are gone
  from user stores; otherwise orphaned proposals accumulate silently.

## C4 — `WAKIL_WORKSPACE_PATH`

**What it is.** An environment variable that overrides the workspace path used as
**store identity** (`config.go:1905-1910`), so a container and its host agree on
the same key. It is set per container in `docker-compose.yml:60,99`.

**Readers** (non-test, `grep -rn 'WAKIL_WORKSPACE_PATH'`) [V]:
`internal/config/config.go:1909` (the only code that reads it);
`docker-compose.yml:15,60,68,97,99,102`; `Dockerfile.daemon:72` (comment: not
set in the image). Callers of `cfg.WorkspacePath()` that turn it into store
identity: `agent/app.go:794`, `agent/project_identity.go:10`,
`wiring/conversation_manager.go:83,492`, `wiring/headless.go:275`,
`wiring/hisn_executor.go:36`, `wiring/sandbox_project.go:29`,
`wiring/bootstrap.go:489`, `cmd/wakil/daemon_server.go:79`, `cmd/wakil/daemon.go:242`.

**Two sources, one name.**

| | Chat / daemon | `plan gc` |
|---|---|---|
| Function | `cfg.WorkspacePath()` (via `agent.ChatProjectPath`) | `ResolveProject` (`cmd/wakil/plan_gc.go`) |
| Order | `WAKIL_WORKSPACE_PATH` → `WorkDir` (direct) / `HostWorkDir` (docker) | `--project` → cwd, then `Abs` + `EvalSymlinks` |
| Reads the env var | yes | **no** (deliberately, since 0.4.3) |

With `WAKIL_WORKSPACE_PATH` **unset**, they give the same store for the same
directory, including a symlinked cwd, in direct and docker mode — locked by
`TestResolveProjectMatchesChatForCwd` (`cmd/wakil/project_resolve_test.go`)
[V, test passes in the full run]. With it **set to another directory**, they
differ by construction.

**Evidence of harm.** On the user's host the variable pointed `plan gc` at
another workspace's store, `46bec6699c79ec8d`, until 0.4.3 made `plan gc` ignore
it (`CHANGELOG.md:24`; test `plan_gc_exists_test.go:67-98` sets a stale
`WAKIL_WORKSPACE_PATH` and asserts the cwd wins). The shell I ran in has no
`WAKIL_WORKSPACE_PATH` defined and the store dirs `46bec6699c79ec8d` and
`5d8efbe880d17149` are **not present** under `~/.local/share/wakil/memory` in
this environment [V: `ls` failed for both]; so I could not reproduce the
original symptom here. [inferred — confirm on the host.]

**Frozen keys** (any change orphans stores): `/mnt/wakil` → memory key
`f3fa3243c6fc07e5`, workspace key `f3fa3243…11ffb` (asserted in
`TestResolveProjectGoldenKeys`; the test hard-codes `/mnt/wakil`, so it is
environment-dependent — see open points).

**Options and their store-migration consequence.**
1. **Keep both sources, document (current).** No migration. Divergence remains
   whenever the env var is set; guarded only for `plan gc` by a test.
2. **Make `plan gc` also honour the env var.** Reintroduces the 0.4.2/0.4.3
   failure (gc opens a store chosen by a stale variable). Rejected by evidence.
3. **Make chat use the cwd identity (`ResolveProject`) and stop reading the
   env var for identity.** One source. **Migration:** every user who sets the
   variable (compose users; the daemon container) gets a *different* key, so
   their existing memory, session history and session-host DBs are orphaned.
   Needs a one-shot copy/rename from the old key to the new, or an explicit
   `WAKIL_WORKSPACE_KEY`. A behaviour release.
4. **Replace the variable by `WAKIL_WORKSPACE_KEY` (already supported,
   `internal/agent/workspace.go:47`)** and keep `WAKIL_WORKSPACE_PATH` for file
   operations only. Plan reviewers could find no file-operation-only reader
   (all readers are identity uses, see above), so this needs a new file-ops
   reader or removal of the claim in `docker-compose.yml:68`. Migration: set the
   key once; keys unchanged, so no store moves. Note: `docker-compose.yml:66`
   tells operators to run `wakil --print-workspace-key <dir>`, but no Go code
   implements that flag (grep over tracked `*.go`: 0 hits) [V]; the compose
   comment's python one-liner is the only working way to get the key today.

**Recommendation.** Option 4, staged: first add a startup warning when
`WAKIL_WORKSPACE_PATH` is set and differs from the cwd identity; then deprecate
in favour of `WAKIL_WORKSPACE_KEY`. Do not do option 3 without a migration.

## Findings outside C1–C4

1. **Tests write into the user's real data directory** [V, table above]. A
   `go test ./...` run adds ~5 memory/sessionhost/sessionhistory directories and
   ~50 `input_history` lines to `~/.local/share/wakil`; `internal/wiring` and
   `internal/tui` do it. Repro: record `ls ~/.local/share/wakil/{memory,sessionhost}
   | wc -l` and `wc -l < ~/.local/share/wakil/input_history`, run
   `go test -count=1 ./internal/wiring ./internal/tui`, compare. Tests should
   set `XDG_DATA_HOME`/`HOME` to a temp dir. This inflates every usage number
   above and pollutes real history (`line one` ×1,231).
2. **`docker-compose.yml:66` documents a flag that does not exist**
   (`wakil --print-workspace-key`); see C4 option 4. Docs-only fix or implement it.
3. **`TestResolveProjectGoldenKeys` hard-codes `/mnt/wakil`.** `ResolveProject`
   runs `EvalSymlinks` only when the path exists; where it does not, the key is
   still computed from the string, so the test passes on other machines unless
   `/mnt/wakil` is a symlink there. Mashūra flagged it as environment-dependent;
   not changed in this pass (listed in the open points of `CLEANUP-2026-11.md`).

## Queries used

`/tmp/wk-usage/q1.py` — table row counts and event kinds across
`sessionhost/*/sessionhost.db`; `q2.py` — workspace column histogram;
`q3.py` — `skills.db` and `memory/*/memory.db` entry counts by status and
`plan/current` presence, plus `grep -cE` counts over `input_history`;
`q4.py`/`q5.py` — `user_message_committed` payload text histogram.
Core SQL: `select kind,count(*) from events group by kind`;
`select created_by,count(*) from sessions group by 1`;
`select workspace,count(*) from sessions group by 1`;
`select status,tier,count(*) from entries group by 1,2`;
`select count(*) from entries where key='plan/current'`.
