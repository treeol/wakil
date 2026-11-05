# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.4.3] - 2026-11-05

### Fixed

- **Workspace roots reached through a symlink were rejected.** When the project
  directory is a symlink (for example `~/coding` → `~/Coding`), every in-workspace path
  failed with `path … is outside workspace …`, and git tools (`git_blame`, `git_show`)
  failed. Workspace roots are now resolved once, before containment is checked, in the
  direct executor, the sandbox backend's host-side identity check, and the path map. Escape
  protection is unchanged: a symlink inside the workspace that points outside is still
  refused, both with a plain and a symlinked root.
- **`wakil plan gc` reported zeros on a machine that had quarantined plans.** Under docker
  mode it derived the memory store from the container path, so it read a store that did not
  exist, and it printed `0 … 0 dead` without an error. It now resolves the host workspace's
  store, prints `store: <absolute path>` first, and exits with an error when the store is
  missing. With no `--project`, the store is the **current directory's** project (the same key a
  chat started there gets); it no longer follows `WAKIL_WORKSPACE_PATH` or the config workspace. `--project <dir>` selects another project and `--store <path>` names a database
  explicitly. Entries whose owner session cannot be checked (a stat error other than "not
  found") are kept, not marked dead. If the sessions directory itself is missing (for
  example a wrong `WAKIL_SESSIONS_DIR`), every owner is treated as unknown and `--apply`
  rejects nothing. Unknown or incomplete flags are a usage error (exit non-zero), so a
  misspelled `--apply` no longer runs as a silent dry run. A partial rejection failure exits
  non-zero.

## [0.4.2] — not released separately; included in 0.4.3

### Changed

- **`dispatch_subagent` / `dispatch_subagents` require `class`** (`read`, `search`, `edit`, `verify`). The class sizes the child's work budget: `workBudget = hardMax × (classPct − reservePct) / 100`, with classPct 40 / 25 / 60 / 40 and reservePct 8 by default (`subagents.budget.{read,search,edit,verify,reserve}`), floored at 50 KB. The budget is **bytes of tool output per turn** (the existing `TurnToolBudget` mechanism), not tokens. Behaviour change vs 0.4.1: the old fixed 35 %-of-hardMax clamp only ever lowered the budget; the class budget now *sets* it (e.g. `edit` is larger than before on big-context backends, `search` smaller). An explicit `subagent_turn_tool_budget` is kept as an operator **cap** — it can lower the class budget, never raise it. A missing/unknown `class` is a runtime `ERROR:` tool result. Config validation rejects any class budget that is not greater than the reserve.
- **Exhaustion always yields a structured partial.** An exhausted child already gets one tools-off final turn from the loop; if that answer is unparseable it gets exactly **one** more turn (a reserve prompt that replaces the generic JSON retry — never stacked on it). If that is garbage too, the result is `partial` with no findings and the raw tail (≤ 2 KB) in `remaining_scope`; nothing goes into `uncertainty`. "Ran out of budget" is never the whole result.
- **Every completion carries a harness-derived result**: `outcome` (`complete` | `partial` | `rejected` | `refused` | `failed`), `files_covered` / `files_not_read` / `files_extra` / `files_listed` from the read ledger, `remaining_scope`, `budget {used, limit, class}`. Ledger entries come from the child's actual `read_file` / `read_file_full` / `search_files`-with-path calls (recorded after the call **succeeds**, under the executor's canonical path, so `./a.go`, `a.go` and an absolute path are one file and a denied/failed read does not count) — never from the child's text. The child is told its assigned list.
- **Off-task rejection.** A dispatch with an explicit `files` list whose ledger shows **zero** listed files read is rejected: its findings are spilled to the cache (path named in the result), not returned; its `external_calls` record is kept. The harness re-runs it **once** with `You read none of the assigned files: …. Read them before reading.`-style ledger text (exempt from the smaller-scope rule — same scope is the point). A second rejection prints `subagent <id> off-task twice: read 0/<N> assigned files — assign differently or take over?` and stops the task. No `files` list → no rejection; the ledger is still returned.
- **Partial follow-ups are harness-enforced.** After a `partial`, a follow-up for the same task must cover a strictly smaller file set (proper subset) with different task text, once. Same files or a superset → refused, no model call. A second partial on the split, or a partial with neither findings nor unread files, prints `subagent <id> partial: <read>/<listed> files, <K> findings — split or take over?` and stops the task. **A stopped task refuses further dispatch until the user sends the next message** (async-wake continuations, internal stream retries and the workflow engine's automatic `continue` turns do not lift it). An identical dispatch already in flight is refused too (no duplicate runs). The parent never silently takes over; the old "consider … taking over" hints in the incomplete warnings are gone. Tasks are linked by file set + task-text hash, so a dispatch with no `files` list can only be linked by identical text.
- **TUI child-completion line** shows `files <read>/<listed>` and `partial` / `rejected` / `refused` (info panel; rides in the existing finished-status field, no schema change).
- **Collapse notices aggregate by tool name** (`read_file ×40, run_shell ×20, … (+N more tools)`, top 4 by count) in the per-request collapse notice and the compaction notice; `call_…` ids are no longer printed there. The inline working-set note (`buildCollapseNote`) is sorted by count but not capped. Other surfaces (daemon web console, info-level logs) were **not** audited.
- **`plan/current` quarantine.** In a tainted session (touched external content) writes to the exact key `plan/current` go to `plan/tainted/<chatID>/current` (a proposed, quarantined entry); the session reads its own back; untainted sessions never see `plan/tainted/` in search/list; `/plan status` says `tainted — not handed off`. (`/taint` is a display command; it does not force a taint.)

### Added

- **`wakil plan gc [--apply]`**: lists *quarantined (proposed)* `plan/current` and `plan/tainted/<chatID>/current` entries whose owning session file no longer exists. Dry-run by default; `--apply` **rejects** them (status → rejected; nothing hard-deleted). An active `plan/current` — the one a handoff reads — is never listed or touched; an entry with no attributable owner is kept.
- Optional `files` parameter on `dispatch_subagent` / `dispatch_subagents` (on the batch form the list is shared by every task in the batch — use `dispatch_subagent` for per-task lists).

### Removed

- `memory_preflight_recall` config key, `MemoryPreflightRecall` field and docs entry — a confirmed no-op (never read; `retrieveMemoryContext` runs unconditionally).

## [0.4.0] - 2026-11-04

### Changed

- **hisn is now the default sandbox backend when it is usable.** It is chosen only when *all* hold: no explicit backend (no `sandbox.backend` in the config file, no `WAKIL_SANDBOX_BACKEND`, no `--sandbox`/`--exec`, no `exec_mode`), `hisn check` passes, and `hisn >= 0.3.1` is in `PATH`. Otherwise Docker is used, and the startup banner says why (`default: docker (hisn 0.2.2 is older than 0.3.1)`); an unapproved or malformed `.wakil/sandbox.json`, an explicit `--workdir`, or a workspace containing Wakil's state directory also keep Docker. A host without hisn sees no behaviour change; set `sandbox.backend: "docker"` to pin Docker. `wakil doctor` reports the default and no longer fails for a missing Docker runtime when hisn will run.
- Under hisn >= 0.3.1 Wakil starts sessions from a generated `hisn-profile/v1` file (`hisn run --profile`) instead of a bare policy; the `[policy]` content is unchanged. hisn 0.2.x keeps the `--policy` path, and 0.3-only features on it are a hard error naming the feature. The generated profile lives in `~/.local/share/wakil/hisn/<chatID>.profile.toml`, never in the project (hisn's `E_PROFILE_UNTRUSTED`).
- The Docker sandbox image pins hisn **0.3.1** (was 0.2.2).
- The staging (kvr) tools no longer require Docker: under hisn they use an in-sandbox service (see Added).

### Fixed

- **hisn: `run_shell` lost its output and exit status** (`exit code unknown (completion marker missing …)` for every command that wrote output). The ctl exec handle closed the kept pipe read ends as soon as the exit line arrived, discarding output still buffered in them, and its stdout and stderr copiers wrote unsynchronised into the same buffer (the tool path passes one writer for both), which corrupted it and dropped the exit marker at its end. Output is now drained to EOF before the exit is reported (bounded by 2 s without progress, 30 s in total, and the caller's context, so a child that inherited the pipes cannot hang a call), copier writes are serialised, and the exit status comes from hisn's exit line.

### Added

- **Per-project sandbox overrides** in `.wakil/sandbox.json` (`hisn.services`, `hisn.forward`, `hisn.host_services`, `hisn.env_set`, `hisn.mounts_ro`, `hisn.mounts_rw`, `hisn.risk_ack`, `hisn.profile`; strict schema, documented with a full example in `docs/sandbox-backends.md`). The file is **read-only for the agent**: write/edit/delete/move through any executor (file tools, edit-tier subagents) and worktree patches that touch it are refused, and hisn masks it inside the sandbox.
- **`wakil sandbox trust [dir]`**: a `.wakil/sandbox.json` that asks for anything only takes effect once *you* approve its exact content (sha256 kept in Wakil's state directory, outside every sandbox). Interactive only (`/dev/tty`); there is no flag that approves.
- **Risk labels.** Wakil asks hisn for the profile's risk label before any session starts and refuses to start — naming `hisn.risk_ack` — when a label above `none` is not acknowledged by the project file (Wakil also asks for `data` and `kernel-surface`). Wakil never writes a `risk_ack` itself. The running session's own risk snapshot is checked after start. Banner: `sandbox: backend=hisn · risk=<label>`; new `/sandbox` command (profile path, risk and capabilities, services, forwards, grants, staging).
- **kvr as an in-sandbox service** (`sandbox.hisn.kvr_binary`, or `kvr-server` in `PATH`; or a project-declared `kvr` service): socket and snapshot under `<workspace>/.wakil/`, host client refuses symlinked or foreign sockets before every connect, a broken kvr never fails the session, and with no usable binary staging stays off with the reason shown (never a host-side kvr).
- `profiles/deploy-smaragd.toml`: docker work under hisn through a forwarded docker socket (root-equivalent). Ships with the `risk_ack` lines commented out; documented in `docs/sandbox-backends.md`.
- `staging.Client.SetPathGuard`; `sandbox.Status`/`StatusReporter`/`KVRProvider`; `exec.SandboxStatuser`.

### Known

- hisn integration tests (`TestIntegration*`) skip without a hisn that passes `hisn check`; the profile/risk/kvr/override tests need hisn >= 0.3.1. See `docs/follow-ups.md` for open points (mask vs `.wakil` rename, kvr socket peer-credential check, one `risk_ack` per project).

## [0.3.1] - 2026-11-03

### Fixed

- Request truncation: the current turn (user message, referenced files, and current-turn tool results) and attachments are never cut; only prior turns are reduced (by summarising compaction, or archived before dropping), and an over-limit current turn fails loudly without sending.
- Reviewer (Mashūra) dispatch sends documents whole or in labelled parts with ~200-token overlap, or fails loudly when an atomic source file or section cannot fit; per-file, total-source, briefing, and findings truncation caps are removed, and `mashura__*` async results are delivered whole.
- Incomplete reviewer replies (`finish_reason=length` / `stop_reason=max_tokens` after a chunked retry, empty replies, or missing/mismatched `LAST SECTION READ:` citation) are marked VOID and never counted as a review round or as "no findings".
- The "collapsed request" line (and compaction notices) names what was archived and where (message range, tool-result IDs, byte and approximate token counts, and archive path).
- `@file` attachments are injected whole, failing loudly above a 64 MB sanity limit instead of truncating at 50 KB / 800 lines.

### Added

- Optional `hisn` sandbox backend (`sandbox.backend = "hisn"`, flag `--sandbox hisn`, env `WAKIL_SANDBOX_BACKEND=hisn`; requires `hisn >= 0.2.1`): control-socket exec with `exec_id` (`SCM_RIGHTS` stdio fd passing, signals on fresh connections, latched acceptance/exit), `--ready-fd` readiness with live confirmation, per-project persistent home (`sandbox.hisn.home_root`), and `sandbox.hisn.*` configuration (`binary`, `home_root`, `grantable_rw`, `env_pass`, `tmp_size_mb`, `shm_size_mb`, `exec_via_cli`).
- `docker_allow_userns` (`sandbox.docker.allow_userns`, off by default) to allow unprivileged user namespaces inside the Docker sandbox container for `hisn` development.
- Per-model reviewer limits under `mashura_models.<model>.{context_tokens,review_max_tokens}`.
- Per-model and per-role reasoning budgets (`mashura_models.<model>.reasoning`, `mashura_roles.{reviewer,check}.reasoning`) with `/think` and `--think` runtime control; reviewer reasoning is capped at 16384 tokens.
- Streaming reviewer calls with idle timeout (120 s no bytes) and a 30 min total cap, replacing the 300 s total timeout; `reasoning N / answer M` in the Mashūra footer.
- `context_tail_answer_bytes` now defaults to `0` (prior assistant reports in the long-session tail are kept whole by default).

### Known

- Reviewer models with large reasoning output can hit the answer budget (fix planned); the 300 s request timeout is now a 30 min total cap with a 120 s idle deadline.
- Browser tools (`browser_*`) and staging (`kvr`) tools are unavailable under the `hisn` backend.

[0.3.1]: https://github.com/treeol/wakil/compare/v0.3.0...v0.3.1
