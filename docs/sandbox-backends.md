# Sandbox backends

Wakil runs every tool call inside a sandbox. Two backends exist:

| backend | selected by | status |
|---|---|---|
| `docker` | default (`sandbox.backend: "docker"`) | production, behaviour unchanged |
| `hisn` | `sandbox.backend: "hisn"` | second backend, opt-in, needs hisn >= 0.2.1 |

`exec_mode: "direct"` (no sandbox at all) is a separate, explicit opt-out and is
**not** a backend. Combining it with `backend: "hisn"` is a configuration error.

This document describes the seam, the path model, the lifecycle, the grant
mapping and the configuration. Binding rules from the task are repeated under
"Invariants".

## Invariants (do not alter)

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

## Layers

```
 tool layer (agent, tools, lsp, browser, tui)
        │  exec.Executor  (+ optional: ChatBinder, SandboxResetter,
        │                    WorktreeHost, FileModer, AccessGranter)
        ▼
 internal/exec
   ├─ DockerExecutor   (unchanged; also exposes Backend() adapter)
   ├─ DirectExecutor   (unchanged)
   └─ BackendExecutor  (new: exec.Executor built on any sandbox.Backend)
        │
        ▼
 internal/sandbox            (leaf package, no wakil imports)
   Backend, Session, ExecSpec, ExecHandle, errors, PathMapper, GrantManager
        ▲
        │ implements
 internal/sandbox/hisn       (new: drives the `hisn` CLI)
```

Why `exec.Executor` stays the tool-layer seam: it is already what ~30 call
sites and every test fake use. `sandbox.Backend` is the lower, narrower
primitive (start/exec/grant/revoke/stop/map-path); `BackendExecutor` turns any
Backend into an Executor by running the same shell snippets `DockerExecutor`
uses. A future backend implements Backend only.

### Docker is adapted, not moved

The task asks for the Docker code to move behind the interface with identical
behaviour. `DockerExecutor` is 1.7k lines of Docker-specific startup (kvr,
CDP port, signing, hardening flags) with ~250 in-package tests. Wrapping it in
the generic `BackendExecutor` would change observable behaviour (error text,
exit semantics, kvr/browser wiring), which violates invariant 1. So:

* `DockerExecutor` keeps being the Executor for backend `docker`.
* `(*DockerExecutor).Backend()` (`internal/exec/docker_backend.go`) returns a
  `sandbox.Backend` adapter over it (Exec → `docker exec`, Stop → `Close`,
  MapPath → `/mnt/<name>` ↔ host mount). Grant/Revoke return
  `sandbox.ErrNotSupported` (docker mounts are fixed at `docker run`). The tool
  layer does not use the adapter; it exists so docker satisfies the same
  contract and so the `/mnt` convention lives in one place
  (`sandbox.DockerAgentRoot`).
* The docker-specific `switch a.Exec.(type)` / `isDockerExecutor` sites in the
  agent are replaced by optional interfaces (below), implemented by Docker with
  identical results.

## Path model

| | docker | hisn |
|---|---|---|
| agent root (`Executor.Cwd()`) | `/mnt/<basename(host)>` | the host cwd, identical path |
| `MapPath(agent)` | agent root → host mount | identity (must be absolute; cwd subtree or an active grant) |
| `UnmapPath(host)` | host mount → agent root | identity |
| sandbox temp dir | container `/tmp` | session `/tmp` (private tmpfs) |
| `$HOME` | in-container sandbox-home bind | `/run/hisn/home` (persistent host dir `home_root`) |

The only place the `/mnt/` convention is encoded is `sandbox.DockerAgentRoot`
(used by `config`). Audit result (non-test, `grep '/mnt/'`):
`config.go:1430/1433` (moved), `proxy/client.go:447` and `wiring/bootstrap.go:99`
(comments only). Hard-coded `/tmp` in `worktree.go` (`mktemp -p /tmp`,
`isWorktreePath`) becomes the backend's `SandboxTempDir()`.

`HostPathToURI`/`URIToHostPath` (LSP) are implemented via the same mapper; for
hisn they are the identity inside the cwd subtree and an error outside it,
exactly like docker's "outside workspace" error.

### Execution paths vs host-representable paths

Two different questions must never be conflated:

* **Execution path**: a path the *sandbox* can resolve (cwd subtree, active
  grants, the session's private `/tmp`, `$HOME`). Valid as `ExecSpec.Cwd`.
* **Host path**: a path Wakil itself can open with `os.*`.

For docker, container-private paths (`/tmp/...`) are obviously not host paths.
For hisn they are *not* obvious: session `/tmp/x` and host `/tmp/x` are
different objects with the same name. Therefore:

* `MapPath(agentPath)` returns a host path **only** for the cwd subtree and
  active grants; for anything else (including `/tmp`) it returns
  `sandbox.ErrNoHostPath`. It never identity-maps `/tmp`.
* `Backend.Exec` accepts any execution path as `Cwd` (`ResolveExecCwd`):
  host-representable paths are checked against the mapper, private paths
  (`SandboxTempDir()` prefix) are passed through untouched.
* `exec.WorktreeHost` reports `WorktreeKind()`: `Host` (direct), `SandboxTmp`
  (docker, hisn). For `SandboxTmp` every worktree operation (mktemp, `git
  worktree add`, patch apply, cleanup, `isWorktreePath`) goes through
  `RunShell` and never `os.*`; `isWorktreePath` does a pure string-prefix
  match in the sandbox namespace and does not `EvalSymlinks` on the host.
  Wakil's own host temp files (`os.CreateTemp`) are never classified as
  worktrees because the classification takes the executor kind, not the path.
* `WorktreeHost.IsWorktreeChild()` is true for the re-rooted child executor,
  so nested dispatch stays refused exactly like the docker wrapper today.
* LSP `HostPathToURI`/`URIToHostPath` use `MapPath`/`UnmapPath`; private paths
  error.
* Background logs live in sandbox `/tmp` and are only ever read through
  `ReadFileTail`/`StatFile` (executor calls), as in docker mode.

Tests: a table test where the same pathname (`/tmp/wakil-wt-x`) holds
different content on the host and in the fake session; `MapPath` rejects it,
the fake backend's `Exec` accepts it, and the worktree helpers never touch the
host copy.

### Where a tool call could escape the sandbox or use the wrong path model

This is the audit the first review round is asked to challenge.

1. **Host-side children of Wakil** run on the host regardless of backend:
   hooks (`hooks.go`), MCP servers (`mcp_manager.go`), `open_url`, clipboard.
   Unchanged from docker mode; documented, not routed through the sandbox.
2. **Browser**: `browser.NewManager` picks the in-container Chromium only when
   `ContainerName() != ""`; otherwise it launches Chromium **on the host**.
   For hisn `ContainerName()` is `""`, so a naive build would silently run the
   browser unsandboxed. Decision: with backend `hisn`, `browser_enabled` is
   refused in config validation **and** in `NewManager` (a non-empty `ContainerName()` is the only route to the in-sandbox browser; any other executor kind that is not `direct` returns an error). No runtime enable path exists besides config. Chromium inside hisn is
   follow-up work.
3. **Worktree isolation** (edit subagents) created temp dirs on host or in
   container `/tmp` depending on a type switch. For hisn the worktree lives in
   the session's `/tmp` (sandbox-internal) and the child executor re-roots via
   `hisn exec --cwd`. Host `os.*` calls on such paths would be wrong; the
   `WorktreeHost.WorktreeKind()` answer (`SandboxTmp`) routes every one of those
   sites to `RunShell`.
4. **Path confinement** (`ConfinePath`) runs `readlink -f` inside the sandbox
   and compares against the agent root, as docker does. The hisn agent root is
   symlink-resolved once at config time because hisn canonicalises `cwd`.
5. **Toolcache spill reads** (`IsToolCacheHostPath`) are served from the host
   by design (never part of the workspace). Unchanged.
6. **`.secrets/`** sits inside cwd and is therefore visible to every exec
   without a grant. hisn's own integration doc suggests masking it; this task
   does not add a mask (non-goal), so it is documented in the backend README
   (`docs/sandbox-backends.md`, "Known limits").
7. **Env**: `hisn exec --env K=V` is accepted only for keys in `env.pass`; the
   default `env_pass` lists the cache variables for completeness, but hisn
   always overrides them with sandbox-local values and rejects them in
   `exec --env`, so Wakil refuses computed names (`HOME`, `XDG_*`, `CARGO_HOME`,
   `GOMODCACHE`, `GOCACHE`, `npm_config_cache`, `PIP_CACHE_DIR`) up front. The
   tool layer passes no env today, so execs inherit only the session base env.
8. **Wakil's own `hisn` processes** get a scrubbed env: `PATH`,
   `XDG_RUNTIME_DIR` (needed to find the session dir), `HOME`, `LANG`, `LC_*`,
   `TZ`, `TERM`, plus the configured `env_pass` names if set. Nothing else is
   inherited, so API keys in Wakil's environment never reach `env.pass`.
9. **Lost exec** (`hisn exec` exit 125 without a `hisn:` diagnostic): the
   program may still run. Recorded on the executor; the result carries an
   explicit message; the session is stopped (killing it) at chat end.
10. **Session ownership**: orphan reconcile only touches sessions that carry a
    Wakil owner record whose owning process is gone. Foreign sessions of the
    same uid are never killed.

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
type Session struct{ ID, HostCwd string }          // ID = chat ID
type ExecSpec struct {
    Argv   []string
    Cwd    string            // AGENT path; the backend maps it
    Env    map[string]string // allowlisted only
    Stdin  io.Reader; Stdout, Stderr io.Writer      // never a tty
}
type ExecHandle interface {
    Wait() (ExitInfo, error)  // ExitInfo{Code int, Lost bool}
    Kill() error
}
```

`ctx` cancellation: SIGTERM to the local `hisn exec` client (hisn then TERMs the
program group and KILLs after 5 s). Backends must be safe for concurrent
`Exec` calls.

## Lifecycle (hisn)

* **Preflight** in the constructor: `hisn --version` parses and is >= 0.2.1,
  and `hisn check` exits 0. Otherwise `NewExecutor` returns an error and Wakil
  exits. No fallback. Unknown `sandbox.backend` values are a config error.
* **One command factory.** Every `hisn` invocation (version, check, policy
  validate, run, exec, grant, revoke, ls, kill, inspect) is built by one
  function that sets a scrubbed environment: `PATH`, `HOME`, `XDG_RUNTIME_DIR`
  (to find the session dir), `LANG`, `LC_*`, `TZ`, `TERM`. Only `hisn run`
  additionally receives the `env_pass` names (hisn copies them from the client
  environment at session start). `env_pass` entries that can alter host
  process startup (`LD_*`, `GCONV_PATH`, `LOCPATH`, `NLSPATH`, `HOSTALIASES`,
  `RES_OPTIONS`, `GODEBUG`, `BASH_ENV`, `ENV`, `PATH`, `HOME`) are rejected at
  config validation. A test greps the package for `exec.Command` outside the
  factory.
* **One session per chat**, `session-id = chatID` (UUID, valid for hisn's
  `[A-Za-z0-9._-]{1,64}`). Subagents share the parent executor.
* **The supervisor IS the session.** `hisn run --detach` does **not** return:
  `--detach` only means "no main command; PID 1 sleeps until stopped". The
  `hisn run` process (the *supervisor*) holds the namespace fds and the control
  socket, and the session ends when it exits. Wakil therefore spawns it with
  `cmd.Start()` (never `Run`/`CombinedOutput`), keeps the `*exec.Cmd` in the
  session record as the session's lifetime handle, and never waits for it to
  return:

  ```
  hisn run --detach -v --session-id <id> --policy <file> --cwd <hostcwd> --network host
  ```

  * **`-v` is required.** hisn prints nothing to stderr without it, so a start
    that fails would be undiagnosable.
  * **Stdio.** `stdin` and `stdout` are `nil` (Go maps that to `/dev/null`): hisn
    refuses to start when stdin or stderr is a terminal, and no descriptor of
    Wakil's TUI may reach it. `stderr` is a pipe teed into two places: a bounded
    in-memory ring (last 64 KiB) and the **supervisor log**
    `<wakil state dir>/hisn/<chatID>.supervisor.log` (`0600`, truncated at every
    start, written on every start, successful or not; it starts with the exact
    command line and ends with a `wakil: supervisor exited: ...` line). The file is
    written by a separate goroutine through a bounded queue and chunks are
    dropped (and counted in the log) when it stalls, so a hung log destination can
    never wedge hisn's stderr nor delay the publication of its death. Every
    start, readiness and dead-session error names that path and appends the ring.
    If the log file cannot be created, Start fails: diagnostics are not optional.
  * **Lifetime belongs to the session, not to a tool call.** The command is built
    on a context derived from `context.Background()` and cancelled only by the
    supervisor's own teardown (kill/reap). The context of the tool call that
    triggered the lazy start is used for the policy validation and the readiness
    wait, and never reaches the supervisor, so cancelling that call (Esc, a
    timeout) cannot kill the session. The supervisor also runs in its own session
    (`Setsid`): a terminal Ctrl-C or hang-up that reaches Wakil's process group
    must not reach it, because hisn treats SIGINT/SIGHUP as "shut down".
  * **`Pdeathsig = SIGTERM`** on the supervisor, so a Wakil crash (or any exit
    that skips `Close`) makes the kernel TERM it; hisn then shuts the whole
    sandbox down. The spawning goroutine is pinned to its OS thread for the
    supervisor's lifetime, because the kernel delivers `Pdeathsig` when the
    *thread* that forked the child exits and the Go runtime may retire idle
    threads: the same goroutine starts the supervisor and blocks in `cmd.Wait`
    for its whole life. Linux only (`sysproc_linux.go`); other platforms compile
    but have no parent-death signal and rely on Stop/reconcile.
  * **Readiness is strict.** Polled every 100 ms for at most **10 s**; ready means,
    all at once: the supervisor has not exited, `hisn ls --json` reports the
    session **`live`**, and `hisn ls --session <id>` exits 0, and the same holds
    again one poll interval later. `stale`, missing, a non-zero exit or an exited
    supervisor is *not ready*. (A supervisor that creates its socket and dies
    right after used to pass; the second probe and the exit check close that.) A
    supervisor that exits first fails Start with its exit status and stderr; on
    timeout it is SIGKILLed, reaped, and the error carries the last probe's
    reason, the stderr and the log path. There is no silent success: a
    half-started session never receives a tool call, because it is only
    published (`cur`) after readiness *and* instance verification.
* **Stop** reaps the supervisor Wakil spawned **unconditionally**: it is an exact
  process handle, so no record or id lookup gates it. `hisn kill --session <id>`
  (its own 5 s bound, independent of the caller's context, so a hung client or
  an already-cancelled ctx cannot stall shutdown) asks the session to end, then
  `cmd.Wait()` is capped at **5 s**; on the cap the supervisor is SIGKILLed and
  waited for again (its init dies through its own `PDEATHSIG`). Idempotent and
  tolerant of "session not found": the supervisor having exited is what counts,
  not the CLI's answer. Only the *bookkeeping* is gated on the owner record
  (a record belonging to another instance is left in place and reported).
  A SIGKILLed supervisor cannot remove its runtime directory, which would make
  every later `hisn run` with that id fail with "exists". Wakil removes it only
  with **proof of ownership**: the directory is not a symlink, its
  `policy.resolved.toml` (which hisn writes before binding the socket) carries
  this instance's random hostname token, and nothing answers on its socket.
  A foreign directory, or one without that proof (supervisor died before writing
  the policy), is never touched. A supervisor that cannot be reaped
  (uninterruptible sleep) keeps the owner record and stays in the `orphans` list;
  `Start`/`Stop`/`Close` retry it.
* **Health.** A session is dead when its supervisor has exited **or** a hisn
  client reported `Connection refused` on its control socket (a supervisor that
  is alive but no longer accepting). The text alone never decides: an exec
  client's stderr also carries the *program's* stderr, so on `exit 125` plus a
  refusal-looking line Wakil probes the session itself (`hisn ls --session`,
  `hisn ls --json`) and only a failed probe marks the session dead. `Exec`, `Grant` and `Revoke` on a session
  already known dead return `sandbox.ErrSessionDead` (exit status, captured
  stderr, log path) *before* any `hisn` client is spawned; a refused connection
  discovered by a client marks the session dead and fails that call with the same
  error (the program did not run). The executor drops the dead
  session (generation advances, so bg-registry entries are marked lost) and the
  chat's next tool call starts a fresh one, exactly as for a missing session;
  the failing call itself is not retried (it may not be idempotent).
  `Start` for the same id replaces a dead session instead of treating it as
  running. A tool call is **never** failed with "previous session not stopped":
  if the previous session cannot be stopped, one line is logged, the leftover
  stays tracked (retried by `Stop`/`Close`), and the new session starts anyway
  (a clash on the *same* id is still caught by the start steps below).
* **Ownership is exclusive and instance-bound.** The records directory
  `<state>/hisn/sessions/` is guarded by an `flock`ed lock file for every
  start, stop and reconcile step. A start:
  1. under the lock, `hisn ls --json`: if the id is present and **`live`**,
     refuse (`session id in use`) — Wakil never adopts, claims or kills a
     session it did not just create. If it is present but **`stale`** (the
     directory is there, nothing answers) and no live Wakil owns the chat (step
     2), it is a dead leftover of this very chat id: it is removed and one line
     is logged (stderr and the supervisor log), then the start proceeds. The
     removal needs the directory to be named exactly `<id>` under hisn's runtime
     roots, not a symlink, to refuse connections twice 300 ms apart, and, when
     hisn has already written its `policy.resolved.toml`, that policy must carry a
     `wakil-` instance hostname (a stale directory of a session somebody else
     started under this id is left alone and the start fails with
     `ErrSessionInUse`). Without a policy file nothing contradicts ownership and
     the directory is replaced: the chat id is a UUID only Wakil uses, and the
     300 ms settle is a heuristic, not proof (Known limits). Other ids are never
     touched;
  2. if a record `<id>.json` exists with a live owner (pid + start time), refuse
     (the chat is active in another Wakil process); a dead owner's record is
     reconciled (below) first;
  3. create the record with `O_EXCL`: `{pid, pid_start, token, cwd}` where
     `token` is a fresh random 128-bit value;
  4. spawn the supervisor (above) with `session.hostname = "wakil-<token[:16]>"`
     in the policy and wait for readiness;
  5. verify the instance (`hisn inspect --session ID` → resolved policy
     hostname equals the token hostname) and mark the record `started`; on any
     failure the supervisor *Wakil spawned* is killed and reaped. This is an
     exact identity (the process handle), not a lookup by id, so a foreign
     session can never be signalled by a failed start.
* **Lifecycle vs execution.** `BackendExecutor.life` (RWMutex) serialises
  transitions with *admission*. Every call captures a `(chat, generation)`
  scope at entry; `admit` re-checks it under the lock and returns
  `errRebound` instead of retrying against whichever session is current, so a
  call that began under chat A can never start or run inside chat B's session
  (multi-step operations such as `MovePath` pin one scope for all their steps).
  Execs hold the lock only while being handed to the backend. The hisn
  `Backend` additionally binds every exec/grant/revoke to its `live` session
  instance (context cancelled when the session is replaced or stopped; a
  grant that finishes after replacement does not touch the new path map).
  `Generation()` advances on every replacement (bind to another chat, reset,
  close). Worktree child executors share the session but can never bind,
  reset or close it. Holding `life` while `Backend.Start` runs is safe: the
  backend never calls back into the executor.
* **Failed starts keep their cleanup obligation.** If the supervisor was
  spawned but could not be reaped, the session is tracked in the backend's
  `orphans` list and retried by `Start`/`Stop`/`Close`; its owner record is
  kept until the supervisor is verifiably gone.
* **Kill safety.** While Wakil holds its own unreaped supervisor, that process
  is the only owner of the id, so `hisn kill --session <id>` in Stop cannot hit
  a foreign session (a supervisor Wakil has reaped is never signalled again).
  The token re-verification (`hisn inspect`) before each signal remains for the
  *reconcile* path, where Wakil has no process handle. hisn offers no
  instance-bound kill; a residual window of one CLI round trip remains there
  (Known limits).
* **Bind** (`exec.ChatBinder`): called from the one place the chat ID changes
  (`App.setChatID`, replacing the five `Client.ChatID =` writes). Binding the
  same ID is a no-op; binding a different ID stops the current session and
  starts lazily on the next exec. Handoff therefore needs no record hand-over:
  the old session is stopped (record removed) and the new chat starts its own.
* **Reset** (`/rewind`): after a fully successful rewind the session is
  stopped and the generation advances so bg-registry entries are marked
  "process lost"; the next call restarts lazily for the same chat once the id
  has disappeared from `hisn ls`. A refused rewind does not reset anything.
* **Reconcile** at construction is now a *fallback*: normally the supervisor
  dies with Wakil through `Pdeathsig` and nothing is left to reap. It still
  runs for the cases the kernel cannot cover (SIGKILL of a hisn child, a
  non-Linux build, an upgrade from a build without the supervisor handle). Under
  the lock: for each record whose owner
  process is gone (pid absent or start time differs): if `hisn ls` lists the id
  and the instance token verifies, `hisn kill` + wait; if the id is absent,
  just drop the record; if it is present but unverifiable, keep the record and
  log. Re-read-and-compare under the lock prevents killing a session that a
  new owner replaced. Sessions without a record are never touched. Errors are
  logged, never fatal.
* **Background and interactive processes.** All pid-based operations
  (`StartBackground`, `KillPgid`, `IsProcessAlive`, `IsProcessGroupAlive`)
  run *inside the session* through `hisn exec` with the same shell snippets
  docker uses, so pids and pgids are always sandbox-namespace values.
  `StartBackground` runs `setsid nohup sh -c … & echo $!`; the launcher exec
  exits immediately and the setsid'ed child is no longer the exec's program,
  so cancelling/closing the launcher client does not kill it (to be confirmed
  by the integration test "background survives launcher exit"); it dies with
  the session. `StartInteractive` always returns pid **0**: the only pid Wakil has is the
  host-side `hisn exec` client's, and every pid-based call
  (`IsProcessAlive`/`KillPgid`) runs in the sandbox pid namespace where that
  number could name an unrelated process. Callers treat 0 as "no process guard".
  The pipes are real OS pipes (not `io.Pipe`), so reaping the client never
  depends on the caller draining stdout or closing stdin. `ExecHandle.Kill` cancels the local client (hisn TERMs, then KILLs
  after 5 s).
* **Lost exec**: `hisn exec` exit 125 *without* a `hisn:` diagnostic line on
  stderr (v0.2.0 prints none for a lost exec). A program that itself exits 125
  silently is indistinguishable, so it is treated as possibly-lost: the result
  says so, the executor records it, and the session is stopped at chat end.

## Policy generation

A Go struct is marshalled with `github.com/BurntSushi/toml` into
`<state>/hisn/sessions/<id>.toml`, then checked with
`hisn policy validate`. Contents:

```toml
version = "hisn-policy/v1"
[session]
cwd = "<host cwd>"      # rw, identical path
home = "<home_root>"    # persistent, created 0700
network = "host"
[mounts]
grantable_rw = [ ... ]  # sandbox.hisn.grantable_rw; each must exist at start
[env]
pass = [GOFLAGS, GOCACHE, GOMODCACHE, CARGO_HOME, RUSTFLAGS,
        npm_config_cache, PIP_CACHE_DIR, CI]   # sandbox.hisn.env_pass
```

`network = "host"` has no filtering; this is the decided trade-off (builds need
the network) and is called out in the README of the backend.

## Grants (tier mapping)

`sandbox.GrantManager` wraps a Backend and the cwd:

| request | result |
|---|---|
| path inside cwd | nothing to do |
| read tier, outside cwd | `Grant(path, rw=false)` |
| edit tier, outside cwd | `Grant(path, rw=true)` only if under a `grantable_rw` prefix, otherwise `ErrNotGrantable` is returned to the user; no retry, no widening |
| withdrawal | `Revoke(path)` — **soft** (per the task). hisn's own guidance is `--hard` whenever revocation is a security decision; soft only guarantees the path stops resolving, not that open fds die. This residual risk is documented in "Known limits". |

Today the file tools confine every path to the workspace, so nothing in the
tool layer requests outside-cwd access yet. The manager is exposed on the
executor as the optional `exec.AccessGranter` interface so a future
`/grant`-style feature (or tools-tier MCP data dirs) has the seam; this change
adds no new user-facing command and **no caller of `EnsureAccess` exists yet**
(it is covered by unit tests with a fake backend and by the real-hisn
integration test).

## Config

```json
"sandbox": {
  "backend": "docker",
  "hisn": {
    "binary": "hisn",
    "home_root": "",
    "grantable_rw": [],
    "env_pass": ["GOFLAGS","GOCACHE","GOMODCACHE","CARGO_HOME","RUSTFLAGS",
                 "npm_config_cache","PIP_CACHE_DIR","CI"]
  }
}
```

Flag `--sandbox docker|hisn`, env `WAKIL_SANDBOX_BACKEND`. Empty `home_root`
means `<wakil state dir>/hisn-home/<workspace-hash>`. With `hisn`: `kvr` is
forced off (docker-only), `browser_enabled` is rejected, `docker_*` keys are
ignored, `WorkDir == HostWorkDir`.

## Known limits (hisn backend)

* `.secrets/` and every other file in cwd is readable by all execs.
* `network = "host"`: no network isolation.
* Soft revoke only (see Grants).
* Toolchains installed under the host `$HOME` (rustup, nvm) are not mounted;
  hisn's `mounts.ro` is not exposed by this change.
* Browser tools unavailable.
* Worktree dirs live in the session `/tmp` tmpfs; very large repos can hit
  hisn's fixed tmpfs size.
* Instance verification relies on `session.hostname` carrying a random token
  and on `hisn inspect --json` returning the resolved policy as a TOML string
  (checked against hisn v0.2.0 source and `hisn policy validate` output, not
  against a running session on this build host). A session whose policy
  carries no hostname is treated as *unverifiable* (record kept, error
  reported), never as foreign.
* The reconcile-path kill re-verification window (one CLI round trip) cannot
  be closed without an instance-bound kill in hisn.
* Reconcile does not remove a non-live runtime directory it finds by id: with
  no supervisor handle it cannot tell a crashed one from a foreign session that
  is still starting. Such a directory (left by a SIGKILLed supervisor of a
  *previous* Wakil process) is only cleaned when that same chat id starts again
  (the stale replacement above); other stale entries in `hisn ls` stay until
  removed (`rm -r $XDG_RUNTIME_DIR/hisn/<id>`). Stop removes its own (with proof).
* hisn's `Pdeathsig` safety net is a no-op if the `hisn` binary carries
  setuid/setgid or file capabilities (the kernel clears the death signal on such
  an execve). Check with `getcap "$(command -v hisn)"`; the startup Reconcile
  is then the only fallback after a Wakil crash.
* `hisn kill --session <id>` after the exit check has a window of one CLI round
  trip in which a foreign process could acquire the id (requires the id of a
  UUID chat to be reused); hisn offers no instance-bound kill.
* That `hisn ls --session` and the supervisor's SIGTERM handling behave as
  described is taken from the hisn v0.2.0 source (`src/main.rs`,
  `src/supervisor.rs`); `TestIntegrationStartExec` and
  `TestIntegrationSupervisorDiesWithParent` are the on-host proof and have not
  run on this build host.
* The stale-directory replacement is not a proof of ownership when hisn died
  before writing `policy.resolved.toml`: a *foreign* supervisor that took longer
  than the 300 ms settle to bind its socket under the *same* chat id would lose
  its directory. Chat ids are UUIDs that only Wakil generates, which is the
  mitigation; the design has no stronger evidence to offer.
* `ErrSessionDead` comes from the supervisor process exiting or from a client
  reporting `Connection refused` (matched on that text in hisn's `hisn: ...`
  diagnostic). A supervisor that is alive, accepting, but wedged is not noticed
  until an exec fails on its own; there is no periodic health probe between tool
  calls.
* `Pdeathsig` covers Wakil's exit and crash, not a hisn child that is itself
  SIGKILLed independently of Wakil.
* `StartBackground` uses `setsid nohup … &` inside `hisn exec`. That the
  detached process survives its launcher exec is asserted by
  `TestIntegrationStopKillsSleeper` (once) but has **not been run**, and there
  is no readiness handshake between launcher exit and `setsid`.
* Host<->URI mapping (LSP) is refused for paths that resolve outside the
  workspace on the host (e.g. `ws/link -> /tmp/x`), because the same name can
  be a different object inside the sandbox.
* The hisn integration tests skip when `hisn check` fails (no user namespaces);
  they have not been run in the build sandbox (see the final report for the
  host commands).
