# Performance of start, /new, /resume and exit — 0.6.0

Status: **sandbox numbers, not rubin.** The "Baseline" section is B1 (measured
before any optimisation, for reference); the "After B2" section is the same
harness on the same host after B2. The rubin re-run is still owed.

Rubin timeline deltas (`rubin-khatam.json` = B1, `rubin-khatam-b2.json` = B2):

| transition | rubin B1 | rubin B2 | note |
|---|---:|---:|---|
| start to prompt | 2990 ms | **149 ms** | probes, hisn reconcile and sandbox start deferred past the prompt |
| exit | 2780 ms | 5132 ms | B2 traced the ilm `close` at **5002 ms** |

The B1 exit did not instrument the ilm emitter close (that span was added in B1's
own instrumentation), so the split between the `Release` poll and the ilm flush
in B1 is not observable from the B1 file. In B2 the ilm `close` step is
**5002 ms** of the 5132 ms exit — a flush to an unreachable ilm endpoint hitting
the sender's 5 s stop cap. Removing the emitter (ADR 0013) drops that step
entirely, leaving the terminal restore plus `hisn stop`; that is measured in the
sandbox below (116.7 ms). Rubin's post-removal exit is owed.

| transition | target | where it is measured |
|---|---:|---|
| warm start to prompt | < 200 ms | `start`: offset of `prompt ready` |
| `/new` | < 150 ms | `/new`: offset of `rotation applied in tui` |
| `/resume` with history | < 300 ms | `/resume`: same |
| exit | < 300 ms | `exit`: total, from the quit key to the process ending |

## How the numbers are produced

```
WAKIL_TRACE=1 wakil            # or: wakil --trace
```

writes `timeline-<UTC stamp>-<pid>.json` (schema `wakil-timeline/v1`) into the
trace directory (`--trace-dir`, `WAKIL_TRACE_DIR`, default
`~/.local/share/wakil/traces`) and prints its path after the terminal is
restored. It records, per transition (`start`, `/new`, `/resume`, `/handoff`,
`exit`), every phase with its offset and duration in milliseconds. It also
records the time between the kernel creating the process and `main` (package
initialisation). `scripts/perf/run.sh` runs a scripted session and summarises
the files:

```
scripts/perf/run.sh [-n RUNS] [-b WAKIL_BINARY] REPO_DIR...
```

Each run: start, send one chat message (so a transcript exists), `/new`,
`/resume` of that transcript, `/new`, quit. Run 1 of a repo is reported as cold
(new stores, new hisn home), runs 2..N as warm (median, min, max). The "ready"
time of a transition is when the user can type again: the offset of the
`prompt ready` instant for `start`, of `rotation applied in tui` for `/new` and
`/resume`, the transition's end for `exit`. The *unattributed* column is the
part of that time that no recorded phase covers.

### What the harness fixes, and what it hides

- A **local stub** answers the model endpoint's metadata routes and one chat
  turn instantly, so provider latency is not in the numbers. On a real endpoint
  the three provider probes at start run in parallel with a 6 s ceiling and
  the `/resume` context-limit probe is a real network call: add the endpoint's
  round trip to both.
- Everything lives under `$PERF_DIR` (private XDG tree and `HOME`): the real
  sessions, memory and config are never read.
- The pty driver **answers the terminal's colour and cursor queries**, as a
  real terminal does within milliseconds. See "A terminal that does not answer"
  below for what happens without that.

## Baseline — build sandbox

Host: x86_64 container, kernel 7.2, hisn 0.4.0 (release binary, sha256
`b2fe06ce…`), host toolchain (no rootfs). wakil 0.5.3 + this branch, 7 runs per
repo.

| repo | start (cold) | start (warm) | /new | /resume | exit |
|---|---:|---:|---:|---:|---:|
| fresh test repo (2 files) | 692 ms | **684 ms** | 109 ms | 88 ms | 12 ms |
| wakil (777 tracked files) | 685 ms | **683 ms** | 110 ms | 115 ms | 13 ms |

Against the targets: **start misses by 3.4x; `/new`, `/resume` and exit meet
them** in this environment.

## After B2 — build sandbox

B2 moves the startup work that does not need to block the first prompt off the
start path. `prompt ready` now arrives before the provider probes, the hisn
`ls --prune` reconcile, and the lazy sandbox session start, all of which run in
the background once the prompt is up. Same host and harness as the baseline.

| transition | before (warm median) | after (warm median) | target |
|---|---:|---:|---:|
| start | 669 ms | **51–58 ms** | < 200 ms |
| /new | 109 ms | 107 ms | < 150 ms |
| /resume | 87 ms | 48–63 ms | < 300 ms |
| exit | 12 ms | 12 ms (no live session) / 117 ms (live session) | < 300 ms |

**start now meets its target** (about 12x faster). The rubin baseline put
2990 ms on start (923 ms of it the MCP + probe + reconcile block); the same
deferrals apply there. On rubin the largest start cost was the MCP connect
block (1226 ms) and the provider probes (735 ms); both are now off the prompt
path.

What B2 changed, and why the *timeline still shows* the deferred phases:

- **Provider probes** (model list, backend list, context limit) run after the
  prompt; the status line shows `…` for the model until the probe resolves it.
- **hisn reconcile** (`ls --prune`) runs after the prompt. It is still traced
  under the `start` transition window (the transition stays open until the
  timeline is flushed at exit), so it still appears in the top-five list — but
  `prompt ready` no longer waits for it. `hisn ls --prune` in 0.4.0 has no
  name/prefix filter, so the sweep cannot yet be scoped to `wakil-<projectkey>-`
  directories; that needs a hisn-side change and is recorded as an open item.
- **MCP servers** connect concurrently in the background; their tools appear as
  each connects, and a tool call to a server still connecting waits only on that
  server.
- **The lazy sandbox session start** and the startup-note scans moved into the
  same deferred phase.
- **The Telegram approval bridge** was removed (it was the only consumer of the
  MCP long-poll teardown path the old `Close` comment guarded).

`exit` gained two fixes from the rubin timeline:
1. The 50 ms x N `Release` retry loop inside `close old facade` (about 2.6 s on
   rubin when a turn was still winding down) is gone. Exit now makes one release
   attempt and, if a turn is still cancelling, releases in the background rather
   than blocking the terminal.
2. The ilm emitter is **removed entirely** (ADR 0013). Its `close ilm emitter`
   step was **5002 ms of the 5132 ms rubin exit** — a flush to an unreachable
   ilm endpoint hitting the sender's 5 s stop cap. With the emitter gone the
   step no longer exists; `close old facade` is 0.04 ms in the sandbox.

Sandbox exit after the removal, measured with a live hisn session and no
preceding `/new` (so the `hisn stop` is included): **116.7 ms** — 12 ms to the
terminal restore, 104.6 ms `hisn stop` (the client round trip), 0.05 ms facade
cleanup. The standard harness reports 12 ms because it stops the session with
the preceding `/new`, leaving nothing to stop at exit.

### Top five costs after B2 (warm median)

`start` — all but `process start to main` now run AFTER `prompt ready`:

- 302 ms  hisn reconcile (`ls --prune`) — deferred, off the prompt path
- 117 ms  hisn session ready (deferred with the startup notes)
- 35 ms   process start to main (runtime + package init) — still before the prompt
- 25 ms   tui view (render a frame)
- 3 ms    signing resolution

`/new` 103 ms (`hisn stop`), `/resume` 60 ms (reflow + the deferred probes now
land here when the first prompt's deferrals overlap), `exit` 12 ms. All three
meet their targets.


### Top five costs per transition (warm median)

**start** (683 ms). Both repos agree to within 1 ms per phase.

1. **302 ms — `hisn reconcile` (`hisn ls --prune`).** Archives the `stopped`
   directory the previous run left. `hisn ls --prune` measured on its own: 2 ms
   with nothing to prune, 313 ms with one stopped directory, 613 ms with one
   crashed directory. Present at every start after a normal exit.
2. **117 ms — `hisn session ready`.** Supervisor start until the readiness
   line, plus the two confirmation probes 100 ms apart (`readyPoll`).
3. **35–40 ms — process start to `main`.** Runtime and package
   initialisation.
4. **26 ms — rendering frames** (14 calls, summed) while the first screen is
   built.
5. **3 ms — signing resolution** (git config, ssh key, agent).

The 117 ms session start is lazy: it is paid by whatever first reaches the
sandbox, which at start is the staging scan in `BootstrapTUI`. It is therefore
inside the time to the first prompt, not deferred to the first tool call.

**/new** (110 ms).

1. **103 ms — `hisn stop`** of the old session (client round trip; the
   supervisor is gone within 1 ms after it).
2. 2.6 ms — handling the Enter key.
3. 1.1 ms — provider probe: model list.
4. 1.0 ms — finalize the old conversation (history).
5. 0.9 ms — provider probe: context limit.

The `hisn stop` is in the critical path because `/new` stops the old sandbox
session before the next one can start (`BindChat` → `Backend.Stop`).

**/resume** (88 ms fresh, 115 ms wakil; the transcript is two messages).

1. **51–70 ms — TUI reflow** (re-wrap and viewport refresh, two calls).
2. **43–69 ms — handling the rotation message** in the TUI, which contains the
   reflows.
3. **41 ms — context-limit probe for the saved model.** A network call to the
   endpoint; against the stub it still costs 41 ms because of a TCP connect and
   the HTTP exchange. On a real endpoint this is the endpoint's round trip.
4. 22–25 ms — handling the Enter key (includes dispatching `/resume`).
5. 9–20 ms — rebuilding the transcript view items / frames.

**exit** (12 ms).

1. 2 ms — final frame render.
2. 0.2 ms — handling the quit key.
3. — everything else rounds to 0.0 ms.

The sandbox session had already been stopped by the preceding `/new`, so `exit`
here does not include a `hisn stop`. On a session that is still running, exit
adds that ~103 ms round trip, which is within the 300 ms target; the next
measurement on rubin should include a run that quits with a live session.

## A terminal that does not answer

Without answers to its colour query, `termenv` (used by the TUI library) waits
its full `OSCTimeout` of **5 s** before the program starts drawing. In the
sandbox this appears as `process start to main` growing from 35 ms to 5.8 s:
the time lands before `main`, in package initialisation. The pty driver of the
first measurements did not answer; the numbers above do. A real terminal
answers in milliseconds, but a terminal multiplexer, a serial console or an
unusual emulator may not. Before B2 it is worth knowing whether rubin's
terminal answers (the `process start to main` line shows it directly).

## What the timeline cannot see

- Time spent in the Go runtime before the first instruction of Wakil's own
  code is included in `process start to main` (kernel process start time, 10 ms
  resolution); anything before the kernel forks the process (shell, `wakil`
  launcher) is not.
- The first frame the terminal paints after the TUI draws is not observable;
  `prompt ready` is when Wakil handled the first window size message.
- Concurrent phases (the three provider probes) are marked and do not add to
  the transition's total; accumulated phases (frame renders, message handlers)
  are sums of many short calls and are labelled with their call count.

## Reproducing on rubin

See the commands at the end of the Phase B1 report; they run
`scripts/perf/run.sh` against three repositories and print the same tables.
