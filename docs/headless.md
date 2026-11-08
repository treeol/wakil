# Running Wakil headless: `wakil run`

`wakil run` runs one task without a terminal UI and exits with a code. It reads
the same config file as the interactive command, resolves the sandbox backend
the same way (the startup banner goes to stderr), and writes a JSON-lines event
stream to stdout, or to a file with `--transcript`.

```sh
wakil run [flags] "<task>"
```

The task is a single quoted argument. Flags go before it; an unknown flag or a
second positional argument is an error (exit 3). Source: `cmd/wakil/run.go`
(argument parsing) and `internal/wiring/headless.go` (behaviour).

## Approval

A headless run cannot prompt. Every tool call that would ask for confirmation
in the TUI is decided by flags:

- Without `--auto`, a call that needs confirmation is declined
  (`confirmation required (rerun with --auto)`) and the run exits 1.
- With `--auto`, calls are approved, except destructive shell commands
  (declined unless `--allow-destructive`) and calls to an external backend
  (declined unless `--allow-external`).
- A `--policy` file or `--profile` is evaluated first. A `deny` rule always
  declines; an `ask` rule declines because nothing can answer it.

## Flags

| Flag | Meaning |
|---|---|
| `--plan` | run the task as a `/plan` workflow (gather, plan, review, implement) instead of a single turn; see [the `/plan` workflow](tui.md#plan-workflow) |
| `--auto` | approve non-destructive tool calls automatically |
| `--allow-destructive` | also approve destructive shell commands (`rm`, `mv`, `git reset`, and similar) |
| `--allow-external` | pre-authorise external backends for the egress consent gate; without it a run that would route to an external backend is declined instead of sending session context to a provider |
| `--auto-counsel` | fire `mashura__debug` automatically when the struggle detector triggers; needs `--auto`, or the call is declined and still uses a slot |
| `--max-counsel N` | cap on automatic counsel calls per session; defaults to 3 when `--auto-counsel` is set |
| `--no-oracle` | skip Mashūra review entirely |
| `--transcript <file>` | write the JSON-lines events to this file instead of stdout |
| `--attach-image <path>` | attach an image file to the first message (png, jpeg, gif, webp; comma-separate several) |
| `--policy <path>` | load a policy JSON file (`default` plus `rules`) and install it for this run |
| `--profile <name>` | use a built-in policy profile: `read-only`, `auto-safe`, `auto-destructive` or `ci`; `--policy` wins if both are given |
| `--verify` | run the configured or auto-detected verification commands at the end of a `--plan` workflow |
| `--budget $N` | per-session spending ceiling in dollars (must be > 0); a soft cutoff checked after each inference call, so a turn can overshoot |
| `--continue` | experimental bounded continuous mode; requires `--max-turns` and `--max-time` |
| `--max-turns N` | with `--continue`: maximum number of agent turns (positive integer) |
| `--max-time D` | with `--continue`: time bound for the whole run, as a Go duration such as `30m` |
| `--verify-cmd CMD` | with `--continue`: an explicit acceptance command; repeatable. Without one a run can only end `complete_unverified` |

`--max-turns`, `--max-time` and `--verify-cmd` are rejected without
`--continue`. `--continue` cannot be combined with `--plan` or with `--verify`
(use `--verify-cmd` so the acceptance contract is explicit).

`--continue` is experimental: it does not make unattended destructive work safe,
and it has no replay protection for external side effects.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | task completed |
| 1 | a tool call was declined |
| 2 | `--plan` only: the final review flagged unresolved gaps, or verification failed |
| 3 | runtime error, fatal request error, or bad arguments |
| 4 | backend error that exhausted its retries; the session is saved and resumable |
| 5 | `--continue` only: the bounded run stopped without a verified goal |

## Output

One JSON object per line. The event types in `internal/wiring/headless.go` are
`output` (a line of agent output), `warning`, `error`, `tokens` (input and
output token counts, plus `cost_usd` and budget fields when pricing or `--budget` is set; not emitted by `--plan`), and a final
`done` record whose `outcome` is `pass`, `declined`, `backend_failure` (with a
`resume_id`) or, for `--plan`, `gaps` or `verify_failed`. In `--continue` mode
the `done` record carries `schema_version`, `mode`, `outcome`, `reason`,
`turns`, `model_calls`, `elapsed_ms`, `resumable` (always false), `exit_code`
and `verification`; `turn` and `verify` events appear along the way.
