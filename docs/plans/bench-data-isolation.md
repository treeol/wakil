# Bench data-isolation plan (work/0.8.12)

Base: `a8f1f24`. Branch: `work/0.8.12`. Goal: a headless bench run must
not create, change or read the user's real skill store or any other real
user data (sessions, toolcache, sessionhistory/sessionhost DBs — see §1.2),
and must not change normal behaviour when the new override is unset. The
sandbox-approval store is never moved. Session-history/session-host DBs are
isolated in Block 3 via `WAKIL_REPO_STATE_DIR` with eyes open about the
repo-state side effect (see below); `WAKIL_REPO_STATE_DIR` itself never
moves the approval store (verified: `hisnStateDir` resolves only from
`XDG_DATA_HOME`/HOME, `internal/wiring/hisn_executor.go:149-158`).

## 1. Verified findings (file:line)

### 1.1 How Wakil locates and opens the skill store

- `agent.SkillDBPath()` is the single definition site. It returns
  `filepath.Join(stagingDataRoot(), "skills", "skills.db")`, global (not
  workspace-keyed), `""` if the data dir cannot be resolved; the dir is not
  created here (`internal/agent/staging.go:100-114`).
- `stagingDataRoot()` returns `filepath.Dir(repoStateDir())`
  (`internal/agent/staging.go:53-71`); `repoStateDir()` resolves through
  `resolveDataDir("WAKIL_REPO_STATE_DIR", "repo-state")`
  (`internal/agent/repostate.go:121-126` via `internal/agent/persist.go:18-30`).
  Precedence: env value as-is → `$XDG_DATA_HOME/wakil/<sub>` →
  `~/.local/share/wakil/<sub>`; `""` when Home cannot be resolved.
- The only production open is `internal/wiring/bootstrap.go:438-449`:
  `agent.SkillDBPath()` → `memory.Open(path, "")` → `agent.NewSkillsProfile`.
  Open failure only prints to stderr and leaves `SkillStore` nil.
  `memory.Open` does `MkdirAll(dir, 0700)`, sqlite open, WAL/busy_timeout
  pragmas, schema creation (`internal/memory/store.go:152-200`). The skill
  store is opened **outside** the `--no-memory` guard, so `--no-memory`
  does not protect it (`internal/wiring/bootstrap.go:438-449`).
- No dedicated env override exists today (no `WAKIL_SKILLS*`/`SKILLS_DB`
  anywhere). Indirect redirection is possible only via `XDG_DATA_HOME` or
  `WAKIL_REPO_STATE_DIR`, both of which also move unrelated stores.
  `SkillDBPath()` only ever distinguishes "path" vs `""`, never an error.

### 1.2 Every other place a headless `wakil run` writes to real user data

Entry: `cmd/wakil/main.go:81-99` → `RunHeadless`
(`internal/wiring/headless.go:253-332`). Data-root precedence everywhere is
`persist.go:18-30`. Per store:

| Store | Write site (file:line) | Bench verdict |
|---|---|---|
| Skill store `skills/skills.db` | `internal/wiring/bootstrap.go:438-449` (open), `internal/memory/store.go:152-200` (create; WAL mode `:172-184` → `-wal`/`-shm` sidecars possible) | **BLOCKING** — the subject of this plan. Open is **outside** the `--no-memory` guard (`bootstrap.go:432-449`); it stays best-effort (open failure → stderr + nil store). |
| Sessions `sessions/<chat>.json` + resume index `.resume-index` (0600) | `internal/agent/session.go:91-106` (`WriteSession`), `internal/agent/resume_index.go:111-167` | **Problem** — bench sessions land in the user's real sessions dir and appear in `/resume`. The bench run passes `--transcript`, which is exactly the `SaveSession` trigger (`internal/wiring/headless.go:460-462,570-572`). |
| Toolcache `toolcache/<chatID>/` (spills + `archive-index.txt`) | `internal/tools/toolcap.go:92-114` (base: `Dir(WAKIL_SESSIONS_DIR)` when set), `:378-416` (spill) | **Problem** — every bench run spills into the user's real toolcache; no sweep deletes headless-run dirs (sweep is TUI/daemon only, `internal/wiring/bootstrap.go:694-714`). NOTE: toolcache is a *sibling* of the sessions dir, so `WAKIL_SESSIONS_DIR` must be `<private>/sessions` — setting it to the private dir itself would put toolcache in the private dir's *parent* (e.g. `/tmp/toolcache/`), outside the 0700 dir and never cleaned (Mashūra R1 B1, verified against `toolcap.go:92-103`). |
| Memory `memory/<key>/memory.db` | `internal/wiring/bootstrap.go:402-429` | **Skipped by bench** — bench passes `--no-memory` (`scripts/bench.sh:979-995`). Harmless. |
| Session-history `sessionhistory/<key>/sessions.db` | `internal/wiring/bootstrap.go:454-465` (`SessionHistoryDBPath`, `internal/agent/staging.go:124-134`) | **Real write** — workspace-keyed and disposable, but it still creates `<data>/sessionhistory/<key>/` under the real data root on every bench run. Isolated in Block 3 via `WAKIL_REPO_STATE_DIR` (see verdict below). |
| Session-host `sessionhost/<key>/sessionhost.db` | `internal/wiring/headless.go:472,627,888-898` (`SessionHostDBPath`, `internal/agent/staging.go:145-155`) | **Real write** — same verdict as session-history. |
| Staging `staging/<key>/` | `internal/agent/staging.go:41-51,161-175`; docker `NewExecutor` only (`internal/wiring/bootstrap.go:386-396`) | **Harmless** for the default backend (hisn); docker-mode only plus KVR socket writes. |
| Repo-state `repo-state/<sha>.json` | `internal/agent/repostate.go:205-231,340-347`; read via `LoadRepoState` (`repostate.go:154-209`) | **Read in headless run** (not write): `LoadRepoState` at `repostate.go:211,250,687` feeds per-repo settings (context mode, mashura level, network). Redirecting `WAKIL_REPO_STATE_DIR` for bench means bench runs see *no* repo state — a fidelity change, not a data-write. Accepted: bench already fixes context-modules/mashura per run, and the merged config sets the rest. |
| Config `~/.config/wakil/config.json` | `internal/config/config.go:1890-1905` | **Read-only**; bench already redirects it (`WAKIL_CONFIG`) to the merged-config temp file (`scripts/bench.sh:890-897`). Harmless. |
| Hisn approval store (`hisn/approved`, trust root) | `internal/wiring/hisn_executor.go:149-158` (`hisnStateDir`: `XDG_DATA_HOME` else HOME only — never `WAKIL_REPO_STATE_DIR`); consumed at `internal/sandbox/hisn/backend.go:336-352` | **Never written/redirected by this plan.** `wakil sandbox trust` keeps working — see Risks. `WAKIL_REPO_STATE_DIR` and `WAKIL_SESSIONS_DIR` cannot move it (different resolution chain); `XDG_DATA_HOME` is not changed by bench. |
| Trace store | only when `cfg.Trace` (`internal/wiring/bootstrap.go:467-469`) | Off in bench (no trace key is set by bench; the user's merged config is not force-offed for trace, but no bench task sets it). Harmless as run. |
| Diag log | `diag.Printf` → per-session diagnostic log file (path derived from the data root) | **Read-side residual write**: several `diag.Printf` call sites fire on headless runs (e.g. `internal/wiring/hisn_executor.go:141`, `internal/wiring/bootstrap.go:423`). Writes go to the user's diag log under the real data root. No existing redirect; listed under Open points (needs a `WAKIL_DIAG_DIR` override or equivalent). |
| Input history | `WAKIL_HISTORY_FILE` (`internal/tui/history.go`), TUI-only | **Not written by headless runs.** Harmless. |
| File-based skill import | none found (skill reads go through `App.SkillStore` only) | Harmless. |

Existing redirect mechanisms that already exist and are clean:
- `WAKIL_SESSIONS_DIR` (sessions dir, `internal/agent/session.go:62-66`).
  IMPORTANT (Mashūra R1 B1): `toolCacheBase()` returns
  `filepath.Dir(WAKIL_SESSIONS_DIR)` (`internal/tools/toolcap.go:92-103`),
  so setting `WAKIL_SESSIONS_DIR=<private>/sessions` puts BOTH sessions
  (`sessions/<chat>.json` + `.resume-index`) and toolcache
  (`<private>/toolcache/<chatID>/`) inside the 0700 private dir. Setting it
  to the private dir itself would leak toolcache to the parent. Block 3
  asserts the spill path, not just the env var.
- `WAKIL_REPO_STATE_DIR` redirects `stagingDataRoot()` (parent of the value,
  `internal/agent/staging.go:53-71` via `repostate.go:124-126`), which moves
  sessionhistory, sessionhost, staging and memory — for bench, memory is
  already off. Setting `WAKIL_REPO_STATE_DIR=<private>/repo-state` moves all
  of them into the private dir in one existing mechanism. NEVER moves the
  approval store (`hisnStateDir` ignores it — `hisn_executor.go:149-158`).
  Side effect (accepted): bench runs see no repo-state (per-repo settings
  read via `LoadRepoState`). Bench already pins context-modules/mashura
  per run; the merged config sets the rest. This is why the same
  `WAKIL_REPO_STATE_DIR` that used to be "the only indirect skill redirect"
  is now the Block-3 isolation lever — and why skill isolation itself gets
  its own var (Block 1): `stagingDataRoot` is shared, so a skills-only
  redirect needs `WAKIL_SKILLS_DB`.
- `XDG_DATA_HOME` redirects everything **including the approval store**
  (`hisn_executor.go:149-158`) — never set by bench. The task context
  §12.6 (`docs/plans/bench-hard.md:1398-1410`) reaches the same conclusion.

### 1.3 How bench launches runs today (facts for Block 2)

- Launch: `scripts/bench.sh:979-995` — `env -u SEARXNG_URL
  WAKIL_CONFIG=$_cfg_file [WAKIL_MODEL=...] timeout -k <grace> <to> <bin>
  run --auto --allow-destructive --no-memory ... --result-json
  --transcript ...`. No skill/session/toolcache env is passed.
- Merged config: private dir `_bench_cfg_tmp=$(mktemp -d)` (0700, mode
  0600 file under `umask 077`), created at `scripts/bench.sh:896-897`,
  0600 at `:918-932,973`.
- Cleanup: `bench_cfg_cleanup` (`scripts/bench.sh:542-563`) removes the
  merged file and the recorded mirror symlinks, then `rmdir` (never
  `rm -rf`). Called immediately after `wait` on every run path
  (`:1038-1042`) and on early returns (`:933-947,1069-1077`), plus
  normal-mode EXIT/INT/TERM traps (`:1426-1442`) and `st_on_exit`
  (`:1428-1442`) in self-test.
  **Mashūra R1 fix**: after a run the private dir is *not* empty — it holds
  the skill DB (+ possible `-wal`/`-shm` sidecars: WAL pragmas at
  `internal/memory/store.go:172-184`), `sessions/<chat>.json` +
  `.resume-index`, `toolcache/<chatID>/...`, and the sessionhistory/
  sessionhost DBs. `rmdir` would fail and leak the dir on every run.
  Block 2/3 therefore extend `bench_cfg_cleanup` with an explicit,
  enumerated removal: remove the known entries the run can create
  (`config.json`, mirror links, the skill DB + sidecars, `sessions/`,
  `toolcache/`, `repo-state/` subtree incl. `sessionhistory/` +
  `sessionhost/` of the fake data root) — scoped `rm -rf` of those
  enumerated names only, never the dir itself — then `rmdir`. Unknown
  content is still reported and left (existing behaviour: warn + list top 5).
  A `SIGKILL` of `bench.sh` itself still leaks the temp dir (no trap can
  fire) — recorded under Risks, not fixed.
- Empty-variable guard (Mashūra R1 B3): `WAKIL_SKILLS_DB`,
  `WAKIL_SESSIONS_DIR` and `WAKIL_REPO_STATE_DIR` are all read via
  `os.Getenv(...) != ""`. An *empty* value would silently mean "real store".
  `bench.sh` must hard-fail before launch if any of the three is empty or
  its parent dir does not exist, and the self-test gains a case for it.
- Self-test: isolation cases redirect CSV/logs to temp dirs, point
  `WAKIL_CONFIG` at a fake config, and assert `bench/results` is
  untouched (`scripts/bench.sh:1444-1482,3418-3425,3698-3710`). The new
  assertions extend this pattern: fake "real" skills.db byte-identical,
  private dir gone after success/failure/timeout.
- `bench/README.md` documents the harness env (`WAKIL_HOST_WORKDIR`,
  `WAKIL_CONFIG`, `WAKIL_CONTEXT_MODULES`, `:44-67,128-150`) and says
  nothing about skills/user data. Block 2 adds the historical-rows note
  there, naming the Block-2 commit.

### 1.4 Where env vars are documented

- `scripts/check_config_docs.sh` enforces: every key in
  `config.example.json` (beyond `_comment` keys) must appear in
  `docs/configuration.md`. Env-only variables with no config key are
  exempt by design (`docs/configuration.md:88-108`, "Environment variables
  (not tied to a config key)" — `WAKIL_SESSIONS_DIR`, `WAKIL_HISTORY_FILE`,
  etc.). So `WAKIL_SKILLS_DB` gets one row in that table (default:
  `~/.local/share/wakil/skills/skills.db` or `$XDG_DATA_HOME/...`) and the
  check stays green — no `config.example.json` change.
- Env handling pattern in the codebase: `os.Getenv(name) != ""` with
  `WAKIL_*` preferred over legacy `ILM_*`, last non-empty wins
  (`internal/config/config.go:2148-2186`; helpers `envStr/envBool/envInt`
  at `:2583-2612`). `os.LookupEnv` is never used.

## 2. Design

`WAKIL_SKILLS_DB` — one env var, read inside `agent.SkillDBPath()`:

1. **Unset** → behaviour is byte-for-byte today's resolution
   (`stagingDataRoot()` path). The modules-off tool array and preamble do
   not touch this code path, so `TestContextModulesOff_ByteIdentical` /
   `TestContextModulesOff_PreambleByteIdentical` are unaffected by
   construction (their tests also run with the var unset).
   `("", nil)` is preserved: unset + unresolvable home returns `("", nil)`,
   never an error (Mashūra R1 criterion b). No `ILM_SKILLS_DB` alias —
   `WAKIL_SKILLS_DB` only.
2. **Set** → validate, then return the value as-is. The check covers the
   **leaf and its immediate parent only** (Mashūra R1: an any-ancestor rule
   would false-positive where the temp dir sits behind a symlinked
   grandparent):
   - not absolute → error naming `WAKIL_SKILLS_DB`;
   - immediate parent must exist and be a directory (`os.Stat` on the
     parent must succeed and be a dir) → error naming `WAKIL_SKILLS_DB`;
   - leaf, if it exists, must be a regular file (`os.Lstat`: a symlink or a
     directory is an error naming `WAKIL_SKILLS_DB`; a non-existent leaf is
     valid — `memory.Open` creates it); immediate parent, if it is a
     symlink, is also an error (the check applies to parent + leaf only);
   - an invalid value is returned as an error and the caller fails at
     startup naming the variable — never silently falling back to the
     real store.
   Empty-string semantics: `WAKIL_SKILLS_DB=""` counts as unset (house
   pattern, `os.Getenv(...) != ""`, `persist.go:18-24`). This is stated
   here as accepted: the bench guard (R1 B3) makes an empty value from the
   harness impossible-without-failure; a user setting it empty by hand has
   set nothing.
3. `SkillDBPath()` returns `(string, error)`; its one production caller
   (`internal/wiring/bootstrap.go:438-449`) propagates **only the
   validation error** (fail startup, naming the variable). `memory.Open`
   failure stays exactly as today: stderr line, `SkillStore` nil
   (Mashūra R1 blocking #4 — default behaviour when unset must not change,
   e.g. a corrupt/locked real DB must remain best-effort, never fatal).
   Tests clear `WAKIL_SKILLS_DB`, `WAKIL_REPO_STATE_DIR` and
   `XDG_DATA_HOME` (`t.Setenv`) so results are env-independent.
   Checksum-only comparison (no mtime — granularity flakiness).
4. Bench: `scripts/bench.sh` points `WAKIL_SKILLS_DB` **at a path, not a
   pre-created file** — the file does not need to exist; `memory.Open`
   creates it (parent is the 0700 temp dir itself). `bench.sh` hard-fails
   before launch if `WAKIL_SKILLS_DB`/`WAKIL_SESSIONS_DIR`/
   `WAKIL_REPO_STATE_DIR` is empty (R1 B3). `bench_cfg_cleanup` removes the
   enumerated entries (§1.3) then `rmdir`.

## 3. Blocks (one signed commit each, failing test first)

- **Block 0** — this plan file (`docs/plans/bench-data-isolation.md`).
  **Mashūra round 1** (2026-11-25, panel default, 3 reviewers, all agreed):
  B1 toolcache-outside-private-dir (subdirectory fix, §1.2/§2), B2
  sessionhistory/sessionhost real writes (isolation via
  `WAKIL_REPO_STATE_DIR` in Block 3), B3 empty-variable silent fallback
  (bench hard-fail guard). Non-blocking folded in: cleanup enumeration
  (§1.3), validation spec (§2.2), `("", nil)` preservation (§2.1/§2.3),
  no `ILM_*` alias, path-not-file slot (§2.4), extra table rows (§1.2),
  test env clearing + checksum-only (§2.3), whole-tree snapshot +
  parent-gains-nothing acceptance (§5), SIGKILL leak (§4). Max 2 rounds
  per the procedure; round 1 verdicts and the fold-in are recorded here.
  No round-2 needed: the fixes are mechanical and now in the plan text.
- **Block 1** — `WAKIL_SKILLS_DB` in `internal/agent/staging.go`
  (`SkillDBPath() (string, error)`), caller fix in
  `internal/wiring/bootstrap.go` (propagate validation error only), one row
  in `docs/configuration.md` (operator table, `:92-96` — env-only, so
  `scripts/check_config_docs.sh` stays green; no `config.example.json`
  change). Tests (failing first, `internal/agent/skills_test.go`, temp
  dirs only, env cleared): unset → default XDG path (unchanged) and
  `("", nil)` when home is unresolvable; set → override file used and the
  default file's checksum unchanged across an open/write; relative /
  missing-parent / symlinked-leaf / non-dir values refused with the
  variable named, and the error path never resolves to the real store;
  set + unwritable override parent → still just a validation error, never
  a silent fallback.
- **Block 2** — `scripts/bench.sh` sets `WAKIL_SKILLS_DB` per run (path
  inside the private temp dir) with the empty-variable hard-fail;
  `bench/README.md` historical-rows note naming this commit (rows before
  ran with the user's real skills; results are not comparable with
  skill-populated runs). Self-test: run gets `WAKIL_SKILLS_DB` in its
  private temp dir; dir gone after success, failure, timeout; fake "real"
  skills.db byte-identical; empty-variable case fails before launch.
- **Block 3** — `scripts/bench.sh` + self-test (separate commit: sessions +
  toolcache via `WAKIL_SESSIONS_DIR=<private>/sessions`, sessionhistory/
  sessionhost/staging via `WAKIL_REPO_STATE_DIR=<private>/repo-state`;
  cleanup extended to remove the enumerated entries before `rmdir`).
  Self-test asserts the whole fake data-root tree (paths + checksums) is
  identical before/after, the spill path is inside the private dir, and
  the private dir's parent gains no entries. Verdicts for every store in
  §1.2; the diag-log write goes under Open points. Explicit residual
  risks recorded in the final report, not silently skipped: bench sees no
  repo-state (fidelity note) and no user skills (by design — README note
  covers comparability).
- **Block 4** — CHANGELOG `[Unreleased]` entry (env var, purpose, default
  unchanged) + bench note.

## 4. Open risks

- **Hisn approval store**: lives outside the Wakil data root via hisn's
  own path resolution (`internal/wiring/hisn_executor.go:149-158` —
  `XDG_DATA_HOME`/HOME only; never `WAKIL_REPO_STATE_DIR` or
  `WAKIL_SESSIONS_DIR`). `XDG_DATA_HOME` changes are NOT used anywhere in
  this plan, so `wakil sandbox trust` behaviour is untouched. The bench
  self-test's whole-tree snapshot of the fake data root (§5) would catch an
  accidental move mechanically.
- **Repo-state fidelity**: bench loses per-repo settings (Block 3 trade-off,
  §1.2). Bench pins context-modules/mashura per run and the merged config
  sets the rest; per-repo state files are also not needed for `wakil run`
  correctness (read via `LoadRepoState`, which tolerates absence).
- **SIGKILL leak**: a `SIGKILL` of `bench.sh` itself leaves the private
  temp dir behind (no trap can fire). Same exposure as today's merged
  secret-bearing config dir — no worse. The dir is 0700 and the skill DB
  inside is a fresh empty store, so nothing secret leaks beyond what
  today's code already risks.
- **Session-file modes**: session JSONs are written 0644 into a dir under
  the 0700 private temp dir — private by parent, stated here so the
  dependency is explicit.
- **TOCTOU on symlink check**: the Lstat check and the later `memory.Open`
  are not atomic. The threat model is the bench harness itself (a trusted
  local script), not an attacker racing the agent; the check is defense
  against a mis-set variable pointing at a symlink, not against a live
  adversary. Noted here so the record is honest about the guarantee.
- **Subagents**: skill reads happen through the same `App.SkillStore`;
  no separate open site, so one override covers subagents too
  (`SkillDBPath` has exactly one production caller — grep-verified
  2026-11-25; the repo-wide grep output is available on request).
- **Open points (not in scope)**: diag-log redirect (needs a
  `WAKIL_DIAG_DIR`-style override — new mechanism, out of scope); 0-byte
  pre-created skill file (not used — path-only slot); docker/hisn
  executor filesystem writes (not inventoried beyond §1.2); continuous-mode
  extra writes (bench uses single/plan modes only).

## 5. Gates (final state, each line reported)

`git ls-files '*.go' | xargs gofmt -l` (empty); `go vet ./...`;
`golangci-lint run`; `go test -count=1 -timeout 30m ./...` (every package
line); `go test -race` on changed packages; `scripts/check_config_docs.sh`;
`scripts/bench.sh --self-test` (incl. whole-fake-tree snapshot unchanged,
spill path inside private dir, parent gains no entries);
`TestContextModulesOff_ByteIdentical` and
`TestContextModulesOff_PreambleByteIdentical` pass with `WAKIL_SKILLS_DB`
unset.

## 6. Mashūra round 1 — verdicts and fold-in (2026-11-25, recorded)

The plan above already incorporates round 1 (panel default, 3 members, all
agreed on each blocking item). Full reasoning was checked against the code
before folding in: B1 confirmed at `toolcap.go:92-103`; B2 confirmed at
`staging.go:63-71` + `bootstrap.go:454-465` / `headless.go:472`;
B3 confirmed at `persist.go:18-24`. Blocking #4 (open failure must stay
best-effort when unset) confirmed at `bootstrap.go:438-449`. The
`WAKIL_REPO_STATE_DIR` isolation lever was additionally verified:
`stagingDataRoot()` is its parent (`staging.go:53-71`), which moves
sessionhistory/sessionhost/staging/memory, and `hisnStateDir()`
(`hisn_executor.go:149-158`) never reads it.
