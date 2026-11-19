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

## How a run works

For each task and run number:

1. `bench/tasks/<name>/repo/` is copied to a fresh temporary folder and
   given `git init` + one commit, so every run starts from the same state
   and nothing is shared between runs.
2. If the task ships a `setup.sh`, it runs in the work copy before the
   agent.
3. `wakil run --auto [--auto-counsel] --result-json <file> --transcript
   <log>` runs with the task text as the final argument, under a
   `timeout` guard. The transcript is the full output log.
4. `check.sh` from the task folder runs in the work folder; exit 0 is a
   pass.

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
that actually ran, not the one requested. Values a binary does not
report are `n/a`: the harness also works with a binary that lacks
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
