# hisn 0.2.1 control-socket client — design note (final)

Wakil-side client for the hisn session control socket
(`$XDG_RUNTIME_DIR/hisn/<id>/control.sock`, fallback `/tmp/hisn-$UID/<id>/control.sock`,
0600, owner = our uid). Protocol (POLICY-SPEC §7): one JSON object per line,
one response line per request, in order, max line 64 KiB. 0.2.1 adds
`--ready-fd`, `exec_id`, `--prune`, `session.shm/shm_size/tmp_size`, frozen
`--json` schemas, stale-kill semantics. **Preflight requires hisn >= 0.2.1**
(refuse older, as today for < 0.2.0): no ctl, no ready-fd against 0.2.0.

## 1. Framing (ctl.go)

A `conn` wraps one `net.Conn` per control operation.

- Dial: `net.DialContext(ctx, "unix", sockPath)`. `ECONNREFUSED` marks the
  session dead (the supervisor owns the socket). Residual window: the dial
  happens after `admit()`, so a foreign replacement of the same id could
  own the socket path at dial time — the same one-round-trip window the CLI
  client has today (Known limits); every operation re-checks `l.dead()`
  before the request line is sent and a refused dial marks the instance dead.
- Request line: `json.Marshal` (argv bounded by the tool call), ONE
  `WriteMsgUnix` carrying bytes + (exec only) exactly 3 fds as SCM_RIGHTS.
  Writes serialized by a per-conn mutex. No partial-send retry with rights.
- Response: `ReadString('\n')` on `bufio.Reader` sized 64 KiB + slack; a line
  > 64 KiB is a protocol error, conn closed.
- `{"ok":false,...}` maps: `E_BAD_REQUEST`→caller bug, `E_PATH_FORBIDDEN`→
  `sandbox.ErrPathForbidden`, `E_NOT_GRANTABLE`→`sandbox.ErrNotGrantable`,
  `E_NOT_FOUND`→stale exec_id (§3), `E_EXISTS`→already granted,
  `E_INTERNAL`→backend error. Session liveness is decided only by Wakil's
  own probe (`hisn ls --json`), never by error text.
- Deadlines: from ctx on request round trips. Cancellation cleanup uses its
  own bounded budget independent of the caller (§3).

## 2. exec fds — orientation and ownership

The 3 passed fds become the program's stdio inside the sandbox:

| program's fd | passed (SCM_RIGHTS) | kept in Wakil |
|---|---|---|
| stdin  | read end of stdin pipe | write end (copier: spec.Stdin → pipe) |
| stdout | write end of stdout pipe | read end (copier: pipe → spec.Stdout) |
| stderr | write end of stderr pipe | read end (copier: pipe → spec.Stderr) |

`tty=false` always. The JSON line + fds go in one `WriteMsgUnix`
(POLICY-SPEC §7: fds travel in the same message; accepted only on the first
line of a connection, exec only). **`sendmsg` does not consume the sender's
descriptors: Wakil closes its copies of the three passed ends immediately
after `WriteMsgUnix` returns** — success or failure — so the program's
writers are solely the sandbox side and copier EOF depends on no fd Wakil
holds. The stdin copier closes the write end at `spec.Stdin` EOF (or
immediately when nil). Stdio copiers start **after** the response line
(accepted); on rejection all kept ends close at once.

## 3. exec lifecycle — one exec conn, one dispatcher; signals on fresh conns

The exec owns **one exec connection for its whole life**, never closed while
the program runs, with **one reader goroutine** (the dispatcher) owning the
`bufio.Reader`: it parses every line and **latches** two outcomes — the
acceptance response and `{"exit":...}`. A latch is written exactly once
(`sync.Once`) and broadcast by closing a `done` channel; readers wait on
`<-done` and then read the stored value. **Nothing consumes an outcome from
a channel**: `WaitExit` can be called from any number of goroutines, before
or after the exit (e.g. `cancel()`'s bounded wait and a later `Wait()`), and
every call sees the same payload. The earlier single-shot channel was
drained by `cancel()`, so a later `Wait()` blocked forever. The lost outcome
(EOF, over-long line, closed conn) is latched the same way.

**The exec connection carries only the request line, the acceptance
response, and the exit line.** hisn (DESIGN §14.7) does **not read it**
after the request line: `signal` travels on its OWN new connection to the
session socket — `{"op":"signal","exec_id":"e-n","signal":"TERM"}`, one
response line, close. A signal written to the exec connection would hang
forever (no response ever comes); the fake hisn reproduces this so a
regression is caught in tests. The replacement-identity hazard the earlier
"signal on the exec conn" design closed is still closed: the signal is
keyed by `exec_id`, and a replacement session's exec ids start fresh, so a
stale id in a replacement yields `E_NOT_FOUND`, never a hit on a foreign
process.

States: `launching` (line sent, no response) → `running` (response has
`exec_id`) → `terminal` (exit line received).

- **launching + cancel**: close the exec conn → hisn cancels the exec (close
  = TERM then KILL after 5 s, protocol §7). No `exec_id` exists; no signal
  is possible. `Wait` → `Lost=true`.
- **running + Kill/cancel**: `signal {exec_id, TERM}` **on a fresh conn**,
  wait ≤ 5 s (cleanup budget, not the cancelled caller's deadline) for the
  exit line on the exec conn, then `signal {KILL}` on another fresh conn,
  wait ≤ 5 s more; closing the exec conn is the backstop (hisn TERMs then
  KILLs after 5 s on its own). Exit line → the real status (any code, incl.
  125 — the CLI stderr heuristic dies with ctl). Otherwise `Lost=true`,
  recorded like the CLI path. `E_NOT_FOUND` on a signal → the exec already
  exited; the exit line decides, never synthesized.
- **EOF / transport error before exit line**: `Lost=true` (transport
  failure); session death is **not** inferred from the conn — only Wakil's
  own `hisn ls` probe marks the session dead, as today.
- **`{"exit":{"lost":true}}` / malformed exit**: `Lost=true`.
- **Wait returns** after the terminal state AND the copiers finished (or a
  2 s drain bound, the WaitDelay analogue), so a background child holding
  stdout can not hang it.

## 4. Readiness: ready-fd AND live-confirm

`hisn run --detach -v --session-id X --policy P --cwd C --network host
--ready-fd 3`; the pipe write end is the child's fd 3 (`ExtraFiles`). Wakil
closes its write end right after `cmd.Start()` — EOF then means every
writer is gone. Ready = ALL, one total 10 s budget:
1. `"ready\n"` whole line from the read end (partial text without `\n` is
   not ready; supervisor exit → EOF → not ready),
2. `sessionState(id) == "live"` — `hisn ls --json`, frozen schema
   `{"schema":"hisn-ls/v1","sessions":[{id,state,…}]}`, `state=="live"`,
3. `verifyInstance` (token hostname) and epoch/admission checks — unchanged,
4. confirm one `readyPoll` later: not exited, still `live`.

The polling loop is deleted (pipe read + confirm probes remain). Not-ready
→ SIGKILL, reap, error with last probe reason + stderr + log path.

## 5. Stale sessions — hisn owns dir removal

`removeOwnSessionDir`/`removeDeadSessionDir` are deleted.

- **Reconcile**: `hisn ls --prune` first (hisn removes only dirs it proves
  stale: socket refused AND pid gone/starttime mismatched, checked twice
  300 ms apart; removal is fd-relative, owner-checked, runtime-root
  confined — no live session is touched). Then the existing record
  reconcile for dead owners (verifyInstance → killSession → drop record).
- **Start, id present but not live**: `hisn run --session-id X` replaces the
  stale dir itself (0.2.1); keep the one log line. A live foreign id still
  refuses with `ErrSessionInUse` (`E_EXISTS` surfaced).
- **Stop**: reap our supervisor (exact handle, unconditional). Then
  `sessionState(id)`: `stale` → `hisn kill --session X` (stale → dir
  removed, exit 0; hisn re-verifies staleness itself before removal, so a
  replacement becoming live in between is killed only inside the same
  one-round-trip window killSession already documents); `live` after our
  supervisor is reaped → a replacement by a foreign session: **leave it
  alone, report**. Never `hisn kill` on a live id we no longer own.
  Records/lock bookkeeping unchanged. Failed starts: identical reap; the
  orphan obligation is unchanged.

## 6. Policy generation

`buildPolicy` gains `session.shm = true`, `session.shm_size` (default 64),
`session.tmp_size` (default 1024) from config; validated by
`hisn policy validate` as today.

## 7. Config

```
sandbox.hisn.tmp_size_mb     = 1024  // MiB → session.tmp_size (16..=65536)
sandbox.hisn.shm_size_mb     = 64    // MiB → session.shm_size (1..=65536)
sandbox.hisn.exec_via_cli    = false // CLI exec fallback, one release
sandbox.docker.allow_userns = false // hisn development only
```

`allow_userns = true` gives the container Wakil's seccomp profile variant
that allows the namespace/mount syscalls for the unprivileged uid (not
`seccomp=unconfined`) and appends `--security-opt apparmor=unconfined
--security-opt systempaths=unconfined`; `no-new-privileges` stays. The
legacy top-level `docker_allow_userns` is an alias. Default false = docker behaviour identical. Startup
prints one warning when true:
`docker sandbox isolation reduced: user namespaces enabled`.

## 8. EnvPass through the ctl path

The env validation is **shared code** (`validateExecEnv`): the same
allowlist (`opts.EnvPass` minus computed `HOME XDG_* CARGO_HOME GOMODCACHE
GOCACHE npm_config_cache PIP_CACHE_DIR`) runs before the ctl request is
marshalled; a bad key rejects the whole call before any fd exists.
Server-side enforcement is POLICY-SPEC §7 ("whole request refused").
Tests: computed key listed in EnvPass explicitly → whole-call rejection;
accepted override visible in `env`; base env parity.

## 9. Invariants kept

1. Docker unchanged unless `allow_userns` explicitly set (default false).
2. No silent fallback: ctl failure → typed error; `exec_via_cli` is a
   config-time choice; nothing ever runs on the host.
3. Readiness = ready-fd line AND `state=="live"` AND confirm AND instance
   verify. Nothing weaker.
4. Env allowlist unchanged through the ctl path (§8).