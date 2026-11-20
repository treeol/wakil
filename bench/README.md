# bench

A small benchmark harness for Wakil. It runs a set of reference tasks
through `wakil run` and records one CSV row per run in `bench/results/`
(gitignored), so a model can be compared across releases without hand-
staging experiments.

## Run

```
scripts/bench.sh [--wakil PATH] [--model NAME] [--task NAME] [--runs N]
                 [--timeout SECONDS] [--mashura LEVEL] [--self-test]
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
`wakil sandbox trust <path>` command the user must run once. The
harness itself never runs that command and never reads from the
terminal. The acknowledgement is keyed on:

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
directory itself is recreated by the hisn backend on each run with the
same risk label (same config, same hisn version), so the hash matches.
If the risk label changes (different hisn version, different config),
the content changes, the hash changes, and the approval is invalid —
the user must re-acknowledge with `wakil sandbox trust <path>`.

## How a run works

For each task and run number:

1. `bench/tasks/<name>/repo/` is copied to a **fixed** folder
   (`${XDG_CACHE_HOME:-$HOME/.cache}/wakil-bench/work/<task>`) and given
   `git init` + one commit with a repo-local identity
   (`user.name "wakil-bench"`, `user.email "bench@localhost"`), so every
   run starts from the same state and the stable path means the user's
   one-time sandbox acknowledgement holds. The bench runs with the
   user's normal profile — no `GIT_CONFIG_GLOBAL` override, no
   `GIT_CONFIG_*` unsetting, no `commit.gpgsign` override.
2. If the task ships a `setup.sh`, it runs in the work copy before the
   agent.
3. `wakil run --auto --no-memory [--auto-counsel] --result-json <file>
   --transcript <log>` runs with the task text as the final argument,
   under a `timeout` guard. `--no-memory` skips per-project memory so
   there is no carry-over between runs (no memory tools, no `plan/current`
   read or write, no memory digest in the prompt). `WAKIL_HOST_WORKDIR`
   is set to the fixed work folder so the sandbox mounts it (not the
   configured `work_dir`).
4. `check.sh` from the task folder runs in the work folder; exit 0 is a
   pass.
5. **Workdir guard.** After each run, the harness reads the `workdir`
   field from the result JSON and compares it to the fixed folder. On
   mismatch the run is marked `error` with `exit_reason=wrong-workdir`
   and the whole bench stops at once.
6. **Sandbox acknowledgement check.** Before the first task, the
   harness checks each selected folder non-interactively (stdin from
   `/dev/null`). When a run ends with the "not acknowledged" error, the
   exit reason is `sandbox-not-acknowledged` and the bench stops with
   the label, the folder, and the one `wakil sandbox trust` command to
   run.

The task's `check.sh`, `hidden/` files and `solution/` reference result
are **never** copied into the work folder: the agent cannot pass by
reading or editing the check. After each run the harness also verifies
the work folder does not contain any of them and reports a leak if it
does.

Model selection follows what `wakil run` offers (see Part 1 of the task
spec): there is no per-run `--model` flag, so `--model` sets `WAKIL_MODEL`
in the subprocess environment only — `WAKIL_MODEL` overrides the config
file's model (`internal/config/endpoint_test.go: TestWakilModelEnvOverridesEndpointModel`).

Mashura level: `off` passes no counsel flag; `low`, `medium` and `high`
pass `--auto-counsel` (the only per-run counsel flag `wakil run` has).
`--no-oracle` is plan-mode-only in the current driver, so it is not
used here.

Each run's CSV row carries the *resolved* model from the run's
`--result-json` when the binary reports one, so the CSV names the model
that actually ran, not the one requested. When `--model` is not given
and no run has reported yet, the column is `unknown` (never `config`).
`exit_reason` column carries the reason string from `--result-json`
(`pass`, `declined`, `backend_failure`, `gaps`, `error`) when the
binary reports one, or `n/a` otherwise. Values a binary does not report
are `n/a`: the harness also works with a binary that lacks
`--result-json` (pass/fail and seconds only).

## Self-test

```
scripts/bench.sh --self-test
```

Runs every task against stub `wakil` binaries (no model involved):
a stub that changes nothing (every task must report `fail`), a stub that
copies the task's `solution/` into the work folder (every task must
report `pass`), a stub that sleeps past the timeout (reported as
`timeout`), a stub that reports a wrong workdir (must stop the bench
with `wrong-workdir`), a freshness check (a marker file left by run 1
is gone after the folder is emptied and refilled), and a path-stability
check (the work folder path is identical across runs). The same run
also asserts that `check.sh`, `hidden/` and `solution` never exist in
the work folder during the run.

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