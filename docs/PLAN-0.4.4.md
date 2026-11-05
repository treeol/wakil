# Wakil 0.4.4 — what was built

This file records what 0.4.4 contains. The per-item plan and the review rounds are
kept below it for the record; where they differ from the code, the code is right.

## Built, by item

- **T1–T5 (tool layer):** completion envelopes carry the status and the output (whole up to 6 KiB,
  else head 2 KiB + marker + tail 4 KiB); finished background jobs are retained for
  `read_process_log` (last 32 / 64 MiB, oldest first, evictions reported); `read_file` and
  `read_file_full` read the sandbox's own `/tmp` in docker and backend executors only; user
  commands run under bash when the sandbox has it; collapses that would not shrink a turn are
  skipped.
- **H1 (test isolation):** `internal/testutil.Main` redirects HOME/XDG/sessions to temp dirs
  and fails a package that changes the real Wakil directories.
- **L1–L2 (locale):** helper commands run under `LC_ALL=C LANG=C`; not-found is decided by
  exit status after a failed read. The German-stderr shim tests are the effective coverage here;
  a real de_CH run needs a host with the locale generated.
- **B1–B3 (browser, docker backend):** lazy launch on first use; a direct child of a `docker exec`
  session in its own process group with a per-launch profile dir (also `HOME`); stop on idle
  (`browser_idle_timeout_seconds`), `/new`, chat end and exit; stop = TERM group, 3 s, KILL,
  straggler sweep, rm; loopback-only tunnel; no published CDP port. Review fixes R1–R3:
  ownership check before signalling, lifetime tied to the manager, bounded exit wait in direct
  mode. Live check against the hardened `wakil-dev` image (`WAKIL_BROWSER_LIVE_IMAGE`).
- **M1–M2:** `wakil --print-workspace-key`; `--think` applies on `/new` and `/resume` without
  saved repo-state.
- **D1–D3:** deprecation lines for daemon / `--remote` / web; skill auto-creation default `off`
  and its notices leave tool results; `WAKIL_WORKSPACE_PATH` announced at startup and in `/sandbox`.

## Not built (remains open)

- A true read-only store open (`mode=ro`) for inspect callers.
- `plan gc` still loads config it does not use (changing that changes the broken-config exit).
- Browser state after a crashed Wakil stays until the container is removed (no startup reap).
- The R-group items are closed for the code paths above; no other review point is open.

---

# Planning record (as written before the build)

# Wakil 0.4.4 — plan

Items T1–T5 (tool layer), H1 (test hygiene), L1–L2 (locale), B1–B3 (browser
lifecycle, docker backend), M1–M2 (small fixes), D1–D3 (deprecations/defaults).
One commit per item, in that order. Gate after each commit (separate commands,
bash): `go build ./...`, `go vet ./...`, `golangci-lint run`,
`LC_ALL=C go test -p 2 ./...`; one `LANG=de_CH.UTF-8 LC_ALL=` run after L.

Hard constraints (user): no store-key / identity derivation change; deprecated
features keep working; tests are fixed or extended, never weakened; user commands
keep the user's locale; no hisn backend / profile / browser-under-hisn change;
never copy real config/credentials into test setups; no tag, no push.

Observed facts this plan rests on (each re-checked this session):

- `docker/entrypoint.sh` does **not** start chromium. The eager start is
  `browser.newDockerManager` (internal/browser/manager.go:136) called from
  `wiring/bootstrap.go:146`; it runs `chromium … &` and a python relay via
  `RunShell` (so the leader is a detached background of a short `sh`), with fixed
  `--user-data-dir=/tmp/wakil-chrome-profile`. Live container showed chromium pgid 75
  (leader pid 81, ppid 1), two `chrome_crashpad_handler` in their own groups (pgid
  102/104), and the python relay on `0.0.0.0:9223`.
- Host reaches CDP via `docker run -p 127.0.0.1::9223` → relay → `127.0.0.1:9222`
  (exec.go:825, manager.go:229).
- `Manager.Close` = `pkill -f` + `rm -rf`, no TERM→KILL (manager.go:376-387).
- Detached/notify_on_exit completion text: `last output:` = last **2 lines** of an
  8 KB tail + "use read_process_log(...)" hint (tool_handlers.go:574-586, 613, 651).
- Finished background jobs are deleted from `bgProcs` by the reaper on natural exit
  (tool_handlers.go:266) and by the in-deadline path; `read_process_log` then says
  "no background process with id" (tool_handlers.go:1613).
- `ConfinePath` in all executors rejects anything outside the workspace root;
  only the host toolcache is special-cased in `handleReadFile` (tool_handlers.go:780).
- `sh -c` is spelled in `exec.go` (Direct RunShell/StartBackground),
  `exec.go:1253` (Docker RunShell), `exec_ops.go:373,387` (Docker/Direct StartBackground),
  `backend_executor.go:359`. The exit-marker wrapper is POSIX only
  (exit_marker.go:20-23,75); a bash-only construct in dash yields a parse failure ⇒ no
  marker ⇒ "exit code unknown".
- Sandbox has no `de_CH` locale and no locale sources (`/usr/share/i18n/locales`
  has no `de_*`), so a real German-locale run is impossible here.

---

## T — tool layer

### T1 envelopes carry exit code + output
Replace `shellTailPreview` (last 2 lines) by `shellOutputPreview(entry)`:
status line (exit code via marker) + body = whole output if ≤ cap, else
head (2 KB) + `\n[… N bytes omitted, full log: <path> …]\n` + tail (4 KB).
Built from `StatFile` + `ReadFileBounded` (head) + `ReadFileTail` (tail, marker
parsed first so the end anchor holds). Used by `publishBgCompletion`,
`notifyDetachedShellExit`, the inline "process already exited" returns and the TUI
tab Done. Hint text kept after the output (`read_process_log(bgN)` still works, T2).
The budget is for the **fully rendered** message (header + cmdDigest + status + body +
hint + log path), ≤ 7500 bytes, so neither `asyncEnvelopeOpCap` (8000) nor the tab
preview cap (8000) re-cuts it; both delivery paths (inbox envelope, tab Done) are tested.
Test: completion text for an `exit 3` job contains the real status text
`exited with code 3` (as `ExitStatusLine` renders it), the output, and for a large log
the omitted-bytes marker and the log path.

### T2 finished jobs stay readable
`bgRegistry` gains `bgFinished` (id → entry, insertion-ordered), `bgFinishedBytes`, and
`bgEvicted` (set of evicted ids; counter arithmetic is NOT used: `bgCounter` advances on
declined/failed launches too). `retireBg(id)` is **idempotent** (no-op if the id is
already retired/evicted/never present) and runs under `bgMu`. It is called only **after**
the last reader of the log is done: reaper natural exit → after
`publishBgCompletion`/`announceShellDone`; in-deadline path → after the log tail was read;
kill_process → after the group is confirmed gone or SIGKILL sent. Retired entries are not
in `bgProcs`, so shutdown (`StopAllBackgroundProcs`, to be read before coding), liveness
polling, async-slot release, notification suppression and tab finalisation are untouched
(they key off `bgProcs`/`entry.asyncOp`, which retirement does not alter). Deadline/cancel
branches that look up only `bgProcs` and find a retired entry report
`already finished: see read_process_log(bgN)` instead of "no longer tracked".
Bounds: 32 jobs / 64 MiB of retained log (`StatFile` at retire; a single log larger than
the byte cap is retained alone, never evicts itself while being read); eviction is
oldest-first, deletes the log (best effort) and records the id in `bgEvicted`.
`read_process_log` on a retired id returns the log tail and `exited (code N)`; on an id in
`bgEvicted` → `ERROR: background job bgN was evicted (retention: last 32 jobs / 64 MiB,
oldest first)`; never-issued/declined ids keep "no background process with id".
Retired entries from an older executor generation report "process lost".
Tests (deterministic): read after exit; eviction by count and by bytes; double retire;
declined launch id is "no background process", not "evicted"; retire racing the deadline
branch; shutdown with retired entries.

### T3 read_file may read the sandbox's /tmp (read-only)
Scope statement (corrected during implementation): `run_shell cat /tmp/x` already reads
these files in a sandbox, but `run_shell` is gated and `read_file` is not. So the exception
applies only where `/tmp` is the sandbox's own (optional `exec.SandboxTmpReader`, implemented
by the docker and backend executors); under the direct executor `/tmp` is the host's and
stays rejected. Mechanism, fail-closed: if
`ConfinePath` rejects and the path is absolute, contains no `..` after `Clean`, and is
`/tmp` or lexically under `/tmp/` (component boundary), canonicalise inside the sandbox
with a single quoted `readlink -f -- <path>` through `RunShell` (error, empty or ambiguous
output ⇒ reject); accept only if the canonical path is still `/tmp` or under `/tmp/`;
read it with `ReadFileBounded` (same size limit, binary check). A symlink swapped between
canonicalisation and read can only point at something readable by a sandbox process
anyway; documented as such. Applies to `read_file` and `read_file_full` only; writes,
edit, move, delete, `list_dir`, `search_files` keep `ConfinePath`. Same code path for all
executors via `Executor` methods; no change in `internal/sandbox/hisn`.
Test: file in `/tmp` readable; `/tmp/link → /etc/x` rejected; `/tmp/../etc/passwd`
rejected; `/tmpx/…` rejected; write to `/tmp` still rejected; tool description updated.

### T4 bash when available (revised after round 1)
No executor change. The shell choice is made **only in the run_shell / run_background tool
handlers**, so helper scripts (find/search/ConfinePath/…, which also use `RunShell`) and
`BackendExecutor`/hisn are untouched. The handler probes once per executor generation
(`RunShell("command -v bash")`, cached) and, if bash exists, rewrites the user command to
`bash -c <shQuote(command)>` before it reaches `RunShell`, or before `WrapExitMarker` for
background runs (`WrapExitMarker("bash -c '…'")`: the sh-parsed wrapper only ever sees a
quoted string, so bash-only syntax cannot break the marker; `exit N` inside the command
exits the bash child and the wrapper still captures N). Without bash the command is passed
unchanged (sh). The run_shell tool description states the shell, built from the probe
(`UserShell()` on `App`). Under hisn the probe runs in the sandbox like any command; if
bash is absent there nothing changes.
Tests: `false | true; echo ${PIPESTATUS[0]}` prints `1` (separate expectation from the
exit status, which is 0); `bash -c 'exit 3'`-style background job reports code 3 via the
marker; bash-only syntax (`[[ ]]`, arrays) works foreground and background; quoting with
single quotes / trailing backslash / syntax error (fails closed, "code unknown" as
documented); no-bash fallback via a fake executor; description names the shell.

### T5 collapse never inflates
At the point where the collapsed turn text is materialised, compare the byte size of
the collapsed form to the original; if not smaller, keep the original (truncated by the
normal cap) and the note says "collapse skipped: collapsed form would not be smaller
(X ≥ Y bytes)". Exact hook chosen after reading `collapse.go` (`formatCollapseNotice`,
`materializeCollapsed`). Test: tiny working set whose summary+pointer is larger than
the original is left unchanged and noted.

## H — test hygiene
H1: new `internal/testutil` with `Main(m *testing.M) int`: captures the real HOME /
XDG dirs, points `XDG_DATA_HOME`, `XDG_CONFIG_HOME`, `WAKIL_HISTORY_FILE`,
`WAKIL_SESSIONS_DIR` (and HOME if the suite tolerates it) at a temp dir, runs, then
compares a before/after listing (depth-limited names + `input_history` size) of the real
`~/.local/share/wakil` (and `~/.config/wakil`) and fails the package on any difference
(`WAKIL_TEST_ALLOW_REAL_DIR_CHANGES=1` disables the guard — a concurrently running
real chat on the same machine would otherwise cause false positives). Called from every
existing `TestMain`; a new one for `internal/tui`; any further leaker found by running
the suite with the guard only. Leak packages known: wiring, tui.
Test of the guard itself: a helper test that creates a file in a fake "real" dir and
checks the comparison reports it.

## L — locale
L1: helper commands whose output/stderr the executors parse run with `LC_ALL=C LANG=C`
(docker `exec -e`, backend `sh` prefix, direct `cmd.Env`); `RunShell` and the user
command inside `StartBackground` do not. Error classification prefers exit status /
`test -e` over stderr text (`isShellNotFound` stays as fallback).
L2: `apply_skill_template` existence check treats not-found by stat/exit status, not
stderr text. Regression test with an injected German "Datei oder Verzeichnis nicht
gefunden" stderr and with `LANG=de_CH.UTF-8 LC_ALL=` applied into an empty dir.
de_CH full run: locale cannot be generated in the sandbox ⇒ run the suite with the
variables set (falls back to C here) **and** state that the injected-stderr tests are the
effective coverage; real German-locale confirmation is a host step.

## B — browser lifecycle (docker backend)
Design (replaces the 0.4.4-draft B4 tuple design):

- **B1 lazy + direct child.** `NewManager` launches nothing (docker mode). First tool call →
  `ensure()` (mutex-serialised with stop; rollback of dir/process on any partial failure):
  `StartInteractive` runs `sh -c 'mkdir -p D/home && echo $$ > D/pid && exec chromium
  --headless=new --no-sandbox --disable-gpu --disable-dev-shm-usage --disable-crash-reporter
  --disable-breakpad --remote-debugging-port=0 --user-data-dir=D'` with `HOME=D/home` and
  `D=/tmp/wakil-chrome-<random>`. The process is a direct child of the `docker exec`
  session (observed earlier: own session, ppid 0, pgid=sid=pid) and stays alive holding the
  pipes. Because Chromium picks the port itself, the endpoint is read from
  `D/DevToolsActivePort` (written by chromium): a stale browser from a crashed Wakil can never
  be mistaken for the new one (no fixed 9222).
- **Containment.** `HOME=D/home` places crashpad's `--database=` path (and any other
  `$HOME`-relative state) under `D`, so every process that escaped the group still has `D`
  in its cmdline. Stop = TERM the recorded group, wait 3 s, KILL; then a `/proc` scan
  (`/proc/*/cmdline` containing `D`, plus pgid == recorded) → TERM/KILL stragglers; then
  `rm -rf D`; then verify the scan is empty and report if not. Live check on the real
  `wakil-dev` image with `ps -eo pid,ppid,pgid,sid,args` during and after (output captured in
  the report); if crashpad escapes without `D` in argv this is reported, not hidden.
- **B2 stop triggers:** idle timer (`browser_idle_timeout_seconds`, default 600; flat key per
  repo convention; the brief's `browser.idle_timeout`) that never fires while an operation is
  in flight (in-flight counter, timer re-armed on op end), `/new`, chat end, `Close` (Wakil
  exit). The next tool call starts a new browser in a new dir.
- **B3 loopback only.** No `0.0.0.0` listener: python relay removed, `-p 127.0.0.1::9223`
  removed. Host opens `127.0.0.1:0`; each accepted connection starts
  `docker exec -i <c> python3 -c <forwarder to 127.0.0.1:<port from DevToolsActivePort>>`
  with raw binary bidirectional copy, half-close/EOF handling, stderr discarded to a bounded
  buffer, and the exec process killed and waited on disconnect, stop and failed start.
  chromedp's remote allocator connects to the host loopback URL. Required test: real CDP
  `/json/version` **and** a WebSocket action (`Navigate about:blank` + `Title`) through the
  tunnel against the live image; plus a fake-executor unit test (no launch until first call,
  idle stop with injected timeout, stop on /new, per-chat dirs differ, tunnel listener is
  `127.0.0.1`, docker args no longer publish the CDP port, startup with a stale listener
  present).
- Direct (host) mode: same lazy/idle/stop lifecycle via `NewExecAllocator`, but the first
  `chromedp.Run` must keep using the manager-owned context (existing constraint,
  manager.go:283-291), never an operation context; test pins that. hisn stays off.

## M — small fixes
M1: `wakil --print-workspace-key` prints, for the current cwd/config: chat memory store
key, sessionhost workspace id, `plan gc` (ResolveProject) key, each with its source
(cwd / `WAKIL_WORKSPACE_PATH` / config). Pure reporting over the existing derivations —
no derivation change. Handled before the TUI/bootstrap start.
M2: `/resume` on a workspace without RepoState also applies `--think` (and the other
per-chat CLI flags `/new` honours); trace in `conversation_manager.restoreRepoState`.

## D — deprecations and defaults
D1: `wakil daemon`, `--remote`, web console print `deprecated — will be removed in a
future release` once at start; CHANGELOG "Deprecated"; docs note. No behaviour change.
D2: `skill_creation.mode` default `off` (explicit setting wins — empty string = default);
when `on`, the suggestion is a UI/status notice, never appended to a tool result
(`appendSkillHint` at skill_suggestion.go:195-213 / turn_phases.go:477); pipeline marked
deprecated. Skills store, templates, `load_skill` untouched.
D3: with `WAKIL_WORKSPACE_PATH` set, one startup line (path, resulting store keys, "overrides
the cwd") from M1's resolver; also in `/sandbox`.

## Mashūra round 1 (this plan)
astra (gpt-6-astra): read, nine findings, all triaged above (T1 budget, T2 idempotent
retire + evicted-id set, T3 scope statement, T4 handler-only rewrite, B containment via
HOME=D/home, DevToolsActivePort, tunnel tests, idle in-flight guard). fable-5.1: VOID on
the first attempt; retried once (see close-out). Not adopted: moving H1 before T (does not
change a test result; the gate runs leak into the real data dir until H1 lands — noted).
Opt-in vs default real-dir guard: the brief asks for a failing guard; kept, with
`WAKIL_TEST_ALLOW_REAL_DIR_CHANGES=1` as the escape hatch.


## Round-1 corrections (second reader, fable-5.1, on the revised plan)
Checked locally before adopting: `sh -c "bash -c 'if then'; echo rc=$?"` prints `rc=2`, and the
exit-marker wrapper around `bash -c '…'` therefore records **2**, not "unknown".

- **T4 test expectation fixed:** with bash present a syntax error is `exited with code 2`
  (marker written); with no bash the old fail-closed "code unknown" stays. The limitation
  comment in `exit_marker.go` is updated accordingly.
- **T2 (atomic claim):** the move `bgProcs → bgFinished` happens in ONE step under `bgMu`
  at the point where today's code deletes from `bgProcs` (reaper natural exit, in-deadline
  path, kill paths) — whoever deletes today claims and publishes; behaviour of those paths is
  otherwise unchanged. Destructive eviction (rm of a log) is gated by a per-entry reader
  count (`readers` incremented by `read_process_log`, completion preview and in-deadline
  read) so a log being read is never removed; an entry with readers>0 is skipped and evicted
  at the next retire.
- **B1 (drain + env):** `StartInteractive` (exec.go:1452) is `docker exec -i … sh -c` without
  `-t`, has no env parameter: HOME is set inside the script (`HOME=D/home exec chromium …`).
  The browser launcher drains stdout and stderr in goroutines into a bounded ring (last
  8 KiB, used for launch-failure diagnostics) and kills + waits the local `docker exec`
  client on stop.
- **B2 containment scan self-match:** the straggler scan passes `D` via an env var read by a
  script that excludes its own pid/ppid; test asserts an empty scan after a clean stop.
- **B1 readiness:** `DevToolsActivePort` is accepted only when it has two lines.
- **T3 on macOS:** compare the canonical path against `readlink -f /tmp` (canonical /tmp,
  `/private/tmp` on macOS) computed in the same sandbox call; fail closed if `readlink -f`
  is unsupported. (Tests run on Linux in CI; darwin only cross-compiled.)
- **L1 and hisn:** `BackendExecutor` (internal/exec/backend_executor.go) is the generic
  Wakil-side executor over `sandbox.Backend`; `internal/sandbox/hisn`, profile generation and
  `.wakil/sandbox.json` are NOT touched. L1 puts `LC_ALL=C LANG=C` in the helper script text
  built by the executor; T4 changes only the handler's command string. Recorded as a
  deliberate, scoped use of "no hisn change".
- **B3 live test:** needs docker; it skips when docker or the image is unavailable (same
  pattern as the hisn integration tests), and is run by hand here (docker is available in
  this sandbox) with the output captured in the close-out. It is NOT part of what the
  `go test` gate proves when skipped; the close-out says which ran.
- **B3 and D1:** the only consumers of the published CDP port are `exec.go:825/913/1085`
  and the browser manager (grep); the deprecated daemon/web stack does not use it. Tests
  asserting the `-p` argument are updated, not weakened.
- **T4 drift:** grep for tests relying on dash `echo -e/-n`: none found
  (`grep -rln "echo -[enE]" internal --include=*_test.go` empty).
- **T4 description:** built lazily at tool-definition time from the cached probe; before the
  first probe it says "sh (bash when available)".
- **T1:** whole-output threshold = 6 KiB (head 2 KiB + tail 4 KiB), so head/tail never overlap.
- **B2:** `/new` and stop wait for the in-flight counter (bounded 5 s, then stop anyway; the
  op errors).
- **Deviation recorded:** config key is flat `browser_idle_timeout_seconds` (repo convention),
  not nested `browser.idle_timeout`; CHANGELOG says so.
- **Version:** `internal/buildinfo.Version = "dev"` (ldflags) — nothing to bump in Go; the
  version is stated in CHANGELOG `[0.4.4]` only. T2 logs are removed at shutdown
  (`StopAllBackgroundProcs` extended to retired entries).

## Open points (carry into close-out)
- H1 lands after T, so T-commit gates still write test data into the real data dir.
- Stale chromium after a **crashed** Wakil stays until the container is removed
  (no startup reap in this release).
- H1 guard can false-positive when a real chat runs concurrently (override env documented).
- Real de_CH run is a host step.
