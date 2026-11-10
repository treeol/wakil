# Sandbox backends

Wakil runs every tool call inside a sandbox. Backends:

| backend | selected by | notes |
|---|---|---|
| `hisn` | the **default when usable** (see [Choosing the backend](#choosing-the-backend)), or `sandbox.backend: "hisn"` | rootless, no daemon; needs hisn >= 0.4.2 (named sessions, `hisn-ls/v2`, the client record, `hisn stop`, the `run --detach` contract described [below](#supervisor-and-the-detach-contract)) |
| `docker` | `sandbox.backend: "docker"`, or the fallback when hisn is not usable | behaviour unchanged |
| `direct` | `sandbox.backend: "direct"` (or the deprecated `exec_mode: "direct"`) | no sandbox at all: explicit opt-out |

Combining `exec_mode: direct` with `backend: "hisn"` is a configuration error.

## Choosing the backend

Since v0.4.0 hisn is the default **only when all three hold**:

1. nothing chose a backend — no `sandbox.backend` in the config file, no
   `WAKIL_SANDBOX_BACKEND`, no `--sandbox`/`--exec`, no `exec_mode`;
2. `hisn check` passes;
3. hisn >= 0.4.2 is in `PATH`.

Otherwise the backend is docker, and the startup banner says why:

```
sandbox: backend=docker · browser tools: on · default: docker (hisn 0.3.3 is older than 0.4.2)
sandbox: backend=hisn · browser tools: on (inside hisn session) · default: hisn 0.4.2 (hisn check ok)
```

A host without hisn therefore sees no behaviour change. Further reasons that
keep docker (never a hard error through the default alone): an explicit
`--workdir` (docker-only), an unapproved or malformed `.wakil/sandbox.json`,
or a workspace that contains Wakil's state directory (hisn would refuse the
generated profile). An **explicit** choice is never second-guessed in either
direction: `sandbox.backend: "docker"` always means docker, and an explicit
`hisn` with an unusable hisn is an error (no fallback).

`LoadConfig` is pure (it resolves to docker and records `BackendDefaulted`);
`wiring.ResolveSandbox` settles the default once in the TUI and in `wakil run`,
and the resolved config is what everything else sees.
Docker-only startup probes (`docker version`/`docker info`, image checks) run
only for backend docker; `wakil doctor` reports the default and no longer
fails for a missing docker runtime when hisn will run.

## Invariants

1. Docker behaviour is unchanged. `DockerExecutor` is not rewritten; it is only
   extended with additive, optional interfaces. Docker stays the fallback when
   hisn is not usable and the choice is not explicit.
2. No silent fallback from an EXPLICIT hisn to docker or to no sandbox. Every hisn failure
   (preflight, policy, session start, exec) is surfaced as an error. A tool
   call is never executed on the host because a check failed.
3. The tool layer (`internal/agent`, `internal/tools`, `internal/lsp`,
   `internal/browser`, `internal/tui`) never knows which backend runs. It sees
   `exec.Executor` plus optional capability interfaces. All host<->agent path
   translation goes through the executor/backend mapping (`MapPath`,
   `HostPathToURI`); `DockerExecutor` keeps its existing mapping methods,
   which the docker `Backend` adapter delegates to.
4. Exec environments are allowlisted (`sandbox.hisn.env_pass`), never the host
   environment. Wakil also hands `hisn` itself a scrubbed environment.

## Architecture

```
 tool layer (agent, tools, lsp, browser, tui)
        │  exec.Executor  (+ optional: ChatBinder, SandboxResetter,
        │                    WorktreeHost, FileModer, AccessGranter)
        ▼
 internal/exec
   ├─ DockerExecutor   (unchanged; also exposes Backend() adapter)
   ├─ DirectExecutor   (unchanged)
   └─ BackendExecutor  (exec.Executor built on any sandbox.Backend)
        │
        ▼
 internal/sandbox            (leaf package, no wakil imports)
   Backend, Session, ExecSpec, ExecHandle, errors, PathMap, GrantManager
        ▲
        │ implements
 internal/sandbox/hisn       (drives the `hisn` CLI + control socket)
```

`exec.Executor` is the tool-layer seam (~30 call sites, every test fake).
`sandbox.Backend` is the lower, narrower primitive (start/exec/grant/revoke/
stop/map-path); `BackendExecutor` turns any Backend into an Executor by
running the same shell snippets `DockerExecutor` uses. A future backend
implements `Backend` only.

### Docker is adapted, not moved

`DockerExecutor` keeps being the Executor for backend `docker`. Its
`Backend()` adapter (`internal/exec/docker_backend.go`) returns a
`sandbox.Backend` over it (Exec → `docker exec`, Stop → `Close`,
MapPath → `/mnt/<name>` ↔ host mount). Grant/Revoke return
`sandbox.ErrNotSupported` (docker mounts are fixed at `docker run`).

## Path model

| | docker | hisn |
|---|---|---|
| agent root (`Executor.Cwd()`) | `/mnt/<basename(host)>` | the host cwd, identical path |
| `MapPath(agent)` | agent root → host mount | identity (must be absolute; cwd subtree or an active grant) |
| `UnmapPath(host)` | host mount → agent root | identity |
| sandbox temp dir | container `/tmp` | session `/tmp` (private tmpfs) |
| `$HOME` | in-container sandbox-home bind | `/run/hisn/home` (persistent host dir `home_root`) |

The only place the `/mnt/` convention is encoded is `sandbox.DockerAgentRoot`
(used by `config`). Hard-coded `/tmp` in `worktree.go` is replaced by the
backend's `SandboxTempDir()`.

`HostPathToURI`/`URIToHostPath` (LSP) use the same mapper; for hisn they are
the identity inside the cwd subtree and an error outside it.

### Execution paths vs host-representable paths

Two different questions must never be conflated:

* **Execution path**: a path the *sandbox* can resolve (cwd subtree, active
  grants, the session's private `/tmp`, `$HOME`). Valid as `ExecSpec.Cwd`.
* **Host path**: a path Wakil itself can open with `os.*`.

For hisn, session `/tmp/x` and host `/tmp/x` are different objects with the
same name. Therefore:

* `MapPath(agentPath)` returns a host path **only** for the cwd subtree and
  active grants; for anything else (including `/tmp`) it returns
  `sandbox.ErrNoHostPath`. It never identity-maps `/tmp`.
* `Backend.Exec` accepts any execution path as `Cwd` (`ResolveExecCwd`):
  host-representable paths are checked against the mapper; private paths
  (`SandboxTempDir()` prefix) are passed through untouched.
* `WorktreeHost.WorktreeKind()` reports `SandboxTmp` for hisn. Every
  worktree operation (mktemp, `git worktree add`, patch apply, cleanup,
  `isWorktreePath`) goes through `RunShell` and never `os.*`;
  `isWorktreePath` does a pure string-prefix match and does not
  `EvalSymlinks` on the host.
* Background logs live in sandbox `/tmp` and are only read through
  `ReadFileTail`/`StatFile` (executor calls), as in docker mode.

### Escape audit

1. **Host-side children**: the exceptions to "every tool call runs in the
   sandbox" are open_url (opens in your host browser), lifecycle hooks, MCP servers and the clipboard. They run on the host regardless of backend
   (`open_url`: `internal/tools/open.go`; hooks: `internal/agent/hooks.go`,
   `sh -c` with the host environment; MCP servers: `internal/agent/mcp_manager.go`,
   where a stdio server is a host child process with the host environment and an
   HTTP server is reached over HTTP by Wakil's own process; clipboard:
   `internal/tui/clipboard.go` and `internal/tui/tui_select.go`, host clipboard
   commands, or terminal OSC 52 escape bytes in `osc52` mode).
2. **Browser**: under hisn, Chromium runs inside the hisn session, not on the
   host. `browser.NewManager` refuses an executor whose `HostBrowserAllowed()`
   is false; for the backend executor that holds exactly when the backend has a
   CLI transport (`sandbox.CLIExecer`; hisn has one). Chromium is started
   in the session through `StartInteractiveCLI`; the host client's argv carries
   the session id, which is the ownership marker for teardown. The DevTools
   port is chosen by Chromium (`--remote-debugging-port=0`) and is
   loopback-only inside the session. Wakil reaches it through a listener on
   `127.0.0.1` on the host that pipes each connection through `python3` in the
   session; no port is published. `browser_enabled` is not forced off under
   hisn; when it is set, the banner reads `browser tools: on (inside hisn
   session)`. Chromium (`chromium`, or `browser_path`) and `python3` must exist
   inside the session (probed at launch). A backend executor without a CLI
   transport keeps the browser off.
3. **Worktree isolation** (edit subagents): the worktree lives in the
   session's `/tmp` and the child executor re-roots via `hisn exec --cwd`.
   `WorktreeHost.WorktreeKind()` (`SandboxTmp`) routes every `os.*` site to
   `RunShell`.
4. **Path confinement** (`ConfinePath`) runs `readlink -f` inside the sandbox
   and compares against the agent root, as docker does. The hisn agent root
   is symlink-resolved once at config time because hisn canonicalises `cwd`.
5. **Toolcache spill reads** are served from the host by design (never part
   of the workspace). Unchanged.
6. **`.secrets/`** sits inside cwd and is therefore visible to every exec
   without a grant. This is a known limit (below); masking it is future work.
7. **Env**: per-exec env is allowlisted (`env_pass` only, never computed
   names). The tool layer passes no env today, so execs inherit only the
   session base env.
8. **Wakil's own `hisn` processes** get a scrubbed env: `PATH`, `HOME`,
   `XDG_RUNTIME_DIR`, `LANG`, `LC_*`, `TZ`, `TERM`, plus the configured
   `env_pass` names on `hisn run` only. Nothing else is inherited, so API
   keys in Wakil's environment never reach `env.pass`.
9. **Lost exec** (exit 125 without a diagnostic, or a dropped ctl connection
   without an exit line): the program may still be running. The result says
   so, the executor records it, and the session is stopped at chat end.
10. **Session ownership**: orphan reconcile only touches sessions that carry
    a Wakil owner record whose owning process is gone. Foreign sessions of
    the same uid are never killed.

## Backend interface

```go
type Backend interface {
    Start(ctx context.Context, s Session) error
    Exec(ctx context.Context, spec ExecSpec) (ExecHandle, error)
    Grant(ctx context.Context, path string, rw bool) error
    Revoke(ctx context.Context, path string) error
    Stop(ctx context.Context) error
    MapPath(agentPath string) (hostPath string, err error)
    UnmapPath(hostPath string) (agentPath string, err error)
}
type Session struct{ ID, HostCwd string }
type ExecSpec struct {
    Argv   []string
    Cwd    string            // AGENT path; the backend maps it
    Env    map[string]string // allowlisted only
    Stdin  io.Reader; Stdout, Stderr io.Writer
}
type ExecHandle interface {
    Wait() (ExitInfo, error)  // ExitInfo{Code int, Lost bool, BackendError bool}
    Kill() error
}
```

`ctx` cancellation on the CLI path: SIGTERM to the local `hisn exec` client
(hisn then TERMs the program group and KILLs after 5 s). On the ctl path:
TERM by `exec_id` on a fresh signal connection, then KILL on a fresh
connection, then close the exec connection as a last resort. Backends must
be safe for concurrent `Exec` calls.

## Lifecycle (hisn)

### Preflight

`Open` calls `Preflight`: `hisn --version` parses and is >= `MinVersion`
("0.4.2"), and `hisn check` exits 0. Otherwise `Open` returns an error and
Wakil exits. No fallback for an explicit hisn. Unknown `sandbox.backend` values
are a config error. Then `Open` plans the session
once, before any session or UI exists: the kvr service decision, then the risk
check (see Risk labels) — a refusal is a hard error here.

The **default-backend probe** (`hisn.Probe`: `--version` and `check`, nothing
else; read-only) is separate and runs before the executor exists, see
[Choosing the backend](#choosing-the-backend).

### Command factory

Every `hisn` invocation (version, check, policy/profile validate and explain,
run, exec, grant, revoke, ls, kill, inspect) is built by `Backend.command`, which sets a
scrubbed environment: `PATH`, `HOME`, `XDG_RUNTIME_DIR`, `LANG`, `LC_*`,
`TZ`, `TERM`. Only `hisn run` additionally receives the `env_pass` names
(hisn copies them from the client environment at session start). A test
greps the package for `exec.Command` outside the factory.

### Session

One session per chat, `session-id = chatID` (UUID, valid for hisn's
`[A-Za-z0-9._-]{1,64}`). Subagents share the parent executor.

### Supervisor and the detach contract

This section is the one description of the contract; other documents link
here. Wakil requires **hisn >= 0.4.2** (`MinVersion`). An older hisn is refused
at preflight with a message naming 0.4.2, and there is no compatibility path
for it ([ADR 0014](adr/0014-hisn-minimum-0.4.1.md)).

`hisn run --detach` starts a session with no main command (PID 1 sleeps until
stopped) and **returns once the session is ready**:

```
hisn run --detach -v --session-id <id> --profile <file> --cwd <hostcwd> --ready-fd 3 --name <name>
```

* **Ready.** hisn writes `ready\n` to the `--ready-fd 3` pipe and
  `<id> <name|-> ready` to stdout, then the `hisn run` process exits 0. That
  exit is success, not death.
* **The supervisor outlives the run process.** The supervisor holds the
  namespace fds and the control socket. It is not Wakil's child, so Wakil
  cannot `Wait` on it, and the `Pdeathsig` set on the run process does not
  reach it.
* **`supervisor.pid`.** hisn writes `<session-dir>/supervisor.pid` as
  `"<pid> <starttime>"`: the pid and field 22 of `/proc/<pid>/stat`. Wakil takes
  the session directory from `hisn ls --json` (field `dir`), reads the file
  (retrying for at most 2 s) and tracks the supervisor by that pid **and**
  start time. A bare `<pid>` is accepted too (the start time is then read from
  `/proc`). A start time that disagrees with `/proc` means the pid was reused
  and the start fails.
* **Liveness (`procAlive`).** A supervisor is alive only if `kill(pid, 0)`
  succeeds, `/proc/<pid>/stat` is readable, its state is not `Z` or `X`, and
  its start time matches. A zombie is dead. A reaped pid is dead too: when the
  `/proc` entry vanishes between the two reads, a readable procfs plus a second
  `kill(pid, 0)` answering `ESRCH` proves it is gone. Only a state that cannot be
  proven either way (no procfs, a pid-namespace mismatch, permissions) reads as
  alive.
* **Which shape.** Wakil decides by behaviour: after the ready line it waits up
  to 500 ms for the run process to exit. Exit 0 means the session is tracked
  through `supervisor.pid`. A run process that stays alive is treated as the
  supervisor itself and reaped by handle (the code still handles that shape).
  A non-zero exit before ready is a start failure.
* **Stopping.** Close, Stop, a failed start and a session replacement all end a
  session the same way:
  1. Prove identity first: the tracked supervisor (pid and start time) is
     alive, or the instance token in the session's policy hostname matches
     (`hisn inspect --session ID --json`). A session that is provably not ours
     is never stopped; one whose identity cannot be checked is an error and the
     obligation is kept.
  2. `hisn stop --session <id>` (its own 5 s bound, independent of the caller's
     context).
  3. Wait on the supervisor pid (polling `procAlive`), capped at **5 s**
     (`stopCap`). On the cap, SIGKILL goes to that exact pid (only while its
     start time still matches) and Wakil waits again; a supervisor that still
     lives is an error. A supervisor that could not be tracked is waited on
     through `hisn ls --json` reporting the session over.
* **Names.** Sessions are created with `--name wakil-<project key>-<chat id>`.
  From hisn 0.4.1 a `stopped` session keeps its name reserved: `hisn run --name`
  exits 125 (`name ... is already in use`). `Start` therefore runs
  `hisn ls --prune` before the spawn whenever a dead (`stopped` or `crashed`)
  directory has this chat's id or holds its name. The prune is plain
  `hisn ls --prune`; hisn 0.4.1 also accepts `--name-prefix` to scope it, which
  Wakil's own prune does not use yet.
* **A crashed Wakil leaves its session running.** The run process is gone
  before Wakil can die, so nothing ties the supervisor to Wakil. The owner
  record (Wakil's pid and start time) is what lets the next start's Reconcile
  stop it (see Reconcile).

Wakil spawns the run process with `cmd.Start()` and never waits for it on the
tool call's context:

* **`-v` is required.** hisn prints nothing to stderr without it.
* **Stdio.** `stdin` is `nil` (Go maps that to `/dev/null`). `stdout` is
  captured (the `<id> <name|-> ready` line). `stderr` is a pipe teed into a
  64 KiB in-memory ring and a per-chat supervisor log file
  (`<state>/hisn/<chatID>.supervisor.log`, `0600`, truncated at every start).
  The file is written by a separate goroutine through a bounded queue; chunks
  are dropped (and counted in the log) when the queue is full, so a stalled log
  destination can never wedge hisn's stderr or delay the publication of its
  death.
* **Lifetime belongs to the session, not to a tool call.** The command derives
  from `context.Background()` and is cancelled only by the session's own
  teardown. The tool call's context is used for validation and readiness only.
  The run process runs under `Setsid` so a terminal Ctrl-C or hang-up cannot
  reach it (hisn treats SIGINT/SIGHUP as "shut down").
* **`Pdeathsig = SIGTERM`** on the run process (Linux only; other platforms
  compile but have no parent-death signal). It covers a Wakil that dies while the
  run process is still alive, that is, before ready. The spawning goroutine is
  pinned to its OS thread (`runtime.LockOSThread`) for the process's whole life,
  because the kernel delivers `Pdeathsig` when the *thread* that forked the
  child exits and the Go runtime may retire idle threads.
* **Readiness (`--ready-fd 3`).** The run process is started with the write end
  of a pipe as fd 3; Wakil closes its copy right after the spawn. Ready means,
  all at once, within a **10 s** budget (`readyCap`): the supervisor has not
  exited, the whole `"ready\n"` line arrived on the pipe, `hisn ls --json`
  (schemas `hisn-ls/v1` and `hisn-ls/v2`, field `state`) reports the session
  **live** (`running`; v1 said `live`) under the name Wakil gave it, and the
  same holds again one poll interval (`readyPoll` = 100 ms) later. A partial
  line, EOF, `creating`, `stopping`, `stopped`, `crashed` (v1: `stale`),
  missing, a name that points elsewhere, an error, an exited supervisor, or a
  timeout is *not ready*. An `ls` schema Wakil does not know is an error, not a
  guess. On timeout the supervisor is SIGKILLed, reaped, and the error carries
  the last probe's reason, the stderr, and the log path. A half-started session
  never receives a tool call: it is published (`cur`) only after readiness
  *and* instance verification.

### Start

`Start` is idempotent for the running session. A session whose supervisor
died is replaced (stopped and started fresh), not treated as running.
Before the replacement, the active grant set is snapshotted
(`PathMap.Grants()`); after the new session starts, the grants are replayed
(rw grants re-issued through `hisn grant`, ro grants re-added to the path
map). `stopAllLocked` clears the grants, so the snapshot must happen first.
Grants belong to one chat's session: the snapshot is replayed only when the
replacement has the **same id** as the session it replaces. Starting a
different id never inherits them (one diagnostic line says how many were
not carried over).

If the previous session cannot be stopped, one line is logged, the leftover
stays tracked (retried by Stop/Close), and the new session starts anyway.
A clash on the *same* id is still caught by `startLocked`.

`startLocked`, under the state lock:

1. Reconciles a record left by a dead owner; a live owner means the chat is
   active elsewhere (`ErrSessionInUse`).
2. `hisn ls --json`: if the id is present and **live**, refuse
   (`session id in use`) — Wakil never adopts or kills a session it did not
   create; the same if a LIVE session that Wakil did not create already holds
   the chat's *name* (the refusal names the holder and its clients). If the id is present but not live (`stopped`, `crashed`;
   v1 `stale`) and no live Wakil owns the chat, it is a dead leftover of this
   chat id: `hisn run --session-id` replaces it itself and one line is
   logged. A `crashed` leftover of this id or holding this name is pruned
   first (`hisn ls --prune` archives it and frees the name, which hisn keeps
   reserved until then). A `stopped` directory holds its name too (hisn 0.4.1:
   `name ... is already in use`, exit 125), so a `stopped` leftover is pruned
   first as well.
3. Creates the owner record with `O_EXCL`: `{pid, pid_start, token, name,
   cwd}` where `token` is a fresh 128-bit hex value.
4. Spawns the supervisor with `--name wakil-<project key>-<chat id>` (letters,
   digits, `.`, `_`, `-`; at most 64 characters, a longer name keeps its prefix
   and ends in a hash) and `session.hostname = "wakil-<token[:16]>"` in the
   policy, and waits for readiness. Every later `hisn exec`, `grant`,
   `revoke`, `inspect` and `ls --session` addresses the session by that name.
5. Verifies the instance (`hisn inspect --session ID --json` → resolved
   policy contains the token hostname) and marks the record `started`. On
   any failure the session Wakil created is ended through the stop procedure
   above (identity first), then pruned and its record removed, so a foreign
   session is never stopped by a failed start.

A failed start that left a possibly-running supervisor keeps it in the
`orphans` list for retry by Start/Stop/Close.

### Stop

`Stop` ends the session through the procedure under "Supervisor and the detach
contract": identity, `hisn stop --session <id>`, then a wait on the supervisor
pid. The session's execs are terminated, the directory is kept as `stopped` and
the stop is audited. Idempotent and tolerant of "session not found": the
supervisor having exited is what counts. Only the *bookkeeping* is gated on the
owner record (a record belonging to another instance is left in place and
reported).

After the supervisor is gone, a `live` socket on the id is a replacement by a
foreign session: left alone and reported. A `stopped` dir is the intended end
state: it stays for hisn's audit and `hisn ls --prune` (next Start, Reconcile)
archives and removes it. A `crashed` dir (the supervisor died before it could
record the stop) is pruned at once, so its name is free.

A supervisor that cannot be stopped (uninterruptible sleep) keeps the owner
record and stays in the `orphans` list; Start/Stop/Close retry it.

### Kill safety

hisn offers no instance-bound stop, so Wakil proves the identity of the session
before it sends `hisn stop`: the tracked supervisor (pid and start time) is
alive, or the instance token matches. Signals go only to the exact tracked pid
whose start time still matches. A residual window of one CLI round trip remains
between the check and the stop (Known limits).

### Health

A session is dead when its supervisor has exited **or** a hisn client
reported `Connection refused` on its control socket. The text alone never
decides: an exec client's stderr also carries the *program's* stderr, so
on `exit 125` plus a refusal-looking line Wakil probes the session itself
(`hisn ls --session`, `hisn ls --json`) and only a failed probe marks the
session dead. `Exec`, `Grant` and `Revoke` on a session already known dead
return `sandbox.ErrSessionDead` (exit status, captured stderr, log path)
*before* any `hisn` client is spawned; a refused connection discovered by a
client marks the session dead and fails that call with the same error (the
program did not run). The executor drops the dead session (generation
advances, bg-registry entries marked lost) and the chat's next tool call
starts a fresh one. The failing call itself is not retried.

`Start` for the same id replaces a dead session instead of treating it as
running. A tool call is **never** failed with "previous session not
stopped": if the previous session cannot be stopped, one line is logged,
the leftover stays tracked, and the new session starts anyway.

### Grant replay on replacement

After a dead-session replacement (`Start` over a stale dir), the active
grants are replayed: both RO and RW grants are re-issued through
`hisn grant` so Landlock is updated, and re-added to the path map. The
policy (and thus `grantable_rw`) does not change across replacements of the
same session, so replay always succeeds for the same set. A failed replay is
best-effort: logged, not a start failure; the user re-grants if needed.

**One ledger.** The backend's `PathMap` is the source of truth for active
grants. After `BackendExecutor` starts a session it makes its
`GrantManager` equal to the backend's ledger (`GrantManager.Sync`, via the
optional `sandbox.GrantLister` interface), instead of forgetting the grants:
a later `Withdraw` must still reach the backend. Backends without a ledger
of their own start with no grants (`Reset`).

`TestStartReplaysGrantsOnReplacement` covers replay;
`TestGrantLedgerIdenticalAfterReplacement` asserts the manager and the path
map report the identical set after a replacement and that `Withdraw` works;
`TestStartDoesNotCarryGrantsToAnotherSession` covers the id check.

### Bind (`ChatBinder`)

Called from the one place the chat ID changes (`App.setChatID`). Binding
the same ID is a no-op; binding a different ID stops the current session
and starts lazily on the next exec. The old session is stopped (record
removed) and the new chat starts its own.

### Reset (`SandboxResetter`, used by `/rewind`)

After a fully successful rewind the session is stopped and the generation
advances so bg-registry entries are marked "process lost"; the next call
restarts lazily for the same chat once the id has disappeared from
`hisn ls`. A refused rewind does not reset anything.

### Reconcile

At construction (`Open`), reconcile cleans up after a Wakil that died without
stopping its session. `hisn run --detach` exits once the detached supervisor is
up, so `Pdeathsig` does not reach the supervisor: a Wakil that crashes leaves
its session running, and step 2 below (the dead owner's record) is what stops
it at the next start.

1. `hisn ls --prune`: hisn itself archives (`grants.jsonl` and
   `session.json` to `$XDG_STATE_HOME/hisn/audit/<id>.jsonl`) and removes every
   provably dead dir, `stopped` or `crashed` (socket refused AND supervisor pid
   gone or starttime-mismatched, twice 300 ms apart; removal fd-relative,
   owner-checked, runtime-root confined) and repairs its name index. No live
   session is touched; no owner record is needed.
2. Under the lock: for each record whose owner process is gone (pid absent
   or start time differs) — re-read-and-compare under the lock first, so a
   session replaced by a new owner is left alone. If the id is absent from
   `hisn ls`, drop the record. If it is present and the instance token
   verifies, `hisn stop` (then a KILL signal if it is still served) + wait. If
   present but unverifiable, keep the
   record and log. Sessions without a record are never touched. Errors are
   logged, never fatal.

### Background and interactive processes

All pid-based operations (`StartBackground`, `KillPgid`, `IsProcessAlive`,
`IsProcessGroupAlive`) run *inside the session* through `hisn exec` with
the same shell snippets docker uses, so pids and pgids are always
sandbox-namespace values.

`StartBackground` runs `setsid nohup sh -c … > <log> 2>&1 & echo $!`; the
launcher exec exits immediately and the setsid'ed child is no longer the
exec's program, so cancelling the launcher client does not kill it; it dies
with the session.

`StartInteractive` always returns pid **0**: the only pid Wakil has is the
host-side `hisn exec` client's, and every pid-based call runs in the
sandbox pid namespace where that number could name an unrelated process.
Callers treat 0 as "no process guard". The pipes are real OS pipes (not
`io.Pipe`), so reaping the client never depends on the caller draining
stdout or closing stdin.

### Exec paths (ctl default, CLI fallback)

**Ctl path (default, `exec_via_cli = false`):** tool-call execs speak the
§7 protocol directly (`internal/sandbox/hisn/ctl`) instead of spawning one
`hisn exec` client per call — one fewer process per tool call, and no
pid-reuse window. The exec request line and the program's three stdio fds
travel in **one `sendmsg`** (SCM_RIGHTS; fds only on the first line of a
connection, exec only, `tty=false` always). Wakil closes its copies of the
passed ends right after the send. The acceptance response carries
`exec_id` (`e-<n>`).

The exec connection carries only the request line, the acceptance, and
the async `{"exit":…}` line — hisn does not read it further (DESIGN
§14.7). Signals (`{"op":"signal","exec_id":…}`) travel on their **own**
fresh connections, one response line each. One dispatcher goroutine owns
the exec conn's reader and routes the `{"exit":…}` line.

Cancellation after acceptance: the exec's `ctx` (the caller's context,
joined with the session instance) is watched for the life of the exec. When
it ends before the program does, the same sequence as `Kill` runs (once —
`Kill` and the watcher share it), and `Wait` returns with the latched exit
(a signalled status such as 143), not "lost" and not hung. If the sequence
cannot obtain the exit line, the exec conn is closed so `Wait` still returns.
The caller's deadline bounds only the dial and the acceptance; it is cleared
once the exec is accepted so it cannot race the real exit line.

Cancel/Kill: TERM by `exec_id` on a fresh conn, ≤ 5 s grace, then KILL on a
fresh conn, then close the exec conn (close alone cancels: hisn TERMs then
KILLs after 5 s). Exit codes are the program's own (any code, incl. 125 —
the CLI stderr-heuristic does not apply on this path). A dropped connection
without an exit line probes the session: an unreachable session is a typed
session death, not "lost".

**CLI path (`exec_via_cli = true`, one release fallback):** one
`hisn exec` client process per call. Exit 125 without a `hisn:` diagnostic
means the program may still be running (Lost). The exec client's stderr
also carries the program's stderr, so a text match alone proves nothing:
Wakil probes the session itself and only a failed probe marks it dead.

### Lost exec

Exit 125 *without* a diagnostic (CLI) or a dropped connection without an
exit line (ctl, session still reachable): the program may still be
running. The result says so, the executor records it, and the session is
stopped at chat end.

## Profiles

Wakil starts sessions from a **`hisn-profile/v1`** file: the envelope's `[policy]` is exactly the policy Wakil always
generated (content unchanged), plus optional `[[services]]`, `[[forward]]` and
`[[host_services]]` tables from the project file.

```
hisn run --detach -v --session-id <chatID> --profile <state>/hisn/<chatID>.profile.toml \
         --cwd <hostcwd> --ready-fd 3
```

* The file is validated with `hisn profile validate` before `hisn run`.
* **Where it lives.** `~/.local/share/wakil/hisn/<chatID>.profile.toml` — never
  under the project. hisn refuses a profile that lies under the process cwd,
  the session cwd, a rw grant or the persistent home (`E_PROFILE_UNTRUSTED`:
  the sandbox could rewrite it and acknowledge its own risk). Wakil therefore
  also runs the supervisor in a neutral cwd (`<state>/hisn/proc-cwd`) and
  checks the rule up front, naming what to move (typical trigger: running
  Wakil with `$HOME` as the workspace). The file is removed when the session
  ends.
* The network (`host`), `shm`, tmpfs sizes, `grantable_rw` and `env.pass` come
  from `[policy]`, not from the CLI.
* A named profile (`hisn.profile`) is used **as is**: Wakil does not generate a
  file and only adds `--grant PATH:ro|rw` for `mounts_ro`/`mounts_rw`
  (`hisn.services|forward|host_services|env_set` next to it is an error).
  Such a session has no instance token in its policy, so it is identified by
  the supervisor process Wakil spawned (pid + start time, kept in the owner
  record), also for orphan reconcile. Built-in names (`base`, `wakil`) only
  resolve if a `profiles/` directory sits next to the hisn binary — the release
  binary ships none — so use `$HISN_PROFILES` (passed through to hisn) or a
  path-installed copy.

`network = "host"` has no filtering; this is the decided trade-off (builds
need the network) and is called out in Known limits. `shm = true` is always
set (Chrome, Node, POSIX shm users need `/dev/shm`).

## Per-project overrides: `.wakil/sandbox.json`

The project asks for more than the defaults in `<workspace>/.wakil/sandbox.json`.
Absent file = nothing extra. Unknown keys are errors (the message names the key).

| key | type | meaning |
|---|---|---|
| `hisn.services[]` | `{name, exec[], socket?, ready_socket?, ready_timeout?, stop_signal?, stop_grace?}` | processes started INSIDE the sandbox (`[[services]]`, label `none`). Relative `socket`/`ready_socket` are resolved against the workspace (hisn stats them as written). `restart` is always `none` |
| `hisn.forward[]` | `{name, socket, expose, risk?}` | expose an existing HOST unix socket inside, read-only (`[[forward]]`). Label from the socket (docker → `root-equivalent`, ssh-agent → `signing`, D-Bus → `host-exec`); an unrecognised socket needs an explicit `risk` |
| `hisn.host_services[]` | `{name, exec[], socket, expose, ready_socket?, ready_timeout?, stop_signal?, stop_grace?, risk?}` | a process that runs **on the host as you**, its socket forwarded in (`[[host_services]]`, at least `host-exec`) |
| `hisn.env_set{}` | name → value | literal environment variables inside the sandbox (`env.set`; not `HOME`) |
| `hisn.mounts_ro[]` | absolute paths | extra read-only binds (`mounts.ro`); with `hisn.profile`: initial `--grant PATH:ro` |
| `hisn.mounts_rw[]` | absolute paths | extra read-write binds (`mounts.rw`); with `hisn.profile`: initial `--grant PATH:rw` (bounded by that profile's `grantable_rw`) |
| `hisn.risk_ack` | string | **your** acknowledgement, verbatim as hisn expects it (`"root-equivalent"`, `"signing"`, `"host-exec"`, `"data"`, ...). Copied into every `[[forward]]`/`[[host_services]]` entry; Wakil never writes one itself. One string means one label: a project mixing capabilities with different labels uses `hisn.profile` |
| `hisn.profile` | name | a built-in or `$HISN_PROFILES` profile used instead of generation; Wakil then only adds grants |

One complete example (a project that runs a database inside the sandbox, reads
reference data, and uses the host docker daemon for deploys):

```json
{
  "hisn": {
    "services": [
      {"name": "db", "exec": ["/usr/bin/pg_ctl", "-D", "/work/app/.wakil/pgdata", "start", "-w"],
       "socket": ".wakil/db.sock", "ready_socket": ".wakil/db.sock", "ready_timeout": 30,
       "stop_signal": "TERM", "stop_grace": 10}
    ],
    "forward": [
      {"name": "docker", "socket": "/var/run/docker.sock", "expose": "/run/docker.sock"}
    ],
    "env_set": {"APP_ENV": "dev"},
    "mounts_ro": ["/opt/reference-data"],
    "mounts_rw": ["/srv/out"],
    "risk_ack": "root-equivalent"
  }
}
```

**Precedence with `sandbox.hisn.*` config:** the project file *adds*; it never
replaces. `grantable_rw`, `env_pass`, `home_root`, sizes and the binary always
come from the config; `env_set`, mounts, services, forwards and the
acknowledgement only from the project file. A project `mounts_rw` is a static
rw bind, separate from `grantable_rw` (which only bounds dynamic grants).

### The agent cannot change it, and a changed file needs your approval

* **Executor guard.** Every mutating executor operation — write, write-bytes,
  delete, move (either end) and removal/rename of the `.wakil` directory —
  refuses `.wakil/sandbox.json`. File tools and edit-tier subagents all go
  through the executor, so one choke point covers them; reads stay allowed. A
  worktree child's patch that touches the file is refused in `applyPatch`.
  Under hisn the file is also masked inside the sandbox (reads as empty), so
  `run_shell` cannot edit it either — defence in depth only.
* **Approval gate (the real control).** A shell-capable agent could still write
  a new file that would take effect at the next start. So a file that asks for
  anything is honoured only when its sha256 matches the one **you** approved,
  stored in Wakil's state directory (outside every sandbox):

  ```
  wakil sandbox trust [dir]     # shows the file and what it asks for; type "yes"
  ```

  There is no flag that skips the question and no non-terminal path (it reads
  `/dev/tty`; see the note on the state directory below). Any change to the file — by anyone — makes it unapproved again;
  Wakil then refuses to start the hisn session ("run `wakil sandbox trust`
  yourself — an agent cannot approve it"). A file that asks for nothing needs
  no approval. When the backend was only the *default*, an unapproved file
  keeps docker instead of failing.

## Wakil's state directory is never writable from the sandbox

`~/.local/share/wakil/hisn/` holds your approval of `.wakil/sandbox.json`,
generated profiles and owner records — the trust root. Wakil refuses to start
the hisn backend when any writable surface would contain, equal or lie under it:
`sandbox.hisn.grantable_rw`, `hisn.mounts_rw`, `sandbox.hisn.home_root`
(`would make Wakil's state directory writable from the sandbox`), and refuses a
dynamic rw grant over it. A broad `grantable_rw` such as your whole home
directory therefore has to exclude the Wakil data directory (move it with
`XDG_DATA_HOME`, or narrow the prefix). Be aware that a process that can open
its own pseudo-terminal could answer the `wakil sandbox trust` prompt: the
protection is that the sandbox cannot reach the state directory, not the
terminal check.

## Risk labels

hisn computes a label for everything a profile widens: `none`, `data`,
`signing`, `host-exec`, `root-equivalent`, `kernel-surface`. Wakil surfaces it:

* **Plan time, before any session or UI exists:** `hisn profile explain` of the
  profile exactly as sessions will start it. A label above `none` needs the
  project's own `hisn.risk_ack` with exactly that label (hisn demands one only
  for `signing`, `host-exec` and `root-equivalent`; Wakil also asks for `data`
  and `kernel-surface`). Missing or wrong is a **hard error naming the key**:

  ```
  sandbox: this workspace's sandbox risk is not acknowledged (label root-equivalent) — the profile's risk is "root-equivalent"; run `wakil sandbox trust` to review and acknowledge it (Wakil never acknowledges a risk on your behalf)
  ```

  Run `wakil sandbox trust` in the repo: it lists the capability labels, the
  resulting risk, and asks `Approve this exact content for this workspace? Type 'yes'`.
  Without a `.wakil/sandbox.json` it writes `{"hisn":{"risk_ack":"<risk>"}}` only
  after `yes`, and deletes it on any other answer. A bootstrap-form file whose ack
  no longer matches is updated the same way; a hand-written file is never rewritten
  (the command names the key to change). In the interactive TUI the same offer
  appears inline (`acknowledge now? [y/N]`); `wakil run` and other non-interactive
  runs never prompt.

  `hisn profile validate` is a second net (`E_RISK_ACK_REQUIRED`/`_MISMATCH`
  map to the same error).
* **After start:** the running session's own snapshot (`hisn inspect --json`,
  `risk[]`) must not exceed what was acknowledged, else the session is stopped.
* **Banner:** `sandbox: backend=hisn · risk=<label>` in the TUI startup note,
  and `wakil run`.
* **`/sandbox`** (TUI) prints backend, mode (generated profile / named profile /
  bare policy), the profile file, the risk and each capability with its label,
  services with live state, forwards, host services, env names, grants, kvr and
  hisn's notes.

## Staging (kvr) as an in-sandbox service

Under hisn the staging store runs **inside** the sandbox as a
`[[services]]` entry — the same confinement as every tool call — and the tools
reach it through its socket under `<workspace>/.wakil/` (the workspace is
bound at its identical path, so host and sandbox see the same socket).

* **Which service.** The project's own `hisn.services` entry named `kvr` (its
  `socket` is required, must be directly under `<workspace>/.wakil/`, and
  `ready_socket`, if set, must equal it); otherwise Wakil's default, built from
  `sandbox.hisn.kvr_binary` or `kvr-server` in `PATH` plus `kvr_max_entries`,
  `kvr_sweep_interval_secs`, `kvr_snapshot_interval_secs` (socket
  `<workspace>/.wakil/kvr-<instance>.sock`, snapshot `staging.kvr`). The socket
  name carries a per-process instance id so two Wakil processes on one
  workspace never share or unlink each other's live store; Wakil never
  unlinks a socket that still answers a PING, and sweeps dead `kvr-*.sock`
  files. (The snapshot file is shared per workspace, as before.)
* **Lazy session.** The hisn session starts on the first tool call, so the
  staging client exists from the plan and its connect guard starts the session
  if needed, requires the service to be up, and vets the socket — the first
  staging call works even before any other tool has run.
* **No binary, no staging.** The binary must be visible inside the sandbox
  (symlinks resolved first): under `/usr`, `/opt`, `/nix`, … or a directory the
  project lists in `hisn.mounts_ro`. Without one — or with `kvr_disabled`, a named profile, or `direct` — the staging tools stay off and the
  reason is in `/sandbox`. There is **no fallback to a host-side kvr**.
* **A broken kvr never fails the session.** Wakil's default service has no
  `ready` (hisn fails the whole session closed on a readiness timeout); Wakil
  PINGs the socket itself and disables staging with the reason if it does not
  answer.
* **The socket is writable by the sandbox**, so a hostile command could replace
  it. Before every (re)connect the host client therefore requires `.wakil` to be
  a plain directory and the socket a plain unix socket owned by you, directly in
  it (no symlinks) — otherwise it refuses to dial. Wakil removes a stale socket
  file of a dead session before the next start (hisn does not). The kvr protocol
  has no authentication; see Known limits.

## Docker work under hisn: `profiles/deploy-smaragd.toml`

The repository ships `profiles/deploy-smaragd.toml`: host networking, `cwd` rw,
`grantable_rw = ["/srv/out"]` and a **forward of the host docker socket**
(hisn "Docker T2"). That is **root-equivalent** on the host: the sandbox can ask
the daemon to start a privileged container mounting any host path, and
containers outlive the session. It ships disabled — both `risk_ack` lines are
commented out — and is not loaded automatically. To use it:

1. copy it to a directory in `$HISN_PROFILES` (outside any project);
2. in that copy uncomment the two lines `# risk_ack = "root-equivalent"`;
3. in the project: `{"hisn": {"profile": "deploy-smaragd", "risk_ack": "root-equivalent"}}`
   in `.wakil/sandbox.json`, then `wakil sandbox trust`.

hisn checks the acknowledgement in the profile, Wakil checks the one in the
project file; both are yours. Make sure `/srv/out` exists (or edit
`grantable_rw`). Wakil uses the profile as is (named-profile mode: grants only,
no kvr service).

## Grants (tier mapping)

`sandbox.GrantManager` wraps a Backend and the cwd:

| request | result |
|---|---|
| path inside cwd | nothing to do |
| read tier, outside cwd | `Grant(path, rw=false)` |
| edit tier, outside cwd | `Grant(path, rw=true)` only if under a `grantable_rw` prefix, otherwise `ErrNotGrantable`; no retry, no widening |
| withdrawal | `Revoke(path)` — **soft**. The path stops resolving but already-open descriptors may survive. This residual risk is documented in Known limits. |

Today the file tools confine every path to the workspace, so nothing in
the tool layer requests outside-cwd access yet. The manager is exposed on
the executor as the optional `exec.AccessGranter` interface so a future
`/grant`-style feature has the seam; no caller of `EnsureAccess` exists
yet (covered by unit tests with a fake backend and by the real-hisn
integration tests).

## Config

```json
"sandbox": {
  "backend": "docker",
  "hisn": {
    "binary": "hisn",
    "home_root": "",
    "grantable_rw": [],
    "env_pass": ["GOFLAGS","GOCACHE","GOMODCACHE","CARGO_HOME","RUSTFLAGS",
                 "npm_config_cache","PIP_CACHE_DIR","CI"],
    "tmp_size_mb": 1024,
    "shm_size_mb": 64,
    "kvr_binary": "",
    "exec_via_cli": false
  }
}
```

Flag `--sandbox docker|hisn|direct`, env `WAKIL_SANDBOX_BACKEND`. Omit all of
them to get the default described in [Choosing the backend](#choosing-the-backend)
(the `"backend": "docker"` above is only what you write to PIN docker). Empty
`home_root` means `<wakil state dir>/hisn-home/<workspace-hash>`.
`kvr_binary` empty means `kvr-server` from `PATH`. With `hisn`:
`browser_enabled` is not forced off (Chromium and `python3` must be in the session), `docker_*` keys are
ignored, `WorkDir == HostWorkDir`, and `kvr` runs as an in-sandbox service
when a binary is available (otherwise it stays off with the reason).

`env_pass` entries that can alter host process startup (`LD_*`,
`GCONV_PATH`, `LOCPATH`, `NLSPATH`, `HOSTALIASES`, `RES_OPTIONS`,
`GODEBUG`, `BASH_ENV`, `ENV`, `PATH`, `HOME`) are rejected at config
validation. Computed names (`HOME`, `CARGO_HOME`, `GOMODCACHE`, `GOCACHE`,
`npm_config_cache`, `PIP_CACHE_DIR`, anything prefixed `XDG_`) are set by
hisn itself inside the sandbox and are rejected in per-exec env.

## Known limits (hisn backend)

* `.secrets/` and every other file in cwd is readable by all execs.
* `network = "host"`: no network isolation.
* Soft revoke only (see Grants): already-open descriptors may survive.
* Toolchains installed under the host `$HOME` (rustup, nvm) are not mounted;
  hisn's `mounts.ro` is not exposed.
* Browser tools need Chromium (`chromium`, or `browser_path`) and `python3` inside the session; without them the first `browser_*` call fails with an error naming the missing binary.
* Worktree dirs live in the session `/tmp` tmpfs; the size is configurable
  (`tmp_size_mb`, default 1024 MiB) but bounded.
* Instance verification relies on `session.hostname` carrying a random token
  and on `hisn inspect --json` returning the resolved policy as a TOML
  string. A session whose policy carries no hostname is treated as
  *unverifiable* (record kept, error reported), never as foreign.
* The reconcile-path kill re-verification window (one CLI round trip)
  cannot be closed without an instance-bound kill in hisn.
* Reconcile does not remove a non-live runtime directory it finds by id:
  with no supervisor handle it cannot tell a crashed one from a foreign
  session that is still starting. Such a directory is only cleaned when the
  same chat id starts again (the stale replacement in Start); other stale
  entries stay until removed manually.
* `Pdeathsig` on the `hisn run` process ends with that process, which exits at
  ready. A Wakil crash or SIGKILL after start leaves the detached supervisor
  running until the next start's Reconcile stops it, and only a session with an
  owner record is touched.
* `StartBackground` uses `setsid nohup … &` inside `hisn exec`. That the
  detached process survives its launcher exec is asserted by
  `TestIntegrationStopKillsSleeper` but there is no readiness handshake
  between launcher exit and `setsid`.
* Host<->URI mapping (LSP) is refused for paths that resolve outside the
  workspace on the host (e.g. `ws/link -> /tmp/x`), because the same name
  can be a different object inside the sandbox.
* **kvr protocol has no authentication.** The staging socket sits in the
  workspace, which the sandbox can write: a hostile command can kill the
  service and bind a replacement, and the host client only checks the path (a
  plain directory and socket owned by you, re-checked before every connect) —
  that narrows the window, it does not close it. Staging content is untrusted
  anyway (Wakil never executes it). A peer-credential check on the connection
  is a follow-up.
* **A named profile is a weaker mode.** Wakil adds only grants; the project file
  is not masked, there is no kvr service, and the instance is identified by the
  supervisor process, not a token.
* **`.wakil/sandbox.json` created later.** The mask only covers a file that
  exists when the session starts; the approval gate (a changed file is
  unapproved) is what stops an agent-authored file from taking effect. Whether
  a sandboxed command can rename the `.wakil` directory (taking the mask with
  it) has not been verified against a real hisn.
* The label vocabulary is hisn's: `RISK: none` means "no 0.3 surface widening",
  not "no widening" — `mounts_rw`, host networking and the project's own mounts
  are not labelled by hisn. The banner shows what hisn labels.
* The hisn integration tests skip when `hisn check` fails (no unprivileged
  user namespaces); they have not been run in the build sandbox. The profiles
  Wakil generates ARE checked against a real hisn >= 0.4.2 wherever the binary
  exists (`TestRealHisn*`: `profile validate`/`explain` are file checks and need
  no user namespaces), so the contract does not rest on the test fake alone.
  Run the session tests on a host with hisn >= 0.4.2:
  `go test -count=1 -v -timeout 10m -run TestIntegration ./internal/sandbox/hisn/`