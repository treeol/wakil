# 0014 — hisn minimum raised to 0.4.1

- **Status:** Accepted
- **Date:** 2026-11-08
- **Origin:** CHANGELOG 0.6.0 ("hisn 0.4.1 compatibility"); `internal/sandbox/hisn/backend.go` (`MinVersion`), `internal/config/config.go` (`MinHisnVersion`).

Supersedes the minimum-version line of [0012](0012-hisn-0.4.0-session-model.md);
the rest of 0012 stands.

## Context

[0012](0012-hisn-0.4.0-session-model.md) set the minimum at hisn 0.4.0. hisn
0.4.1 changed the contract Wakil depends on for starting and stopping a session:

- `hisn run --detach` **returns once the session is ready**. In 0.4.0 the `run`
  process was the supervisor and never returned; in 0.4.1 it writes the ready
  line, exits 0, and the supervisor lives on as a separate process.
- The supervisor's pid is written to `<session-dir>/supervisor.pid` as
  `"<pid> <starttime>"` (the `/proc/<pid>/stat` start time), so a pid can be
  checked against reuse.
- A `stopped` session keeps its name reserved: `hisn run --name` exits 125
  (`name ... is already in use`) until `hisn ls --prune` removes it.
- `hisn ls --prune` accepts `--name-prefix`.

The two shapes need different code. Parsing `supervisor.pid` as a bare pid
failed every 0.4.1 start after the session was already up, and the failed-start
path then reaped only the already-exited run process, which left the session
running (CHANGELOG 0.6.0).

## Decision

- The minimum hisn version is **0.4.1**, for the explicit backend (preflight) and
  the default-backend gate alike. 0.4.0 and older are refused at start with a
  message naming 0.4.1. There is no compatibility path.
- Wakil tracks the supervisor by `supervisor.pid` (pid and start time) and ends
  a session with `hisn stop --session <id>` followed by a wait on that pid,
  after proving the session is its own. The contract is described once, in
  [docs/sandbox-backends.md](../sandbox-backends.md#supervisor-and-the-detach-contract).
- `Start` prunes a `stopped` leftover that has the chat's id or holds its name
  before spawning.

## Consequences

- A host with hisn 0.4.0 or older gets the Docker backend by default, with the
  reason in the startup banner, and an error if it asked for hisn explicitly.
- Wakil no longer dies together with its session's supervisor: `Pdeathsig` on
  the run process ends when that process exits at ready. A crashed Wakil leaves
  its session running until the next start's reconcile stops it through the
  owner record.
- The code still contains the 0.4.0 shape (a run process that stays alive is
  treated as the supervisor). The CHANGELOG lists unifying the "detached"
  decision (behaviour, with a 500 ms wait) and the "stopped holds the name"
  decision (version check) on one signal as a known follow-up.
- Wakil's own prune is plain `hisn ls --prune`; scoping it with `--name-prefix`
  is not done yet.
