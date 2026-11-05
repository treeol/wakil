# Wakil 0.5.0 — project identity, gc, locale, browser reaping, hisn 0.3.2 parity

Version target: 0.5.0. No tag, no push, no release. Built on v0.4.2 (`4b2d77d`).
hisn surface is fixed at 0.3.2 (`[home] persist`, `[rootfs] name|path`, `[ssh] agent`
+ `keys`, `mounts.ro/rw`, `shm`, `[[services]]`, `[[host_services]]`, `[[forward]]`).
No hisn repo changes. The docker backend is unchanged. Scope excludes
`internal/sandbox/hisn` profile *generation logic beyond the keys above* only where
stated below — see per-item file lists.

## Round 1 revision log (before any implementation)

Round 1 (gpt-6-astra, claude-fable-5.1; both: revise before implementation) found the B1
migration premise wrong. Verified here:
- `workspaceKey("/home/valon/Coding/wakil")` = `5d8efbe880d17149`; `MemoryDBPath` uses that
  same key. So `memory/5d8efbe880d17149` is the canonical store. The raw SHA-256 of
  `~/coding/wakil` is `7aefd10601dfa3b0`, which matches neither host directory.
- The second host ID `882223611950f186` is therefore unexplained. The plan's `legacyID`
  (raw hash) is not shown to match any directory. The migration as written is NOT
  established and is withdrawn until the host's directory listing is checked.
- Consequence: B1 migration and `state merge` are re-scoped (see B1 revised) — no rename
  or merge ships in this session until the two IDs are identified from the host.

## Behaviour contracts (DO NOT ALTER)

1. One project-ID function, computed from the symlink-resolved path, used by every store. The state merge never deletes or overwrites: sources are archived.
2. `plan gc` errors on a missing store and prints the store it opened.
3. User commands keep the user's locale; only internal helper commands get `LC_ALL=C`.
4. Every browser launch has a reaper on all exit paths.
5. Generated profiles use only the hisn 0.3.2 keys above.
6. ssh signing and the agent proxy are opt-in (default off); no key material is written into the repo or the session.
7. The docker backend keeps working unchanged.
8. No tag, no push, no release.

---

## Findings from Phase 0 (verified in source)

- **B1 cause.** Four identities for one workspace:
  - `workspaceKey` (`agent/workspace.go`) = SHA-256 of the **symlink-resolved** path. Used by repo-state, session scoping, staging, memory (`MemoryDBPath` → `memory/<key16>/memory.db`), sessionhistory, sessionhost (`sessionhost/<key16>/`), and `hisn-home` (fallback). Resolved — **correct**.
  - `wiring.WorkspaceIDFromConfig` (`wiring/conversation_manager.go:491`) = SHA-256 of the **raw** `cfg.WorkspacePath()` → sessionhost workspace ID `wsp_…`. **Not resolved.** This is one of the two identities split on the host.
  - `WAKIL_WORKSPACE_KEY` env override bypasses both (daemon path).
  - Result: `~/coding` → `~/Coding` symlink produces two keys for the same repo. The `hisn-home` fallback (`wiring/hisn_executor.go:40`) uses the resolved key, so it is consistent; the raw-path hash is in `WorkspaceIDFromConfig` and anywhere `cfg.WorkspacePath()` is hashed directly.
  - **Action:** one function `agent.ProjectID(ws) string` (= `workspaceKey`), used by `WorkspaceIDFromConfig`, staging, memory, sessionhistory, sessionhost, hisn-home. Delete the raw hash.
  - Containment (`memory/store.go:446`) already resolves both sides; `tools/toolcap.go:213`, `exec/exec_ops.go:214` also resolve. The git_blame failure (`path … is outside workspace`) comes from an unresolved executor root compared against a resolved candidate — located in `exec/exec.go:1550` / `exec/exec_ops.go:167`, see the B1 Changes section.
- **B2 cause (confirmed).** `cmd/wakil/plan_gc.go:77` opens `agent.MemoryDBPath(cfg.WorkDir)`. The session stores under `app.SessionWorkspace()` = `Cfg.WorkspacePath()` = `HostWorkDir` under hisn, `WorkDir` otherwise — two different strings for the same project. `memory.Open` calls `MkdirAll` and creates an empty DB, so the wrong path silently yields zero rows and no error.
- **B3 cause (confirmed).** `exec/exec_ops.go:256 isShellNotFound` and `exec/backend_executor.go:386 fileErr` match English stderr only. `BackendExecutor.sh` (`:345`) runs helper scripts with no locale override.
- **B4 cause (confirmed).** `browser/manager.go` launches chromedp with `NewExecAllocator` and cancels contexts on `Close`. Nothing kills the chromium process group; the docker path only `pkill`s by pattern. Under hisn `browser_enabled` is forced off (`wiring/hisn_executor.go:26`).
- **Current home mount.** `wiring/hisn_executor.go:34-41` passes `HomeRoot` = `cfg.Sandbox.Hisn.HomeRoot` or `<state>/hisn-home/<key>` to `hisn.Options`, which the backend uses as `Home` in `policyParams` (`sandbox/hisn/session.go:245`). The mount is therefore produced by Wakil's own policy writer today; P1 moves it to hisn's `[home] persist` and removes the Wakil-side mount.

---

## ITEM B1 — Project identity + state merge (REVISED after round 1)

Shipped in this session: the executor root fix (below) and `ProjectID` documented as `workspaceKey`. NOT shipped: the rename migration and `state merge` — they stay as open points until the host's two IDs are identified (`ls ~/.local/share/wakil/*/` and the hash of each candidate path). Round-1 requirements recorded for when they are built: no-clobber archives under `stagingDataRoot()` at 0700, plain INSERT with an id-remap table, merge marker inside the transaction, rename ENOENT treated as already-migrated, skip migration when `WAKIL_WORKSPACE_KEY` is set, backup of WAL state before moves, memory collisions decided by `updated_at`.


### Changes
- `internal/agent/workspace.go`: export `ProjectID(ws string) string` = `workspaceKey` (symlink-resolved SHA-256; the `WAKIL_WORKSPACE_KEY` override stays). `WorkspaceKey` becomes an alias.
- `internal/wiring/conversation_manager.go`: `WorkspaceIDFromConfig` returns `"wsp_" + ProjectID(ws)[:16]` (the same 16-hex prefix length the stores use), so the sessionhost workspace ID agrees with every other store.
- `hisn_executor.go`: fallback home uses `agent.ProjectID` (already resolved; keep).
- **Migration** (`internal/agent/project_migrate.go`, run at startup, before any store opens):
  - `legacyID(ws)` = SHA-256 of the raw `cfg.WorkspacePath()` (what `WorkspaceIDFromConfig` used) — the sessionhost dirs keyed by the legacy workspace ID.
  - For each store root (`memory`, `sessionhistory`, `sessionhost`, `hisn-home`, `staging`): if `<root>/<legacy16>` exists and `<root>/<new16>` does not → `os.Rename` (atomic, same filesystem) and one notice line. If both exist → **no merge**; one notice naming both IDs and the command `wakil state merge --from <legacy16> --into <new16>`; use the new ID.
  - Only dirs whose content matches the workspace are considered; the notice is printed once per process.
- **`wakil state merge --from <id> --into <id> [--apply]`** (`cmd/wakil/state_merge.go`; uses only exported agent/memory/sessionhistory helpers):
  - `memory.db`: for each row in source, insert into target if no row with the same `(key, status='active')` exists; if the same logical key exists active in both, keep **both** — the source row is inserted as `superseded` with note `merged from <id>`. Never UPDATE an existing target row's value. FTS/index rebuilt after (store's own rebuild call).
  - `sessionhistory/sessions.db`: union by session id; existing target rows untouched.
  - `sessionhost/`: target kept; source archived.
  - `hisn-home/`: target kept if it is the larger tree; otherwise the larger one is kept as target content and the other archived (cache-only; no merge).
  - Dry-run (default) prints per-store counts: `memory: N rows to insert, M key collisions (kept both), K already present`; `sessionhistory: …`.
  - `--apply`: performs the inserts in one transaction per DB, then **moves** each source dir to `~/.local/share/wakil/_merged/<id>-<UTC timestamp>/` and prints the archive path. Nothing is deleted.
- **git_blame / "outside workspace" root cause (located).** `exec/exec.go:1550` `NewDirectExecutor` stores `root: abs` **unresolved**. `DirectExecutor.ConfinePath` (`exec/exec_ops.go:225-245`) resolves the candidate with `EvalSymlinks` and then calls `isInsideWorkspace(path, e.root)` (`exec_ops.go:167`, a plain `filepath.Clean` + prefix test). With `~/coding` → `~/Coding`, the candidate resolves to `/home/valon/Coding/wakil/…` while the root stays `/home/valon/coding/wakil`: the prefix test fails. `BackendExecutor.ConfinePath` (`exec/backend_executor.go:473`) has the same shape against `e.root`.
  - **Fix:** resolve the root once at construction (`EvalSymlinks(abs)` falling back to `abs`) in `NewDirectExecutor` and in the backend executor's root setup, and compare resolved-to-resolved. Keep `isInsideWorkspace` as the single predicate.
  - Audit the remaining containment predicates for the same shape: `tools/toolcap.go:213-230` (already resolves both), `memory/store.go:446-461` (already resolves both), `exec/exec.go:1517` (URI mapping, container paths — out of scope).

### Config / profile
None (no new keys). Behaviour change: project IDs change for symlinked workspaces (migrated).

### Tests
- `ProjectID` equal for a path and its symlinked alias.
- Rename-only migration: legacy dirs only → renamed to new ID; notice printed once.
- Both exist → no rename, no merge, notice naming both IDs.
- Merge dry-run counts on two fixture memory DBs with overlapping keys.
- Merge apply: no source row lost (count(source ∪ target) = count(result)); colliding key → both present, one superseded; archive dir exists, source dir gone, archive path printed.
- Containment: workspace root given via a symlink; candidate `<alias>/file` and `<real>/file` both accepted; `../` escape still rejected.

---

## ITEM B2 — plan gc resolves the session's store

### Changes
- `cmd/wakil/plan_gc.go`: resolve the store with `agent.ProjectID(ws)` and `agent.MemoryDBPath(ws)` where `ws` is `--project <dir>` or the cwd (resolved), **not** `cfg.WorkDir`. `--store <path>` overrides. Before opening: if the DB file does not exist → error `plan gc: no memory store at <path> (project <ws>); nothing to do` and exit 1 — **no `memory.Open`** (which would create it).
- First output line: `store: <path>`.
- Classification unchanged (`classifyPlanEntry`), listing proposed entries only.

### Tests
- Missing store → non-zero exit, error text, no file created.
- Store path printed first.
- Integration (in-process): create a session App with a tainted state via the real handlers (`handleMemoryPut` on `plan/current` with `touchedExternal` set), close, run the gc resolver against the same project dir, expect the quarantined row listed as dead when the owning chat file is absent.

---

## ITEM B3 — Locale

### Changes
- `exec/backend_executor.go` `sh()`: helper commands only. Set `LC_ALL=C` and `LANG=C` in the exec env for helper argv (`sh -c` invocations generated by `BackendExecutor` file ops: `cat`, `stat`, `ls`, `test`, `mkdir`, `head`, `rm`, `mv`, `find`, `realpath`). Enumerated from the call sites (`grep -n 'e.sh(\|e.run('`), recorded in a list constant `helperLocaleEnv`.
- `run_shell` and any model-requested command: **no env change**. The locale env passed to user commands is untouched.
- `fileErr` / `isShellNotFound`: prefer the exit code (`stat`/`test` return 1 on missing) and a structured probe `test -e <path>` before text matching; keep English text match as fallback only.

### Tests
- `TestBackendExecutorLazyStartAndFileOps` runs with `LANG=de_CH.UTF-8` and `LC_ALL` unset: passes.
- A user command (`run_shell "echo $LANG"`) sees the original locale, not `C`.

---

## ITEM B4 — Browser process ownership and reaping

### Changes
- `browser/manager.go` (local mode): launch chromium with `SysProcAttr{Setpgid: true}` via the allocator's exec path (chromedp `ExecAllocator` with a custom `cmd` is not exposed; instead launch chromium ourselves, `exec.Command(browserPath, flags…)` in its own process group, and connect chromedp with `chromedp.NewRemoteAllocator` on a random local port `127.0.0.1:<port>`). Record `pgid` and `port` in the session state file `<state>/browser/<chatID>.json`.
- `Manager.Close()` (and every exit path): `kill(-pgid, SIGTERM)`, wait 3 s, `kill(-pgid, SIGKILL)`, then remove the state file.
- Idle timeout: `config browser.idle_timeout` (seconds, default 600); a goroutine calls Close after inactivity.
- At most one browser per chat: launch while one exists → reuse if alive, else replace (close old first).
- Startup reaper: on Wakil start, read `<state>/browser/*.json`; for each whose chat is not live (no matching session file, or the pgid's leader is not wakil-owned), `kill(-pgid, SIGKILL)` and remove the file.
- Docker-mode code path untouched (`pkill` by pattern stays).

### Tests
- Launch → Close: no process in the group (scan `/proc/*/stat` for pgid) after Close returns.
- Idle timeout fires Close (short timeout in test).
- Tool-goroutine panic/crash still reaches Close via `defer` at chat end; the test simulates the chat-end path and checks no process remains.
- Startup reaper kills a recorded group whose chat is gone.

---

## ITEM P1 — Persistent home via hisn `[home] persist`

### Changes
- Profile generator (`sandbox/hisn/profile.go`): emit `[home]\npersist = "<abs>"` for `persist` mode; omit `[home]` for `tmpfs`. Path = `<state>/hisn-home/<ProjectID>` (the existing location; one mechanism).
- Remove the Wakil-side home mount: `policyParams.Home` no longer adds a `$HOME` rw mount in the policy (`sandbox/hisn/session.go:245`, `risk.go:134`).
- Config: `sandbox.hisn.home` = `"persist"` (default) | `"tmpfs"`. Project override `.wakil/sandbox.json` → `hisn.home`.
- Caches: set `GOMODCACHE`, `GOCACHE`, `npm_config_cache`, `PIP_CACHE_DIR`, `CARGO_HOME` only when the tool's default would not already resolve under `$HOME` (checked per variable; in the generated `[env]` only).
- Version gate: `[home]` requires hisn ≥ 0.3.2; older → feature off, one notice line.

### Tests
- Generated profile contains `persist = "<state>/hisn-home/<id>"` for the resolved project ID.
- `tmpfs` override omits `[home]`.
- Older hisn version → `[home]` not emitted, one notice line.
- Host-run integration: a file written in session 1 is present in session 2 (`hisn` 0.3.2).

---

## ITEM P2 — Rootfs per project + host binaries

### Changes
- Config: `sandbox.hisn.rootfs` (global; `name` form only in project file; `path` form accepted only in global config). `.wakil/sandbox.json` → `hisn.rootfs` (name). Empty = host toolchain (today's behaviour).
- Profile: `[rootfs]\nname = "<name>"` (XOR path). Empty `[rootfs]` never emitted.
- Session start: run `hisn profile validate <profile>`; on failure with rootfs missing → error `rootfs "<name>" is not in the hisn store; fix: hisn rootfs import <name> --from-image <name>`.
- Injected host binaries (kvr-server; enumerate from `sandbox/hisn/kvr.go` and `wiring/hisn_executor.go` — no others found in Phase 0 beyond the browser, which is handled in P4): mounted ro at `/run/wakil/bin/<name>` via `mounts.ro`, not via host `/usr/local`.
- Startup check: after validate, `hisn exec -- sh -c 'for b in sh bash git; do command -v $b || echo MISSING $b; done'` (rootfs check, one call); missing → warning naming them. Coreutils checked via `ls --version`.
- `docker/wakil-dev.Dockerfile` (path confirmed at implementation): ensure `git openssh-client ca-certificates chromium` and current stable Go (fetched from `https://go.dev/VERSION?m=text` at build time, not pinned). README section: `docker build -t wakil-dev -f … . && hisn rootfs import wakil-dev --from-image wakil-dev --replace`.
- Banner: risk label `rootfs` (Trust, low) — verify existing label rendering.

### Tests
- Profile fragment for `name`; path form rejected in project file.
- Missing rootfs → error text with the exact `hisn rootfs import` fix.
- kvr reachable under a rootfs (host-run integration).

---

## ITEM P3 — Git identity + ssh signing

### Changes
- Config:
  - `git.identity = {name, email}` (default: host `git config --global user.name/user.email`, read once at session start).
  - `git.signing = "ssh" | "off"` (default `"off"`).
  - `git.signing_key = "<path to .pub>"` (default: host `git config --global user.signingkey` if it is an ssh public key, i.e. starts with `ssh-`/`key::` form or ends `.pub`).
  - `sandbox.hisn.ssh_agent = false` (default off).
  - `sandbox.hisn.ssh_keys = [fingerprints]` (extra; for push/auth).
- Session git config file at `<session runtime>/gitconfig` (not in the repo), `GIT_CONFIG_GLOBAL=<that file>`:
  - `user.name`, `user.email`.
  - When signing = ssh: `gpg.format=ssh`, `user.signingkey=key::<pubkey contents>`, `commit.gpgsign=true`, `tag.gpgsign=true`, `gpg.ssh.allowedSignersFile=<generated file>` (generated from the identity email + pubkey).
  - `core.sshCommand` / `GIT_SSH_COMMAND="ssh -o UserKnownHostsFile=<host known_hosts, ro> -o StrictHostKeyChecking=yes"`.
- Profile: `[ssh]\nagent = true\nkeys = ["SHA256:…"]` only when `sandbox.hisn.ssh_agent = true`: keys = fingerprint of `git.signing_key` + `sandbox.hisn.ssh_keys`. `SSH_AUTH_SOCK` set by hisn.
- `~/.ssh/known_hosts` mounted ro via `mounts.ro` only when ssh_agent or signing is on.
- Banner: ssh-agent label with key count when enabled.
- No key material written to repo or session; only the public key contents go into the session gitconfig (a public key is not secret).

### Tests
- Generated git config content (identity; signing on/off).
- Signing off → no `gpg.*` / `commit.gpgsign` keys.
- `ssh_agent=false` → no `[ssh]` table.
- Host-run integration: a signed commit inside a hisn session verifies with `git log --show-signature`.

---

## ITEM P4 — Browser under hisn

### Changes
- Decision (`wiring/hisn_executor.go`, replaces the forced-off rule): browser enabled under hisn iff the session rootfs contains a chromium binary (check `<rootfs>/usr/bin/chromium` or `…/chromium-browser` via `hisn rootfs path <name>` at session start; `path`-form rootfs: stat the tree). Otherwise off with one line: `browser tools off under hisn: rootfs "<name>" has no chromium (rootfs with chromium enables them)`.
- Under hisn: `shm = true`; chromium started with `--no-sandbox --headless=new` (hisn is the sandbox); transport on `127.0.0.1:<random port>` (recorded in session state), same pgid reaper as B4.
- Screenshots: `<cwd>/.wakil/screenshots/` (add `.wakil/screenshots/` to gitignore template), opened host-side as today.

### Tests
- Enable/disable decision table (rootfs with/without chromium, no rootfs, older hisn).
- Host-run integration: open a local test page, screenshot file exists, teardown leaves no chromium.

---

## Phase 3 — Tests, docs, verify

- Host-run hisn integration suite (hisn 0.3.2): rootfs session + kvr; persistent home survives two sessions; signed commit; browser screenshot + reap; gc against a real session store.
- CHANGELOG 0.5.0 — behaviour changes: project ID change + migration; browser under hisn; gc store line; locale for helper commands; git identity/signing defaults.
- `docs/configuration.md` — every new key.
- Commit per item: B1, B2, B3, B4, P1, P2, P3, P4, docs.

---

## Open questions (for Mashūra round 1)

1. What state can the merge lose? (memory key collisions; sessionhistory session-id collisions; hisn-home content with the same path in both trees.)
2. What reaches the host through the generated profile that did not before? (`[home] persist` path; ssh-agent socket; known_hosts ro mount; kvr at /run/wakil/bin.)
3. What still runs from the host toolchain under a rootfs? (`git` executed by the executor's hisn-side helpers; `kvr-server` binary; chromium for the browser; hisn itself.)
4. Is resolving the executor root at construction sufficient for every caller? (Callers that pass an unresolved root to `isInsideWorkspace` directly are to be enumerated by grep in B1.)

## Open points — not done

To be filled at close-out.
