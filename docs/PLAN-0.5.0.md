# Wakil 0.5.0 — per-project identity (C4), hisn 0.3.2 parity (P1–P4)

Supersedes `docs/PLAN-0.5.md` (its B1–B4 items are a different scope: the
B-items were not built in 0.4.5 and are NOT part of this plan; see "Carried
over" at the end). Base: master = v0.4.5 (tag `v0.4.5`). No tag, no push in
this session. Version string: `0.5.0`.

## Environment facts (verified in this sandbox)

- Repo HEAD v0.4.5; `CHANGELOG.md` has `## [0.4.4] - unreleased` at the top and no
  0.4.5 section. The 0.5.0 section is added above it; the 0.4.4 heading is left
  alone (flagged in the close-out, not silently rewritten).
- Sandbox `hisn` is **0.3.1** (`/usr/local/bin/hisn`). It has no `rootfs`
  subcommand. Consequence: P1–P4 profile output can be unit-tested here (string
  content); the hisn-run behaviour (`hisn profile validate` with `[home]`/`[rootfs]`/`[ssh]`,
  `hisn rootfs`) is **host-only** (rubin, hisn 0.3.2). Stated in the close-out as
  unverified here.
- `ProjectID` today: `agent.workspaceKey(ws)` (SHA-256 of the symlink-resolved path,
  `WAKIL_WORKSPACE_KEY` override) truncated to 16 hex = `stagingKey`. This is what
  `MemoryDBPath`, `SessionHistoryDBPath`, `SessionHostDBPath` use.
- Chat path today: `agent.ChatProjectPath(cfg) = cfg.WorkspacePath()`, which is
  `WAKIL_WORKSPACE_PATH` → `WorkDir` (direct) / `HostWorkDir` (docker). Raw, not resolved.
  This is the C4 split.
- `wiring.WorkspaceIDFromConfig` (sessionhost workspace id) hashes the RAW
  `cfg.WorkspacePath()`. `agent.SessionHostWorkspaceID` duplicates it.
- `hisn-home` dir today: `wiring/hisn_executor.go:34-41` keys it by
  `agent.WorkspaceKey(cfg.WorkspacePath())` (full 64-hex, resolved input, raw
  `WorkspacePath()` as input → the same split).
- `plan gc` / `ResolveProject(flagDir)` (`cmd/wakil/plan_gc.go:184`) = Abs +
  EvalSymlinks of cwd/`--project`. Its memory key = `stagingKey(resolved)`.
- Consequence on rubin per the prompt: chat keys `46bec6699c79ec8d` (from
  `HostWorkDir` `/home/valon/wakil-work`), plan gc key `5d8efbe880d17149`.

## Decisions taken (from the prompt, unchanged)

D1–D10 as written in the task prompt. Each one is an item below. Notable
interpretation choices (stated, not re-decided):

- **D1 scope**: `ProjectID` = `stagingKey(ResolveProject(...))` (16 hex, the
  existing store key). Full 64-hex workspace key (`WorkspaceKey`) stays for
  repo-state/session scoping only, unchanged. `WorkspaceIDFromConfig` and
  `SessionHostWorkspaceID` switch to hashing the resolved path (`wsp_` + 16 hex of
  the resolved path). This changes the sessionhost workspace id for existing sessions;
  that is the intended C4 fix and is in the Migration notes.
- **D1 docker**: `host_workdir` stays the bind mount; store keys come from
  `ResolveProject(cwd)` on docker too.
- **D1 `WAKIL_WORKSPACE_KEY`** keeps its verbatim-override meaning inside
  `workspaceKey` (unchanged).
- **D1 `WAKIL_WORKSPACE_PATH`**: no effect on store keys for hisn/direct/docker;
  the existing startup warning stays.

## Frozen keys (golden tests)

`TestResolveProjectGoldenKeys` (`/mnt/wakil` → memory `f3fa3243c6fc07e5`) must keep
passing. New golden: sessionhost workspace id for a fixed resolved path is pinned
in a test (so any future hash change is caught).

---

## C1 — ResolveProject is the chat key source (commit C1)

**Config keys**: none new.

**Code**
- `internal/agent/project_identity.go`: `ChatProjectPath(cfg)` becomes
  `ProjectDir(cwd-or-argument)`: returns the resolved project directory for chats.
  Chat launch passes the directory the chat was started in (`cfg.HostWorkDir`
  for docker is NOT used for keys). Resolution function moves into `internal/agent`
  so `cmd/wakil` and `wiring` both call it (`cmd/wakil.ResolveProject` becomes a
  wrapper). Keep `ResolveProject` symbol name for `plan gc` tests.
- `internal/wiring/conversation_manager.go:494` `WorkspaceIDFromConfig`: hash the
  resolved project dir, not `cfg.WorkspacePath()`.
- `internal/agent/staging.go` `MemoryDBPath/SessionHistoryDBPath/SessionHostDBPath`:
  unchanged signatures (they take a path and call `stagingKey`, which resolves).
- `internal/wiring/bootstrap.go:296,339`, `conversation_manager.go:83-84`,
  `headless.go:275,725`: pass the project dir (not `WorkspacePath()`) to the
  store helpers.
- `internal/wiring/hisn_executor.go:36`: home key = `stagingKey(projectDir)`
  (16 hex) → `<state>/hisn-home/<key16>`. Existing `hisn-home/<64hex>` dirs are
  not touched (D5).
- `internal/agent/workspace_keys.go`: `ResolveWorkspaceKeys` reports the new
  `ChatPath` (resolved cwd); the Diverges warning becomes "legacy key differs"
  (C4 below). Format() updated.
- `WAKIL_WORKSPACE_PATH` startup line kept (`EnvOverrideLine`), text says it no
  longer changes store keys.

**Tests**
- Golden: resolved path of `/mnt/wakil` unchanged (existing test).
- Chat vs gc same store for hisn, direct, docker configs (table test with cwd
  and a symlinked cwd).
- `WAKIL_WORKSPACE_PATH=/elsewhere` set → hisn and direct keys equal those of the
  cwd (no effect), startup line still printed.
- Sessionhost workspace id golden for a fixed path.

**Risk noted**: sessions recorded with the old `wsp_` id will not match the new
id in `SessionScope`. Sessions are listed under "legacy" (C2), not lost.

## C2 — Legacy read-through (commit C2)

**Legacy key** (computed, never written): `legacyKey(cfg)` = `stagingKey` of
the OLD chat path = `cfg.WorkspacePath()` (raw, as today). Only computed when it
differs from the new key. On rubin: `46bec6699c79ec8d`.

**Code**
- `internal/agent/legacy.go` (new): `LegacyMemoryDBPath(cfg)`,
  `LegacySessionHostDBPath(cfg)`, `LegacyEntryCount()` (read-only open via
  `memory.OpenExisting`; missing → 0, never created).
- `internal/memory`: no change to Store; a `ReadOnly` wrapper in agent:
  search/get run on project store, then on legacy store; each legacy result is
  tagged `Legacy: true` (new field on the result struct used by `memory_tools.go`
  and the TUI render).
- Writes: `memory_put` etc. go to the project store only (unchanged path).
- `/resume` and `SessionListText`: legacy sessions (from the legacy sessionhost
  DB, read-only) rendered under heading `legacy (shared store)`. Not moved.
- The read-only legacy open uses `memory.OpenExisting` and is NOT the
  `mode=ro` open (open point from CLEANUP round 2). The legacy DB is opened
  read-only in intent; OpenExisting still runs WAL/migration — this is a write
  to the user's legacy DB. **Decision**: legacy open uses SQLite `mode=ro`
  immutable URI if the store package exposes it; if not, the item is blocked on
  adding a read-only open to `internal/memory` (small, in scope). Plan: add
  `memory.OpenReadOnly` (URI `file:...?mode=ro`, no pragma, no migration).
  This resolves the "legacy store is never rewritten" rule.

**Tests**
- Legacy read-through order: project hit shadows legacy; legacy-only hit returns
  with `Legacy: true`.
- `OpenReadOnly` on a fixture store: no file change (mtime/hash before/after).
- `/resume` listing shows the legacy heading with a fixture legacy sessions DB.

## C3 — `wakil state migrate --from-legacy` (commit C3)

**Command**: `wakil state migrate --from-legacy [--apply] [--project <dir> ...]`.
Known projects = the `--project` dirs given, else the cwd's project plus every
memory store dir whose `anchors`/recorded workspace resolve (see attribution).

**Attribution** (D3): a legacy memory entry is attributed to project P iff
(a) any of its file anchors lies under exactly one known project path P, or
(b) its recorded workspace path lies under exactly one known project path P.
No match or >1 match → stays legacy. Attribution uses resolved paths.

**Code**: `cmd/wakil/state_migrate.go` (new). Uses `memory.OpenReadOnly` for
the source, `memory.Open` for the target only in `--apply`. Copies an entry with
`INSERT` into the project store if no active row with the same key exists there;
else skipped (counted). Marks the legacy row as migrated via a new
`migrated_to` note in the legacy store — **this is a write to the legacy store**.
Decision: do NOT mark the legacy row (it would be a rewrite). Instead, migration
state is kept in a sidecar `<project store dir>/migrated-from-legacy.json`
(list of legacy entry ids copied). This keeps legacy DBs byte-unchanged (D2/D3).

**Dry-run default** prints `project <path> (<key>): N attributable, M already present, K legacy-only` per project plus `unattributed: U`.

**Tests**: dry-run counts on a fixture legacy store with two project anchors and
one ambiguous entry; apply copies attributable entries, legacy DB hash unchanged,
no row deleted in target; re-running apply is a no-op.

## C4 — First-start notice + print-workspace-key (commit C4)

- Notice (D4) printed once per project: flag file `<project store dir>/notified-legacy-v1`
  (created after printing). Text: `memory now keyed per project; N legacy entries readable, run \`wakil state migrate --from-legacy\` to copy attributable ones`. Printed only when N > 0 and the legacy store exists.
- Also prints the D5 line when old `hisn-home/<64hex>` dirs exist: "old hisn-home dirs under <state>/hisn-home/<64hex> are caches and can be deleted".
- `--print-workspace-key` reports: new project key, legacy key, legacy entry count
  (via OpenReadOnly), and sessionhost legacy key. Read-only; creates no files
  (existing test `workspace_keys_test.go:56` stays).

**Tests**: notice printed once (flag file); notice not printed when N=0; print-workspace-key output lines and no file created.

---

## P1 — Persistent home (commit P1)

**Config keys**
- `sandbox.hisn.home` = `"persist"` (default) | `"tmpfs"`.
- `.wakil/sandbox.json` `"hisn": {"home": "tmpfs"}` (project override; wins).
- Existing `sandbox.hisn.home_root`: still the base dir? **Decision**: keep
  `home_root` as the parent override; default path is
  `<state>/hisn-home/<ProjectID>` (D5). Existing `home_root` semantics unchanged.

**Generated profile fragment** (persist):
```toml
[home]
persist = "/home/<user>/.local/share/wakil/hisn-home/<key16>"
```
tmpfs: no `[home]` table.

**Code**
- `internal/sandbox/hisn/profile.go`: `profileFile` gets `Home *homeTOML `toml:"home,omitempty"``. `homeTOML{Persist string toml:"persist"}`. Emitted only when `hisn >= 0.3.2` (new `ProfileHomeMinVersion = "0.3.2"`); older → not emitted + notice (D10); the 0.4.x home mount is kept in that case.
- `internal/sandbox/hisn/session.go:245` / `risk.go:134`: the Wakil-side `$HOME` rw mount is removed **only when** `[home] persist` is emitted (one mechanism per version). For hisn < 0.3.2 the existing mount stays.
- Caches (D9): generated `[env]` sets `GOMODCACHE`, `GOCACHE`, `npm_config_cache`, `PIP_CACHE_DIR`, `CARGO_HOME` only when the default would not resolve under `$HOME` (decided per var by checking `$HOME` vs `os.UserCacheDir`/`GOPATH`-default). Practical default: none set, because Go/npm/pip/cargo default under `$HOME`. Recorded in the test.
- Note in banner: `home-persist` label (`Data, medium`) is provided by hisn at session start.

**Tests**
- Profile contains `persist = ".../hisn-home/<key16>"` for the resolved project.
- `tmpfs` override: no `[home]`.
- hisn 0.3.1 (sandbox) → `[home]` not emitted and the 0.4.x mount stays (profile test + notice).
- Host-run (rubin): file written in chat 1 is in chat 2; `go env GOMODCACHE` warm.

## P2 — Rootfs (commit P2)

**Config keys**
- `sandbox.hisn.rootfs` = name (global default) or absolute path (global only). Empty = host toolchain.
- `.wakil/sandbox.json` `"hisn": {"rootfs": "<name>"}` — name only; project wins.

**Generated profile fragment** (exactly one key, never an empty table):
```toml
[rootfs]
name = "wakil-dev"
```
or `path = "/abs/dir"` (global config only).

**Code**
- `internal/config/config.go`: `HisnConfig.Rootfs string`; validation: path form
  rejected in `.wakil/sandbox.json` (strict schema in the project loader).
- `internal/sandbox/hisn/profile.go`: `rootfsTOML{Name, Path}` with `omitempty`; emitted only when set; `Surface.Rootfs` carries it. Version gate 0.3.2.
- Session start: `hisn profile validate <profile>`; failure with rootfs missing → error `rootfs "<name>" is not in the hisn store; fix: hisn rootfs import <name> --from-image <name>`.
- Startup check (one call): `hisn exec -- sh -c 'for b in sh bash git; do command -v $b >/dev/null || echo MISSING $b; done; ls --version >/dev/null 2>&1 || echo MISSING coreutils'`. Missing → warning naming them.
- Injected host binaries: kvr-server is mounted ro at `/run/wakil/bin/kvr-server` via `mounts.ro` (not via host `/usr/local`). Source: `sandbox/hisn/kvr.go`. Only kvr-server is injected by Wakil in this code (verified by grep for `KVRBinary`); the browser is handled in P4.
- `docker/wakil-dev.Dockerfile` (confirm path at implementation): ensure `git openssh-client ca-certificates chromium`; Go from `https://go.dev/VERSION?m=text` at build time (not pinned). README: `docker build -t wakil-dev -f … . && hisn rootfs import wakil-dev --from-image wakil-dev --replace`.
- Banner: `rootfs` label (Trust, low) rendered from hisn's RISK lines (existing rendering).

**Tests**
- Profile fragment for `name`; path form rejected in project file.
- Missing rootfs → error text with the exact `hisn rootfs import` fix.
- Host-run: kvr reachable under the rootfs via `mounts.ro`.

## P3 — Git identity + ssh (commit P3)

**Config keys**
- `git.identity = {name, email}`; default: host `git config --global user.name/user.email`, read once at session start.
- `git.signing = "ssh" | "off"` (default `"off"`).
- `git.signing_key = "<path to .pub>"`; default: host `git config --global user.signingkey` if it names an ssh public key (a `.pub` path or `key::`/`ssh-` form). **A `key::` value with inline key material is read from the config value; a path is read from disk.** Only the public key text is copied into the session gitconfig.
- `sandbox.hisn.ssh_agent` (default false), `sandbox.hisn.ssh_keys` (extra fingerprints, default []).

**Generated profile fragment** — `[ssh]` only when `ssh_agent` = true AND computed fingerprints non-empty (D7). Otherwise no `[ssh]`, banner line explains why:
```toml
[ssh]
agent = true
keys = ["SHA256:…"]
```
Fingerprints: `ssh-keygen -lf <pub>` on the host (computed; the .pub is read, not a key secret). **Open point**: fingerprint computation via `ssh-keygen` vs Go `golang.org/x/crypto/ssh` (`ssh.FingerprintSHA256`); the Go path is preferred (no shell, no new host dependency) — decided: Go (`x/crypto/ssh` if already in go.mod, else `ssh-keygen` fallback). Checked at implementation.

**Session git config** `<session home>/.wakil-gitconfig` (inside the persisted home, not the repo); `GIT_CONFIG_GLOBAL` points at it:
```
[user]
  name = …
  email = …
[gpg]
  format = ssh
[gpg "ssh"]
  allowedSignersFile = <generated file>
[user]
  signingkey = key::ssh-ed25519 AAAA…   (public key text only)
[commit]
  gpgsign = true
[tag]
  gpgsign = true
```
Signing off → none of the `gpg`/`commit.gpgsign`/`tag.gpgsign`/`signingkey` keys are written.

`known_hosts`: `~/.ssh/known_hosts` mounted ro (`mounts.ro`) only when ssh_agent or signing is on, and `GIT_SSH_COMMAND="ssh -o UserKnownHostsFile=<that path> -o StrictHostKeyChecking=yes"`.

**Tests**: gitconfig content for identity; signing off → no gpg keys; ssh_agent false → no `[ssh]` + banner reason; empty allowlist → no `[ssh]`. Host-run: signed commit shows in `git log --show-signature`.

## P4 — Browser under hisn (commit P4)

**Decision** (replaces the forced-off rule in `wiring/hisn_executor.go` and `config` LoadConfig): browser enabled iff the session rootfs contains a chromium binary. Check at session start: path-form stat `<dir>/usr/bin/chromium` or `/usr/lib/chromium/chromium`; name-form via `hisn rootfs path <name>` then the same stat. Else off with the explanatory line from the prompt.

**Launch**: inside the hisn session via `hisn exec` / control-socket exec (never on the host), lifecycle as 0.4.4 docker path: lazy start, idle stop (`browser_idle_timeout_seconds`), pidfd ownership, per-chat profile dir (`$HOME`-relative), CDP on `127.0.0.1:<random>` inside the session, tunnelled over session exec like the docker relay. Flags: `--headless=new --no-sandbox --disable-gpu --disable-dev-shm-usage --disable-crash-reporter --disable-breakpad --remote-debugging-port=0`. Profile `shm = true`.

**Screenshots**: `<cwd>/.wakil/screenshots/` (add to gitignore template).

**Tests**: enable/disable decision table (rootfs with/without chromium, no rootfs, hisn 0.3.1); tunnel over session exec (unit with a fake exec); host-run: local page screenshot file exists; `hisn exec <s> pgrep -fa chrom` non-empty only while active, empty after idle/`/new`.

---

## Round 1 revision log (Mashūra: gpt-6-astra, claude-fable-5.1; both read the attached files)

Both readers said the plan must not be implemented unchanged. Facts I verified in
source before accepting each point:

| Finding | Verified how | Result |
|---|---|---|
| `memory.OpenExisting` writes (WAL pragma, `CREATE TABLE IF NOT EXISTS`, migration) | `internal/memory/store.go:165,197-207` | **Accepted (BLOCKING).** Legacy reads need a new true read-only open. Revised C2 below. |
| `immutable=1` is unsafe with a live writer; rubin's current project writes the legacy DB when its resolved cwd equals `host_workdir` | reviewer reasoning + plan logic | **Accepted.** Use `mode=ro`, never `immutable`. |
| `ssh_signing` / `WAKIL_SSH_SIGNING` / `--ssh-signing` already exist (`config.go:282,1470,1509`, `internal/exec/signing.go`) and pass the agent socket through | grep | **Accepted (BLOCKING: two switches for one socket).** P3 folds into `ssh_signing`; no parallel `git.signing`. |
| Browser "enabled iff chromium" would ignore `browser_enabled` (default false, `config.go:681`) | read default | **Accepted, corrected.** Rule is `browser_enabled` AND in-session chromium. |
| Removing `BrowserForcedOff` (`config.go:2401`) and `newHisnExecutor` guard exposes `browser.NewManager` on the host (`bootstrap.go:151`) | grep | **Accepted (BLOCKING).** A hisn launcher must exist before the guard is removed; no host fallback. |
| `home_root` is used directly, not as a parent (`hisn_executor.go:34-36`) | read | **Accepted; plan corrected** (explicit home stays exact). |
| `tmpfs` leaves the old mount in place | reasoning on P1 | **Accepted (BLOCKING).** tmpfs must drop it too. |
| `.wakil/sandbox.json` could set `ssh_agent` / `ssh_keys` (cloned repo gets the agent) | design | **Accepted (BLOCKING).** Global-only; rejected in the project schema. |
| Legacy sessions in the same DB under old `wsp_` id go unreachable | `SessionHostWorkspaceID` hashes raw path | **Accepted.** Scope accepts both ids. |
| Legacy key only from current config (`WAKIL_WORKSPACE_PATH`, past `host_work_dir`) | reasoning | **Accepted as open point with explicit `--from <path|key>`** (C3). Not claimed complete. |
| `sessionhistory` has no legacy read-through | `bootstrap.go:339` | **Accepted.** Included in C2. |
| Old hisn-home dirs described as "caches, delete" | reviewers + P3 writes `.wakil-gitconfig` and tools write real state to `$HOME` | **Accepted.** No deletion advice; the D5 notice says "left in place; not used; you may remove them yourself after checking". |
| `known_hosts` ro mount not needed for signing-only | reasoning | **Accepted.** Mounted only with an ssh-using git transport. |
| Host helper processes outside the rootfs (git, hooks, MCP, ssh-keygen) | `grep exec.Command` (37 sites, `internal/agent/hooks.go`, `worktree.go`, `exec/*.go`) | **Accepted as close-out statement, not fixed in 0.5.0.** The rootfs does not cover host helpers; listed. |
| "Public keys only does not prove safety" (agent forwarding authorizes operations) | reasoning | **Accepted as a hisn-side claim to test**, not a Wakil fix: the host-run test must show an allowed key signs and a disallowed key cannot. |
| `ssh` signing with `ssh_agent=false` produces gpgsign with no agent | reasoning | **Accepted.** Load-time error. |
| Named profiles (`hisn.profile`) bypass generation so new rootfs/home/ssh options could be silently dropped | `Surface.Normalize` | **Accepted (BLOCKING).** Rule below. |

Not confirmed by any reader and therefore not acted on: `OpenExisting` racing
`Open` (CLEANUP open point, unchanged).

## Revised designs (supersede the sections above where they differ)

### Legacy read-only access (C2, revised)

- **New** `memory.OpenReadOnly(dbPath)` and `sessionhistory.OpenReadOnly(dbPath)`:
  - file must exist (missing → error, nothing created);
  - open via the modernc driver with `file:<path>?mode=ro` (no `immutable`);
  - no `journal_mode`, no `busy_timeout` pragma writes, no `createSchema`, no
    `migrateSchema`; **no** `UPDATE` anywhere on this handle (any `Get`/`Search`
    that writes access counters must be a variant that does not; the read-only
    handle enforces it by the driver rejecting writes, and tests assert it);
  - schema check: `PRAGMA user_version` / the expected table set must match;
    otherwise return a visible error (`legacy store <path>: unsupported schema`),
    never "0 entries".
- Tests (fixture legacy store, including **committed-WAL-only** rows):
  - hash of `memory.db`, `-wal`, `-shm` and the directory listing before/after
    listing, counting, search, get, resume, migrate dry-run and `Close`: identical;
  - WAL-only committed rows are readable;
  - missing DB: nothing created (the directory itself too);
  - unsupported schema: visible error.
- Sessionhost legacy read: opened read-only through the same pattern
  (`sqlstore` gets a read-only open). Listing and resume both go through it.

### Legacy session identity (C2, revised)

- `SessionScope` accepts two workspace ids: the new `wsp_<16 hex of resolved path>`
  and the legacy `wsp_<16 hex of raw path>`. Both are matched in the same DB; a
  session matched by the legacy id is shown under `legacy (shared store)`.
- Resume of a legacy session: the session file/record is read; the **resumed
  session's writes go to the project store** (new chat, new id), and the legacy
  record is never rewritten. Fixture test: same DB key, different workspace id,
  resume succeeds and the legacy DB is unchanged.

### Legacy key discovery (C3/C4, revised)

- Legacy key is computed from the current config as before **and** from
  `--from <path|key>` (repeatable). Keys not reachable by either are reported as
  "not discoverable from this config; pass --from". The plan does **not** claim
  complete read-through: the close-out says so.
- `memory/*/` directories are listed and reported as "unattributed store dirs"
  (count only, no read) so the user can see what is not covered.

### Attribution (C3, revised)

- Option (b) (recorded workspace path) dropped unless the memory schema has such a
  column — checked before C3 implementation; if absent, only (a) file anchors
  and `--project` are used.
- Known projects = `--project` dirs + cwd only. No enumeration of hashed store
  dirs (circular, as reviewers noted).

### Old hisn-home (D5, revised)

- Not deleted, not renamed by default. The notice says: "old hisn-home dirs under
  `<state>/hisn-home/<64hex>` are no longer used; left in place; remove them yourself if you do not need their contents."
- Rename-on-copy (`old → new` when new is absent) is listed as an open point; not
  built in 0.5.0 (the reviewers' suggestion conflicts with "never move" for legacy
  stores and the plan keeps that rule).

### Home (P1, revised)

- `sandbox.hisn.home` = `persist` (default) | `tmpfs`.
  - `persist` on hisn ≥ 0.3.2: `[home] persist = <state>/hisn-home/<key16>`, the Wakil-side `$HOME` rw mount is removed.
  - `tmpfs` on hisn ≥ 0.3.2: **no `[home]` table and no Wakil-side `$HOME` mount** (the old mount is dropped; the session has a tmpfs home, which is the privacy behaviour requested).
  - hisn < 0.3.2: old mount kept for `persist`; `tmpfs` on hisn < 0.3.2 is a **hard error** ("tmpfs home needs hisn ≥ 0.3.2"), because the old mount cannot be removed without losing the behaviour the user asked for — never silent.
- `sandbox.hisn.home_root` keeps its current meaning: it is the **exact** home path when set (not a parent). Default stays `<state>/hisn-home/<key16>` (new key) when unset.
- The persist path is passed to `checkProfileTrust`/`StateGuard` (reviewer's point, `profile.go:352`).

### Rootfs and named profiles (P2, revised)

- A named profile (`hisn.profile`, used as-is) **cannot** carry Wakil's rootfs/home/ssh fragments. If any of `rootfs`, `home=tmpfs`, `ssh_agent`, or non-default `home` is requested together with `hisn.profile`, the session **refuses to start** with the name of the option. No check-one-run-another.
- Effective-profile check: after `hisn profile validate`, Wakil compares the rootfs name/path and `[home]` the profile declares with what it requested, where hisn exposes them; if hisn does not expose them, the named-profile combination is refused (not checked by assumption).
- Host helpers that run outside the rootfs (git, hooks, MCP, `ssh-keygen`) are **not** covered by the rootfs. The banner and close-out say so; no claim that the rootfs covers them.

### SSH signing (P3, revised)

- **One** switch: the existing `ssh_signing` (`off` | `auto` | path to `.pub`). No `git.signing`. Git identity keys: `git.identity` only.
- `sandbox.hisn.ssh_agent` and `sandbox.hisn.ssh_keys` are **global-only**; the project schema rejects them (strict schema; test).
- `ssh_signing != off` with `ssh_agent=false` is a **load-time error** ("signing needs the agent: set sandbox.hisn.ssh_agent = true or ssh_signing = off"). Avoids a silent "gpgsign on, no agent".
- `[ssh]` table only when `ssh_agent` = true **and** the fingerprint list is non-empty (D7). Version gate: `[ssh]` requires hisn ≥ 0.3.2; older → hard error when ssh_agent is true.
- Fingerprints: computed in Go from the `.pub` (`x/crypto/ssh` if in `go.mod`; else refuse and report). No `ssh-keygen` fallback.
- `known_hosts` mounted ro under a neutral path, **only** when an ssh transport is used (`ssh_agent` true). Not under `/home/<user>/.ssh`. (0.5.2: `src:dst` form, destination `/wakil/ssh/known_hosts` — hisn 0.3.3 refuses destinations under `/run`.)
- Allowed-key vs disallowed-key behaviour is a **hisn test** (host-run): an allowed key signs, a disallowed key is refused, no raw agent-socket bypass. Wakil does not claim this without that test.

### Browser (P4, revised)

- Enabled iff **`browser_enabled` is true** (user opt-in, default false) **and** the
  selected session has an in-session chromium (checked via the hisn exec of the
  actual session, not a host stat).
- Launcher: a hisn-specific launcher is built **before** the `BrowserForcedOff`
  guard (`config.go:2401`) and the `newHisnExecutor` guard are removed.
  - If the hisn launcher is not available → browser off with the reason (not a fallback).
  - `browser_path` (host path) is ignored under hisn with a startup line (it names a host binary).
  - Local `chromedp` ExecAllocator is never constructed under hisn (`bootstrap.go:151` path gated by backend).
  - A failed hisn exec does not retry locally.
  - The decision runs **after** `ApplyDefaultBackend`'s flip (Fable's point).
- Tests: decision table across explicit/auto hisn, `browser_path` set, named profile, unsupported hisn, injected exec failure (no local launch in each); host-run: screenshot, `pgrep` in-session only.

### P4 as implemented (status)

- Ownership transport: under hisn the browser's long-lived host exec client
  (tunnel and launch) runs through `hisn exec --session <id>` via the optional
  `sandbox.CLIExecer` interface. Its argv carries the session id as the ownership
  marker, and the pid and start time are recorded. This CLI path is the ownership
  transport for 0.5.0, not a fallback; it stays until option 2 below replaces it.
- Guard: `HostBrowserAllowed()` is true only when the backend implements
  `CLIExecer` and has an active session id. Direct and docker are unchanged.
- Chromium probe runs at launch because the hisn session may start lazily.
- Tests: guard decision table, probe refusal, no-session refusal, start/stop/sweep
  against a fake session (the fake execs in its own process group, as a real
  session leader does), and marker ownership. The real-hisn cmdline check and the
  real-rootfs launch skip here and are listed in the host sequence.
- **0.5.x follow-up (option 2):** a control-socket ownership protocol, where the
  in-session launch reports pid and start time over the socket, so the CLI
  transport can be removed.

## Open points (known before implementation)

- C2 read-only open and the `sessionhost` read-only path are new code; the tests above are the acceptance.
- Attribution option (b) depends on a schema column (checked before C3).
- Legacy keys beyond current config are reachable only by `--from`.
- Host-only verification (hisn 0.3.2, rootfs, `[ssh]` agent behaviour, browser in session, signed commit) cannot run in this sandbox (hisn 0.3.1).
- Host helper processes (git/hooks/MCP/LSP) remain outside the rootfs; close-out statement, not a fix.

## Carried over (not in 0.5.0)

PLAN-0.5.md items B1–B4 (rename migration, state merge, locale, browser reaping
for docker/direct) are not part of this plan. B3 (locale) and B4 (reaping) appear
to be covered by the 0.4.4/0.4.5 CHANGELOG entries; verify before dropping them
from the backlog.

## Phase 3 review status (2026-11-06)

- **P3:** partly reviewed. One reader (claude-fable-5.1) returned a usable review
  on the source files; its two BLOCKING items were verified and fixed in `374dd66`
  (repo-local signing key widening the agent allowlist; named profile silently
  dropping identity/signing). Not obtained: `[ssh]` rendering in `buildPolicy`,
  validation of `ssh_keys` entries, and `EnvPass` vs `EnvSet` precedence for
  `GIT_SSH_COMMAND`. Two attempts with gpt-6-astra failed (content filter, then peak
  load); the retry used a different reader. P3 is not fully reviewed.
- **P4 option 2** (control-socket ownership protocol) is the 0.5.x follow-up; the
  CLI exec path stays the browser ownership transport until then.
