# Sandbox backends

Wakil runs every tool call inside a sandbox. Two backends exist:

| backend | selected by | status |
|---|---|---|
| `docker` | default (`sandbox.backend: "docker"`) | production, behaviour unchanged |
| `hisn` | `sandbox.backend: "hisn"` | second backend, opt-in, needs hisn >= 0.2.1 |

`exec_mode: "direct"` (no sandbox at all) is a separate, explicit opt-out. It is
also accepted as `sandbox.backend: "direct"`. Combining `exec_mode: direct`
with `backend: "hisn"` is a configuration error.

## Invariants

1. Docker behaviour is unchanged and stays the default. `DockerExecutor` is not
   rewritten; it is only extended with additive, optional interfaces.
2. No silent fallback from hisn to docker or to no sandbox. Every hisn failure
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

1. **Host-side children** (hooks, MCP servers, `open_url`, clipboard) run on
   the host regardless of backend. Unchanged from docker mode.
2. **Browser**: `browser.NewManager` launches Chromium on the host when
   `ContainerName() == ""`. For hisn `ContainerName()` is `""`, so
   `browser_enabled` is forced off in config validation (sets
   `BrowserForcedOff` for the startup banner) **and**
   `HostBrowserAllowed()` returns `false`. No runtime enable path exists.
   Chromium inside hisn is future work.
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
("0.2.1"), and `hisn check` exits 0. Otherwise `Open` returns an error and
Wakil exits. No fallback. Unknown `sandbox.backend` values are a config
error.

### Command factory

Every `hisn` invocation (version, check, policy validate, run, exec, grant,
revoke, ls, kill, inspect) is built by `Backend.command`, which sets a
scrubbed environment: `PATH`, `HOME`, `XDG_RUNTIME_DIR`, `LANG`, `LC_*`,
`TZ`, `TERM`. Only `hisn run` additionally receives the `env_pass` names
(hisn copies them from the client environment at session start). A test
greps the package for `exec.Command` outside the factory.

### Session

One session per chat, `session-id = chatID` (UUID, valid for hisn's
`[A-Za-z0-9._-]{1,64}`). Subagents share the parent executor.

### Supervisor

`hisn run --detach` does **not** return: `--detach` only means "no main
command; PID 1 sleeps until stopped". The `hisn run` process (the
*supervisor*) holds the namespace fds and the control socket, and the
session ends when it exits. Wakil spawns it with `cmd.Start()`, keeps the
`*exec.Cmd` as the session's lifetime handle, and never waits for it to
return on the tool call's context:

```
hisn run --detach -v --session-id <id> --policy <file> --cwd <hostcwd> --network host --ready-fd 3
```

* **`-v` is required.** hisn prints nothing to stderr without it.
* **Stdio.** `stdin` and `stdout` are `nil` (Go maps that to `/dev/null`).
  `stderr` is a pipe teed into a 64 KiB in-memory ring and a per-chat
  supervisor log file (`<state>/hisn/<chatID>.supervisor.log`, `0600`,
  truncated at every start). The file is written by a separate goroutine
  through a bounded queue; chunks are dropped (and counted in the log) when
  the queue is full, so a stalled log destination can never wedge hisn's
  stderr or delay the publication of its death.
* **Lifetime belongs to the session, not to a tool call.** The command
  derives from `context.Background()` and is cancelled only by the
  supervisor's own teardown. The tool call's context is used for policy
  validation and readiness only. The supervisor runs under `Setsid` so a
  terminal Ctrl-C or hang-up cannot reach it (hisn treats SIGINT/SIGHUP as
  "shut down").
* **`Pdeathsig = SIGTERM`** on the supervisor (Linux only; other platforms
  compile but have no parent-death signal and rely on Stop/reconcile). The
  spawning goroutine is pinned to its OS thread (`runtime.LockOSThread`) for
  the supervisor's whole life, because the kernel delivers `Pdeathsig` when
  the *thread* that forked the child exits and the Go runtime may retire
  idle threads.
* **Readiness (`--ready-fd 3`, hisn >= 0.2.1).** The supervisor is started
  with the write end of a pipe as fd 3; Wakil closes its copy right after
  the spawn. Ready means, all at once, within a **10 s** budget
  (`readyCap`): the supervisor has not exited, the whole `"ready\n"` line
  arrived on the pipe, `hisn ls --json` (schema `hisn-ls/v1`, field
  `state`) reports **`live`**, and the same holds again one poll interval
  (`readyPoll` = 100 ms) later. A partial line, EOF, `stale`, missing, an
  error, an exited supervisor, or a timeout is *not ready*. On timeout the
  supervisor is SIGKILLed, reaped, and the error carries the last probe's
  reason, the stderr, and the log path. A half-started session never
  receives a tool call: it is published (`cur`) only after readiness *and*
  instance verification.

### Start

`Start` is idempotent for the running session. A session whose supervisor
died is replaced (stopped and started fresh), not treated as running.
Before the replacement, the active grant set is snapshotted
(`PathMap.Grants()`); after the new session starts, the grants are replayed
(rw grants re-issued through `hisn grant`, ro grants re-added to the path
map). `stopAllLocked` clears the grants, so the snapshot must happen first.

If the previous session cannot be stopped, one line is logged, the leftover
stays tracked (retried by Stop/Close), and the new session starts anyway.
A clash on the *same* id is still caught by `startLocked`.

`startLocked`, under the state lock:

1. Reconciles a record left by a dead owner; a live owner means the chat is
   active elsewhere (`ErrSessionInUse`).
2. `hisn ls --json`: if the id is present and **`live`**, refuse
   (`session id in use`) — Wakil never adopts or kills a session it did not
   create. If it is present but **`stale`** and no live Wakil owns the chat,
   it is a dead leftover of this chat id: `hisn run --session-id` (0.2.1)
   replaces it itself. One line is logged.
3. Creates the owner record with `O_EXCL`: `{pid, pid_start, token, cwd}`
   where `token` is a fresh 128-bit hex value.
4. Spawns the supervisor with `session.hostname = "wakil-<token[:16]>"`
   in the policy and waits for readiness.
5. Verifies the instance (`hisn inspect --session ID --json` → resolved
   policy contains the token hostname) and marks the record `started`. On
   any failure the supervisor Wakil spawned is killed and reaped. This is
   an exact identity (the process handle), not a lookup by id, so a foreign
   session can never be signalled by a failed start.

A failed start that left a possibly-running supervisor keeps it in the
`orphans` list for retry by Start/Stop/Close.

### Stop

`Stop` reaps the supervisor Wakil spawned **unconditionally**: it is an
exact process handle. `hisn kill --session <id>` (its own 5 s bound,
independent of the caller's context) asks the session to end, then
`cmd.Wait()` is capped at **5 s** (`stopCap`); on the cap the supervisor is
SIGKILLed and waited for again. Idempotent and tolerant of "session not
found": the supervisor having exited is what counts. Only the *bookkeeping*
is gated on the owner record (a record belonging to another instance is
left in place and reported).

After the supervisor is reaped, a `live` socket on the id is a replacement
by a foreign session: left alone and reported. A `stale` dir is removed by
`hisn kill --session <id>` itself (stale-kill: remove + exit 0).

A supervisor that cannot be reaped (uninterruptible sleep) keeps the owner
record and stays in the `orphans` list; Start/Stop/Close retry it.

### Kill safety

While Wakil holds its own unreaped supervisor, that process is the only
owner of the id, so `hisn kill --session <id>` in Stop cannot hit a foreign
session (a supervisor Wakil has reaped is never signalled again). The
token re-verification (`hisn inspect`) before each signal remains for the
*reconcile* path, where Wakil has no process handle. hisn offers no
instance-bound kill; a residual window of one CLI round trip remains there
(Known limits).

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

`TestStartReplaysGrantsOnReplacement` covers this.

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

At construction (`Open`), reconcile is a *fallback* — normally the
supervisor dies with Wakil through `Pdeathsig` and nothing is left to reap.
It still runs for the cases the kernel cannot cover (SIGKILL of a hisn
child, a non-Linux build, an upgrade from a build without the supervisor
handle).

1. `hisn ls --prune` (0.2.1): hisn itself removes every provably stale dir
   (socket refused AND supervisor pid gone or starttime-mismatched, twice
   300 ms apart; removal fd-relative, owner-checked, runtime-root confined).
   No live session is touched; no owner record is needed.
2. Under the lock: for each record whose owner process is gone (pid absent
   or start time differs) — re-read-and-compare under the lock first, so a
   session replaced by a new owner is left alone. If the id is absent from
   `hisn ls`, drop the record. If it is present and the instance token
   verifies, `hisn kill` + wait. If present but unverifiable, keep the
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

## Policy generation

A Go struct (`policyFile`) is marshalled with `github.com/BurntSushi/toml`
into `<state>/hisn/sessions/<id>.toml`, then checked with
`hisn policy validate`. Contents:

```toml
version = "hisn-policy/v1"
[session]
cwd = "<host cwd>"
home = "<home_root>"
network = "host"
hostname = "wakil-<token[:16]>"
shm = true
shm_size = 64           # sandbox.hisn.shm_size_mb (MiB, 1..65536)
tmp_size = 1024         # sandbox.hisn.tmp_size_mb (MiB, 16..65536)
[mounts]
grantable_rw = [ ... ]  # sandbox.hisn.grantable_rw
[env]
pass = ["GOFLAGS", "GOCACHE", "GOMODCACHE", "CARGO_HOME", "RUSTFLAGS",
        "npm_config_cache", "PIP_CACHE_DIR", "CI"]   # sandbox.hisn.env_pass
```

`network = "host"` has no filtering; this is the decided trade-off (builds
need the network) and is called out in Known limits. `shm = true` is always
set (Chrome, Node, POSIX shm users need `/dev/shm`).

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
    "exec_via_cli": false
  }
}
```

Flag `--sandbox docker|hisn|direct`, env `WAKIL_SANDBOX_BACKEND`. Empty
`home_root` means `<wakil state dir>/hisn-home/<workspace-hash>`. With
`hisn`: `kvr` is forced off (docker-only), `browser_enabled` is rejected,
`docker_*` keys are ignored, `WorkDir == HostWorkDir`.

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
* Browser tools unavailable (`browser_enabled` forced off).
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
* hisn's `Pdeathsig` safety net is a no-op if the `hisn` binary carries
  setuid/setgid or file capabilities (the kernel clears the death signal on
  such an execve). Check with `getcap "$(command -v hisn)"`; the startup
  Reconcile is then the only fallback after a Wakil crash.
* `Pdeathsig` covers Wakil's exit and crash, not a hisn child that is itself
  SIGKILLed independently of Wakil.
* `StartBackground` uses `setsid nohup … &` inside `hisn exec`. That the
  detached process survives its launcher exec is asserted by
  `TestIntegrationStopKillsSleeper` but there is no readiness handshake
  between launcher exit and `setsid`.
* Host<->URI mapping (LSP) is refused for paths that resolve outside the
  workspace on the host (e.g. `ws/link -> /tmp/x`), because the same name
  can be a different object inside the sandbox.
* The hisn integration tests skip when `hisn check` fails (no unprivileged
  user namespaces); they have not been run in the build sandbox. Run on a
  host with hisn >= 0.2.1:
  `go test -count=1 -v -timeout 5m -run TestIntegration ./internal/sandbox/hisn/`