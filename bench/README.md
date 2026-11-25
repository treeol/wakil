# bench

A small benchmark harness for Wakil. It runs a set of reference tasks
through `wakil run` and records one CSV row per run in `bench/results/`
(gitignored), so a model can be compared across releases without hand-
staging experiments.

## Run

```
scripts/bench.sh [--wakil PATH] [--model NAME] [--task NAME] [--runs N]
                 [--timeout SECONDS] [--mashura LEVEL] [--context-modules on|off]
                 [--no-preflight] [--self-test]
scripts/bench.sh --setup
```

Defaults: the `wakil` binary from `PATH`, the user's configured model,
all tasks, one run per task, 600 seconds per run, and Mashura off.

At the end of a run the script prints a table for that run and, next to
it, the most recent earlier CSV whose model matches, plus the total cost
of both runs. Exit status is 0 when the harness itself worked, whatever
the task results were; non-zero only on harness errors.

Before each run the script prints one line with the task index, task
name, run number, and log path. After each run it prints one line with
the result (`pass` / `fail` / `timeout` / `error` / `invalid` — `invalid`
is only a completed `collapse-long` run with 0 collapses; a run that never
reached the model is `error` with a specific reason such as `bootstrap`,
`config-unreadable`, `config-relative-escape`, or `missing-workdir`),
elapsed seconds,
tokens in/out, cost, and — for non-pass results — the exit reason from
`--result-json` when available. When stdout is a terminal, it also
prints one short heartbeat line every 30 seconds with the elapsed time
while a run is in progress, so a long task does not look hung; nothing
extra is printed when output is piped or redirected.

## Setup (one-time)

```
scripts/bench.sh --setup
```

Creates all task work folders and prints, per folder, the exact
`WAKIL_HOST_WORKDIR=<path> wakil sandbox trust` command the user must
run once. The harness itself never runs that command and never reads
from the terminal. The acknowledgement is keyed on:

1. **Path** — `sha256(canonicalWorkspace(path))` in
   `internal/agent/workspace.go:44` (`workspaceKey`). A stable path
   means the key is stable.
2. **Content hash** — `sha256` of the raw bytes of
   `.wakil/sandbox.json` in `internal/sandbox/project/project.go:99-100`
   (`Load`). The approval file (`<stateDir>/approved/<wsKey>`,
   `project.go:146-152`) stores the hash; `TrimSpace(stored) == hash`
   means approved.

Emptying and refilling the folder does **not** invalidate the
acknowledgement as long as the risk label (and thus the
`.wakil/sandbox.json` content) is the same: the hash is of the file's
bytes, and the same content yields the same hash. The `.wakil/`
directory is **preserved** when the folder is emptied and refilled —
the harness deletes everything inside the folder except `.wakil/` —
so the approved `.wakil/sandbox.json` stays in place and its hash is
stable. If the risk label changes (different hisn version, different
config), the content changes, the hash changes, and the approval is
invalid — the user must re-acknowledge with `WAKIL_HOST_WORKDIR=<path> wakil sandbox trust`.

## Go requirement

The Go tasks (`add-flag`, `fix-test`, `rename`, `refactor-multi`, `debug-subtle`, and `long-read`'s and `collapse-long`'s setups) need the
toolchain their `go.mod` names (the same as the repo's `go.mod`) in the
**local cache**. `scripts/bench.sh` resolves it once at start, in the repo
root with your environment (`GOPROXY=off go env GOVERSION`; the host's own Go
may be older, it then uses the toolchain it already downloaded) and remembers
where that toolchain is on disk (`go env GOROOT`). Every Go command of a check,
of a `setup.sh` and of the preflight runs that toolchain's own `bin/go` with
`GOTOOLCHAIN=local` and `GOPROXY=off`, so it works with any `HOME`; the Go
cache locations are resolved the same way. **A check never downloads a
toolchain or a module.** Run `go version` once inside the
repo while online to fetch the toolchain; a host with `GOSUMDB=off` cannot
switch toolchains (Go verifies the cached one against the checksum database).

Before the first run, and so before any model request, the harness runs
`setup.sh` and every Go check once on each task's reference solution. If that
cannot work it prints one line and exits 1 without writing a CSV row:

```
bench.sh: the Go checks cannot run: the tasks need go >= 1.26, found go1.25.1 — go: go.mod requires go >= 1.26 (running go 1.25.1; GOTOOLCHAIN=local)
```

`--task steps` needs no Go at all (no Go is resolved and the preflight is
skipped); `long-read` and `collapse-long` need it for their `setup.sh`. `python3` is checked before the first run when a selected task's `check.sh` or `setup.sh` uses it. GNU userland is assumed (`tar`, `timeout -k`, `flock`, `sha256sum`, `find`).

## How a run works

For each task and run number:

1. `bench/tasks/<name>/repo/` is copied to a **fixed** folder
   (`${XDG_CACHE_HOME:-$HOME/.cache}/wakil-bench/work/<task>`) and given
   `git init` + one commit with a repo-local identity
   (`user.name "wakil-bench"`, `user.email "bench@localhost"`), so every
   run starts from the same state and the stable path means the user's
   one-time sandbox acknowledgement holds. The `.wakil/` directory is
   **moved aside** (mv) before the folder is emptied and moved back after,
   so the directory inode and its content stay untouched — the user's
   approved `.wakil/sandbox.json` is never copied or re-created. `.wakil/`
   is added to `.git/info/exclude` so it does not show up as untracked.
2. If the task ships a `setup.sh`, it runs in the work copy before the
   agent and before the folder's one commit, so nothing it generates from
   (a generator, a seed) lands in the folder's git history. A failing
   `setup.sh` aborts the run (it is not scored as a model failure).
   Right after the refill a leak guard checks that no `check.sh`,
   `hidden`, `solution`, `gen`, `setup.sh`, `answer-key.json` or seeded generator
   is in the folder or in its history; a hit aborts the bench. If a task's
   `setup.sh` leaves an `answer-key.json` in the work folder (v2's
   generator did — the key landed next to the docs output), the harness
   removes it before the commit so it never sits in git history, and the
   leak guard fails the prep if it reappears.
3. `wakil run --auto --allow-destructive --no-memory [--auto-counsel]
   --result-json <file> --transcript <log>` runs with the task text as
   the final argument, under a `timeout` guard (with stdin from
   `/dev/null`). `--allow-destructive` is passed because the work folder
   is a throwaway copy inside the sandbox and the workdir guard is in
   place — without it, the bench measures approval friction instead of
   the model. `--no-memory` skips per-project memory so there is no
   carry-over between runs (no memory tools, no `plan/current` read or
   write, no memory digest in the prompt). `WAKIL_HOST_WORKDIR` is set
   to the fixed work folder so the sandbox mounts it (not the configured
   `work_dir`). **Every task** (not only the collapse tasks) runs with
   `WAKIL_CONFIG` set to a merged per-run config: a copy of the user's
   own resolved config (`$WAKIL_CONFIG`, else
   `~/.config/wakil/config.json`, exactly the path Wakil resolves) from
   which every MCP/external-tool key is stripped (`mcp_servers`,
   `subagent_mcp_servers`, `mcp_module_keywords`, `searxng_url`, all
   oracle/mashura keys, the workflow counsel keys, `browser_enabled`,
   `lsp_enabled`), with the enabled-by-default ones forced off
   (`oracle_enabled: false`, `wf_final_review: false`,
   `mashura: {level: "off"}` — deletion alone would fall back to the
   default "medium" level), and — for the collapse tasks only — two keys
   added: one ceiling per task (`context_collapse_ceiling_tokens: 45000`
   for `collapse-long`, `21000` for `collapse-long-v2` and
   `collapse-long-v3`, see "Ceiling calibration" below) and
   `context_collapse_min_bytes: 2000`. **Why the strip:** until commit
   `1ac8e01` the run config came from the user's own file, so agents
   under test had the user's MCP servers available and called them — 46
   run logs in `bench/results/` contain MCP tool calls (list in plan
   §12.7); the caj tools run in a different sandbox than the bench
   workdir, so work landed in a foreign workspace. `SEARXNG_URL` is
   unset for the launch too (env overrides apply after the file).
   The ceilings are low, bench-only,
   and identical for modules off and on, so every model collapses at the
   same projected size regardless of its probed window. Wakil reads exactly
   one config file and the two collapse keys have no env override, so a
   merged file is the only way to layer them on top of the user's
   endpoints without replacing them; Wakil itself is unchanged. The
   merged file carries the user's endpoints (with keys). Privacy: it
   lives in a private `mktemp -d` dir (0700, never inside the repo or
   under `bench/results/`), is created at mode 0600 (subshell
   `umask 077`), is deleted right after `wait` on every path (success,
   failure, timeout), and its contents, path, and key values are never
   printed, logged, copied into the run log or the CSV. A normal-mode
   EXIT/INT/TERM trap in the bench `main()` is the backstop when the
   bench shell itself is interrupted (the merged file exists for every
   task now). The run log says only
   `merged config: mcp/external keys stripped, counsel off[, ceiling=<N> min_bytes=2000]`
   with the task's N (45000 for collapse-long, 21000 for v2/v3). The sandbox only
   mounts the fixed work folder, so the agent under test cannot read the
   host temp dir; wakil's own error output is not covered (an endpoint
   error could name a URL in the transcript). The temp dir also
   **mirrors** the user's config directory: every entry of
   `~/.config/wakil/` except the config file itself is symlinked into it,
   so relative paths resolve exactly as in the user's normal setup
   (`style_guide_paths: "styles/go.md"` and the implicit `agent.txt` next
   to the config file — Wakil resolves both from the config file's
   directory). A normal `wakil run` writes nothing into the config dir
   (only `wakil init` and the data-dir memory store write, and neither is
   a bench run), so the links are read-only in practice; the mirror is
   never made of a shared directory (a `WAKIL_CONFIG` in `$HOME` or `/`
   gets a merged file only, no mirror). Cleanup deletes `config.json`
   first, then the recorded symlinks, then `rmdir` — never `rm -rf` on a
   dir holding links, and a leftover is reported by name, not deleted. A
   `style_guide_paths` value that `Clean`s to an escape from the config
   dir (e.g. `../x`) is stopped as `error` / `config-relative-escape` /
   `valid=false:config` (the temp dir sits at a different depth, so such a
   path would mis-resolve), and the bench continues. A missing user config
   is merged onto `{}` plus the strip (env-only setups still run — a
   missing file means defaults+env apply in Wakil too); an unparseable
   user file is recorded
   as `error` / `config-unreadable` / `valid=false:config` and the bench
   continues. The user's own
   config is read by the host-side bench at run time, never by
   `--self-test` (which uses a fake config with a fake secret sentinel —
   see "Self-test"). A run that fails during bootstrap (before any model
   request, no result JSON — its `bootstrap error:` line is in the run
   log) is `error` / `bootstrap` and the bench stops; only the known-safe
   `style_guide_paths` shape of that line (key name + resolved path, no
   secrets) is printed to the terminal, other bootstrap errors are
   reported by name only.
   **MCP guard.** After each completed run, the run log is scanned for
   MCP tool calls: a transcript line whose tool name immediately after
   the `· ` marker contains `__` (the `<server>__<tool>` naming scheme)
   and does not start with `mashura__` (Wakil's built-in counsel tools —
   the strip already turns those off). A hit means the agent reached an
   MCP server despite the strip: the row is `invalid` /
   `mcp` / `valid=false:mcp`, the tool names are written to the log, and
   `check.sh` is skipped (an isolation failure is not a task result).
   With no servers configured a hit should be impossible — the strip is
   the primary control, the guard is the tripwire. Rows before commit
   `1ac8e01` may contain MCP calls (46 logs; list in plan §12.7) and are
   not comparable with rows after it.
4. `check.sh` from the task folder runs in the work folder after every
   run that ended (`pass`, `declined`, or `error`); `result` is `pass`
   or `fail` from `check.sh`, and `exit_reason` keeps what the run
   reported. `timeout`, `wrong-workdir` / `missing-workdir` /
   `workdir-not-verifiable`, `leak-detected` and
   `sandbox-not-acknowledged` skip `check.sh`. A bootstrap failure skips
   `check.sh` too: a run that never reached the model never reaches the
   check, and is `error` / `bootstrap`, never `invalid` (the `invalid`
   rule fires only on a completed `pass`/`fail` with 0 collapses).
5. **Workdir guard.** After each run, the harness reads `workdir` and
   `sandbox_backend` from the result JSON (a file under
   `${XDG_CACHE_HOME:-$HOME/.cache}/wakil-bench/run/<task>.json`, outside
   the folder the agent can write to) and compares `workdir` to the fixed
   folder or its `pwd -P` form (hisn reports the symlink-resolved path).
   A mismatch is `error` / `wrong-workdir`; no `workdir` is `error` /
   `missing-workdir`. Under the **docker** backend the binary reports a
   container path (`/mnt/<name>`), which cannot be compared with the host
   folder: the run is `error` / `workdir-not-verifiable` (the line says
   `workdir not verifiable (docker)`), so docker cannot be benched until
   the binary reports a host path. A binary without `sandbox_backend`
   (older, or a stub) gets the plain comparison. Every guard stop skips
   `check.sh` and stops the bench at once.
6. **Post-run "not acknowledged" / "has not been approved" detection.**
   When a run ends with either sandbox approval error, the exit reason is
   `sandbox-not-acknowledged` and the bench stops with the folder and the
   one `WAKIL_HOST_WORKDIR=<path> wakil sandbox trust` command to run,
   before any workdir guard message. The harness never runs `wakil sandbox
   trust` itself — only `--setup` prints the commands for the user.

The task's `check.sh`, `hidden/` files, `solution/` reference result and
`setup.sh` are **never** copied into the work folder: the agent cannot pass
by reading or editing the check. After each run the harness also verifies
(by file existence only; the folder's `.git` is agent-controlled after the
run) that the work folder does not contain `check.sh`, `hidden` or
`solution`, and reports `leak-detected` if it does.

**Timeouts.** `timeout -k` ends a run that ignores TERM with exit 137 after
a grace period (`BENCH_KILL_GRACE`, default 10 seconds). Exit 124 and exit
137 are both `result=timeout`; the result JSON and transcript of such a run
are not trusted (they are agent-controlled, and a hung run may have left a
`pass` in them), and neither the workdir guard nor `check.sh` runs. A 137
that arrives before the deadline is a kill from outside (for example the OOM
killer); it is also `timeout`, with `exit_reason=killed`.

**Parallel runs.** One `flock` per task
(`${XDG_CACHE_HOME:-$HOME/.cache}/wakil-bench/locks/<task>.lock`, outside the
work folder) is taken once per task and held from before the folder is first
emptied until the last of its `--runs` has ended (never released between two
runs). No program the bench starts (wakil, the checks, `git`, the heartbeat
and its `sleep`) inherits the lock, and nothing of a run is left alive after
it. A second bench on the same task prints `task <t> is already being run by
another bench`, exits 75, writes no CSV row and leaves the folder alone.
Different tasks can run in parallel. The harness refuses to run without
`flock`; it also requires `jq`, `timeout`, `sha256sum` and `git`. The tools
are checked first, with shell builtins only: a missing one gives one line
(`required tool(s) not found: a b c`, naming all of them) and exit 1, before
anything else runs, `-h` included; the list is `harness_tools` at the top of `scripts/bench.sh`.

**Fake passes.** The Go tasks (`fix-test`, `rename`) never run the agent's
tree. `check.sh` (via `bench/lib/gocheck.sh`) works on a clean copy of the
work folder (no `.git`, no `.wakil`): it refuses symlinks, `go.work` and
`vendor/`; deletes every `*_test.go` and `testdata/` (the agent's tests are
ignored, not penalised); restores the task's reference `go.mod` and drops
`go.sum`; refuses any non-test package that imports outside a per-task
allow-list of standard-library packages (so agent code cannot reach `os`,
`syscall`, `runtime`, `unsafe`, `embed` or cgo, and cannot call `os.Exit`) or
that carries assembly, C or cgo files (the import closure is walked, so a package in a `_x` or `.x` directory, which `./...` skips, is checked too); copies the task's `hidden/` tests in
and runs them with a fixed Go environment (`GOFLAGS=-buildvcs=false`,
`GOWORK=off`, `GOENV=off`, `GOTOOLCHAIN=local` with the resolved toolchain's `bin/go` first on `PATH`,
`CGO_ENABLED=0`, `GOPROXY=off`; see "Go requirement"). The test run must exit 0, print no `--- FAIL`, and print a
`--- PASS: <name>` line for every named hidden test. The self-test covers a
`TestMain` that exits 0 and an `init()` that prints a `PASS` line and exits,
and an `init()` in an underscore-directory package that the main package imports, on both Go tasks (each must score `fail` with a `check:` reason in the log). A timeout row records no tokens or cost (the result JSON of a cut-off run is not trusted).

**Result class of a check.** `check.sh` exits 0 (`pass`), 1 (`fail`: the check
ran and judged the agent's work) or 99 (`error`: it could not judge). `fail` is
reserved for a judgement: `gocheck_fail` exits 1 and leaves a verdict marker,
and an exit trap turns a status 1 without it (a failed `cp`, a crashed helper)
into 99. Any status other than 0 or 1 (missing `python3`, a kill, the
`BENCH_CHECK_TIMEOUT` of 900 seconds) is `error`. When a go command fails in a
check, the check is re-run on the task's reference solution: if that fails too
the environment is to blame (`error`), else the agent's tree is (`fail`). An
`error` row has `exit_reason=check-error`, the log carries a `check:` line, and
the bench stops with exit 1 (the same environment would repeat on the next run).
`add-flag` runs the agent's binary under `BENCH_BINARY_TIMEOUT` seconds
(default 10, then KILL after `BENCH_KILL_GRACE`; both must be >= 1): a hang is
`fail` with `check: binary timed out`, and a non-zero exit status where the
task expects success is `fail`. A status of 125/126/127 is ambiguous (`timeout`
or the exec failed, or the agent's binary exited with it itself), so it is
decided by the reference solution: it runs here -> `fail`, it does not ->
`error`. `long-read`'s judgement uses exit status 3 inside its python step, so
a python traceback (status 1) is an `error`.

Every CSV cell is written by one quoting function (RFC 4180: a value with a
comma or a double quote is quoted and its quotes doubled; a CR or LF becomes
a space so a row stays one line), and the table, cost and "earlier run"
readers use a quote-aware reader, so a model name such as `a,"b"` is safe.

The `seconds` column is the wall-clock time of the whole `wakil` process
(bootstrap included), measured by the harness around the process; it has
been so since the harness was written. The `seconds` field of
`--result-json` also includes bootstrap since `44fa662` (before that it
started after bootstrap); the CSV never used it, so CSVs from before
`44fa662` are comparable on time with later ones.

Each run's CSV row carries the *resolved* model from the run's
`--result-json` when the binary reports one, so the CSV names the model
that actually ran, not the one requested. When `--model` is not given
and no run has reported yet, the column is `unknown` (never `config`).
`tokens_cached` is the input-token count the provider served from its
prompt cache (empty/`n/a` when the provider reports none); the table
shows it as a `cached-%` column and prints cost with 4 decimals.
`collapses` counts mid-turn collapses only; `collapses_turn_end` counts
full turn-end collapses. `context_modules` and `modules_loaded` carry the
context-modules switch state and loaded module names from `--result-json`
when the binary reports them (off by default).
`valid` is `true` for a scorable run and `false:<reason>` otherwise:
`false:no_collapse` for a completed `collapse-long` run with 0 collapses
(its `result` is `invalid`, not `pass`/`fail`; exclude `invalid`,
`timeout` and `error` rows from pass-rate denominators),
`false:timeout` for a timeout, `false:error` for an error. `provider`
carries the OpenRouter provider that served the requests when the binary
reports it in `--result-json` (empty otherwise, including non-OpenRouter
backends; `mixed` is reserved for a run served by more than one).
There is no `--provider` pinning flag: the repo documents no OpenRouter
provider-pinning mechanism (the only `provider` object in the wire code
is the ZDR merge in `internal/proxy/policy.go`), so the flag was left
out and the column stays empty until a binary reports it. Old CSVs
(without the appended columns) are still readable by every reader
(`print_table`, `sum_cost`, `find_prev_csv`), which read by header name.
`exit_reason` column carries the reason string from `--result-json`
(`pass`, `declined`, `backend_failure`, `gaps`, `error`) when the
binary reports one, or `n/a` otherwise. Values a binary does not report
are `n/a` (tokens, cost, tool counts). A binary must report the work
folder in `--result-json`: without the `workdir` field the harness cannot
verify where the agent ran, stops with `exit_reason=missing-workdir` and
scores nothing.

## Self-test

```
scripts/bench.sh --self-test
```

Runs every task against stub `wakil` binaries. It needs no model, no
sandbox and no network (only the local Go toolchain, for the Go task
checks). Output (CSV and logs) goes to
`${XDG_CACHE_HOME:-$HOME/.cache}/wakil-bench/selftest/`, never into the
repo, and the run asserts that nothing under `bench/results/` changed.
The self-test sets `WAKIL_CONFIG` to a **fake** user config (fake
endpoints with a fake secret sentinel `AKIA_FAKE_SENTINEL`) before any
run, so it never reads the host's real
`~/.config/wakil/config.json` and passes on a host with no config at
all. It asserts the fake secret never reaches the run log, the CSV or
stdout, that the merged config (built for **every** task) keeps the
fake endpoints, strips every MCP/external-tool key, forces the
enabled-by-default ones off (`oracle_enabled`, `wf_final_review`,
mashura level), and sets exactly the two collapse keys for the collapse
tasks, that the run env has `SEARXNG_URL` unset, that the merged
file/dir are 0600/0700
and are gone after success, failure and timeout, and that a missing
config still runs (merged onto `{}`) while an unparseable one is
`error` / `config-unreadable` / `valid=false:config`. It also covers
the MCP guard: a stub whose run log contains `· caj__run_command …`
lines scores `invalid` / `mcp` / `false:mcp` with the tool names in the
log, while `mashura__` lines and a clean log are not flagged. The v3
cases: grep-only fail, a report with a `notes` field and correct totals
passes, a wrong report fails, the generator self-check rejects a
duplicated-entry fixture, the committed `gen/answer-key.json` equals
the `check.sh` constants, and two generations are byte-identical.
Cases: a stub that changes nothing (every task must report `fail`;
the collapse tasks report `invalid` with `false:no_collapse`); a
stub that copies the task's `solution/` into the work folder (every task
must report `pass`; the collapse tasks need `collapses >= 1` — the solve
stub emits 3 at the chosen ceiling);
wrong solutions (a wrong `report.json`, a wrong per-package constant in
`refactor-multi`, a bloated fix outside the buggy function in
`debug-subtle`) each scored `fail`; an old-format (14-column) CSV read
back by `print_table`/`sum_cost`/`csv_cell`, and the appended
`valid,provider` columns; declined runs; agent-written test code that tries to
fake a pass; no generator, seed or check in the `long-read` folder or its
history, and `leak_check` refusing every leak kind; a planted `solution/`
(`leak-detected`); a model name and version with a comma, quote and newline
(CSV); a held task lock (exit 75, folder untouched); `--runs 2` with the
heartbeat on (two rows, the lock held in both runs, no started program holds
it, a second bench exits 75 in both, nothing left alive, `lock-series`); a
missing tool (`tools-first`, `tar`, `python3`: each case's PATH is a private
directory with exactly the harness's tools minus the one under test) and a
run of every task with only the listed tools (`tools-list-sufficient`); a
stub that sleeps past
the timeout and one that ignores TERM (both `timeout`, no `check.sh`); the
workdir guard for docker, hisn (plain and symlinked), direct and a binary
without `sandbox_backend`; wrong and missing workdir; the two sandbox
approval wordings; folder freshness, `.wakil/` preservation and path
stability.

The Go toolchain cases: every go call of a check and of `setup.sh` carries the
resolved toolchain's `bin/go` and `GOPROXY=off` (`toolchain-pinned`), and a check passes with an empty `HOME` and a failing `go` on `PATH` (`go-empty-home`); a failed case prints the `check:` reason and the log tail (`failure-report`); the status
mapping (`rc-mapping`, `verdict-required`, `preflight-real-go`); a hanging
`add-flag` binary (`binary-hang`, `binary-exit`); and `bench/lib/toolchain_cases.sh`, which
runs a copy of the harness with a stub `go` that prints the Go 1.25.1 error
above: `preflight-stops` (one line, exit 1, no CSV row, no model request),
`preflight-stops-on-check` and `check-error-not-fail` (with the preflight
bypassed by the test-only `--no-preflight` flag (its CSV and logs go to the cache, never to `bench/results`), the result is `error`,
never `fail`). It also takes a path (`sh bench/lib/toolchain_cases.sh <tree>`),
e.g. a `git archive` of an older commit, to show the cases failing there.

CI runs the self-test and `shellcheck -s sh` on `scripts/bench.sh`,
`bench/tasks/*/*.sh` and `bench/lib/*.sh` (job `bench-harness`);
`scripts/host_verify.sh` runs both as `bench-selftest` and
`bench-shellcheck` (a visible `SKIP` line when shellcheck is not installed).

## Known limits

- `long-read` can be solved by writing a script that scans the files
  instead of reading them. The task is not redesigned for that: it measures
  how the agent handles a lot of text, and a script is a legitimate way to
  do it. Its generator and seed stay outside the work folder, so the
  expected answer cannot be derived from anything the agent can read.
- `collapse-long` can likewise be solved by scripting, and an agent that
  externalizes facts to disk as it reads never needs to retain them across
  the collapse. No collapse then means the run is `invalid`
  (`false:no_collapse`), not a pass or fail: report the invalid rate per
  model next to the pass rate. The task measures retention across a forced
  collapse for agents that read in-turn.
- **`collapse-long-v2`** is the additive answer to v1's three real runs
  (2026-11-25, Haiku): all passed the check with **0 collapses** (6–7 tool
  calls, 16–22 KB of conversation growth — one orientation read of
  `note-01.txt`, one grep, then a Python script that parses all 40 files in
  the shell). Each `read_file_full` returned 8.3 KB ≈ the 8,000-char
  `ToolResultCap` (`internal/config/config.go:493`, applied `app.go:2464`),
  so per-call growth is capped at ~8 KB. v2 counters that: 40 files × ~60 KB,
  each with a ~12-fact ledger block past the 8 KB cut at 20–45 KB in
  (whole-file reads truncate before it), dense number-laden filler decoys
  so a candidate-line script outputs > 180K chars, and corrections that
  supersede earlier figures. Must-read text ≈ 800 KB. v2's ceiling is
  **21,000 tokens** (84,000 chars), calibrated from the real 2026-11-25
  Haiku run described below (commit `1404d0d`, plan §11): the same value,
  same `ToolResultCap`, same settings for modules off/on, and — unlike
  the earlier claim in this section — **no `tool_result_cap` or
  `context_mode` pins**: the bench merge sets only the two collapse
  keys (`scripts/bench.sh`, the private merged config), so a user's
  `tool_result_cap` / `context_mode` values still govern the run. The
  calibration assumes the defaults (8,000 / `long-session`); a host run
  whose effective values differ is a different ceiling in practice —
  that assumption is stated, not asserted at run time.
  A ranged-read strategy needs ~100 calls; the recommended timeout is
  `--timeout 1800`. v1 is **unchanged** (its CSV rows stay comparable as
  "0-collapse invalids"); v2 is a new task name. Validity is **≥1 collapse** —
  a completed v2 run with 0 collapses is `invalid`/`false:no_collapse` like
  v1. Rows before commit `1404d0d` used the old 45,000-token ceiling and
  are **not comparable** with rows after it: the single v2 row before
  that commit (`20261125T013001`, 0 collapses, `invalid`) belongs to the
  old ceiling and is the very run the new ceiling is calibrated from.
  The pre-commit gate for the fixture: a best-effort script-only solver
  (dedupe + number-word filter + last-wins) must produce a wrong report
  (verified: it reported 1,472–1,704 per category vs the true
  1,074–1,350 — every category wrong). Determinism: two generations are
  byte-identical (`diff -r` verified). The generator writes its answer key
  beside `gen/` (outside the workdir) and the self-test asserts it equals
  the `check.sh` constants, so a generator bug cannot leave a green
  self-test over an unsolvable task. Known limits (stated, not hidden):
  a per-file process-and-forget loop defeats retention structurally and
  scores `invalid`; an "explore then script" run can pass validity without
  retaining anything — the task measures "crossed the ceiling with a
  correct report", and `check.sh` cannot inspect scratch-file writes.
  A strategy that keeps its in-conversation tail below the ceiling
  without reading the fixture in-conversation (the approach the
  calibration run itself used) scores 0 collapses and is `invalid` —
  that is the intended safety net, not a bug: the ceiling is a question
  about what crosses it in the conversation, and the task measures
  retention across a forced collapse for agents that read in-turn.
- **`collapse-long-v3`** is the successor of v2 (v1/v2 files unchanged;
  rows across task names are never comparable). v2 had three defects
  its run logs exposed (plan §12.0): the task text described years and
  item codes the data does not have; corrections referred to "entry N"
  with no visible numbering (the real rule was an implicit 0-based
  index); and the generator duplicated ledger lines and doubled words
  ("The the", "the the miller") — careful agents refused to answer, and
  two Haiku runs that had computed the exact expected sums wrote no
  report or asked for the rule. v3 fixes all three: **every ledger
  entry line carries a visible per-file 1-based identifier** ("entry 7
  of orchard" — the same number in another file is a different entry),
  the withdrawal line names the identifier it amends, binds within its
  own file, and each entry has at most one withdrawal; the rule ("a
  withdrawal replaces the figure of the entry it names, in the same
  file") and the Ledger-block delimiter are stated in the task text;
  the sentence templates never prefix "the " to an alias. A generator
  self-check fails the generation on a duplicated ledger line, a
  doubled word, two withdrawals naming one entry, or an identifier
  that does not resolve within its file. The answer key is written
  only to `gen/answer-key.json` in the task dir (committed,
  deterministic; v2's generator had written it into the work folder —
  two v2 run logs show agents noticed `answer-key.json` in the repo
  root and declined to read it; the harness now also removes/flags it
  as a leak) and the self-test asserts it equals the `check.sh`
  constants. The task text tells the agent to always write
  `report.json` with its best answer and to put doubts in a `notes`
  field — `check.sh` ignores `notes` (any JSON type) and reads only
  the six category totals, so a correct answer with doubts still
  passes. Same fixture shape as v2 (40 files × ~44 KB, ledger past the
  8 KB cut, number-laden decoys, must-read ≈ 800 KB ≈ 4× the ceiling).
  Ceiling **21,000 tokens** (84,000 chars), same as v2: verified by
  the plan §12.3 replay (recorded-strategy model, v3 task-text
  length) — **1 mid-turn collapse in both modes under all three
  argument distributions**; same defaults assumption and strategy-model
  caveat as the v2 calibration (the replay models the recorded Haiku
  strategy, not a v3-specific run; a quieter strategy still scores
  `invalid`, the safety net). The pre-commit gate: a careful one-shot
  script-only solver (all ledger figures, no withdrawal replacement)
  produces wrong sums (779–875 per category vs the true 853–1345).
  Determinism: two generations byte-identical. Validity is ≥1 collapse
  like v1/v2; recommended timeout `--timeout 1800`.
- **`debug-subtle` minimality rule** (rows before the commit that
  introduced this rule are **not comparable** with rows after it — the new
  rule is not compatible with the old "all changed lines ≤ 6"): changed
  lines (gofmt-normalized, hidden tests removed first) are counted **only
  outside the `windowTake` window** (function + its contiguous comment
  block above); inside the window any size is allowed. Outside: at most
  **2 changed lines** (covers one one-line edit, e.g. a `Take` doc fix;
  an import addition alone is 1 line). In total: at most **30 changed
  lines** (stops a whole-file rewrite). Removed lines classify by
  old-file position, added lines by new-file position; a hunk spanning
  the boundary is split per-line. Agent-written `*_test.go` edits are
  excluded from the diff entirely. The 6 host runs that all failed the
  old rule with "8 changed lines" (reference fix is 5) now pass: the
  extra 3 lines were inside the function (loop rewrite plus a `k<0`
  clamp), 0 outside. Self-test cases: 5-line reference passes, 11-line
  rewritten loop passes, correct fix + unrelated helper outside fails,
  whole-file rewrite fails, existing bloated-helper case still fails.
- **Ceiling calibration (off vs on).** `collapse-long` keeps 45,000 tokens
  (180,000 chars), identical for both modes, chosen from a calibration
  replay of that task through a fake backend that drove one long-session
  turn with 40 `read_file` calls in fixture order and the real preamble
  each mode sends. Measured mid-turn collapses at that ceiling: 3 in
  modules-off and 3 in modules-on (the onset of doubt: 40,000 gives off 3,
  on 4). The measured prefix is ~53,084 bytes off vs ~53,500 bytes on —
  the module index is appended inside the counted preamble, so modules-on
  collapses marginally earlier and the off/on counts are not exactly
  equal. That asymmetry is accepted, not compensated (no per-mode
  values): on this task, off-vs-on partly measures prefix size, stated
  plainly so the comparison stays honest. `context_collapse_min_bytes`
  is set only to neutralise a user value: `wakil run` is one user turn,
  so the turn-end counter does not fire. The ceiling counts
  `sentSize(contextProjection())` = content + tool-call arguments in
  Conv (system prompt and task message included, tool schemas excluded)
  with a 4-chars-per-token estimate (`internal/agent/collapse.go`,
  `internal/agent/styleguides.go`, `internal/core/format/format.go`).
  The 1–3-collapses claim is a property of the replay (fixed order, the
  `read_file` path, defaults — not the user's `tool_result_cap` /
  `context_mode`, and without mid-run `load_module` traffic in on mode);
  a real model's read order and strategy will differ, which is why the
  `invalid` rate is a first-class metric.
- **collapse-long-v2 ceiling calibration (real run, 2026-11-25).** The
  v2 ceiling is **21,000 tokens** (84,000 chars), identical for both
  modes, calibrated from the recorded real run
  (`scripts/bench.sh --task collapse-long-v2 --runs 1 --model
  anthropic/claude-haiku-5.5 --context-modules off --timeout 1800
  --mashura off`: 99 s, 1,177,802 in / 18,679 out, 89% cached, $0.0356,
  18 tool calls, 0 collapses, check passed → `invalid`). The model
  solved the task by moving the work into the sandbox (heredocs,
  `python3`, `wc`, `sed`/`grep` dumps), so the conversation never
  reached the old 45,000-token ceiling; the turn-end collapse note
  (`collapses_turn_end=1`) records the working set exactly (12,108
  tokens = 48,432 chars, description `msgs 2–37, run_shell ×14,
  git_status ×1, list_dir ×1, read_file ×1, …`). The conversation
  sequence is 1× `list_dir` (1,126 B), 1× `search_files` (247 B),
  1× `read_file` (8,499 B = 8,000-char cap + spill suffix), then 14×
  `run_shell` with in-conversation results 150–6,042 B (the two 8,499-B
  `run_shell` results are the exfiltration dumps), and 1× `git_status`
  (75 B); every call carries 85–367 B of assistant text and 42–50 B of
  arguments except the 11,784-B heredoc argument that wrote the
  parser. Measurement method: the run log carries byte sizes, not full
  request bodies, so the calibration replayed the recorded sequence
  (sizes, arguments, and assistant texts reconstructed from the
  transcript and the exact `W` anchor) through the real `streamTurn` +
  `collapseMidTurnPrefix` against a fake backend and a fake executor,
  with each mode's real `buildPreambleForNetwork` output (53,083 B off,
  53,499 B on) and the real v2 task text (751 B); `ToolResultCap` was
  ≥ 8,499 so the recorded post-cap bodies are the in-conversation
  ones, and the verifier is `CollapseCounts` (mid-turn = `Partial`
  records). Counts (mid-turn collapses, `CollapseCounts`):

  | ceiling (tokens) | off/A | off/B | off/C | on/A | on/B | on/C |
  |---|---|---|---|---|---|---|
  | 20,000 | 3 | 2 | 2 | 3 | 3 | 2 |
  | **21,000** | **2** | **1** | **2** | **2** | **1** | **2** |
  | 22,000 | 1 | 1 | 1 | 1 | 1 | 1 |
  | 23,000 | 1 | 0 | 1 | 1 | 1 | 1 |
  | 24,000+ | 0 | 0 | 0 | 0 | 0 | 0 |

  A/B/C are three argument distributions of the same 12,588-B total
  (A = heredoc-concentrated as recorded, B = even spread, C =
  late-heavy). 21,000 is the only candidate with 1–3 in both modes
  under all three distributions with no count boundary within ~1.5 KB
  (20,000 thrashes on restored context; 22,000 is within 1.5 KB of
  the 0 boundary; 23,000 fails under B). 21,000 and 22,000 happen to
  produce identical counts in this replay; 21,000 is chosen as the
  more robust of the two (its post-collapse tails are ~800 B away
  from thrashing at every collapse, 22,000's are ~700 B). `hardMax` in
  the replay is 3,015,095 chars at every candidate, so the ceiling —
  not the guard — is the trigger. The "both modes" claim is scoped to
  this recorded strategy: a host on-mode run was not made (no real
  model requests except Mashūra), so behaviour under mid-run
  `load_module` schema pinning is unmeasured. The calibration assumes
  the defaults for `tool_result_cap` (8,000) and `context_mode`
  (`long-session`), supported by counts and by the run log's 8,499-B
  results and 52,879-byte prompt header; it is not asserted at run
  time (see the correction in the `collapse-long-v2` bullet above).
- `check.sh` runs on the host, unsandboxed, and executes the agent's
  non-test code when the hidden test imports the package. The static checks
  above stop the known ways to fake a pass; code that reads the hidden test
  source at runtime is not stopped.
- With the `direct` backend the agent can read `bench/tasks/*/check.sh` by
  absolute path. With hisn or docker the repo is not mounted.
- `.wakil/` in a work folder is preserved between runs (the approval lives
  there) and the agent can write to it. The harness never writes into it; it
  prints a warning when a run changed a file under it.
- A check cannot tell an environment failure that exists only while the
  agent's tree is built (a cache that fills up during the run) from the
  agent's fault; one that also breaks the reference solution is caught.
  A `chmod 000` file in the work folder makes the copy step fail: `error`.
- The preflight proves the Go toolchain and the checks on the reference
  solution, not that the agent's binary can run on every filesystem beyond
  the one the preflight used (`add-flag` reports a binary that cannot be
  started as `error`).
- Agent processes that survive a KILL are not hunted down.
- Docker cannot be benched (`workdir-not-verifiable`, see above).

## Adding a task

Create `bench/tasks/<name>/` with:

- `repo/` — the project the agent works in (small Go project; no network
  needed).
- `task.txt` — the prompt, verbatim.
- `check.sh` — exit 0 = pass; runs in the work folder after the run.
- `hidden/` (optional) — files `check.sh` copies in at check time (the
  agent never sees them).
- `solution/` — the reference result, used only by the self-test.
- `setup.sh` (optional) — runs in the work copy before the agent.

Each task must satisfy the untouched `repo` fails `check.sh`, and the
reference `solution` passes it.