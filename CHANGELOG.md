# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.6.0-dev] - unreleased

### Added

- **Phase timeline: `WAKIL_TRACE=1` and `--trace`.** Writes the milliseconds each
  phase of start, `/new`, `/resume`, `/handoff` and exit takes
  (`timeline-<stamp>-<pid>.json` in the trace directory; the path is printed
  after the terminal is restored, never during the alternate screen). Covers
  config load, the hisn probes, project-file hash and approval, signing
  resolution, every store open, the provider probes, the lazy sandbox session
  start, session stop and the history flush, and the time between the kernel
  starting the process and `main` (package initialisation). `--trace` keeps its
  JSONL capture and now also writes the timeline; the standing `trace_sessions`
  setting does not. `scripts/perf/run.sh` drives a scripted session through a
  pty against a local stub provider and summarises the numbers; results are in
  `docs/PERF-0.6.md`.

### Changed

- **hisn 0.4.0 is the minimum, and the session model follows it** (ADR 0012).
  Against hisn 0.4.0 every session start timed out after 10 s although the
  session was up: `hisn ls --json` now reports schema `hisn-ls/v2` with state
  `running` where 0.3.x said `live`, and Wakil only accepted `live`. Wakil now
  reads both schemas (and refuses an unknown one) and treats `creating`,
  `stopping`, `stopped`, `crashed` and `stale` as not live. Sessions are created
  with `--name wakil-<project key>-<chat id>` and addressed by that name; every
  control request carries a client record (`label` `wakil@<host>`, a `req_id`
  unique per request) so hisn's per-client audit shows which Wakil acted; a
  session ends with `hisn stop` (directory kept, audited) and `hisn ls --prune`
  archives it; a `crashed` leftover that holds the chat's name is pruned before
  the next start. The doctor, the default-backend gate, the sandbox image pin
  (Dockerfile, SHA256s from the v0.4.0 release) and the fixtures moved to 0.4.0.
  hisn 0.3.x is refused with a message naming 0.4.0.

- **LSP servers are probed before launch.** When a language server binary is
  missing from the sandbox (for example `gopls` in the hisn rootfs), the LSP
  tool reports `gopls not found in the sandbox (rootfs <name>); install it in
  the image` instead of a startup crash. The rootfs name is omitted as "host
  toolchain" when the executor runs outside a named rootfs. Installing the
  binary and retrying works without restarting Wakil.
- **hisn is auto-selected only when it passes the explicit-backend gate.**
  The default-backend version floor is now the same as the explicit floor
  (`MinHisnDefaultVersion = MinHisnVersion`, 0.3.3 when this was written, 0.4.0
  now). Previously 0.3.1 could be
  chosen as the default and then refused at startup. A host with hisn 0.3.1 or
  0.3.2 now gets Docker or Podman by default, as it would for a missing hisn.
  Set `sandbox.backend: hisn` only with 0.3.3 or newer.

### Removed

- **Telegram approval bridge.** The `telegram-bridge` MCP integration — the
  TUI-side approval consumer (`internal/wiring/telegram_consumer.go`) that
  mirrored `ApprovalRequested` events to a Telegram message with inline buttons
  and fed the button press back through `RespondToApproval` — is gone. The TUI
  keypress is the only approval path again. The consumer was the only caller of
  `sessionhost.Host.NotifySessionNote`, which remains as a public host API
  (with its tests) but is no longer wired into a client. This removes the
  long-polling contention the `MCPManager` `Close` path was written to guard
  against; the manager teardown itself is unchanged.
- **Daemon, remote TUI, web console and Connect API** (`wakil daemon`,
  `--daemon`, `--socket`, `DaemonMode`/`DaemonSocket`). Evidence and decision
  in `docs/adr/0001`: no web sessions and no API tokens in use. The local TUI,
  `wakil run` and the hisn backend are unchanged. Removed with the transport:
  `internal/remote`, `internal/server/connect`, `internal/protoconv`,
  `internal/auth` (apitoken, jointoken, oidcresolver, tokenresolver,
  tokenstore, peercred), `api/`, `web/`, `docker-compose.yml`,
  `Dockerfile.daemon`, `buf.yaml`, `buf.gen.yaml`, `docs/remote-provisioning.md`.
  Dead code the removal left behind was deleted in the same change.
- Process records no longer kept in `docs/`: the 0.4.x and 0.5.x plan and close-out
  documents, the November cleanup and decision sheets, the inventories, and
  `design-note-a1-a6.md`. Their decisions are in `docs/adr/`; the text stays in git history.

### Kept

- **Skill auto-creation** (`/skill-creation`, the `skill_creation` config,
  suggestions, and every `/skills` subcommand) is kept. Suggestions are off by
  default; the user opts in. See `docs/adr/0002`.

## [0.5.2] - 2026-11-07

### Changed

- **Sandbox image pins hisn 0.3.3.** `Dockerfile` pins the 0.3.3 release for
  the variants it already used (amd64, arm64), SHA256-verified. The hisn
  minimum is 0.3.3 everywhere it is checked (startup refusal, profile feature
  gates, docs); hisn 0.3.2 is refused with a message naming 0.3.3.
- **known_hosts mount uses `src:dst` (deviation removed).** The P3 profile
  mounts the host `~/.ssh/known_hosts` read-only at `/wakil/ssh/known_hosts`
  inside the session, and `GIT_SSH_COMMAND` pins `UserKnownHostsFile` to that
  in-session path. The same-path form and its deviation note are gone.
- **First-run acknowledgement without hand-written JSON.** `wakil sandbox trust`
  with no `.wakil/sandbox.json` shows the capability labels and the resulting
  risk and writes `{"hisn":{"risk_ack":"<risk>"}}` only after an explicit `yes`
  (anything else deletes the file it wrote). A bootstrap-form file whose ack no
  longer matches is updated the same way. A hand-written file is never rewritten;
  the command names the key to change. In the interactive TUI, a missing or
  mismatched ack is offered inline (`acknowledge now? [y/N]`). Non-interactive
  runs (`wakil run`, `--print-*`, scheduled) never prompt and keep the hard error.
  The startup error now names `wakil sandbox trust` and the labels.

### Unchanged

- No acknowledgement is written without an explicit `yes`/`y` in that invocation.
- F1 stays fail-closed for unparseable and unapproved project files.
- Home-persist under Wakil's own root stays self-acknowledged.

## [0.5.1] - 2026-11-06

### Changed

- **Per-repo signing-key resolution (P3).** `ssh_signing = auto` under
  `sandbox.hisn` now resolves WHICH key per repository, on the host, at plan
  time — reading the repo's effective git config with the repo as cwd, so a
  repo-local `.git/config` wins over global, exactly as a host commit would
  resolve it. `commit.gpgsign` not true → signing is OFF for the session
  (nothing injected, no allowed-signers file, no `[ssh]`-signing config);
  `commit.gpgsign` = true → the repo's `user.signingkey` is resolved
  (`key::` literal, `.pub` path, or private path + `.pub` sibling — relative
  paths resolve against the repo), its fingerprint must be holdable by the
  host ssh-agent (`ssh-add` attempted once when a private path is known), and
  the plan FAILS loud — naming the repo, key, and fingerprint — if the key is
  unset or cannot be loaded. A git invocation that itself fails (missing git,
  unreadable repo) fails the plan too: never a silent unsigned start. The
  anti-shadowing guarantee is unchanged: the resolved key is injected at
  COMMAND scope via `GIT_CONFIG_*`, so a repo-local `.git/config` or a
  user-typed command still cannot change the signing choice.

### Removed

- **`sandbox.hisn.ssh_keys` no longer gates signing.** The signing key's
  fingerprint was previously required to be in `ssh_keys` (the 0.5.0 F5 check,
  global-scope resolution). `ssh_keys` is now purely the agent-forwarding
  allowlist — which keys the proxy exposes in-session — and the resolved
  signing fingerprint is appended to it automatically. The per-repo resolution
  replaced the global-only `DetectSigningTrusted` path.

### Migration

- A signing repo needs `sandbox.hisn.ssh_agent = true` (unchanged). Nothing
  else to migrate: repos that don't sign now simply start unsigned instead of
  demanding a global signing key.

## [0.5.0] - 2026-11-06

### Review status

- P3 (hisn git identity and ssh signing) was reviewed by one reader only; its
  two blocking items are fixed in `374dd66`. The rest of P3 was not reviewed.
  Two review attempts failed; the plan record was removed in the 0.6.0-dev
  cleanup and its open items are in `docs/adr/0010-open-items-carried-forward.md`.

### Changed

- **Per-project identity (C1).** Chat stores (memory, session host, session history,
  staging, hisn home) are keyed by the project directory (`ResolveProject`, symlink-resolved),
  the same key `wakil plan gc` uses. `host_workdir` now only sets the docker bind mount, and
  `WAKIL_WORKSPACE_PATH` no longer changes store keys (startup line says IGNORED).
  The sandbox trust approval key is unchanged (it is a security approval, not a store key).

### Added

- **hisn rootfs (P2).** `sandbox.hisn.rootfs` (store name or absolute path) and the project override `"hisn": {"rootfs": "<name>"}` (name only) select the session root filesystem; the generated profile carries `[rootfs]`. Needs hisn >= 0.3.2. A missing store rootfs fails at session start with the `hisn rootfs import` fix. Not combinable with `hisn.profile`.
- **Legacy shared store is readable (C2).** The pre-0.5.0 shared memory store is opened
  read-only (`mode=ro`) and read after the project store: `memory_get` and `memory_search`
  fall through to it and mark results `[legacy]`. Sessions recorded under the old shared path
  appear in the resume picker under a "legacy (shared store, read-only)" heading.
- **`wakil state migrate --from-legacy [--apply]` (C3).** Copies legacy entries whose writer
  session or absolute anchors place them in exactly one known project. Dry-run by default;
  `--apply` only adds. Nothing is deleted and the legacy file is not written.
- **First-start notice and legacy count (C4).** One notice line per project when a legacy
  store has entries. `--print-workspace-key` shows the legacy key and its active-entry count.

### Migration

- On first start, each project with a legacy store prints one notice line. Nothing is moved.
- Run `wakil state migrate --from-legacy` to see the counts (dry-run), then add `--apply` to
  copy attributable entries. Legacy entries that cannot be attributed stay in the legacy store.
- Sessions are not moved; they stay listed under "legacy".

### Known limitations

- **Browser under hisn: name resolution fails on hosts where `/etc/resolv.conf` only lists
  the systemd-resolved stub (127.0.0.53); hisn 0.3.2 drops loopback resolvers and leaves
  none. Fixed in hisn 0.3.3.** Host evidence: network reachable by IP, `getent` fails,
  `/etc/resolv.conf` empty inside the session.

## [0.4.5] - 2026-11-06

### Fixed

- **Test isolation.** `internal/config` and `internal/exec` tests now run under the isolated
  HOME (`testutil.Main`), like the other packages. Before, `LoadConfig(nil)` in config tests
  created `~/.config/wakil/config.json` in the real home, and the H1 guard in `internal/agent`
  reported those writes as its own under `go test -p N`.

## [0.4.4] - unreleased

> Release state not confirmed. Local tag `v0.4.4` (commit `b41c750`) exists and
> `git ls-remote --tags origin` lists it, so this section may have been published.
> Verify with `git ls-remote --tags origin v0.4.4` before treating it as unpublished.

### Deprecated

- **The daemon, the remote TUI and the web console.** `wakil daemon`, the remote TUI
  (`--daemon`) and the web console (`wakil daemon --http-addr`) print one line on start —
  "will be removed in a future release" — and otherwise work as before. Nothing is removed
  in 0.4.4. (The user's host data shows no sessions created through them.)
- **Skill auto-creation** (`/skill-creation`): the default mode is `off`
  (see Changed); an explicit `skill_creation` setting still wins. Skill templates, the
  skills store and `load_skill` are unchanged.
  - *Correction (2026-11-07):* this entry's "Deprecated" heading is withdrawn.
    Skill auto-creation is not deprecated; it stays, off by default. See
    `docs/adr/0002`.

### Changed

- **`skill_creation` defaults to `off`.** Suggestions used to be on by default and were
  appended to tool results as "[app note]" text; when a suggestion is enabled it is now a
  status notice and no longer enters a tool result.

### Fixed

- **Completion envelopes carry the exit code and the output.** The notice for a finished
  `notify_on_exit` / auto-backgrounded shell showed only the last two log lines and a
  `read_process_log` hint. It now carries the status (`exited with code N`) and the output:
  everything up to 6 KiB, otherwise the first 2 KiB, an explicit `N bytes omitted, full log:
  <path>` marker and the last 4 KiB. The rendered message stays under the 8000-byte envelope
  and tab caps.
- **`read_process_log` finds finished background jobs.** Finished jobs are kept for the rest of
  the chat (last 32 jobs / 64 MiB of log, oldest first; a log being read is never evicted;
  logs are removed at shutdown). An evicted job reports `evicted` with the bound instead of
  "no background process with id".
- **`read_file` / `read_file_full` can read the sandbox's own `/tmp`** (docker and hisn
  backends; read-only; fail-closed canonicalisation inside the sandbox). Writes, edits, moves,
  deletes, `list_dir` and `search_files` stay confined to the workspace; the direct executor,
  where `/tmp` is the host's, keeps rejecting.
- **`run_shell` / `run_background` run under bash when the sandbox has it** (`sh` otherwise;
  the tool descriptions say which). Bash constructs such as `${PIPESTATUS[0]}` or `[[ ]]` work
  and detached jobs report their real exit code; a bash syntax error is exit status 2 instead
  of "code unknown". Helper commands Wakil builds itself stay `sh`.
- **Collapse never inflates the request.** If the collapsed form of a turn (or the
  `collapse_context` checkpoint) would not be smaller than the original, the original is kept
  and the notice / tool result says so. A partial collapse that is upgraded later is measured
  against the form it replaces (what the request carries for that turn now), not against the
  original turn; an upgrade that would grow the request is refused and the previous form stays.
- **A read of a background job pins its log.** `read_process_log` holds a reference on the job's
  entry for the whole read, so retirement and eviction cannot delete the log mid-read; the entry
  is retired or evicted when the last reader releases it.
- **A `run_shell` that finishes exactly at its deadline is reported as finished.** The deadline
  timer used to race the reaper and could report "entry no longer tracked" for a completed job;
  completion is now re-checked under the same lock the reaper takes.

- **The headless browser starts on first use, stops when idle, and listens only on loopback**
  (docker backend). Previously the browser and a CDP relay were started with the container
  whether or not the browser was used; the relay bound `0.0.0.0:9223`, the profile dir was the
  fixed `/tmp/wakil-chrome-profile`, and stopping was a `pkill -f`. Now Chromium launches on the
  first `browser_*` call as a direct child of a `docker exec` session (own process group, a
  per-launch profile dir that is also `HOME`, `--disable-crash-reporter --disable-breakpad`,
  Chromium-chosen debugging port) and is stopped by TERM on the group, KILL after 3 s, a sweep
  of stragglers that carry the profile dir, and removal of the dir. It stops after
  `browser_idle_timeout_seconds` (new, default 600; flat key like the other `*_seconds` keys,
  not a nested `browser.idle_timeout`), on `/new`/`/handoff`, chat end and exit; the idle timer
  never fires under a running call. The container no longer publishes a CDP port (the
  `Executor.CDPPort` method and `DockerOpts.BrowserEnabled` are gone) and the python relay is
  removed: the host opens a `127.0.0.1` listener whose connections are piped through
  `docker exec -i … python3`. Direct mode keeps chromedp's own launcher, now lazy with the same
  lifecycle. Under hisn the browser stays off. Chromium may still start `chrome_crashpad_handler`
  despite the flags (observed on the image's Chromium 156); those processes carry the profile
  dir and are swept on stop. Ownership is checked before any signal (a host pid is killed
  only if its start time and cmdline still match what was recorded at start; a reused pid is
  left alone and one line is logged). The browser belongs to the manager, not to the tool call
  that launched it: it survives the end of that call and stops only on idle, `/new`, chat end or
  exit. Direct-mode teardown waits a bounded 3 s for Chromium to exit, KILLs what still carries
  the launch's `--user-data-dir`, then removes the directory.
- **Helper commands no longer depend on the locale (L).** Executors parse the output and stderr
  of their own helper commands; under a German shell "Datei oder Verzeichnis nicht gefunden"
  was never recognised as "not found" (`TestBackendExecutorLazyStartAndFileOps` failed, and
  `apply_skill_template` into an empty directory failed with "cannot inspect existing
  target"). Helpers now run with `LC_ALL=C LANG=C`, and "not found" is decided by a `test(1)`
  exit status after a failed read. The model's own commands keep the user's locale.
- **Tests can no longer open anything on the desktop or start a real browser by accident.**
  `OpenOnHost` refuses in a test binary; direct-mode browser launch refuses in a test binary
  unless `WAKIL_BROWSER_LIVE=1` (headless, private profile). A test used to run the real
  `xdg-open` for `https://example.com`.
- **Direct-mode browser launch is private and checked.** It starts the real Chromium binary
  rather than a distro wrapper script, on a DevTools pipe (no TCP port) with a fresh profile,
  and fails with a clear error when the process it started exits at once (a handoff to an
  already-running browser) instead of reporting success.
- **Process ownership uses a pidfd** on Linux (taken at spawn, signalled through), with the
  start time and, where readable, the cmdline as extra guards.
- **`wakil --print-workspace-key` exists** (docker-compose.yml already told operators to run it).
  It prints the workspace path, the memory store key and db path, the workspace key, the
  sessionhost id/key and what `wakil plan gc` would resolve from the current directory, each with
  its source (cwd, `WAKIL_WORKSPACE_PATH`, config, `WAKIL_WORKSPACE_KEY`), and warns when a chat
  and `plan gc` would use different stores. It only reports: no key derivation changed and it
  creates no files.
- **`WAKIL_WORKSPACE_PATH` is announced.** When it is set, Wakil prints one startup line naming
  the path, the resulting memory and sessionhost keys and that it overrides the working
  directory (and, if it differs, which key `plan gc` would read); `/sandbox` shows the same line.
  No key changed: the variable stays supported.
- **Tests no longer write to the real user data dirs (H1).** `internal/testutil.Main` points
  HOME and the XDG dirs at temp dirs and fails the package if anything is created or appended
  under the real `~/.local/share/wakil` or `~/.config/wakil` (the wiring and tui packages leaked
  memory directories and `input_history` lines). `WAKIL_TEST_ALLOW_REAL_DIR_CHANGES=1` skips the
  check.

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
