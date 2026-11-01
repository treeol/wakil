# Sandbox backends

Wakil runs every tool call inside a sandbox. Two backends exist:

| backend | selected by | status |
|---|---|---|
| `docker` | default (`sandbox.backend: "docker"`) | production, behaviour unchanged |
| `hisn` | `sandbox.backend: "hisn"` | second backend, opt-in, needs hisn >= 0.2.0 |

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
* `(*DockerExecutor).Backend()` returns a `sandbox.Backend` adapter over it
  (Exec → `docker exec`, Stop → `Close`, MapPath → `/mnt/<name>` ↔ host mount).
  Grant/Revoke return `sandbox.ErrNotSupported` (docker mounts are fixed at
  `docker run`).
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

* **Preflight** in the constructor: `hisn --version` parses and is >= 0.2.0,
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
* **Ownership is exclusive and instance-bound.** The records directory
  `<state>/hisn/sessions/` is guarded by an `flock`ed lock file for every
  start, stop and reconcile step. A start:
  1. under the lock, `hisn ls --json`: if the id is already present, refuse
     (`session id in use`) — Wakil never adopts, claims or kills a session it
     did not just create;
  2. if a record `<id>.json` exists with a live owner (pid + start time), refuse
     (the chat is active in another Wakil process); a dead owner's record is
     reconciled (below) first;
  3. create the record with `O_EXCL`: `{pid, pid_start, token, cwd}` where
     `token` is a fresh random 128-bit value;
  4. run `hisn run --detach` with `session.hostname = "wakil-<token[:16]>"`
     in the policy;
  5. on success, verify the instance (`hisn inspect --session ID` → resolved
     policy hostname equals the token hostname) and mark the record
     `started`; on any failure, `hisn kill` is attempted **only if the
     post-failure `hisn ls` shows the id and the verified hostname matches**,
     then the record is removed. A pre-existing foreign session can never
     match the token.
* **Stop** (rotation, `/rewind`, `Close`) takes the lock, checks record
  `pid == own pid` and `token`, verifies the live instance via the hostname
  token, runs `hisn kill`, waits until `hisn ls` no longer lists the id
  (bounded, 5 s), then removes the record. A failed or unverified kill keeps
  the record (recovery information) and returns the error; the id is not
  reused until the session is gone.
* **Lifecycle vs execution.** A `sync.RWMutex` + epoch protects the session:
  `Exec`/`StartBackground`/`StartInteractive` take the read lock to *admit*
  and capture the current session handle; `Bind`/`Reset`/`Close` take the write
  lock, cancel the handle's context (which cancels every in-flight `hisn exec`
  client of that epoch), wait for them (bounded), then stop the session and
  advance the epoch. `Generation()` returns the epoch and is advanced on
  **every** replacement (bind to a different chat, reset, close). A stale
  call that arrives after the transition either sees "no session" and fails,
  or — only if the executor is still bound — lazily starts the session for
  the *currently bound* chat; it can never target a previous epoch's handle.
  Turn admission is serialised against these transitions by the existing
  `TransitionCoordinator` (`/new`, `/resume`, handoff) and by `rewind`'s
  "refuse while a turn is active" guard.
* **Bind** (`exec.ChatBinder`): called from the one place the chat ID changes
  (`App.setChatID`, replacing the five `Client.ChatID =` writes). Binding the
  same ID is a no-op; binding a different ID stops the current session and
  starts lazily on the next exec. Handoff therefore needs no record hand-over:
  the old session is stopped (record removed) and the new chat starts its own.
* **Reset** (`/rewind`): same as a bind to the same ID with a forced stop; the
  epoch advances so bg-registry entries are marked "process lost". The restart
  waits for the id to disappear from `hisn ls` first.
* **Reconcile** at construction, under the lock: for each record whose owner
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
  the session. `StartInteractive` returns the **host** pid of the local
  `hisn exec` client, with the same meaning as docker's host `docker exec`
  client pid; callers never mix it with in-sandbox pid calls (LSP only logs
  it). `ExecHandle.Kill` cancels the local client (hisn TERMs, then KILLs
  after 5 s).
* **Lost exec**: `hisn exec` exit 125 *without* a `hisn:` diagnostic line on
  stderr (v0.2.0 prints none for a lost exec). A program that itself exits 125
  silently is indistinguishable, so it is treated as possibly-lost: the result
  says so, the executor records it, and the session is stopped at chat end.

## Policy generation

A Go struct is marshalled with `github.com/BurntSushi/toml` into
`<state>/hisn/sessions/<id>/policy.toml`, then checked with
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
tool layer requests outside-cwd access yet. The manager is wired onto the
executor as the optional `exec.AccessGranter` interface so a future
`/grant`-style feature (or tools-tier MCP data dirs) has the seam; this change
adds no new user-facing command.

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
* The hisn integration tests skip when `hisn check` fails (no user namespaces);
  they have not been run in the build sandbox.
