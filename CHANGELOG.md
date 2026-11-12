# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **Pruning prominence and balance.** With pruning on, the pruning section (1126 chars) is now the FIRST section of the system prompt, before the agent operating instructions (full mode: 51179 chars, core mode: 17183 chars, measured), in full mode and core mode alike. The system section and the user-prompt suffix (296 chars) now say: tool results are re-sent in full until marked, mark finished results while you work, keep what you are still using. The cautions moved to the one place that keeps them — the `context_mark` tool description (736 chars): what drop/stub/keep mean, that a stub replaces the result so only the note remains and a thin note is rejected, that reading a file again after stubbing it costs more than keeping it, and that marking right before the final answer saves nothing. The backlog line and the reread line keep their text. Prune mechanics are unchanged; with prune off the wire is byte-identical.
- **Request debug dump (`WAKIL_DEBUG_REQUESTS`).** Debug aid (see `docs/configuration.md`): one JSON file per model request (<session id>-NNNNNN-<timestamp>.json, 0600 in a 0700 directory), messages and tool definitions as sent, no header and no key material; failure logs one warning and continues.

## [0.7.0] - 2026-11-12

### Added

- **Context pruning is new, experimental, and off by default.** Enable it per chat with `/prune on` (`/prune off`, `/prune status`) or globally with `context_prune: "on"`. With it on, the model marks tool results it has finished with using `context_mark`: it stubs a result with a note (only the note remains) or drops it entirely, and Wakil reminds it to do so in the system prompt, at the end of each user prompt and on tool results. Results the model does not rate are not pruned, apart from the two automatic cases: superseded rounds (the same file re-read, edited or written later, or the same command repeated) are auto-stubbed while the trigger runs, and at the emergency level (at least 90% of the hard max) Wakil can stub unrated rounds so the request fits. How much actually gets pruned depends on the model, and on `prev`/`[[…]]` ratings or a configured rating pass alongside `context_mark`. A pruned result is archived and can be re-read. The long-session collapse is unchanged and works alongside pruning. With it off, tool schemas, arguments and wire bytes are identical to 0.6.5.
- **Dynamic context pruning by per-round value** (Wakil 0.7 feature, behind a switch). New config keys `context_prune` (`off` default | `on`), `context_prune_at` (0.70), `context_prune_target` (0.55), `context_prune_protect` (3), validated to satisfy `0 < target < at < 1` and `protect >= 1`. When on, the model rates the previous tool round with `[[keep]]` / `[[drop]]` / `[[stub: <one line>]]`; tags are stripped before storing. When the projected request exceeds `context_prune_at` × the existing hard max, lowest-value rounds (drop before stub, oldest first) are replaced in the projection by one-line stubs; the original round is archived first (`round-<N>` in the tool cache). With `context_prune: off` (default), behaviour and wire bytes are identical to 0.6.5. See `docs/long-session-mode.md` and `docs/configuration.md`. Pruning is experimental and model-dependent: apart from superseded rounds and the emergency level, results are only pruned following a model rating, so what gets saved depends on how well the model keeps up with its own ratings.
- **context_prune_marks**: new key — `immediate` (default): rounds marked drop/stub via `context_mark` are pruned regardless of the trigger, at the first request build after the model's next real tool call (the last-N protection does not apply to marked rounds; `/keep`, pinned, collapsed, archive-failure and never-inflate protections still apply). `on-trigger`: trigger-based behaviour. Only `context_mark` marks are immediate; `prev`/`[[...]]`/rater-pass ratings keep trigger-based behaviour.
- **Prune defaults**: `context_prune_at_tokens`/`context_prune_target_tokens` default to `0` (fractions only — set both to enable absolute triggers) and `context_prune_rater` defaults to `"off"` (no rating pass; set `"main"` or an endpoint name to opt in).
- **context_mark**: the model marks earlier tool rounds by `[rN]` id (drop/stub/keep) so they can be trimmed; with the default `context_prune_marks: immediate` a marked round is pruned at the first request build after the model's next real tool call, regardless of the trigger (see above); marking never prunes by itself. `/mode status` shows `marks N`.
- **Prune rating pass** (off by default): when pruning is triggered with unrated rounds, one LLM call rates them (keep/drop/stub with a takeaway) so stubs carry what to remember — no re-reads. `prev` stays as an optional fast path; the pass is a separate request (cost counted normally) and never enters the transcript. New config: `context_prune_rater` (`"off"` default; `"main"` or an endpoint name opts in), `context_prune_rater_max_rounds` (`40`), `context_prune_rater_timeout` (`60`s). `/mode status` adds `passes N`; `/prune status` shows the rater model.
- **Prune reminder**: with pruning on, every request carries a short context-upkeep reminder at the end of each live user prompt (projection only: the stored conversation and the TUI keep the original prompt). It tells the model that `[rN]` results are re-sent in full until marked and asks it to `context_mark` results once it no longer needs them. It states the cost of marking too early (rereading a stubbed file means paying for it twice) and says to keep a result it will still cite if the note cannot carry what is needed. The model still decides what it marks; Wakil never marks for it (apart from the superseded/emergency cases above). About 170 tokens per live user prompt, byte-stable across requests (no prompt-cache churn). With pruning off the wire is unchanged.
- **Prune reminder everywhere the model looks**: with pruning on, the reminder is in three places. (1) A prominent `## Context pruning: mark rounds once you are done with them` section in the system prompt (first among the context rules; keeps "optional" and "a later pass will rate it" out unless a rater is configured). (2) The existing suffix on live user prompts. (3) One backlog line on the newest round's last tool result in the projection (rounds accumulate their own line as they become newest): `[context · unmarked for 3+ steps: r4 (~10k tokens), r5 (~4k tokens) · if you are done with one, context_mark it: stub (only your note remains: facts with file:line) or drop. Keep what you will still cite]` (rounds unmarked and at least 3 real rounds old, at most 6 named largest first, then `+N more`). A round that rereads a path an earlier model-marked pruned round had read carries the reread line instead: `[reread · you stubbed this file in r8 and are reading it again: your note did not carry what you needed. Keep this result until you no longer need the file, or stub it with a fuller note]` (`dropped` for a drop). Both lines are stored on the round record at recording time, so they are byte-identical on every request; neither is stored in `a.Conv`, archived or shown in the TUI, and both are absent with pruning off. The `context_mark` tool description says "Unmarked results are re-sent in full on every step". Unmarked rounds are not pruned except in the superseded/emergency cases above; with pruning off the wire is unchanged.
- **A mark takes effect after the next real tool call** (`context_prune_marks: immediate`): a round marked drop or stub with `context_mark` is pruned only once a round of other tool calls has been recorded after the mark. A lone `context_mark` right before the final answer prunes nothing while that answer is written (it saved nothing and made the model answer from its notes); its tool result says `… · takes effect after your next tool call`. In a mixed message such as `[context_mark, read_file]` the marks still apply at the next request build. Pending marks are kept in the round record (`mark_seq`, persisted with session and handoff) and apply under the same rule in a later request; the size trigger does not apply them early, except at the emergency level (≥ 90% of hard max), where pending marks are released before any unmarked round is stubbed. A pending round is sent with the bytes it had before the mark. `on-trigger` is unchanged.
- **Thin stub notes are rejected**: when `context_mark` stubs a round of 4 KB or more, the note must be at least `clamp(result bytes / 64, 200, 1200)` characters and, for a round over several files, name every file's base name. A failing note is rejected for that round only (the round stays as it was, the rest of the call is processed) and the tool result says why, e.g. `r8 not stubbed: note too thin for 38 KB across 4 files (32 characters, at least 608 needed). … (missing: authz.go, web.go), or drop it.` Drops and rounds under 4 KB are unaffected; never-inflate still applies. `context_mark` notes are stored up to 4000 characters (other rating channels keep 200) and checked as stored; never-inflate counts a note longer than 200 characters once per result of the round. Constants, no new config keys.
- **Prune texts say what a stub is**: the system prompt section, the user-prompt suffix, the backlog/reread lines and the `context_mark` description use the same terms: a stub replaces the result and only the note remains; the note carries every fact still to be used, per file, with file:line; marking is for results the model is finished with, not a step after every result; marking too early costs more than keeping it, so a result the model will still cite is kept until then. When a mark takes effect follows `context_prune_marks` (in the system section and at the end of the tool description); the `prev` sentence says its one-line stub is not enough for a large result.

### Fixed

- A mixed assistant message such as `[context_mark, read_file]` registers a round for the `read_file` call (it got no `[rN]` and could never be marked); only the `context_mark` call is excluded.
- `/mode status` `marks` survives a session resume and `/handoff` (it was reset to 0 while `pruned`/`stub`/`saved` survived).
- `/mode status` `prev missing` was always 0: its counter was no longer called anywhere. It counts assistant replies with tool calls (after round 1) that carried no `prev`; a reply made only of `context_mark` calls is not counted. It counts replies, while `unrated` counts rounds that no channel (`prev`, `[[...]]` tags, `context_mark`, rating pass) has rated, so the two numbers measure different things and need not be equal.
- The stream tag buffer drains an unclosed `[[` tail at end of stream, and double brackets that are not a rating tag, such as `[[notes]]`, are shown verbatim.

### Security

- Go toolchain bumped to 1.26.11 (GO-2026-6859, `net/http`).

## [0.6.5] - 2026-11-10

### Changed

- Requires hisn 0.4.2: `MinHisnVersion`/`MinHisnDefaultVersion` (config) and the hisn backend's `MinVersion` are 0.4.2; `install.sh` `HISN_VERSION` and the `Dockerfile` wakil-dev image pin (amd64/arm64 SHA256s from the v0.4.2 `SHA256SUMS`) are 0.4.2.

### Fixed

- Generated profiles are validated against hisn 0.4.2 strictness: service/host-service `ready_timeout` and `stop_grace` above 86400 seconds are now refused with a clear error naming the key.
- NUL bytes in generated-profile exec/argv/socket/env strings are now refused with a clear error naming the key.

## [0.6.4] - 2026-11-10

### Fixed

- Mashūra no longer marks complete reviews VOID when the reviewer cites a labelled block (diff/file) with its `──` header decoration.

## [0.6.3] - 2026-11-09

### Added

- **`wakil init`** creates the config: provider (OpenRouter or any
  OpenAI-compatible server), credentials, model list, one test call with a tool,
  approval mode and the sandbox probe. Interactive on a terminal, flags
  otherwise (`--provider --base-url --model --key-env --approval --sandbox
  --skip-test --force --yes`). There is no flag for the key itself. An existing
  config is refused unless `--force`, which backs it up first (0600).
- **`wakil config show [--json]`** prints the effective config with each value's
  source (default, file, env, flag). Secrets are never shown, not even in part:
  `set (<N> chars)` or `unset`.
- Endpoint field **`api_key_env`**: the name of an environment variable read at
  request time; an explicit `auth_header` wins over it.
- Top-level config key **`approval`** (`ask`/`auto`); `auto` is `--auto` for the
  interactive TUI (**`wakil run` ignores it**).

### Changed

- **First start no longer writes an endpoint-less stub.** On a terminal wakil asks
  "No config found. Run setup now? [Y/n]"; otherwise it prints
  `no config: run `wakil init``. Existing configs, including stubs written by
  older versions, load exactly as before.
- **`install.sh` writes no config and handles no key.** It keeps an existing
  config, or runs `wakil init` (non-interactively with `--key-env
  OPENROUTER_API_KEY` when that variable is set, so the key never lands in the
  file), or prints `next: run `wakil init``. Checksum and tamper checks are
  unchanged. `config.quickstart.json` remains a release asset and is now embedded
  in the binary as the single source of the default OpenRouter model.

### Fixed

- Error messages suggested the deprecated `--exec direct`; they now say
  `--sandbox direct`.
- An OpenRouter 404 `free-model-training-violation` (your privacy settings
  exclude the model's only provider) is shown as one line with the settings URL
  instead of raw JSON, in `wakil init` and in chat; the raw body goes to the
  session log.
- In a container, when hisn fails on user namespaces, the startup banner and
  `wakil init` explain that Docker's default seccomp profile blocks them and what
  to do.

## [0.6.2] - 2026-11-09

### Changed

- **Docs state the sandbox boundary and its host-side exceptions** (`open_url`,
  lifecycle hooks, MCP servers, clipboard) and describe the browser under the
  hisn backend as it works today.

### Fixed

- **OpenRouter attribution now uses `https://wakiil.pro`** (HTTP-Referer; 0.6.1
  sent a wrong referer domain). OpenRouter treats this as a new app page; the
  app name (`X-Title: wakil`) and categories (`cli-agent`) are unchanged.

## [0.6.1] - 2026-11-09

### Added

- **`install.sh`: one-command installer.** `curl -fsSL
  https://github.com/treeol/wakil/releases/latest/download/install.sh -o install.sh
  && sh install.sh` installs Wakil, hisn 0.4.1 and (with root or sudo) kvr-server
  0.2.0, verifying every download against the release's sha256 sums before
  anything is extracted or installed. It verifies the cosign signature when
  `cosign` is present (`--require-signature` makes that mandatory), picks the
  glibc or musl hisn build, never overwrites an existing config and supports
  `--uninstall`. Every download is https-only. The manual install in the README
  stays as it was. It is attached as a release asset from this release on, so
  `releases/latest/download/install.sh` resolves.
- **`config.quickstart.json`.** A minimal OpenRouter config with a free,
  tool-capable model, written by `install.sh` with your key (mode 0600) when you
  have no config yet. It is a release asset, checksummed in `checksums.txt`.
  `config.example.json` is unchanged.

### Changed

- **OpenRouter attribution referer set to a domain the project does not own
  (corrected in 0.6.2).**
- **Sandbox image ships kvr-server 0.2.0.** The Dockerfile pins kvrust at
  v0.2.0 (commit `9b4c0f9…`, tag `v0.2.0`). v0.2.0 renamed the cargo binary
  from `server` to `kvr-server`; the build and COPY use the new name. The wire
  protocol, snapshot format KVR1 and the 1 MiB frame limit are unchanged;
  Wakil's staging client needs no change.
- **Symbol-map omitted-files count is fixed** (`ea4cdbe`). The `/repomap --symbols`
  marker no longer overcounts omitted files when the byte budget is hit, and an
  oversized single file now gets a `(truncated)` note instead of an "N files
  omitted" marker with a wrong count.

### Fixed

- **`install.sh` refuses non-https downloads.** curl runs with
  `--proto '=https' --proto-redir '=https'` and wget with `--https-only`; a wget
  that cannot enforce this (BusyBox) is refused.

## [0.6.0] - 2026-11-08

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

- **Startup no longer blocks the first prompt (B2).** The three provider probes
  (model list, backend list, context limit), the hisn orphan reconcile
  (`ls --prune`) and the lazy sandbox session start — with the staging/memory
  startup-note scans — now run in the background *after* the prompt is shown,
  instead of before it. In the build sandbox warm start to prompt went from
  669 ms to 51-58 ms (target < 200 ms); on the rubin timeline start to prompt
  went from 2990 ms to 149 ms, and the deferred blocks were 1226 ms (MCP),
  735 ms (probes) and 608 ms (reconcile) (`docs/PERF-0.6.md`). The status line
  shows `…` for the model until the context probe resolves it. Headless runs
  (`wakil run`) still do this work up front — they have no prompt to return to.
  `hisn ls --prune` in 0.4.0 has no name filter, so the prune cannot be scoped
  to `wakil-<projectkey>-` directories there; hisn 0.4.1 adds `--name-prefix`,
  which Wakil's own prune does not use yet (see Known follow-ups).
- **MCP servers connect in the background, concurrently.** `NewMCPManager`
  starts every connect at once and returns immediately; tools appear as each
  server becomes ready, and a tool call to a server that is still connecting
  waits only on that server's own readiness (bounded by the call context). The
  `/mcp` listing and info panel read status through a locked snapshot
  (`SnapshotServers`) instead of unlocked field reads. This removes the block
  that dominated the rubin start timeline.
- **Exit never waits on a winding-down turn.** The `50 ms x N` retry loop in
  `facade.Close` (about 2.6 s on the rubin timeline when a turn was still
  cancelling) is replaced by a turn-`done` signal: exit makes one release
  attempt and, if the turn is still winding down, releases in the background and
  returns. The wait is bounded by the tool's own cancellation, not a fixed sleep.
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
  hisn 0.3.x is refused with a message naming 0.4.0. (The minimum was raised
  again to 0.4.1; see the next entry.)
- **hisn 0.4.1 compatibility.** In 0.4.1 `hisn run --detach` prints the ready
  line and exits 0; the supervisor lives on as a separate process, so
  `Pdeathsig` no longer ties it to Wakil (`f352d56`). Wakil tells the two modes
  apart at start (the run process exits after the ready line, or it does not),
  and in the detached mode tracks the supervisor by its own pid. Four
  follow-on fixes (`fc0a1e3`):
  - `supervisor.pid` is parsed as `<pid> <starttime>`, the 0.4.1 format. The
    previous bare-pid parse failed every 0.4.1 start after the session was
    already up, and the failed-start path then reaped only the already-exited run
    process, leaving the session running (the session leak seen in the hisn
    integration run).
  - `Close`, `Stop` and a failed start now stop the real supervisor: its
    identity is established first (tracked pid and start time, otherwise the
    instance token), then `hisn stop`, then Wakil waits on the supervisor pid
    (or on `hisn ls` when the pid is not tracked). A foreign or unverifiable
    session is never stopped. Signals go only to the exact tracked process, and
    a zombie counts as exited.
  - On hisn 0.4.1 a `stopped` session keeps its name reserved (`hisn run --name`
    exits 125, `name ... is already in use`). On 0.4.1 or newer, `Start` prunes
    a stopped leftover of the chat before starting; 0.4.0 behaviour is unchanged.
  - The hisn integration tests stop and prune the sessions they create, and
    `TestMain` sweeps `wakil-it-*` and `toolpath-*` sessions before and after
    an integration run.
  - `procAlive` no longer reads a reaped supervisor as alive. A detached
    supervisor that died was a zombie until init reaped it; if the reap landed
    between `kill(pid, 0)` and the `/proc/<pid>/stat` read, the unreadable entry
    counted as "cannot prove it is gone", so `Start` kept the dead session and
    the next exec failed with `ErrSessionDead`. That failure reached the caller
    as one failed tool call (the call is not retried, and the following call
    started a fresh session), so a user-visible failure was possible, though
    only in that narrow window. A vanished entry (`ENOENT`, or `ESRCH` when the
    reap lands between open and read) with a readable `/proc` and a second
    `kill(pid, 0)` answering `ESRCH` now reads as gone; any other unreadable
    case still reads as alive.
  The minimum supported hisn is now 0.4.1: `MinHisnVersion`,
  `MinHisnDefaultVersion` and the hisn backend's `MinVersion` are 0.4.1, the
  sandbox image pin (`Dockerfile`, with the amd64 and arm64 SHA256s of the
  v0.4.1 release) is 0.4.1, and the README, `CONTRIBUTING.md`,
  `docs/configuration.md` and `docs/sandbox-backends.md` state it. A host with
  hisn 0.4.0 is refused with a message naming 0.4.1 (explicit backend) or gets
  Docker or Podman by default. The 0.4.0-only code paths remain and are still
  tested with the floor lowered in the test.

- **LSP servers are probed before launch.** When a language server binary is
  missing from the sandbox (for example `gopls` in the hisn rootfs), the LSP
  tool reports `gopls not found in the sandbox (rootfs <name>); install it in
  the image` instead of a startup crash. The rootfs name is omitted as "host
  toolchain" when the executor runs outside a named rootfs. Installing the
  binary and retrying works without restarting Wakil.
- **hisn is auto-selected only when it passes the explicit-backend gate.**
  The default-backend version floor is now the same as the explicit floor
  (`MinHisnDefaultVersion = MinHisnVersion`, both 0.4.1). Previously 0.3.1 could
  be chosen as the default and then refused at startup. A host with hisn older
  than 0.4.1 now gets Docker or Podman by default, as it would for a missing
  hisn. Set `sandbox.backend: hisn` only with 0.4.1 or newer.

### Removed

- **ilm-stack emitter.** The `internal/ilm` package (the shadow/assist emitter
  and its assist client), the `ilm_stack` config block and its env vars, the
  `/assist` command, and the emitter call sites in the agent and wiring layers
  are gone. The emitter's `close ilm emitter` step cost 5002 ms of a 5132 ms
  exit on the rubin timeline — a flush to an unreachable ilm endpoint hitting its
  fixed timeout — and the ilm-stack ingestion path is no longer used. See
  `docs/adr/0013`. `sessionhost.Host.NotifySessionNote` is removed with it (no
  caller since the Telegram bridge removal). In the build sandbox, exit with a
  live hisn session after the removal measured 116.7 ms (12 ms to terminal
  restore, 104.6 ms `hisn stop`); the post-removal exit on rubin has not been
  re-measured (`docs/PERF-0.6.md`). Old configs keep loading: an `ilm_stack`
  block is ignored and the `ILM_STACK_*` variables are no longer read.
- **Telegram approval bridge.** The `telegram-bridge` MCP integration — the
  TUI-side approval consumer (`internal/wiring/telegram_consumer.go`) that
  mirrored `ApprovalRequested` events to a Telegram message with inline buttons
  and fed the button press back through `RespondToApproval` — is gone. The TUI
  keypress is the only approval path again. This removes the long-polling
  contention the `MCPManager` `Close` path was written to guard against; the
  manager teardown itself is unchanged.
- **Daemon, remote TUI, web console and Connect API** (`wakil daemon`,
  `--daemon`, `--socket`, `DaemonMode`/`DaemonSocket`). Evidence and decision
  in `docs/adr/0001`: no web sessions and no API tokens in use. The local TUI,
  `wakil run` and the hisn backend are unchanged. Removed with the transport:
  `internal/remote`, `internal/server/connect`, `internal/protoconv`,
  `internal/auth` (apitoken, jointoken, oidcresolver, tokenresolver,
  tokenstore, peercred), `api/` (including the generated `api/gen`), `web/`,
  `docker-compose.yml`, `Dockerfile.daemon`, `buf.yaml`, `buf.gen.yaml`,
  `docs/remote-provisioning.md`. Code orphaned by the removal was deleted
  afterwards (`18c171d`). Measured from `v0.5.2` to this tree (`git diff
  --shortstat`): 269 files changed, 7016 insertions and 48094 deletions in
  total; Go files 5447 insertions and 40921 deletions, of which non-test Go is
  2821 and 26437 and tests 2626 and 14484 (these cover everything since
  `v0.5.2`, including the timeline, perf and hisn 0.4.x work, so they are not
  the size of the removed stack alone). `go.mod` drops its
  `connectrpc.com/connect` and `google.golang.org/protobuf` requirements (two
  lines; four `go.sum` lines). Binary size, `go build ./cmd/wakil` with the same
  Go 1.26.10 toolchain and no extra flags, built from `v0.5.2` and from this
  tree on one machine: 51,469,635 bytes to 41,040,625 bytes (10.4 MB, about
  20% smaller). This is the plain build, not a release (goreleaser) build.
- Process records no longer kept in `docs/`: the 0.4.x and 0.5.x plan and close-out
  documents, the November cleanup and decision sheets, the inventories, and
  `design-note-a1-a6.md`. Their decisions are in `docs/adr/`; the text stays in git history.

### Kept

- **Skill auto-creation** (`/skill-creation`, the `skill_creation` config,
  suggestions, and every `/skills` subcommand) is kept. Suggestions are off by
  default; the user opts in. See `docs/adr/0002`.

### Known follow-ups

- **hisn 0.4.1 is detected by behaviour in one place and by version in
  another.** Whether a session is detached is decided by whether the run
  process exits within the remaining readiness budget, capped at 500 ms, after
  the ready line (`internal/sandbox/hisn/session.go`). The stopped-name
  handling is keyed on the hisn version (`VersionAtLeast(0.4.1)`). A 0.4.1 run
  process that takes longer than 500 ms to exit would be treated as the 0.4.0
  mode. The two should be unified on one signal.
- **Wakil's own prune does not use `--name-prefix`.** Startup reconcile runs
  plain `hisn ls --prune`, which archives every provably dead session on the
  host. Only the integration-test sweep scopes its prune with
  `--name-prefix` (when hisn is 0.4.1 or newer).
- **hisn integration runs must not overlap on one host.** The suite uses fixed
  ids (`wakil-it-*`, `toolpath-*`) and its `TestMain` stops and prunes every
  session with those prefixes, so two concurrent runs stop each other's
  sessions.
- **Rubin exit is not re-measured** after the ilm emitter removal; see
  `docs/PERF-0.6.md`.

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
