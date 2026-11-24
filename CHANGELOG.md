# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

- **Breaking: one workspace resolution order.** `wakil` now starts in the
  current folder, exactly like `wakil .`. The start folder is resolved in
  one order — positional path, `--host-workdir`, `WAKIL_HOST_WORKDIR`, then
  the current directory — and every consumer (memory, input history,
  session history, repo state, sandbox) sees that one resolved workspace.
  The config file keys `host_work_dir` / `work_dir` (and
  `WAKIL_WORKDIR` / `ILM_WORKDIR` in direct mode) no longer steer the start
  folder; when one is set, startup prints one line that it is ignored since
  0.8.11 and how to work in a folder instead (`cd` into it, or
  `wakil <path>`). Existing sessions, memory and histories are untouched
  and stay reachable with `--resume --all`; no sandbox trust is granted by
  the program itself.
- Status and cost lines never overlap text again: the Mashura panel cost
  line, VOID / NOT COMPLETED lines, the registry-full fallback note, read
  receipts, struggle and auto-counsel lines, subagent warnings, the
  session-budget and context-pressure warnings and the sandbox bind warning
  now all go through the notice path (their own line) instead of the
  streamed output they used to be drawn on top of.
- The browser launch waits for CDP readiness and retries the connect and
  handshake within the launch bound (30 s), so a transient
  accept-then-EOF on the first tunnelled connection (observed as
  "cannot connect to Chromium at … EOF") no longer fails the launch; a dead
  browser fails with the bound named instead of hanging.
- A model that hits the token limit no longer ends the turn with a fatal
  error: the text that already arrived is kept, a tool call whose arguments
  were cut is dropped (never executed), and the model is asked once to
  continue from where it stopped. If the continuation also hits the limit,
  the run stops with one line naming the endpoint, the model, the current
  `max_tokens` value and how to raise it — in headless runs the exit code is
  5, the terminal record's outcome is `token_limit`, and `--result-json`
  reports `exit_reason` as `token_limit` instead of `error`.
- Loaded context modules now survive `/resume`: with `context_modules` on,
  the names of loaded modules are saved with the session, and on resume a
  module that still exists is delivered again the same way a pre-load is
  (a synthetic `load_module` call and result, appended); a module that no
  longer exists (for example an MCP server that is not connected any more)
  is named in one notice. Sessions saved without the field (v0.8.10 and
  earlier) resume exactly as before, with nothing loaded and no notice.

## [0.8.10] - 2026-11-23

Upgrading from 0.8.9, in short (details under Added and Fixed):

- **Context modules, off by default.** The new `context_modules` config key (`"off"` (default) | `"on"`) splits the tool array into a core set (always present) and modules. With it on, the core tool array is ~27 800 bytes (~6 950 est. tokens) instead of ~39 300 bytes (~9 830), saving ~2 875 est. tokens per request for the default config. Other tools and each MCP server become modules: loaded on demand with `load_module(name)`, called with `call_tool(name, arguments)`, through exactly the same approvals and checks as direct calls. Turn it on with the config key `"context_modules": "on"`, the environment variable `WAKIL_CONTEXT_MODULES=on`, or `--context-modules on` (interactive and headless, flag > env > config). Host-measured on one model (anthropic/claude-haiku-5.5, 5 small bench tasks, 3 runs) at the release commit: 15/15 pass with the switch off ($0.3519 total, one long-read run alone $0.1482 at 693k input tokens) and 15/15 with it on ($0.1477); steps input per run 152.6k off, 91.9k on. The measured saving is for that model and task set, not a general claim. Known limits: subagents keep their existing curated tier tool lists (the switch does not apply to them — a child gets its own fixed set, never the parent's full list); an MCP server is pre-loaded on the first request only when its name (or a keyword configured in `mcp_module_keywords`) appears as a whole word in your first message (a configured multi-word keyword matches as a substring, and a configured list replaces the built-in); the system prompt itself is not yet split (Stage 2) — only the tool array is modularised. The feature is experimental: off by default, so with the switch off the request Wakil sends (tool list, prompt, message order) is byte-identical to 0.8.9.
- **Trace files are no longer cut mid-record at 64 KiB.** Each record and its newline are now one write, so a crash or kill can no longer leave a half-written final line, and a resumed session appends after (never onto) a cut tail left by an older writer. Existing cut files are left as they are — the writer never rewrites history.
- **New `scripts/context_usage.sh`.** It prints a per-tool `TOOL/CALLS/SESSIONS` table from trace files (plus the session denominator on stderr, and, with `--union`, the number of distinct sessions across a tool group), skips and counts broken lines, and prints only tool names and integers — never arguments, results or any other trace content.

No configuration key changed except two new additive keys: `context_modules` (default `"off"`) and `mcp_module_keywords` (default nil; the built-in keyword for a server is its name, matched as a whole word). `MinHisnVersion` stays 0.6.1. The bench CSV gains two appended columns (`context_modules`, `modules_loaded`) and `--result-json` gains the same two fields (omitted when the switch is off); the bench table reads columns by header name, so 16-column CSVs from 0.8.9 still render.

### Added

- **Context modules (Stage 1, off by default).** A new `context_modules` config key (`"off"` (default) | `"on"`, env `WAKIL_CONTEXT_MODULES`) splits the tool array into a core set (always present) and modules (loaded on demand via `load_module(name)` + `call_tool(name, arguments)`). With the switch off, the request Wakil sends (tool list, prompt, message order) is byte-identical to 0.8.9 — a golden test proves it. With the switch on, the core tool array is ~27 800 bytes (~6 950 est. tokens) instead of ~39 300 bytes (~9 830 est. tokens), saving ~2 875 est. tokens per request for the default config and ~3 243 with two MCP servers. MCP tool definitions (the largest movable bytes) are in per-server modules (`mcp:<server>`) and never in the core array. A tool called through a module goes through exactly the same checks as a direct call: the shell classifier, approvals and grants, sandbox, network policy, destructive and external gates, declined-call counting, trace and result-json accounting — all run on the inner tool name, never `call_tool`. The `--result-json` summary and the bench CSV gain `context_modules` and `modules_loaded` columns; `scripts/bench.sh` gets `--context-modules on|off`. MCP modules can be pre-loaded on the first request when the task text matches built-in or user-configured keywords (`mcp_module_keywords` in config; the built-in keyword for a server is its name, matched as a whole word, and a configured keyword replaces the built-in). The module index is appended to the preamble (inside `messages[0]`, so it costs nothing after the first request) and is frozen at session start — loading a module mid-session never changes the tool array or invalidates the cache prefix. An MCP server that connects after the registry was built becomes a module (the registry follows connect, disconnect and reconnect, reconciled at turn start); a disconnected server's module answers with a clear "MCP server is not connected" error, and a reconnect that returns a different tool set updates the module. Keyword-pre-loaded modules arrive with their schemas on the first request (a synthetic `load_module` conversation, no `load_module` round trip), and a late-connect announcement is appended to the next tool result as a clearly marked part, never as a user-role message. `wakil --context-modules on|off` and `wakil run --context-modules on|off` exist (flag > env > config). Subagents keep their existing curated tier tool lists (the switch does not apply to them), and `--no-memory` with the switch on still removes every memory tool and leaves no memory module.
- **Loaded modules survive collapse.** A pinned `load_module` schemas message (with its parent tool call and sibling results) is preserved through turn-end, mid-turn and explicit `collapse_context` collapse, so `call_tool` to a module tool keeps working without a new `load_module`, and the collapse note lists the modules it preserved. The pre-load reason is rendered in the module index (frozen in the preamble at turn 1) — e.g. `pre-loaded: "trello" in your message`.
- **New script `scripts/context_usage.sh`.** It prints a per-tool `TOOL/CALLS/SESSIONS` table from trace files (plus the session denominator on stderr, and, with `--union`, the number of distinct sessions across a tool group), skips and counts broken lines (a file cut mid-record by an older writer no longer makes the whole report fail), and prints only tool names and integers — never arguments, results or any other trace content.

### Fixed

- **Trace files are never left with a partial last line.** The trace writer used a fixed 64 KiB buffered writer, which flushed at a byte offset regardless of record boundaries: a record straddling the 65 536-byte mark was written in two parts, so a crash or kill between flushes left an exactly-65 536-byte file whose last line was cut mid-record. Each record and its newline are now one write, a resumed session appends after (never onto) a cut tail, and a session that dropped records because the write channel was full says so once.

## [0.8.9] - 2026-11-22

Upgrading from 0.8.8, in short (details under Added, Fixed and Changed):

- **Read-only `sed` again for plain bracket scripts.** `sed -n '/[a-z]/p' file` and `sed 's/[0-9]//' file` (a `[...]` or `[^...]` expression in an address or in the pattern of `s///`, the whole script one single-quoted word) no longer need `--allow-destructive`; 0.8.8 gated every script with a `[`. What stays gated: `[[:class:]]`, `[]...]`, double-quoted, unquoted or glued scripts, and a file word with a `[` that is not single-quoted (this last one is stricter than 0.8.8: `sed -n 1p f[1].txt` was read-only there; write `'f[1].txt'`).
- **Headless runs print the collapse and the "async results delivered" notices again**, each as one `{"type":"output"}` line of its own (0.8.8 lost the turn-end and mid-turn collapse notice). A script that takes the last `output` record as the answer can now get the turn-end notice line.
- **A collapse checkpoint now has a "Findings so far" part** (files read with line ranges, files written, failed command names with exit status, the agent's last notes; at most 1500 bytes, built by code, no model call). It makes a checkpoint up to 1500 bytes larger, so a small range that collapsed in 0.8.8 may now be skipped by the never-inflate check.
- **Bench harness:** `scripts/bench.sh --runs N` works on a terminal (no more "already being run by another bench" after run 1); the Go preflight can be skipped only with `--no-preflight` (`BENCH_TEST_SKIP_PREFLIGHT` from 0.8.8 and `BENCH_ALLOW_SKIP_PREFLIGHT`, which was never released, are ignored without a message); `scripts/bench.sh -h` needs the required tools installed (it exits 1 naming the missing ones).

No configuration key changed. `MinHisnVersion` stays 0.6.1.

### Added

- **A collapse checkpoint now carries a "Findings so far" part, built by code from the archived range.** After a collapse (the `collapse_context` tool, the automatic mid-turn collapse and the turn-end collapse) the note that stays in the context lists the files read with the line ranges requested (merged and sorted), the files written or edited, the commands that failed (command name and exit status, newest first), the agent's own last notes (newest first, each cut to 160 bytes) and the archive path with the one `search_files` call that searches it. It is at most 1500 bytes, cut from the oldest entries with an "(n more)" marker, deterministic, and recomputed over the whole range on a second collapse (one part, never nested). It is built from the tool-call arguments and the agent's own text: file paths and line ranges from `read_file`, `read_file_full` and the edit and write tools, only the command name (never its arguments or `VAR=value` words) from `run_shell`, and from a tool result only the exit status integer of an `ERROR: exit status N` prefix. No tool output or file contents are copied. Only `run_shell` results that start with `ERROR: exit status N` count as failed commands (a timeout, a signal or a declined call is not listed). The agent's own text can still quote such things, and the part is also written into the 7-day memory index entry of the collapse. The kept tail now leaves room for it under the hard-max clamp. No model call and no network. Whether it reduces re-reading is not measured yet (needs a bench run with a model that collapses on `long-read`).

### Fixed

- **A headless `wakil run` prints the collapse notice and the "async results delivered" notice again, each on a line of its own.** Each is one plain line on the same stream as the tool lines (`{"type":"output"}`), never inside an answer line, with no escape sequence. v0.8.7 printed the collapse notice in headless mode, inside the answer stream. v0.8.8 lost the turn-end and the mid-turn collapse notice there (they went to an event the headless loop ignored); its model-called `collapse_context` notice and its "async results delivered" notice were still written into the answer stream. All of them now print as their own line. Because the turn-end notice comes after the answer text, a script that takes the last `output` record of a run as the answer can now get the notice line; match the answer by something other than its position. The other status notes (the `⚡ auto:` and `⚡ grant:` lines, workflow and handoff progress) stay out of a headless run. The session note payload gained a `Notice` flag for this (absent in older events, which decode as "not a notice").
- **`bench.sh` Go checks no longer depend on `HOME` to find the Go toolchain.** The harness resolved the toolchain by version only, and every `go` call of a check, of `setup.sh` and of the preflight looked it up again by name, so a check run with another `HOME` (the `tools-list-sufficient` self-test case has a private one) could not find the cached toolchain and ended as an `error` (`go: download go1.26.11 ... toolchain not available`) on a host whose installed Go is older than the tasks (seen with Go 1.25.1 on `scripts/host_verify.sh v0.8.8`). `bench.sh` now records where the resolved toolchain lives on disk (`go env GOROOT`, checked by running that toolchain's own `bin/go`, which must report the resolved version) and every Go command runs that `bin/go` with `GOTOOLCHAIN=local`, `GOPROXY=off` and `GOENV=off`; nothing is downloaded and nothing switches. If the toolchain cannot be resolved the one-line preflight error is unchanged. The self-test also keeps its Go build and module caches in a private directory (it only reads the toolchain), and `--no-preflight` now says why a resolve fell back to the `go` on `PATH`. Self-test cases: `go-empty-home` (new), `tools-list-sufficient`, `toolchain-pinned`.
- **A failed `bench.sh --self-test` case shows why.** The exit of a failed self-test prints, before its temporary directories are removed, the `check:` reason lines and the last 15 lines of the log of the task that failed (`tools-list-sufficient` keeps the logs of the failing task for it). Before, a case printed only `row=error` or a one-line message. Self-test case: `failure-report` (a forced failure).
- **`scripts/host_verify.sh` failures are readable.** A failed `go-test` or `go-test-race` prints the `FAIL <pkg>` lines and the `testutil: tests changed the real Wakil directories` block when it is there, with one hint line (a real Wakil session was running; stop it, or set `WAKIL_TEST_ALLOW_REAL_DIR_CHANGES=1` for this run), instead of the last lines of the log, where the guard text was usually not. The logs of `vet`, `golangci-lint`, `go-test`, `go-test-race`, `e2e-harness`, `tui-perf`, `tui-screen` and `real-hisn` in the output directory hold the whole output (they held a tail). The start-up line `WARN: a wakil process is running` names the consequence. The summary code has its own test (`scripts/host_verify_summary_test.sh`, check `host-verify-summary`). The guard itself is unchanged.
- **`bench.sh --runs N` no longer stops after run 1 with "already being run by another bench".** On a terminal, the heartbeat's `sleep 30` inherited the task lock and outlived run 1 (the script had already exited with status 75 and the orphaned `sleep` kept the lock for up to 30 seconds). The lock is now taken once per task for the whole series; no program the bench starts inherits it; the heartbeat is stopped with its `sleep`; releasing the lock also frees it by `flock -u`. A second bench on the same task still exits 75.
- **`bench.sh --self-test` no longer depends on where the host keeps its tools.** The "tar missing" and "python3 missing" cases built their PATH by hand and lost `dirname` with `tar` on a host where both live in one directory, so the case failed there (`dirname: command not found`, `cd: null directory`). Every case that simulates a missing tool now uses a private directory with links to exactly the harness's tools minus the one under test. The self-test also writes its logs to a fresh directory per run, so logs left by an earlier run cannot satisfy or break a case.
- **`bench.sh` checks its tools before it uses any.** The check runs first, with shell builtins only, and a missing tool gives one line naming all missing tools (`required tool(s) not found: dirname tar`) and exit 1; before, `dirname` ran before the check and a missing one gave a shell error.
- **Async and collapse_context notices no longer appear in the streamed reasoning.** The "async results delivered" status text and the collapse_context notice now go through the notice path (the same path as the collapse notice), so they appear on their own line, never written into the assistant or reasoning stream (in the TUI after the tool line they belong to; in a headless run see the first entry under Fixed).
- **Flaky screen test no longer uses a fixed sleep.** The e2e collapse notice test's 300 ms `time.Sleep` is replaced with a `waitScreen` that polls for the notice text (5 s deadline), so the assertions read a settled screen on a loaded runner.

- **The collapse and async notices carry no escape sequence without a terminal.** They are written to the output stream when no event sink is attached (embedders, tests) and are dimmed only when the stream is a terminal and `NO_COLOR` is unset or empty; in a pipe, a file or with a non-empty `NO_COLOR` they are plain text. Other status lines (for example the archive-write warning) still use fixed colours. A headless `wakil run` now prints them (see the first entry under Fixed).
- **`collapse_context` no longer reads the turn number outside its lock.** A data race with `/new` is fixed: the turn is read once under the lock and checked again before the record is stored; if the session changed meanwhile nothing is collapsed and the archive is removed.
- **A sed script with a plain bracket expression is read-only again.** `sed -n '/[a-z]/p' file` and `sed 's/[0-9]//' file` no longer need `--allow-destructive`. Newly read-only: a `[...]` or `[^...]` expression (no `]` first, no `[`, backslash, newline or delimiter inside) in an address or in the pattern of `s///`, and a `[` in the replacement, when the script is one single-quoted word (also as the value of a separate `-e`, `-e 'SCRIPT'`) and every word with a `[` is single-quoted. Still not read-only (the caret `^` as the `s` delimiter next to a negated bracket included): unquoted, double-quoted or glued scripts (`-e'...'`, `--expression=...`), the `w`, `W`, `e`, `r`, `R` commands and the `w`/`e` flags of `s///` (with or without brackets), `-i`, `--in-place`, `-f`, `[]...]`, `[[:class:]]` (a `[` inside the bracket), a delimiter inside a bracket, `[` as a delimiter, an unterminated bracket, and any script the parser does not fully understand. **Stricter than 0.8.8 (a tightening):** a file word with a `[` that is not one plain single-quoted word is no longer read-only either (`sed -n 1p f[1].txt`, `sed 's/a/b/' "f[1]"`, `sed -n p [-]i` were read-only in 0.8.8): the shell can expand it as a glob to a file named `-i` and sed would read that as an option. Quote it with single quotes (`sed -n p 'f[1].txt'`) to keep it read-only.
- **`mashura_rounds` in `--result-json` counts a legacy automatic round once.** A session saved before 0.8.3 holds a synthetic `mashura__debug` call with an `auto-counsel-` id; it was counted as both a model call and an automatic round.
- **A saved per-repo network mode is validated on restore.** Only `open` and `host` are applied; any other value in an old or hand-edited state file is ignored.
- **Bench harness:** `python3` is checked before the first run when a selected task uses it; every `go test` of a check gets a `-timeout` below `BENCH_CHECK_TIMEOUT`; `resolve_go` ignores the caller's `GOFLAGS`; checks run with `GO111MODULE=on`; the preflight runs `setup.sh` in a copy of the task's `repo/`; the leak scan fails closed when `grep` cannot scan; the heartbeat pid is reset per run; the path-stability self-test compares two real preparations; the abort paths of `prep_workdir` (saved copy and live `.wakil/` both present, failed save) are tested. The README usage lists `--setup` and `--no-preflight` and states that the binary must report `workdir`.

### Changed

- **The bench preflight can be skipped only with the `--no-preflight` flag.** `BENCH_TEST_SKIP_PREFLIGHT` (0.8.8) is no longer read and is ignored without a message, so a CI job or dotfile that set it now gets the preflight back (and a Go-version stop where it used to skip); no environment variable switches the preflight off. `--no-preflight` cannot be combined with `--self-test` (exit 2). A run with the flag prints one warning line and writes its CSV and logs under the cache (`wakil-bench/nopreflight`), never under `bench/results`.
- **Bench `prep_workdir` recovers `.wakil/` after an interrupted prep.** The `.wakil/` directory is saved to a sibling under `$work_root` (same filesystem, atomic rename) and the save `mv` aborts before the wipe if it fails. At the start of `prep_workdir`, a saved copy from an interrupted prior prep is moved back before anything else; if both saved and active copies exist, the function aborts. The saved copy is never deleted by the harness.

### Corrections to the 0.8.8 notes

- Exit code 5 maps to `"incomplete"` in `--result-json` (`exit_reason`), not `"exit_5"`. The 0.8.8 notes do not mention this mapping.
- The bench harness (`scripts/bench.sh`) is new in 0.8.8. The 0.8.8 notes describe bench harness fixes and improvements but do not say the harness itself is new.

## [0.8.8] - 2026-11-21

**Breaking:** for Mashūra, an OpenRouter endpoint that is not the active route no longer falls back to the global `api_key`; it needs its own `api_key_env` or `auth_header`, otherwise Mashūra skips it (and fails closed when none of the OpenRouter endpoints has one). `/mashura` names the endpoint in use (`credential: backend config (<endpoint>)`). If you relied on the global `api_key` for a non-active OpenRouter endpoint, add `api_key_env` or `auth_header` to that endpoint before upgrading. The active OpenRouter endpoint is unchanged.

### Added

- **`wakil run --result-json <file>` writes one JSON object at exit** with `wakil_version`, `model`, `exit_reason`, `workdir`, `seconds`, `tokens_in`, `tokens_out`, `tokens_cached` (omitted when the provider reports none), `cost_usd`, `tool_calls`, `collapses` (mid-turn collapses only), `collapses_turn_end` (full turn-end collapses), `mashura_rounds`, and `declined_calls` (count of tool calls declined by the user or policy). Pointer fields are null when the value was not observed; non-pointer fields are empty strings or zero (`model` is `""`, `workdir` is omitted via `omitempty`, `seconds` is 0). The `workdir` field records the resolved workspace folder the session actually ran in, so a bench harness can verify the agent used the expected temp folder. For a bench harness that needs per-run numbers without parsing the JSONL transcript.
- **`wakil run --no-memory` skips per-project memory.** No memory tools are offered, the memory store is not opened, and the memory digest is not included in the prompt — so a headless run has no carry-over from a previous session's memory, plan/current, or mid-tier entries. The bench harness passes `--no-memory` so bench runs have no carry-over.
- **Bench harness uses fixed work folders.** Each task runs in a stable path (`${XDG_CACHE_HOME:-$HOME/.cache}/wakil-bench/work/<task>`) so the user's one-time sandbox acknowledgement holds across runs. `scripts/bench.sh --setup` creates all folders and prints the `WAKIL_HOST_WORKDIR=<path> wakil sandbox trust` command per folder. The harness never runs that command and never reads from the terminal. The result of a run comes from the task's `check.sh` (pass/fail), `exit_reason` keeps what the run reported, and bench runs pass `--allow-destructive` (the folder is a throwaway copy and the workdir guard is in place). The default `model` column is `unknown` (never `config`).
- **Prompt version line warns when an override is stale.** The embedded prompt now carries a first-line `# prompt-version: <n>` comment. When a user's override file has no version line or a lower one, 

### Fixed

- **Bench: the Go checks work on a host whose installed Go is older than the task's `go.mod`, and a check that cannot run is an `error`, never a `fail`.** `check.sh` ran with `GOTOOLCHAIN=local`, so a host with Go 1.25.1 scored every Go task `fail` (`go: go.mod requires go >= 1.26 (running go 1.25.1; GOTOOLCHAIN=local)`), and `add-flag` ran with the user's environment and the agent's `go.mod` and could download a toolchain. The harness now resolves the repo's toolchain once, runs every check and `setup.sh` with `GOTOOLCHAIN` set to that exact version and `GOPROXY=off` (nothing is ever downloaded; the toolchain must be in the local cache, see `bench/README.md`), runs a preflight on the reference solutions before the first run that exits 1 with one line naming the required and the found Go version and writes no CSV row, and maps a check's exit status 0/1/99 to `pass`/`fail`/`error` (`exit_reason=check-error`, the bench stops). The `add-flag` binary runs under a time limit: a hang is `fail` with `check: binary timed out`. Self-test cases: `toolchain-pinned`, `rc-mapping`, `verdict-required`, `preflight-real-go`, `binary-hang`, `binary-exit` and, with a stub `go`, `preflight-stops`, `preflight-stops-on-check`, `check-error-not-fail`.

- **Bench README documents the timeout, lock, fake-pass and workdir rules, the `seconds` column and the known limits.** The CSV `seconds` column is the harness-measured wall time of the whole `wakil` process (bootstrap included) and always was; only the `seconds` field of `--result-json` started to include bootstrap, in `44fa662`. CSVs from before that commit are therefore comparable on time. `long-read` can be solved with a script instead of by reading: stated as a known limit, the task is not redesigned.

- **Bench: shellcheck and the self-test run in CI and in `scripts/host_verify.sh`.** A new `bench-harness` CI job runs `shellcheck -s sh` on `scripts/bench.sh`, `bench/tasks/*/*.sh` and `bench/lib/*.sh` (it fails when shellcheck is not installed) and `scripts/bench.sh --self-test`, which uses stub binaries only: no model, no sandbox, no network. `host_verify.sh` has `bench-shellcheck` (a visible `SKIP  bench-shellcheck (shellcheck not installed)` line when absent) and `bench-selftest` (temp `XDG_CACHE_HOME`). All shellcheck findings are fixed or disabled per line with a reason; a `.shellcheckrc` resolves sourced files. The harness now requires `jq`, `flock`, `timeout`, `sha256sum` and `git` up front (without `jq` every run would have stopped as `missing-workdir`) and a failing `setup.sh` aborts the run.

- **Bench: the workdir guard knows the sandbox backend, and the result JSON no longer lives in the work folder.** `--result-json` now carries `sandbox_backend` (`hisn`, `docker`, `direct`). `workdir` is a host path for hisn (symlink-resolved) and direct but a container path (`/mnt/<name>`) for docker, so the old comparison gave a false `wrong-workdir` under docker and under a symlinked cache dir. Now docker is recorded as `error` / `workdir-not-verifiable` ("workdir not verifiable (docker)", `check.sh` skipped, bench stops), hisn is compared against the folder and its `pwd -P` form, and an absent field (older binary) keeps the plain comparison. The bench reads the result file from `${XDG_CACHE_HOME:-$HOME/.cache}/wakil-bench/run/<task>.json`, outside the folder the agent can write to.

- **Bench: `--self-test` writes to the cache, not the repo.** Its CSV and logs go to `${XDG_CACHE_HOME:-$HOME/.cache}/wakil-bench/selftest/`; `bench/results/` is created only by a real run, and the self-test asserts that nothing under it changed. Files already under `bench/results/selftest/` are left alone.

- **Bench: two benches can no longer run on the same task at once.** Each task takes a `flock` on `${XDG_CACHE_HOME:-$HOME/.cache}/wakil-bench/locks/<task>.lock` (outside the work folder) before the folder is emptied; a second bench on the same task prints "task <t> is already being run by another bench", exits 75 and leaves the folder and the CSV untouched. Without `flock` the harness refuses to run. `--task` rejects names with a `/` or a leading `.`.

- **Bench: every CSV cell goes through one quoting function.** `csv_row` printed its 16 cells raw, so a model name or version line with a comma, a quote or a newline (a model name comes from the config, the version from the binary, the exit reason from the result JSON) split or broke the row, and the table and the earlier-run lookup, which split on every comma, read it wrong. All cells now go through `csv_field` (RFC 4180: a cell with a comma or a quote is quoted and its quotes doubled; CR/LF become a space, so a row is always one line), and the table, the cost sum, the earlier-run lookup and the self-test all read the file with one quote-aware POSIX-awk splitter. Self-test: a model name with a comma, a quote and a newline, and a version line with a comma and a quote.

- **Bench: a run killed after the timeout is a `timeout`, never `pass` or `fail`.** `timeout -k` exits 124 when the run ended on TERM and 137 when it ignored TERM and was killed after the grace period; only 124 was handled, so a killed run went on to the workdir guard and `check.sh` and could be scored from a stale result JSON and a half-finished folder. Both codes now give `result=timeout` with `exit_reason=timeout` (`killed` for a 137 before the deadline, e.g. the OOM killer), skip `check.sh`, and are decided before the transcript is searched for sandbox-approval messages (agent-controlled text). The 10-second kill grace is `BENCH_KILL_GRACE`. Self-test: a fake `wakil` that ignores TERM.

- **Bench: the `long-read` generator and seed no longer reach the work folder.** The generator moved out of `repo/` into `bench/tasks/long-read/gen/`, and `setup.sh` now runs before the work folder's first commit, so neither the generator, the seed nor the expected answer is in the folder or in its git history (the old layout committed `cmd/gen` first and deleted it afterwards, so `git show HEAD:cmd/gen/main.go` recovered it). The generated `docs/` are byte-identical. A failing `setup.sh` now aborts the run instead of being ignored, `prep_workdir` checks that the folder was really emptied, and a fatal guard before the agent refuses a work folder containing `check.sh`, `hidden/`, `solution/`, a generator or a seed, in the tree or in git history. After the run, a `check.sh`, `hidden` or `solution` entry makes the run `error` (`exit_reason=leak-detected`), stops the bench and skips `check.sh`; no git command runs in the folder after the agent. A change under the preserved `.wakil/` is reported (read-only digest).

- **Bench: agent-written test code can no longer fake a pass on `fix-test` and `rename`.** `check.sh` now works on a clean copy of the work folder (no `.git`, no `.wakil`): the agent's `*_test.go` files and `testdata/` are dropped, the task's `go.mod` is restored, and a `go.work`, `vendor/`, a symlink, assembly/cgo files or a non-test package importing outside a small allow-list fail the check. The hidden tests are restored and must report `--- PASS: <name>`. A `TestMain` that exits 0, or an `init()` that prints a PASS line and exits, now scores `fail` (self-test cases for both). `rename` also gets a hidden behaviour test, so defining `NewName` without keeping the behaviour no longer passes. The output of `check.sh` goes into the run log instead of `/dev/null`. The import allow-list is applied to the whole import closure (`go list -deps`), so a package in an underscore directory, which `./...` skips, cannot hide an `init()` that prints PASS and exits.

- **Breaking: Mashūra's OpenRouter credential: a no-credential endpoint no longer hides a usable one.** An OpenRouter endpoint that is not the active route no longer falls back to the global `api_key`; it needs its own `api_key_env` or `auth_header`, otherwise Mashūra skips it (and fails closed when none of the OpenRouter endpoints has one). Details: When the active endpoint is not OpenRouter, Mashūra now uses the first OpenRouter endpoint (sorted by name) that has a credential of its own, instead of failing on the first one without; it fails closed only when OpenRouter endpoints exist and none has a credential, and the `openrouter_api_key_env` variable is used only when none exists. A non-active OpenRouter endpoint no longer picks up the global `api_key` (meant for the main route). The active OpenRouter endpoint is unchanged.

- **Mid-turn collapse runs once per request and never re-archives a covered range.** A mid-turn collapse now fires at most once per request; a second collapse in the same request finds the already-archived range and skips it instead of archiving the same content twice.
- **The kept tail after a mid-turn collapse is clamped to 30% of hardMax.** When the collapse ceiling exceeds the hard max, the retained tool-call/result pairs are clamped so the kept tail always fits under hardMax.

- **A custom `agent_prompt_path` is honoured in core mode.** `/prompt core` now uses a configured `agent_prompt_path` override when one is set, instead of always loading the embedded `core.txt`.
- **`AgentPromptNote` shows the built-in prompt when the override file is missing.** `/prompt` no longer prints an empty prompt body for a missing override file; it shows the built-in prompt instead.
- **`/mashura level` without a `mashura` block in the config says the level is ignored.** Setting a level with `/mashura <level>` when no `mashura` block exists in the config now prints one line saying the level is ignored and what to add to the config.
- **Bare `/network` shows the apply line, and `/sandbox` always shows the network mode.** `/network` without arguments now prints the line saying when the override applies; `/sandbox` now always shows the `network:` line even when no sandbox session is running.
- **`AgentPromptNote` says "could not load" instead of "file not found".** When a custom prompt file is missing, empty, or unreadable, the note now says "could not load <path> — using embedded prompt" instead of the misleading "file not found at <path>".
- **A version-only override file falls back to the embedded prompt.** A custom prompt file containing only `# prompt-version: N` and no prompt body now falls back to the embedded prompt instead of leaving the agent with an empty system prompt.
- **`/prompt status` warns about unversioned overrides.** An override file with no version line now shows "override has no version (built-in v<n>)" in `/prompt status`, matching the start-up warning.
- **`mashuraLevelSetNote` no longer emits `"level": ""`.** When the level is removed and no `mashura` block exists, the example JSON now uses `"medium"` as the placeholder level instead of an empty string.
- **`declinedCalls` increment is guarded by `stateMu`.** The counter is now locked on both the write (in `finalizeToolResult`) and the read (in `DeclinedCallCount`), matching the locking convention used by the collapse counters.
- **`wakil sandbox trust [path]` uses one resolved workspace for both `.wakil/sandbox.json` and the approval key.** Previously, passing a positional `<path>` when `config.json` set a fixed `host_work_dir` wrote `.wakil/sandbox.json` into `<path>` but stored the approval under the config's `host_work_dir` key, so a subsequent `WAKIL_HOST_WORKDIR=<path> wakil run` failed with `has not been approved (or changed since)`. `wakil sandbox trust` now resolves one workspace using the same precedence as `wakil run` (positional path > `WAKIL_HOST_WORKDIR` > config > cwd) and prints `workspace: <resolved path>` before asking.
- **Read-only classification of `sed`, one-line loops and `$(...)` is conservative and fails closed.** `sed` is read-only only when it has no `-i`/`--in-place`, no `-f`/`--file`, no unknown option, and every command of every script (positional, `-e`, `--expression`, separated by `;` or newline) is a print, address or other non-mutating form (`h`, `H`, `g`, `G`, `x`, `z`, `n`, `N`, `l`, `q`, `Q`, `F`, `b`, `t`, `T`, `s///` without `w`/`e` flags, braces, comments); `w`, `e`, `r`, `a`, `i`, `c`, `d`, `y` and `s///w`/`s///e` stay gated. A complete one-line `for/while/until ...; do ...; done` is read-only when its header and body are, and nothing may follow `done`; any other loop, `case`, `if`, brace group or subshell is gated. `$(...)` is read-only only when the inner command AND the outer command are; an unbalanced `$(` is gated. A variable or substitution as an argument is accepted only for `echo`, `printf`, `cat`, `grep`, `ls`, `head`, `tail`, `wc` and similar, never for `find`, `git`, `sed`, `date`, `yq`. A backslash inside an argument is no longer read-only (the shell strips it, so `--\output=x` would reach git as `--output=x`). Tests in `cmd/wakil/readonly_test.go` (`TestShellClassifierBypasses`) pin both directions.
- **Redirects and command words are classified after leading redirects and assignments are skipped.** A redirect before the command word (`>/dev/null rm -rf x`, `2>&1 rm x`, `echo hi; 2>/dev/null rm x`) no longer hides the command: the classifier skips leading `VAR=value` assignments and harmless redirects in any order, then judges the command word. A leading input redirect (`</dev/null rm x`) and any other leading redirect that is not fd duplication or `/dev/null` make the command destructive and not read-only. fd duplication is `>&N`, `N>&M` or `N>&-` with digits, followed by the end, whitespace or a separator: `echo x >&1out.txt` and `>&2x` write a file and are destructive, and `>>&1` is destructive. Redirects to exactly `/dev/null` stay harmless; `>/dev/nullfoo` and any other path stay destructive. A `for` loop whose variable is `PATH`, `IFS`, `LD_*`, `GIT_*`, `HOME`, `CDPATH` or a similar name that a shell or tool reads is gated, and `printf -v` (including `-vNAME`) is gated as read-only.
- **Command words that hide their name are gated, and wrappers are not unwrapped.** A command word with quotes or a backslash (`\rm`, `r""m`, `'r'm`) is destructive and not read-only. Wrappers (`env`, `command`, `nice`, `time`, `nohup`, `timeout`, `exec`, `eval`, `sudo`, `xargs`, `sh`/`bash`/`zsh`, `source` and similar) are destructive and not read-only; they are not unwrapped. `git -C <dir> <sub>` is classified by `<sub>`, and any other global option before the subcommand (`git -c k=v log`, `git --git-dir=x status`) is destructive. The read-only git subcommands refuse the options that run a program or write a file, in any form: `-O` and `--open-files…` for `grep` (attached forms too), `--upload-pack` and `--receive-pack` for `ls-remote`, `--output…`, `--ext-diff` and `--textconv` (`=false` and `--no-…` forms are allowed). `git config` is read-only only for `--get`, `--get-all`, `--list` or `-l` with at most one name, and never with `--` or a second word (`git config -- k --list` writes a value and is gated). `fd`/`find`, `tree`, `less`, `yq`, `hostname`, `date`, `xxd`, `ack`/`ag`, `bat` and `file` refuse their write, execute and set forms in clustered, attached and upper-case spellings (`fd -Hx rm`, `tree -ao out`, `yq -Pi`, `yq --in-place`, `hostname -F`, `date -s2020`, `xxd - out`, `less --LOG-FILE=`, `bat --pager=`, `ack --ackrc=`, `file -C`). A leading `HOME=`, `XDG_CONFIG_HOME=`, `RIPGREP_CONFIG_PATH=`, `ACKRC=` or `BAT_CONFIG_PATH=` assignment gates the command, and `printf` with a `$` in its arguments is gated, since a variable can expand to `-v`. A `$(...)` or a variable argument to a binary that has write flags is gated in both classifiers. `sed` refuses `--`, since a following `-i` is then a file, not a flag. Some of these forms are gated, not destructive (for example `git grep -O./p.sh`, `git ls-remote --upload-pack=…`, `fd -Hx rm`): the headless `--auto` gate asks for them, but `--allow-destructive` is not needed. Tests: `cmd/wakil/readonly_test.go` (`TestShellClassifierBypasses`), `internal/agent/readonly_table_test.go` (both classifiers, every row), `internal/agent/readonly_corpus_test.go` (a 1125-command corpus recorded from v0.8.5; a command that v0.8.5 called destructive may only lose that flag or become read-only through a listed relaxation) and `internal/agent/readonly_exec_test.go` (each read-only corpus command is run under bash in a fresh temp directory, and the tree and contents must be unchanged; commands that reach the network, are interactive or loop without bound are skipped, and the test is skipped as root).

## [0.8.7] - 2026-11-19

### Security

- **hisn 0.6.1 is now required (security fix for `network = "open"`).** In hisn 0.6.0 the open network mode did not fully enforce the network policy for a socket handed to a program before a connect that then failed; 0.6.1 enforces it. A narrower timing limitation remains and is documented by hisn — use `open` only for code you trust until hisn 0.6.2. Wakil, `wakil doctor`, the startup banner and `install.sh` now require 0.6.1 (the sandbox image pins the 0.6.1 release, checksums from its `SHA256SUMS`); an older hisn is refused with a message naming 0.6.1 and the reason, and with no explicit `sandbox.backend` the default falls back to Docker. `network = "host"` (still the default) and `none` behave as before.

### Changed

- **Open mode needs kernel 5.14 or newer** (was 5.9). On an older kernel hisn refuses to start an open session. `/network`, `/help`, the docs and the network line of the system prompt now say that open is meant for code you trust and needs kernel 5.14+.

- **No separate loading screen at start-up.** The normal view (logo, input box) is drawn at once, with a one-line status inside it (for example `· starting container…`) until the session is ready. Text typed while starting is kept and moves into the input when the session is ready; Enter does nothing until then and Ctrl+C quits. Time to the prompt is unchanged (median 37.0 ms before, 38.0 ms after, 13 runs each on the perf harness's stub provider with the direct backend).
- **The start-up logo has one fixed size** (44 columns x 20 rows) instead of scaling with the terminal. In a pane narrower than 44 columns the name is shown as plain text.

### Fixed

- **`/mashura` shows how each panel member is really called.** Direct Anthropic access is gone: an `anthropic:<id>` member is sent through OpenRouter as `anthropic/<id>`. The status now prints that route next to every member (`anthropic:claude-opus-4-8 [via OpenRouter as anthropic/claude-opus-4-8]`) and marks a member with an unknown provider prefix as `NOT USABLE`.
- **`memory_forget` also removes quarantined entries.** Writes from subagents and tainted sessions are stored as proposed (quarantined) entries; forgetting their key used to answer "not found" and left them in the proposed list. `memory_forget` (main agent only, as before) now rejects them with a `forgotten:` note — nothing is hard-deleted, and a forgotten entry can no longer be promoted — and says what it removed.
- **`/network reset` returns to the project's mode.** Clearing the override with `/network reset` now restores the repo's `.wakil/sandbox.json` `"open"` when the project set one (instead of forcing the built-in `host`), otherwise the configured `sandbox.hisn.network_mode`, otherwise the default. The `network:` line now names the effective source (`repo override`, `project`, `config`, or `default`) — for example `network: open (project)` — and an explicit `/network host` over a project `open` prints one line saying it overrides the project's setting. `docs/sandbox-backends.md` documents the precedence.
- **README badge label corrected.** The release-workflow badge is now labelled `release workflow` (it shows the workflow status, not a signature check).
- **Repeat-read refusals are recognised by a field, not by their wording.** The struggle detector used to find a "[already called …]" refusal by matching the start of its text; the trace entry now carries a `DedupRefusal` flag set where the refusal is made, so a file or tool output that happens to begin with that text can no longer be taken for one.
- **Collapse archives are recoverable with every tool the prompt names.** `read_file` could read an archive or spill file under `toolcache/`, but `search_files` on the same path was refused as "outside the accessible workspace", and two such refusals ended the turn with `confinement_breaker`. `search_files` now searches this session's own `toolcache/<chat>/` files on the host (regex, `file_pattern`, `case_insensitive`, 200-match cap); another session's archive, the toolcache root and anything outside are still refused, and a symlink in the directory is not followed. `list_dir` and `find_files` on the own archive directory now say which tools to use, and a refusal on it no longer counts toward the confinement breaker; the same two refusals on any other unreachable path still trip it.
- **A fallback to Docker says that open mode is not applied.** With `sandbox.hisn.network_mode = "open"` and no usable hisn (missing, or older than 0.6.1), the startup note now adds that the open mode is NOT applied on Docker, which has full network access.
- **A forgotten quarantined entry keeps its reason.** `memory_forget` appends the entry's original quarantine note to the `forgotten:` note.
- **A symlinked chat directory under `toolcache/` is refused** by the host-side `search_files`, as is anything outside the session's own directory.
- **A refused open session shows hisn's own error.** When hisn refuses to start a session in open mode (kernel, Yama `ptrace_scope`, DNS upstream), the error now leads with hisn's own line and how to switch back with `/network host`, instead of only a generic "exited before the session was ready".
- **`WAKIL_TUI_PERF=1` exit summary adds the start-time line.** The four-line summary now ends with `tuiperf start first_frame=<ms>ms prompt_ready=<ms>ms`: the first frame drawn and the moment a task can be sent, both measured from process start. If the program quits before prompt ready it prints `prompt_ready=n/a`; a quit before any frame is recorded prints the no-samples line as before and has no start line.

## [0.8.6] - 2026-11-19

### Added

- **Active network mode in the system prompt.** Wakil now appends one line at the end of the per-session system prompt stating the running sandbox's live network mode — `host` (LAN and internet reachable) or `open` (public internet over TCP only; LAN and private addresses are refused with a permission error). When `/network` queues a different mode for the next session, the prompt line stays on the live session's mode until the session is replaced, `/network` notes `applies at the next session; current session is still <mode>` (or `no sandbox session is running; the next one starts on <mode>` when idle, and neither line when the running session is already in that mode), and `/sandbox` shows both (`network: <live> (next session: <queued>)`).

### Changed

- **System prompt updated to match current Wakil features.** `prompts/agent.txt` (and the `/prompt core` split files `prompts/core.txt` and `prompts/extension.txt`) now reflects current features and removes stale references: ungated read-only `git_*` tools and `read_file_full`, Mashūra levels (`off|low|medium|high`) and Wakil-started automatic struggle rounds, per-session `/grants` and `/auto` rules, `/zdr` and `/region` data policy, `/network host|open`, template skill tools (`apply_skill_template`, `capture_skill_template`, `check_skill_template`), what survives mid-turn `collapse_context`, and `/handoff [stop|proceed]`.
- **Step-scoped reading rule in the system prompt.** Added a core-loop rule instructing the model to start with step 1 on numbered tasks, read only what the current step needs, treat task-stated facts as given unless contradicted by command output, and avoid asking for permission the task already grants.

### Fixed

- **Plain file reading no longer starts automatic Mashūra rounds.** `MakeTraceEntry` previously dropped `offset`/`limit` on `read_file` (so paginated reads of two slices of the same file collided as identical traces) and `detectStruggleScan` fired the `"repeat"` and `"repeated failures"` rules on read-only exploration tools. Paginated reads, LSP lookups, and git reads now keep distinct trace identities, and read-only exploration calls alone no longer trigger struggle detection or start automatic Mashūra debug rounds.
- **Mashūra uses the configured OpenRouter backend's credentials.** Mashūra panel calls, availability checks, and `/mashura` status now resolve their OpenRouter credential from the active OpenRouter endpoint (or, when the active endpoint is local, the first configured OpenRouter endpoint sorted by name) instead of reading `OPENROUTER_API_KEY` from the shell environment. The environment variable is used only when no OpenRouter endpoint is configured; an OpenRouter endpoint with no credential fails closed instead of falling back to the environment. `/mashura` reports the credential source by name (`backend config (<name>)` or `environment (<var>)`, with a proxy note when the main route uses an `ilm-proxy` `openrouter` backend) and never prints a key value.

## [0.8.5] - 2026-11-18

### Added

- **`network = "open"` for hisn sessions (hisn 0.6.0).** Set `sandbox.hisn.network_mode = "open"` in your config, or switch with `/network open`. Sessions then reach the public internet over TCP only; anything on your own network (a NAS, a local git server, private LAN addresses) is blocked, as are UDP and other network families — unix sockets and loopback keep working and name resolution runs inside the session. It needs a kernel 5.9+ and a Yama `ptrace_scope` of 0 or 1; hisn refuses to start otherwise and Wakil reports the reason — it never falls back to a wider mode. A project file (`.wakil/sandbox.json` `hisn.network`) may narrow to `"open"` but never widen out of it; the per-repo override (`/network`) may set either. A change applies from the next session start. The startup banner and `/sandbox` show the network mode in use.

### Changed

- **hisn 0.6.0 is now required.** Older hisn is refused at startup; with no explicit `sandbox.backend` the default backend falls back to Docker (with a banner note) when hisn is missing or too old, and fails when neither is available. Upgrade hisn (`install.sh` downloads the 0.6.0 release) or set `sandbox.backend` to `docker`.

### Fixed

- **A flaky TUI test no longer panics.** When a slash command reached the real program through the screen harness, the fake facade could panic through its embedded nil interface and abort the test run; the fake now returns a safe empty result.

### Fixed (review)

- **`/network reset` reaches the sandbox backend.** Clearing the override with `/network reset` now applies the effective mode (config/default) to the backend for the next session start instead of silently leaving the previous override in place for one more run; the status line also names the source that actually decided the effective mode when a `host` override wins over an `open` config.

## [0.8.4] - 2026-11-16

### Fixed

- **Release notes fall back correctly when there is no CHANGELOG section.** The release workflow now passes the notes path as a step output instead of an inline expression. `scripts/release_notes.sh` is unchanged.

- **Per-server HTTP headers for MCP servers.** Add a `headers` map (`header → value`) to an HTTP MCP server to send credentials like `"Authorization": "Bearer …"` with every request. The headers go only to the configured endpoint and scheme — a redirect to another host or a downgraded scheme (https→http) gets none. See the `mcp_servers` entry in `docs/configuration.md`.
- **`--help` shows only built-in defaults.** The help output no longer shows your config-file or environment values as "(default …)" for endpoint host, port and model; the defaults shown are now always the built-in ones. The endpoint flags are also worded neutrally ("endpoint host", "endpoint port (used with --host, default 11400)", "endpoint base URL (overrides host and port)").
- **TUI drag no longer re-renders all of history per mouse step.** `highlightedContent` now re-styles only the visible region; frame cost at 2000 messages went from 53.6 ms to 1.3 ms (Part 2 of the 0.8.4 pass).
- **Dragging to select no longer jumps while dragging, and the view is free after release.** The highlighted page window is anchored at the press position (previously the second mouse-motion rebuilt it as the first page of the transcript — the view jumped to the top mid-drag); the mouse wheel no longer scrolls during a drag; after release the view is immediately free (wheel scrolls, streaming follow resumes). Fixed in the 0.8.4 follow-up.
- **Resizing no longer freezes the terminal.** A width change now updates the layout immediately and re-wraps the transcript on a 50 ms tick (474 ms → ~2 ms for the resize frame at 2000 messages), coalescing resize drags into one re-wrap.
- **Keypresses allocate ~6× less per frame.** The conversation pane is cached while its content and scroll position are unchanged (1.07 MB → 175 KB per frame at 2000 messages).
- **Selection copies exact text across soft wraps and holds steady while streaming.** `selectedText` now maps rows through both the bordered and no-box mirrors (`rowToNoBoxPair`); a `WindowSizeMsg` clears a selection whose coordinates would copy the wrong text. Unicode (wide/combining/RTL) copies the same code points.
- **OSC 52 clipboard escape is sent once per copy, not once per frame.** `View()` now prepends `pendingEscape` at most once via a shared `escapeBox` (the model is a value type; a `[]byte` clear in a value-receiver `View()` would be lost and re-send forever).
- **Status-line height changes always reflow.** Every call site now passes the effective rendered row count (`effectiveStatusRows`) instead of the ungated `statusRows`; allow this to be 0 when the status zone is hidden (the old comparison `1 != 0` fired a spurious reflow on every host-only turn start). The completion and resume pickers now reflow when their visible row COUNT changes, not only when they open/close. A `KindSessionError` now reflows after clearing turn state.
- **A panicking goroutine in the subscription loop no longer crashes the process with the terminal raw.** The subscription `go func()` in `tui_wiring_loop.go` now runs under `safe.Go` (recovered to diag, terminal untouched).
- **Direct-mode agent commands cannot inherit the terminal.** `RunShell` leaves `Stdin` nil (/dev/null) and captures both streams; a test pins the guarantee (`TestDirectExecutorRunShellStdinIsDevNull`). Docker and hisn were already wired this way (documented in code).
- **History flattens multi-line entries.** A multi-line prompt is stored as its single-line rendering so the one-entry-per-line history file round-trips; recall matches the glanceable first line.
- **`/help` documents the handled keys.** ctrl+y, alt+t (+ the Shift bypass), ctrl+r/ctrl+g, ctrl+e, ctrl+d/ctrl+c double-press quit.
- **Status-zone ellipsis no longer overflows the row.** `flowSegmentsN` reserves the ellipsis's own width when deciding whether to truncate.
- **`host_verify.sh` now also runs the TUI performance and screen tests.** Two new PASS/FAIL/SKIP checks: `tui-perf` (the frame-cost regression + harness smoke) and `tui-screen` (the real-output screen tests).
- **`host_verify.sh` preserves Go caches and prints only the summary.** The script exports `GOMODCACHE`, `GOCACHE` and `GOPATH` to the user's real cache dirs before redirecting `HOME` to a temp dir, so Go never downloads a toolchain into the temp home. and `rm -rf` cannot fail on read-only files. All command output goes to log files; the terminal shows only one line per check and the summary table. Signature checks run before HOME is redirected (git needs the global config for SSH signing); if the signing setup is missing, the check is SKIP with a reason, not FAIL.
- **Automatic Mashūra rounds state their counts plainly.** The header given to the model now reads e.g. "[automatic mashura round one — Wakil started this round automatically; you did not call it · two panel members, cap 2 per turn]", so one round with two members is never described as two rounds.
- **Masked exit codes are recorded as failures.** A `run_shell` command that ends in an echo of `$?` and prints a non-zero `exit=` value is now recorded as failed in the struggle trace; nothing else is guessed from output text.
- **Session start names the work folder.** When the work folder is not the current directory, the start banner adds one line naming it and which setting put it there — the comparison resolves symlinks on both sides (starting in a symlinked directory no longer prints the line), and the line names the one setting that chose the folder (positional workspace argument, `--workdir`/`--host-workdir`, `WAKIL_WORKDIR`/host variants, or `work_dir` in the config file), not a list of all possible ones. (No change to which folder is chosen.)
- **`WAKIL_TUI_PERF=1` now prints its exit summary.** Previously the three-line summary went only to the diagnostic log (and nothing at all when quitting during loading); it is now printed on stderr after the terminal is restored, with the diagnostic log path named in the summary line.

### Removed

- **Correction capture is removed.** Automatic detection of your corrections, the confirmation prompt and the `/auto` suspension carve-out are gone, and the `correction_capture` config key is accepted at load and ignored with one notice. Memory entries the feature created are untouched: list pending proposals with `memory_list` (prefix `correction/`, status `proposed`), refuse them with `memory_reject`, delete active ones with `memory_forget`. The general memory tools are unchanged.

### Changed behaviour

- **Release notes come from the CHANGELOG.** `scripts/release_notes.sh <version>` prints the CHANGELOG section for a release; the release workflow uses it for the tag's version and falls back to the git-generated notes when the section is missing, and CI exercises that path on every push.

### Removed (dead-code pass)

- **Gated-tool list consolidated.** The separate `GatedTool` type and `Is*Tool` classifiers (IsMemoryTool, IsSkillTool, IsBrowserTool, etc.) are removed; `gatedToolList` in `toolsmd_parity_test.go` is the single source. `ShouldGateEvenWithAutoApprove` is dropped in favour of testing `SuspendAuto` directly.
- **Skill template wrappers dropped.** `resolveTemplateParams`, `renderTemplate`, `joinSkillTemplate` are removed; tests use the scoped internals (`resolveParamsScoped`, `renderSelection`, `serializeTemplateSection`) the production handlers call.
- **Twenty-eight unreachable functions removed.** deadcode@v0.52.0 reported 92 unreachable; 28 with zero non-test references were removed (e.g. `App.mashuraWatchdogTimeout`, `DetectStruggle`, `ParseLevel`, `NewSessionID`), 24 were kept on an allow list (test-support, intended seams, API surfaces like `DocumentChange.UnmarshalJSON`).
- **Unreachable-code CI check added.** `scripts/check_deadcode.sh` runs deadcode@v0.52.0 and fails when a reported function is not in `scripts/deadcode_allow.txt`. The allow list holds the kept symbols with a short reason each.

## [0.8.3] - 2026-11-15

### Fixed

- **Session grants no longer fail the turn.** Pressing `g` at a tool approval now records a valid grant event and runs the tool, instead of reporting an invalid approval outcome. Sessions containing grant events can be replayed and resumed.
- **One automatic Mashūra round per struggle target.** Repeating a write and repeatedly rewriting the same file now share one symptom identity; repeated failures and repeats of the same command do too. The collapse notice includes the number of automatic rounds started by Wakil.
- **Mid-turn collapse preserves the task and recent work.** The task message stays unchanged, the checkpoint is a separate message, and recent complete tool-call/result pairs are retained up to 30 percent of the collapse ceiling. Immutable task and system content can still exceed that ceiling.
- **Stop-mode handoff keeps its context on the first request.** Building the new session's preamble no longer overwrites the pinned handoff message containing the original task.

- **Mashūra review budget counts panel spend once.** A $1.00 session budget now stops when total spend reaches $1.00, rather than stopping after two $0.30 panel calls because their cost was counted twice.

- **Retry classification is now one rule: HTTP 408, 429, 529 and 5xx are retryable, for the main loop and for Mashūra alike.** The two predicates disagreed (the main loop retried 408/529 where Mashūra failed once). Interactive sessions retry too (previously only auto/headless), each attempt prints the reason and the wait, and `Retry-After` is honoured up to 60 seconds.
- **`backend_max_retries: 0` now really means no retries.** An explicit zero disables retries instead of silently becoming three retries; the normal default remains three.
- **The Mashūra tools are no longer offered when only an Anthropic key is configured.** The direct Anthropic path was removed in 0.8.0 and every Mashūra call needs the OpenRouter key — but v0.8.1 and v0.8.2 advertised the tools for legacy `oracle_enabled: true` configs carrying only an Anthropic key, and those tools could not run. They now read not-offered and not-available, for legacy configs and for the `mashura` block alike. The tool list and call availability now agree, so an offered tool can actually run.
- **Nothing writes to the terminal outside the TUI renderer.** Context-limit probe lines used to be printed to stderr while the TUI was running — on a fresh start and again after `/new` and `/resume` — where they garbled the rendered frame. They now go to the per-session diagnostic log only (headless and `wakil run` keep printing them). The same probe sentence also stops printing a raw `usable_ctx=0`; it shows the computed usable budget instead. The terminal-bell chime fallback is gone (when no native sound player is installed the chime is skipped, never written to stdout from a background goroutine).
- **Automatic Mashūra rounds are shown, attributed and not repeated.** An automatic round now appears in the Mashūra tab like a requested one (marked "started by Wakil"), reaches the model as a message from Wakil rather than as a tool call the model never made, and the second round for the same struggle no longer fires. The collapse line lists automatic rounds separately instead of counting them as the model's tool calls. Bare `/counsel` explains when a round was withheld for the same symptom.
- **Automatic Mashūra rounds printed by their real origin** (see the matching entry under Changed behaviour: Injected rounds are labelled automatic).
- **Mashūra timeout default is now idle+total.** When no timeout is set, the watchdog uses the idle timeout (120 s without data) with a 30-minute total cap instead of a flat 300 s total cap. An explicit number still means a total cap. The `oracle_timeout_seconds` doc and ✓ default are updated to match.

### Changed behaviour

- **Context fallback unified at 128000 tokens.** The config default for `context_tokens_fallback` was 131072 while the counsel fallback was 128000; both now use 128000. Budgets derived from the fallback shrink by 3072 tokens for sessions whose backend fetch failed; sessions with a resolved limit are unaffected.
- **Google search tools removed.** `google_search` and `google_fetch_url`, their config keys (`google_api_key`, `google_cx`) and their docs are gone. A config that still carries the keys loads with one notice. SearXNG serves the same role.

- **Automatic Mashūra rounds respect egress consent.** Sessions whose main
  backend is not OpenRouter ask before the first panel call sends to
  OpenRouter: the ask uses the same external-backend Confirm surface, records
  the grant for the session, blocks the send when declined, and stays
  out of /auto mode's skip (Egress Consent consults the grants first, as the
  `external_backend` carve-out documents). Auto-counsel, the model-callable
  tools, and workflow-triggered panels all go through this one check.
- **The same consent is now asked for every workflow-triggered panel**
  (`/plan review`, `/plan approve`). Previously a workflow-triggered review
  sent without any egress prompt. Headless runs without `--allow-external`
  see the same decline — with the reason named — instead of a silent send.
- **Injected automatic Mashūra rounds say that Wakil started them.** The result given to the model after an automatic debug round now begins with one line stating Wakil started the round automatically and the model did not call it, so a model can no longer claim "I called `mashura__debug` myself".
- **`open_url` asks first.** `docs/tools.md` said `open_url` requires confirmation but the tool was ungated in code (and a test pinned the ungated state). It is now gated like the other host-affecting tools.
- **Session grants now cover writing and running.** Pressing `g` at an approval prompt grants the tool for the session: now also for `write_file`, `write_binary_file`, `edit_file`, `run_shell` and `run_background` (not for `delete_file`, `move_file` or the external-backend ask). A shell grant still prompts for destructive commands — the same destructive test that suspends `/auto` — and destructive prompts never offer `g`. Grants and the reads allowance now survive `/new` and `/resume`; `/auto` destructive never does.
- **Costs on resume now replace, not merge.** Resuming a session replaces its cost rows with the saved ones, so a resumed session's total no longer adds the same spend twice. Headless resume now also clears checkpoints and resets backend consent, matching the interactive path.
- **The legacy Mashūra notice tells the truth.** Only keys the `mashura` block actually supersedes are listed as ignored; keys the block keeps (timeouts, token caps, model, budget, mode, the review budget and the fallback model) are no longer called ignored, and the notice now gives the reasoning-budget numbers (old default: medium effort, 8192 tokens; new level medium: 4096).
- **Session start names the AGENTS.md files.** One line at start names each loaded `AGENTS.md` and says when it was truncated.
- **Unknown hisn session state is a warning, not an instant failure.** A `hisn` session in an unknown state is treated as transitional (ready timeout) instead of failing at once.
- **`/resume`, `/remember` and `/compact` failures say what to try next.** A failed `/compact` (history summary failure) says it will be retried at the next `/compact`.
- **README rewritten for a first-time reader.** It now describes what Wakil is and how to start, with the full tool catalogue in `docs/tools.md`.
- **Sessions can now carry grants, costs and consent across `/new` and `/resume`.** Grant approvals, the reads allowance and (via the saved state) `/auto` survive both rotations; costs on resume now replace rather than merge, and headless resume now clears checkpoints and resets backend consent like the interactive path. See the grants entry above.
- **Long turns collapse mid-turn now.** When the projected request passes the hard-max ceiling (95% of the effective context) or a new absolute ceiling `context_collapse_ceiling_tokens` (default 200000 tokens, `0` disables), the completed prefix of the current turn collapses between tool calls — always keeping the task message, the system prompt, pinned content, the subagent task instruction and tool-call/tool-result pairs intact. It prints the same "collapsed request" line, marked mid-turn. Used to be that only the model could collapse a turn mid-flight by calling `collapse_context` itself.
- **`/handoff` now keeps the task text.** The new session's first request carries the original task text verbatim, every later user message verbatim, and the plan/progress file path when one exists, ahead of the summary — instead of only a summary that paraphrased the task away.

## [0.8.2] - 2026-11-14

### Changed behaviour

- **Mashūra is configured with one block and `/mashura off|low|medium|high`; legacy Mashūra keys are still read and will be removed in a later release.** The `mashura` block (`level` off|low|medium|high, `members`, optional per-tool `overrides`) is the one place to configure Mashūra. `off` closes the tool gate, auto-counsel, and workflow panels; `low` is suggest-only; `medium` suggests on a detected struggle; `high` starts a single-pass `mashura__debug` round on a detected struggle and raises reasoning to `max` (clamped as before) and enables debate for requested rounds. `check` stays at `low` effort below `high`. `/mashura off|low|medium|high|reset` sets a per-repo level (RepoState, never the repository); bare `/mashura` shows the effective level, source, members, overrides, the per-tool table, the auto cap, and the copyable block JSON; `/mode status` shows the level and member count; `/counsel` explains how to turn Mashūra on when it is off. A config with only the old keys is derived into the new block at load (the file is never rewritten); one notice per session says so. When both are present the new block wins and one notice lists the ignored old keys.
- **Fusion, the `oracle__ask` alias and the key-based tool gate are removed.** The `fusion` panel mode (a legacy fusion panel is treated as a normal panel — a one-member panel is a single call), the `CallOracle`/`CallOracleURL` helpers, the `oracle__ask` tool alias (a model calling it gets the normal unknown-tool answer), and the `oracle_api_key_env` tool gate. Tools are now offered under one predicate: effective level not `off`, members non-empty, OpenRouter key present. `wiring/facade.go` now reads the configured `openrouter_api_key_env` name instead of a hard-coded literal.
- **Counsel mode follows the level unless the user chose one: low and medium suggest, high starts a single-pass debug round on a detected struggle, at most 2 per turn by default.** At level `high`, Wakil starts a single-pass `mashura__debug` panel on a detected struggle unless the user chose another counsel mode (`/counsel auto [N]|suggest|off` or the `auto_counsel` config key — the explicit choice wins in either direction). At `low` and `medium` it suggests; at `off` it is silent. Automatic rounds are single-pass at every level (reasoning still follows the level); the announcement line states the level, member count, single-pass marker and round. Automatic rounds are capped at 2 per turn by default (`/counsel auto` without a number uses the default, `/counsel auto N` sets N); bare `/counsel` and `/mashura` show the cap and whether it is the default or the user's value. `/counsel reset` and `/mashura reset` clear the choice so the level decides again. Bare `/counsel` shows the effective mode and its source plus the last struggle evaluation; `/mashura` shows the counsel mode with its source in the per-tool table. When an automatic panel would run but is blocked (no key, no members, no consent, the per-turn cap, or a budget stop), Wakil prints the suggest note plus the reason instead of staying silent.
- **Timeout default.** `oracle_timeout_seconds: 0` now means idle+total as the docs promise: a stream silent for 120s (`stream_idle_timeout_seconds`) is cut, the total cap is 30 minutes — a slow, steadily streaming reply is not cut at 300 seconds. The 300s default is unchanged.
- `phases-only` is documented everywhere as an alias of `on-deviation` (it always behaved the same); no separate code path remains.
- **Reasoning default at `medium`:** the reviewer's default reserve changed from the old `{effort: medium, max_tokens: 8192}` to the `{effort: medium}` table value (4096). Debated panels at `high` now use `max` effort (table top, 16384) with the existing clamp path.

### Fixed

- A read answered from the dedup cache now records a trace entry marked as a repeat, so the struggle detector sees the identical read run twice with no progress (it never did before).
- A symptom is marked as shown only after a line was printed for it, so one silent skip could no longer suppress the hint for the rest of the turn; the per-turn reset of the counsel cap now runs every turn regardless of mode.
- A rule whose symptom was already shown this turn no longer hides a different rule that also fires; the "would show" preview in bare `/counsel` now takes the cap and availability into account.

## [0.8.1] - 2026-11-13

### Fixed

- Inside subagents, a shell command that had succeeded once was refused when run again, even after files changed, and cached file reads were not cleared by shell commands. Deduplication now applies only to read-only file and git tools and is cleared by any write or shell command.

### Changed behaviour

- **Mashūra levels and a single config block.** The `mashura` block (`level` off|low|medium|high, `members`, optional per-tool `overrides`) is now the one place to configure Mashūra. `off` closes the tool gate, auto-counsel, and workflow panels; `low` is suggest-only; `medium` enables auto-counsel (same behavior as today by default); `high` raises reasoning to `max` (clamped as before) and enables debate. `check` stays at `low` effort below `high`. `/mashura off|low|medium|high|reset` sets a per-repo level (RepoState, never the repository); bare `/mashura` shows the effective level, source, members, overrides, the per-tool table, and the copyable block JSON; `/mode status` shows the level and member count; `/counsel` explains how to turn Mashūra on when it is off.
- **Old Mashūra keys still work.** A config with only the old keys is derived into the new block at load (the file is never rewritten); one notice per session says so. When both are present the new block wins and one notice lists the ignored old keys.
- **Removed:** the `fusion` panel mode (a legacy fusion panel is treated as a normal panel — a one-member panel is a single call), the `CallOracle`/`CallOracleURL` helpers, the `oracle__ask` tool alias (a model calling it gets the normal unknown-tool answer), and the `oracle_api_key_env` tool gate. Tools are now offered under one predicate: effective level not `off`, members non-empty, OpenRouter key present. `wiring/facade.go` now reads the configured `openrouter_api_key_env` name instead of a hard-coded literal.
- **Timeout default.** `oracle_timeout_seconds: 0` now means idle+total as the docs promise: a stream silent for 120s (`stream_idle_timeout_seconds`) is cut, the total cap is 30 minutes — a slow, steadily streaming reply is not cut at 300 seconds. The 300s default is unchanged.
- `phases-only` is documented everywhere as an alias of `on-deviation` (it always behaved the same); no separate code path remains.
- **Reasoning default at `medium`:** the reviewer's default reserve changed from the old `{effort: medium, max_tokens: 8192}` to the `{effort: medium}` table value (4096). Debated panels at `high` now use `max` effort (table top, 16384) with the existing clamp path.

## [0.8.0] - 2026-11-13

### Changed behaviour

- **The direct Anthropic path for Mashūra is removed.** All model calls go through the configured transport; an Anthropic key in the config is ignored with a notice.
- `oracle_endpoint` is no longer a config key.
- Correction capture is off by default (`correction_capture`).

### Added

- **Data policy layer (`/zdr`, `/region`) and single transport.** The direct Anthropic path is removed: every model call (main loop, subagents, Mashūra, aux, init test call) goes through `internal/proxy`, enforced by a `go/ast` guard test (`internal/proxy/transport_guard_test.go`) that fails on any outbound HTTP outside the transport. New switches `zdr` (`off` default) and `openrouter_region` (`off` default) with per-repo overrides (`/zdr on|off|auto|reset`, `/region eu|us|off|reset`); with both off the bytes on the wire are identical to the unguarded path. `zdr on` adds `"provider": {"zdr": true, "data_collection": "deny"}` on OpenRouter (merged, never replacing the provider object) and refuses non-local non-OpenRouter hosts before sending; `zdr auto` retries once without the fields on a 404 and remembers the model for the session. `openrouter_region` pins `openrouter.ai` to `eu.openrouter.ai` / `us.openrouter.ai` with no fallback. `trusted_local_hosts` extends the local classification. See `docs/features.md` (Data policy) and `docs/configuration.md` (`zdr`, `openrouter_region`, `trusted_local_hosts`).
- An Anthropic API key, if configured, is now ignored: Mashūra shows one notice per session ("direct Anthropic access was removed; the key is ignored, Mashūra uses the configured backend"). Configured `anthropic:<model>` panel members are served through OpenRouter as `anthropic/<model>`.
- `WAKIL_DEBUG_REQUESTS` also writes one `panel-*.json` capture per Mashūra/counsel call (raw usage objects, served model, status; no headers or keys).
- New config key `mashura_review_budget_usd` (`0` = off): USD cap on one panel operation (members, rounds, retries, parts), checked before each retry, debate round 2 and next fallback member; on stop remaining work is NOT COMPLETED with reason "review budget reached" and obtained reviews are kept. The session `--budget` is respected the same way. Per-operation cost line always printed.
- Panel prompts are now a 7-line skeleton (instructions, material, `<<document>>`, question, `<<question>>`, format instructions, `LAST SECTION READ:` citation), evidence instruction last in the user message, appendix after the document. Answer budget floor 4096 for every tool incl. check; wire `max_tokens` = reasoning + answer budget, reasoning reserved from usable context. Uniform `VOID <model> cause=<c> finish=<f> reasoning=<n> answer=<n> attempts=<n> cost=<usd|unpriced>` line always on, plus `panel-void-<label>-*.json` debug captures.
- **Provider-reported cost and per-source cost split.** When the provider reports `usage.cost` (OpenRouter), that number is the billed amount for that request — the local price table stays as the fallback for backends that report no cost. Costs are split by source (`main` / `subagents` / `Mashūra` / `aux`, where aux covers compaction, the prune rating pass, side questions, and skill planning). `/mode status` shows the session totals (billed, provider-reported vs estimated, cache-read share, split), the TUI info panel shows the same split, and the totals survive save/`/resume`.

### Changed

- `oracle_endpoint` is no longer a user config key: a config file that still contains it loads without error, shows a one-line notice once, and ignores the value. Tests reach the transport endpoint through a test-only hook (`agent.SetOracleEndpointHook`) instead; requests through it still pass `ApplyPolicy`.
- **Correction capture default**: `correction_capture` (new config key, boolean, default `false`) gates the correction-capture loop. When false, no detection runs, no prompt appears and auto mode is never suspended by a correction. Set `correction_capture: true` to restore the previous behaviour.
- **`/zdr` and `/region` mid-session change**: when the value changes after the session has already sent a request, the command output adds one line — "Changing this mid-session resets the prompt cache."
- Missing/wrong `LAST SECTION READ:` citation with non-empty review content is now accepted, flagged coverage not confirmed (obtained review, not VOID); VOID only for empty/citation-only/limit (reasoning ≥80% of total → "reasoning used the budget"). VOID retry by cause (limit → doubled answer budget bounded by model/context + reasoning one step down; empty → reasoning one step down; citation/other → none; at most 2 attempts per member per part; 429/5xx keep single retry). Coverage-not-confirmed members are skipped as responders in final-review gap check and do not auto-advance plan review.
- Mashūra check reasoning default is `low` (was `none`). Budget-stopped and VOID keep workflow unavailable (`available=false`, NOT COMPLETED banner, auto-counsel cap refund).
- `mashura__check` now requires a companion `text_under_review` argument: the exact text whose accuracy should be checked, copied verbatim. The claim is one sentence about what must hold; Wakil rejects the call (no reviewer, no cost) when `text_under_review` is missing or empty. The text is placed as its own `## Text under review` block after `## Claim` and before `## Sources`, inside the material section.
- **OpenRouter session stickiness and no silent compression.** OpenRouter requests now carry `session_id` (the session's chat id; each subagent's own) and `plugins: [{"id":"context-compression","enabled":false}]`, so one provider serves a whole session and OpenRouter never silently drops middle messages. Non-OpenRouter requests stay byte-identical.
- **Budget breach uses the same forced final step.** A session that exceeds its `--budget` now takes the same path as the step limit (tools kept, one "answer now" message, tool calls discarded, one tool-less fallback) instead of stripping tools mid-turn.
- **Forced final step keeps the prompt cache.** Reaching `max_tool_iterations` keeps the tool list and `tool_choice` on the last request (with an injected "answer now" message) instead of stripping tools, which invalidated the whole prompt cache; if the model still answers with tool calls, they are discarded and exactly one tool-less fallback ends the turn. The forced step still always terminates.

### Fixed

- Mashūra panel calls are always recorded: provider-reported `usage.cost` wins; otherwise the price table for the served model (for `openrouter/auto`, the model OpenRouter resolved); otherwise the call is counted as **unpriced** (shown as `unpriced N` in `/mode status` and the info panel) instead of a silent `$0.0000`.
- Zero-token panel results are recorded as calls too.
- **Failed stream cost**: a Stream call that failed after sending now records one cost row from the usage the client published for that request — exact usage when the provider sent it, otherwise the provisional input estimate (marked estimated), unpriced when no price is known. Nothing is recorded for user cancels, policy refusals before sending, or HTTP error statuses before stream data (including the first 404 of the zdr auto retry); Wakil's own idle timeout after stream data does record.
- **Mashūra NOT COMPLETED banner**: it now appears only when no panel member produced a usable answer. When a later member succeeded, each failed member is instead named inline (member + reason). Dispatch, retry, verdict and panel selection are unchanged.

## [0.7.1] - 2026-11-12

### Added

- **Context pruning is experimental and off by default.** In our own tests it gave no reliable saving on large-context models. This release makes it safe to turn on: a file marked and then read again locks for the rest of the session, large drops need a stated reason, marks take effect only after three further tool steps, and thin stub notes are rejected. Everything else (prominent balanced wording, the `WAKIL_DEBUG_REQUESTS` debug dump, the resume refresh) is documented below and in `docs/long-session-mode.md` and `docs/configuration.md`.
- **Marks take effect after three further tool steps.** In `immediate` mode a round marked drop/stub is pruned only after three further real rounds have been recorded after the mark (a tool call in the same message as `context_mark` counts as the first). A model can no longer drop its reads, make one trivial call, and write the final report without the sources. The system section, the tool description and the lone-mark result say "after three further tool steps"; the emergency-level release is unchanged.
- **Stop the drop-and-reread loop.** A file the model marked (drop or stub, any channel) and then read again is now locked for the rest of the session: a later drop or stub on any round carrying that file is refused through every channel, `keep` still works, and the round stays in context (`r12 not marked: <path> was marked in r8 and read again in r12. It stays in context. …`). The reread line now reads `[reread · you dropped this file in r8 and are reading it again. It now stays in context and can no longer be marked]` (`stubbed` for a stub); locked rounds never appear in a backlog line. Paths are compared cleaned and cwd-resolved (`./a.go`, `a.go` and the absolute path match). Wakil-only prunes never lock. The lock survives session save, resume and handoff; `/mode status` shows `locked N`. The lock tracks `read_file`/`read_file_full` paths only (shell, search and LSP reads carry no path and do not lock); a capped read locks the whole path.
- **Large drops need a reason.** `context_mark` gains an optional `drop_reason` map: dropping a round of 4 KB or more without a reason of at least 15 characters is refused (`r3 not dropped: 22 KB. Say in drop_reason why nothing in it is needed any more, or keep it.`). The reason is stored on the round record and shown in the dropped stub line (`[r3 pruned (drop: <reason>): …]`). Small drops are unaffected.
- **One caution back at the top.** The system section now says after "Keep results you are still working with or will cite.": "Do not drop or stub a result you will need again: reading a file a second time costs more than keeping it, and a file you mark and then read again stays in context and can no longer be marked." The user-prompt suffix ends "…and keep what you are still using: a file you mark and then read again can no longer be marked." The `context_mark` description adds the lock and `drop_reason` rules. The system section is now ~1,319 chars (measured); the user-prompt suffix is 357 chars, and the `context_mark` description is 958 chars.
- **Pruning prominence and balance.** With pruning on, the pruning section (1126 chars) is now the FIRST section of the system prompt, before the agent operating instructions (full mode: 51179 chars, core mode: 17183 chars, measured), in full mode and core mode alike. The system section and the user-prompt suffix (296 chars) now say: tool results are re-sent in full until marked, mark finished results while you work, keep what you are still using. The cautions moved to the one place that keeps them — the `context_mark` tool description (736 chars): what drop/stub/keep mean, that a stub replaces the result so only the note remains and a thin note is rejected, that reading a file again after stubbing it costs more than keeping it, and that marking right before the final answer saves nothing. The backlog line and the reread line keep their text. Prune mechanics are unchanged; with prune off the wire is byte-identical.
- **Request debug dump (`WAKIL_DEBUG_REQUESTS`).** Debug aid (see `docs/configuration.md`): one JSON file per model request (<session id>-NNNNNN-<timestamp>.json, 0600 in a 0700 directory), messages and tool definitions as sent, no header and no key material; failure logs one warning and continues. **Warning: the files contain the full session content.**
- **Resumed sessions get the current pruning text.** When `ensurePreamble` finds a stored preamble that does not carry the current pruning section, it refreshes it in place once instead of leaving the stale text.

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
