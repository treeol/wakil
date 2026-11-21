# bench

A small benchmark harness for Wakil. It runs a set of reference tasks
through `wakil run` and records one CSV row per run in `bench/results/`
(gitignored), so a model can be compared across releases without hand-
staging experiments.

## Run

```
scripts/bench.sh [--wakil PATH] [--model NAME] [--task NAME] [--runs N]
                 [--timeout SECONDS] [--mashura LEVEL] [--no-preflight]
                 [--self-test]
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
the result (`pass` / `fail` / `timeout` / `error`), elapsed seconds,
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

The Go tasks (`add-flag`, `fix-test`, `rename`, and `long-read`'s setup) need the
toolchain their `go.mod` names (the same as the repo's `go.mod`) in the
**local cache**. `scripts/bench.sh` resolves it once at start, in the repo
root with your environment (`GOPROXY=off go env GOVERSION`; the host's own Go
may be older, it then uses the toolchain it already downloaded), and every
check and `setup.sh` runs with `GOTOOLCHAIN` set to that exact version and
`GOPROXY=off`, with the Go cache locations resolved the same way. **A check
never downloads a toolchain or a module.** Run `go version` once inside the
repo while online to fetch the toolchain; a host with `GOSUMDB=off` cannot
switch toolchains (Go verifies the cached one against the checksum database).

Before the first run, and so before any model request, the harness runs
`setup.sh` and every Go check once on each task's reference solution. If that
cannot work it prints one line and exits 1 without writing a CSV row:

```
bench.sh: the Go checks cannot run: the tasks need go >= 1.26, found go1.25.1 — go: go.mod requires go >= 1.26 (running go 1.25.1; GOTOOLCHAIN=local)
```

`--task steps` needs no Go at all (no Go is resolved and the preflight is
skipped); `long-read` needs it for its `setup.sh`. `python3` is checked before the first run when a selected task's `check.sh` or `setup.sh` uses it. GNU userland is assumed (`tar`, `timeout -k`, `flock`, `sha256sum`, `find`).

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
   `hidden`, `solution`, `gen`, `setup.sh`, `cmd/gen` or seeded generator
   is in the folder or in its history; a hit aborts the bench.
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
   `work_dir`).
4. `check.sh` from the task folder runs in the work folder after every
   run that ended (`pass`, `declined`, or `error`); `result` is `pass`
   or `fail` from `check.sh`, and `exit_reason` keeps what the run
   reported. `timeout`, `wrong-workdir` / `missing-workdir` /
   `workdir-not-verifiable`, `leak-detected` and
   `sandbox-not-acknowledged` skip `check.sh`.
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
anything else runs; the list is `harness_tools` at the top of `scripts/bench.sh`.

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
`GOWORK=off`, `GOENV=off`, `GOTOOLCHAIN=<the resolved version>`,
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
full turn-end collapses.
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
Cases: a stub that changes nothing (every task must report `fail`); a
stub that copies the task's `solution/` into the work folder (every task
must report `pass`); declined runs; agent-written test code that tries to
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
resolved `GOTOOLCHAIN` and `GOPROXY=off` (`toolchain-pinned`); the status
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