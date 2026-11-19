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

## How a run works

For each task and run number:

1. `bench/tasks/<name>/repo/` is copied to a fresh temporary folder and
   given `git init` + one commit, so every run starts from the same state
   and nothing is shared between runs. The temp repo does NOT inherit the
   host's global git config: `GIT_CONFIG_GLOBAL=/dev/null` is set for
   `git init`, `git commit`, the pre-run `wakil sandbox trust` probe, and
   `wakil run`. This prevents a host with global `commit.gpgsign=true`
   from making the generated hisn profile carry a "signing" risk label
   (git 2.39.5's `--type=bool` maps any non-empty string to `true`, so a
   local `commit.gpgsign=false` cannot override the global value — only
   `GIT_CONFIG_GLOBAL=/dev/null` reliably blocks it). Bench tasks never
   sign.
2. If the task ships a `setup.sh`, it runs in the work copy before the
   agent.
3. **Pre-run sandbox risk probe.** Before the model runs, the harness
   calls `wakil sandbox trust <workdir>` (best effort, no model call) and
   extracts the risk label. If the label is not `none`, the run is marked
   `error` with `exit_reason=sandbox-not-acknowledged` and the bench stops
   with the label named, so the user can `wakil sandbox trust` it. This
   catches a signing (or other) risk that survived step 1.
4. `wakil run --auto [--auto-counsel] --result-json <file> --transcript
   <log>` runs with the task text as the final argument, under a
   `timeout` guard. The transcript is the full output log.
   `WAKIL_HOST_WORKDIR` is set to the temp work copy so the sandbox
   mounts it (not the configured `work_dir`); the agent runs in the
   task's repository, not whatever the user's config file points at.
5. `check.sh` from the task folder runs in the work folder; exit 0 is a
   pass.
6. **Workdir guard.** After each run, the harness reads the `workdir`
   field from the result JSON and compares it to the temp folder it set up.
   On mismatch the run is marked `error` with `exit_reason=wrong-workdir`
   and the whole bench stops at once. A binary that does not report
   a `workdir` field is skipped (only a mismatch is an error, not a
   missing field).
7. **Post-run "not acknowledged" detection.** When a run fails and the
   log mentions "sandbox risk is not acknowledged", the exit reason is
   set to `sandbox-not-acknowledged` and the bench stops with the label
   named.

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

Runs every task twice against stub `wakil` binaries (no model involved):
once with a stub that changes nothing (every task must report `fail`)
and once with a stub that copies the task's `solution/` into the work
folder (every task must report `pass`), plus a stub that sleeps past
the timeout (reported as `timeout`). The same run also asserts that
`check.sh`, `hidden/` and `solution/` never exist in the work folder
during the run.

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

Each task must satisfy: the untouched `repo/` fails `check.sh`, and the
reference `solution/` passes it.
