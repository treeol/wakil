# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.4.0] - Unreleased

### Changed

- **hisn is now the default sandbox backend when it is usable.** It is chosen only when *all* hold: no explicit backend (no `sandbox.backend` in the config file, no `WAKIL_SANDBOX_BACKEND`, no `--sandbox`/`--exec`, no `exec_mode`), `hisn check` passes, and `hisn >= 0.3.1` is in `PATH`. Otherwise Docker is used, and the startup banner says why (`default: docker (hisn 0.2.2 is older than 0.3.1)`); an unapproved or malformed `.wakil/sandbox.json`, an explicit `--workdir`, or a workspace containing Wakil's state directory also keep Docker. A host without hisn sees no behaviour change; set `sandbox.backend: "docker"` to pin Docker. `wakil doctor` reports the default and no longer fails for a missing Docker runtime when hisn will run.
- Under hisn >= 0.3.1 Wakil starts sessions from a generated `hisn-profile/v1` file (`hisn run --profile`) instead of a bare policy; the `[policy]` content is unchanged. hisn 0.2.x keeps the `--policy` path, and 0.3-only features on it are a hard error naming the feature. The generated profile lives in `~/.local/share/wakil/hisn/<chatID>.profile.toml`, never in the project (hisn's `E_PROFILE_UNTRUSTED`).
- The Docker sandbox image pins hisn **0.3.1** (was 0.2.2).
- The staging (kvr) tools no longer require Docker: under hisn they use an in-sandbox service (see Added).

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
