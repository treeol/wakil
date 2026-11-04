# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.4.2] - Unreleased

### Changed

- **Subagent dispatch requires a `class` parameter** (`read`, `search`, `edit`, `verify`) on `dispatch_subagent` and `dispatch_subagents`. The class determines the subagent's token budget as a percentage of its context window: `read` 40%, `search` 25%, `edit` 60%, `verify` 40%. A reserve (default 8%) is held back for the final report turn and never spent on work. Work budget = `hardMax × (classPct − reservePct) / 100`; reserve = `hardMax × reservePct / 100`. Overrides via `subagents.budget.{read,search,edit,verify,reserve}` in the config file. Existing callers that omit `class` get a compile-time error (the parameter is required in the tool schema and in every dispatch function signature).

- **Structured partial results on budget exhaustion.** When a subagent exhausts its work budget, the harness stops dispatching tool calls and forces exactly one final turn with the reserve budget, injecting a fixed instruction to emit a structured partial (`status`, `files_covered`, `files_not_read`, `findings`, `remaining_scope`, `budget`). "Ran out of budget" alone is never the result — a subagent that fails to produce the structure still returns a partial with empty findings and the raw tail in `remaining_scope`. The parent may re-dispatch only with a strictly smaller scope (proper file subset + different task text), one split level; a second partial or a partial with no findings and no `files_not_read` is surfaced to the user as a one-line notice and the parent stops auto-dispatching.

- **Per-file read ledger (off-task rejection).** When a dispatch carries an explicit `files` list, the harness records every `read_file` / `read_file_full` / `search_files`-with-path call and derives `files_covered` / `files_not_read` / `files_extra` from actual tool calls — never from the subagent's text. Zero listed files read → the result is `rejected:off-task` and its findings are discarded; the parent re-dispatches once with the ledger appended, and a second rejection is surfaced to the user (the parent never silently takes over).

- **Collapse line aggregates by tool name.** The collapsed-request line and compaction notices now aggregate dropped tool results by tool name (`read_file ×40, run_shell ×20, … (+N more tools)`), sorted by count descending, top 4 shown. `call_…` tool-result IDs are never printed in user-facing output (TUI, daemon web console, info logs); they remain in the trace.

- **`plan/current` quarantine in tainted sessions.** In a tainted session (taint guard active or `/taint` set), writes to `plan/current` are redirected to `plan/tainted/<chatID>/current`; the handoff reader ignores `plan/tainted/` entirely, so a plan from a session that touched external content is never handed off. `/plan` status says "tainted — not handed off" when it applies.

### Added

- **`wakil plan gc`** (CLI): lists `plan/current` and `plan/tainted/*/current` entries whose owning session no longer has a session file on disk (same liveness check `/resume` uses). `--apply` deletes the dead entries; default is dry-run with counts.

- Optional `files` parameter on `dispatch_subagent` / `dispatch_subagents` for the explicit-file-list read ledger and off-task rejection.

### Removed

- `memory_preflight_recall` config key, `MemoryPreflightRecall` field, and its docs entry — the field was a no-op (never read outside its declaration; `retrieveMemoryContext` runs unconditionally). Confirmed and removed.

## [0.4.0] - Unreleased

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
