# Design note: Wakil ⇄ hisn 0.3.1 (profiles, services, risk labels, hisn as default)

Target tag **v0.4.0** (the default flip is a behaviour change). Baseline: master `5d738b9`.
Sources read: hisn v0.3.1 `DESIGN.md` §15–§19, `WAKIL-INTEGRATION.md`, `profiles/*.toml`,
`src/profile.rs`, `src/supervisor.rs`, and the 0.3.1 binary's offline `profile validate|explain`
output. Facts below marked **(observed)** were checked against that binary/source, not the prose docs.

## 0. Binding rules (from the task, not up for design)

1. Wakil never writes a `risk_ack` on its own. It only copies the string the user put in the project
   file. A profile with a label above `none` without it → hard error before any session starts.
2. The generated profile lives at `<state>/hisn/<chatID>.profile.toml` (state = `~/.local/share/wakil`),
   never under the project cwd (hisn `E_PROFILE_UNTRUSTED`).
3. Default backend becomes hisn only if: no explicit backend (config file / env / flag / `exec_mode`),
   `hisn check` passes, hisn ≥ 0.3.1 in PATH. Otherwise docker, reason in the banner.
4. Mashūra: round 1 on this note, round 2 on the full diff, no third round; only BLOCKING folded,
   the rest → `docs/follow-ups.md`.

## 1. Facts about hisn 0.3.1 that shape the design

* **(observed)** `[services]`/`[[forward]]`… sit next to `[policy]` in the envelope; unknown keys are
  errors (`deny_unknown_fields`). `[[services]]` has **no env table**; `ready` allows only `socket`
  and `timeout` (no `command`, no `tcp`).
* **(observed)** hisn does **not** substitute `{cwd}`. `ready.socket` is `stat`ed by the *supervisor*
  with the path as written, so it must be absolute (the shipped `kvr-example.toml` uses a relative
  one, which would resolve against the supervisor's process cwd). The service itself runs with the
  session cwd as its cwd.
* **(observed)** `E_PROFILE_UNTRUSTED` is checked against the *supervisor process cwd*, the session
  cwd, rw grants, `mounts.rw` and a persistent `home`. Today Wakil spawns `hisn run` with Wakil's own
  cwd (= usually the project), so the supervisor process cwd must be made neutral.
* **(observed)** `hisn profile explain FILE` loads the profile and therefore **fails** with
  `E_RISK_ACK_REQUIRED`/`E_RISK_ACK_MISMATCH` when the ack is missing/wrong; it prints
  `{"risk": "<highest>", "capabilities":[{capability,label}], "notes":[…]}` otherwise.
  `profile validate FILE` prints the resolved policy and exits 125 with the same error codes.
* **(observed)** `inspect --json` carries `"risk":[{capability,label}]` and `"services":[…]`; the
  `RISK: <label> — <cap>` lines go to the supervisor's stderr (already captured in Wakil's log).
* **(observed)** A profile cannot set `session.hostname` from the CLI; with a **named** profile
  (`--profile NAME`) Wakil cannot inject its instance token into the policy.
* **(observed)** Release assets are the bare binary; built-in names (`base`, `wakil`) resolve only if a
  `profiles/` dir sits next to the binary or `$HISN_PROFILES` is set. `hisn.profile` therefore surfaces
  hisn's `E_PROFILE_NOTFOUND` verbatim.
* `mounts.mask` of a path that does not exist is skipped (so it cannot protect a file that is created
  later); a file mask reads as empty and discards writes.

## 2. Step 1 — profile generation replaces bare policy generation

`internal/sandbox/hisn/profile.go` (new). Types mirror the hisn schema (only what Wakil emits):

```go
type profileFile struct {
    Version      string         `toml:"version"`   // "hisn-profile/v1"
    Policy       policyFile     `toml:"policy"`    // = today's policy, unchanged content
    Services     []serviceSpec  `toml:"services,omitempty"`
    Forward      []forwardSpec  `toml:"forward,omitempty"`
    HostServices []hostSvcSpec  `toml:"host_services,omitempty"`
}
```

* `policyFile` gains `Mounts.RO/RW/Mask` and `Env.Set` with `omitempty`; with no overrides the encoded
  `[policy]` is byte-identical in content to today's policy (test asserts equality with the legacy
  encoder's output modulo the `[policy]` prefix).
* **Version gating.** `Preflight` records the parsed version. `profileMode = version ≥ 0.3.1`
  (`ProfileMinVersion`). `MinVersion` stays 0.2.1: explicit `backend=hisn` keeps working on 0.2.x via
  the old `--policy` path, validated with `hisn policy validate`. Features that need the 0.3 surface
  (services, forward, host_services, `hisn.profile`, risk) on 0.2.x are a **hard error naming the
  feature and the required version** — never silently dropped. `env_set`/`mounts_ro`/`mounts_rw` exist in
  hisn-policy/v1 and work in both modes.
* **Start line (profile mode):** `hisn run --detach -v --session-id ID --profile <path> --cwd CWD
  --ready-fd 3` (no `--network host` on the CLI: the profile's policy carries it).
  Validation: `hisn profile validate <path>` (legacy: `hisn policy validate`).
* **Neutral supervisor cwd.** `cmd.Dir = <state>/hisn/proc-cwd` (created 0700, empty). The profile file
  is `<state>/hisn/<id>.profile.toml`, a sibling, so it is under neither the process cwd nor (unless the
  workspace contains the state dir) the session cwd.
* **Trust pre-check** (`checkProfileTrust`): before spawning, Wakil lexically/`EvalSymlinks`-compares
  the profile path against session cwd, `home_root`, `mounts_rw`; a hit is an error saying
  "profile would be E_PROFILE_UNTRUSTED: workspace/home_root contains Wakil's state dir (move one of
  them)". hisn would reject it anyway; this gives the reason up front. Typical trigger: running Wakil
  with `$HOME` as workspace.
* `HISN_PROFILES` is added to the scrubbed client environment (it is a path to hisn's own trusted
  profile dir, not a secret); needed for `hisn.profile` names.
* Supervisor stderr already goes to the log, so the `RISK:` lines are retained there.

## 3. Step 2 — per-project overrides `.wakil/sandbox.json`

Loaded once, at executor construction (process start), from `<cwd>/.wakil/sandbox.json`. Absent file =
no overrides. Decoding is strict (`DisallowUnknownFields`; unknown key → error naming it).

```json
{ "hisn": {
    "services":      [{"name":"db","exec":["pg","-D","data"],"socket":"/abs/.wakil/db.sock",
                       "ready_socket":"/abs/.wakil/db.sock","ready_timeout":15,
                       "stop_signal":"TERM","stop_grace":5}],
    "forward":       [{"name":"docker","socket":"/var/run/docker.sock","expose":"/run/docker.sock","risk":""}],
    "host_services": [{"name":"x","exec":["x"],"socket":"/run/user/1000/x.sock","expose":"/run/x.sock",
                       "ready_socket":"/run/user/1000/x.sock","ready_timeout":15,"risk":""}],
    "env_set":       {"FOO":"bar"},
    "mounts_ro":     ["/opt/data"],
    "mounts_rw":     ["/srv/out"],
    "risk_ack":      "root-equivalent",
    "profile":       ""
}}
```

Rules:
* `risk_ack` is copied **verbatim** into every `[[forward]]`/`[[host_services]]` entry when non-empty
  (hisn ignores it where the label needs none, and demands it for host_services). One string means one
  label: a project mixing capabilities with different labels must use `hisn.profile` (documented).
* Relative `socket`/`ready_socket` paths in `services` are resolved against the cwd (hisn does not);
  `ready_socket` defaults to `socket`.
* A named `hisn.profile` is verified with `hisn profile explain NAME` (**observed**: works with
  `$HISN_PROFILES`) — it supplies `L` for the ack comparison.
* `hisn.profile = NAME` → Wakil does **not** generate a profile: `hisn run --profile NAME --cwd CWD
  [--grant PATH:ro|rw]… --ready-fd 3`. `mounts_ro`/`mounts_rw` become initial grants (rw bounded by that
  profile's `grantable_rw`). Combining `profile` with `services|forward|host_services|env_set` is a
  config error naming the key ("Wakil only adds grants to a named profile"). No generated policy,
  hence no `session.hostname` token: instance identity in this mode is the supervisor process handle
  (exact) plus the `--ready-fd` line; the owner record additionally stores the supervisor's pid +
  starttime and a `mode:"named"` flag; `reconcileOneLocked`/`verifyInstance` use pid+starttime for
  such records (the token check would say "different session" or "unverifiable" for a profile that
  sets its own hostname). kvr service is not added in this mode (§5).
* **`sandbox.json` is read-only for the agent:**
  1. Executor-level write guard (`protectedPath`): `WriteFile`, `WriteFileBytes`, `DeletePath`,
     `MovePath` (src **and** dst) in `DockerExecutor`, `DirectExecutor`, `BackendExecutor` and the
     docker worktree executor refuse `<root>/.wakil/sandbox.json` and an ancestor `.wakil` directory
     move/delete, after `ConfinePath` canonicalisation. The edit-capability tiers (file tools, edit-tier
     subagents) all go through these executors — the single choke point. Reads stay allowed.
  2. `applyPatch` (worktree children → parent `git apply`) rejects a patch whose `--numstat` touches
     that path (otherwise a child worktree would be a bypass).
  3. hisn backend, generated-profile mode: when the file exists at start it is added to
     `mounts.mask` (reads as empty, writes discarded) so `run_shell` cannot edit it either
     (defence in depth; the approval gate in 4. is the actual control).
     Caveat: a tracked `sandbox.json` shows as emptied to `git` *inside* the sandbox.
  4. **User approval gate (Mashūra R1 B1, folded).** The mask cannot cover a file that does not exist
     yet, and `.wakil` could be renamed, so an agent could author a `sandbox.json` that takes effect at
     the next start. Wakil's state dir is outside every sandbox and is the trust root:
     `<state>/hisn/approved/<ws-key>` holds the sha256 of the last file the *user* approved.
     A non-empty `sandbox.json` whose hash is not approved is a **hard error before anything starts**
     ("`.wakil/sandbox.json` is not approved — review it, then run `wakil sandbox trust`"); an absent or
     empty file needs no approval. `wakil sandbox trust [dir]` prints the file and the computed
     summary and requires an interactive `y` on a tty; there is deliberately **no `--yes`** flag and
     no non-tty path (headless/daemon refuse and say so). An agent that runs it inside any sandbox
     cannot write the state dir; in `direct` mode there is no sandbox to protect anyway.

## 4. Step 3 — risk labels surfaced

Generated-profile flow in `Plan` (runs at executor construction, before any session):
1. Render profile with the project `risk_ack` copied in; `hisn profile validate <p>`.
2. validate fails with `E_RISK_ACK_REQUIRED` / `E_RISK_ACK_MISMATCH` → typed `*RiskAckError{Key:
   "hisn.risk_ack", HisnMessage}`; message: `sandbox: hisn.risk_ack is missing/wrong in
   .wakil/sandbox.json — hisn says: …`. Hard error, Wakil does not start.
3. `hisn profile explain <p>` → highest label `L` + capabilities + notes.
   `L ≠ none` and `risk_ack ≠ L` (this also covers `data`/`kernel-surface`, which hisn itself does not
   require an ack for — Wakil does, per the task) → same typed error.
4. After session start: `hisn inspect --session ID --json` → `risk[]`/`services[]` are the
   authoritative snapshot kept in the status; if the highest label differs from the planned `L` the
   session is stopped with an error (the sandbox must not run at a label the user did not ack).
Named-profile mode: `hisn profile explain NAME` gives `L`; the same ack comparison applies.
0.2.x: label is `none` (no 0.3 surface).

Surfacing:
* New `sandbox.Status` + optional `sandbox.StatusReporter` (hisn backend) and an `exec.SandboxStatuser`
  executor interface; docker/direct report just `backend=…`.
* `wiring.ResolveSandbox(cfg) (cfg, *Plan, banner, err)` is called by the three entry points (TUI, headless,
  daemon) **before** the TUI starts; prints `sandbox: backend=hisn · risk=<label>` plus a second
  line for kvr/default-reason where relevant. `NewExecutorPlanned(cfg, plan)` reuses the plan;
  `NewExecutor(cfg)` stays as a wrapper (tests, other callers).
* `/sandbox` (TUI only, like `/taint`): profile path + source (generated/named), risk, capabilities
  with labels, services (live state from `inspect`), forwards, grants, notes, kvr status.

## 5. Step 4 — kvr as an in-sandbox service

* **Enabled when** backend hisn, profile mode, `kvr_disabled` is false, and either (a) the project
  declares `hisn.services` entry named `kvr` — used as-is, Wakil takes its `socket` as the staging
  socket (missing `socket` → error) — or (b) Wakil's default: binary from `sandbox.hisn.kvr_binary`
  (new config key) or `exec.LookPath("kvr-server")`.
* **Socket containment (R1 B2, folded).** Whether default or project-declared, the kvr socket must
  canonicalise to a path **under `<cwd>/.wakil/`** (and `ready_socket`, if any, equal it); anything
  else (e.g. `$SSH_AUTH_SOCK`, `docker.sock`) is a config error. The host client never dials a path
  outside that directory.
* **Default service** (no env table exists, so env travels via `/usr/bin/env`):
  `exec = ["env","KVR_SOCKET_PATH=<cwd>/.wakil/kvr.sock","KVR_SNAPSHOT_PATH=<cwd>/.wakil/staging.kvr",
  "KVR_SNAPSHOT_ON_SHUTDOWN=true","KVR_MAX_ENTRIES=…","KVR_SWEEP_INTERVAL_SECS=…",
  "KVR_SNAPSHOT_INTERVAL_SECS=…", "<binary>"]`, **no `ready`** (**observed**: a readiness
  timeout fails the *whole session* closed, so a broken kvr must not be able to; Wakil PINGs instead),
  `stop = {TERM, 5}`, `restart = "none"`. Absolute paths throughout (§1). A project-declared
  `kvr` service that sets `ready` keeps hisn's fail-closed semantics — the user's choice.
* **Binary visibility** (path is `EvalSymlinks`-resolved first). The sandbox only sees `/usr /bin /sbin /lib* /opt /nix` read-only (+ project
  `mounts_ro`). A binary outside those ⇒ staging off with reason `kvr binary <path> is not visible inside
  the sandbox (install it under /usr or /opt, or list its directory in hisn.mounts_ro)`.
* **No binary / disabled / named profile / 0.2.x** ⇒ `KVRSocketPath()==""`, staging tools stay off,
  reason in the banner. **No fallback to a host-side kvr.**
* **Lifecycle.** Before spawn, a stale socket file (dial fails) is removed by Wakil (hisn does not);
  `.wakil` is created/checked with the same Lstat guard (no symlinks) so a planted symlink cannot
  redirect the unlink.
  After readiness Wakil `PING`s (len=1, op 3 → 0x10, 5 s, like docker's `waitForKVR`); on failure the
  session continues, staging is disabled for the process with a warning (never a session failure).
* **Confused-deputy guard.** The socket lives in a directory the sandbox can write. The host-side
  client therefore `Lstat`s `.wakil` and the socket before each (re)connect: both must be non-symlinks,
  the socket an `S_IFSOCK` owned by the current uid; otherwise it refuses to dial (a symlink to
  `docker.sock`/ssh-agent would make the host process speak to it). Hook:
  `staging.Client.SetPathGuard`. TOCTOU between check and dial is narrowed, not removed; the protocol
  has no auth and staging content is already untrusted (open point).
* Staging tools' "requires docker mode" message becomes backend-neutral.
* Config: `kvr` force-off changes from "anything but docker" to "direct only" (hisn decides in the plan).

## 6. Step 5 — `profiles/deploy-smaragd.toml`

Shipped in the Wakil repo (not auto-loaded). A hisn profile: `[policy]` host network, `grantable_rw`
for deploy outputs, `[[forward]] docker` and `[docker] tier = "socket"`, **risk_ack lines commented out**
with the exact line to uncomment; header explains: copy to `$HISN_PROFILES`, set `"hisn": {"profile":
"deploy-smaragd", "risk_ack": "root-equivalent"}` in `.wakil/sandbox.json`. Verified offline with the
0.3.1 binary's `profile validate` (once `/srv/out` exists).

## 7. Step 6 — default flip, docker probes, image, docs

* `config.SandboxConfig` gains `BackendDefaulted bool` (`json:"-"`): true when nothing set a backend
  (file, `WAKIL_SANDBOX_BACKEND`, `--sandbox`/`--exec`, `exec_mode`). `LoadConfig` stays pure (resolves
  to docker as today) so no existing test/caller starts probing hisn.
* `config.ApplyDefaultBackend(cfg *Config, avail HisnAvailability)` is called from
  `wiring.ResolveSandbox`; `avail` is produced by `hisn.Probe(ctx, binary)` (binary found via
  `LookPath`, version ≥ 0.3.1, `hisn check` exit 0). Flip ⇒ `Backend=hisn`, re-run the hisn workspace
  and browser/kvr normalisation (`finalizeBackend`, factored out of `LoadConfig`). An explicit
  `--workdir` (docker-only) keeps docker with that reason. A trust pre-check failure (§2) or an
  unapproved `sandbox.json` with a **defaulted** backend is likewise a non-flip reason (stay on
  docker, say why) — a working docker setup never becomes a hard error through the default alone.
  `NewExecutor(cfg)` (wrapper) never probes and treats an unresolved default as docker; the three
  entry points resolve once and hand the *resolved* cfg down (subagents share the executor). Reasons (`hisn not found in PATH`,
  `hisn 0.2.2 < 0.3.1`, `hisn check failed: …`, `explicit workdir`) appear in the banner.
* Docker-only probes stay gated on `backend=docker` (`probeScriptFor` already; `wakil doctor`'s
  container-runtime FAIL becomes a WARN when the resolved backend is hisn, plus a hisn check line).
* Dockerfile hisn pin → **0.3.1** (SHA256 from the release's `SHA256SUMS`: x86_64 `fc502bc1…bc05`,
  aarch64 `be00f17a…bc6a`).
* `docs/sandbox-backends.md` rewritten for the new flow (profile mode, project file schema + one full
  example, risk labels, kvr, deploy profile, default flip), `docs/configuration.md` +
  `config.example.json` for `sandbox.hisn.kvr_binary`, `CHANGELOG.md` `[0.4.0]`, README mentions.

## 8. Tests

* Fake hisn (`fake_test.go`) learns: `profile validate|explain` (TOML-decodes the profile with the
  repo's TOML lib; docker.sock needs `root-equivalent`; host_services need `host-exec`), `run --profile`
  (writes policy + `risk.json`), `inspect --json` `risk[]/services[]`, `--version` from file (0.2.2 vs 0.3.1).
  Unit tests run **both** version paths: 0.2.x → `--policy`, no `--profile`, features error;
  0.3.1 → `--profile`, `profile validate`.
* Unit: profile encoding equality; override strict decoding; risk-ack matrix (missing / mismatch /
  data without ack / ok); trust pre-check; named-profile conflicts; write guard on all four ops;
  patch-touches-sandbox.json rejection; kvr default service args; binary-visibility; path guard;
  default flip matrix (explicit vs defaulted × version × check × workdir).
* Integration (`TestIntegration*`, skip when `hisn check` fails / hisn < 0.3.1): profile start,
  risk refusal (no session created), kvr service staging put/get round-trip through the socket,
  override precedence (project file vs `sandbox.hisn.*` config: `env_set`, `mounts_*` additive, config
  `grantable_rw`/`env_pass` still apply, project services/forwards only from the file).
* Gates: gofmt, go vet, golangci-lint, `go test -race ./...`, `scripts/check_config_docs.sh`.

## 9. Open points carried into the report (not blocking this work)

* Agent-created `sandbox.json` at the next start (§3.4) — needs a hisn-side or user-side trust root.
* One `risk_ack` string per project (labels must agree) — use `hisn.profile` for mixed capabilities.
* Named-profile mode: no mask for `sandbox.json`, no kvr service, identity by supervisor handle.
* kvr protocol has no authentication; guard narrows but cannot close the swap window.
* Release binary ships no `profiles/` dir, so built-in names need `$HISN_PROFILES`.

## 10. Mashūra round 1 — disposition

Reviewers: claude-fable-5.1 answered; gpt-6-astra was rate-limited upstream (no second opinion; no retry
— the budget is one round).

| finding | verdict | action |
|---|---|---|
| B1 agent authors `sandbox.json` → self-ack | BLOCKING, folded | approval hash in state dir + `wakil sandbox trust` (§3.4) |
| B2 kvr socket path arbitrary | BLOCKING, folded | must be under `<cwd>/.wakil/` (§5) |
| N1 mask weaker than described | wording + `Mask` field folded; parent-rename untested → follow-up | |
| N2 `explain NAME` | verified against the 0.3.1 binary: works | §3 |
| N3 named-mode identity | folded (record flag, pid+starttime) | §3 |
| N4 dropping `--network host` | profile carries network; confirm in host integration test | follow-up |
| N5 ack on `data` entries | test row: "data with matching ack passes validate" | §8 |
| N6 readiness kills session | verified in source (fail closed) → default kvr has no `ready` | §5 |
| N7 SO_PEERCRED | follow-up | docs/follow-ups.md |
| N8/N10 symlink/Lstat | folded | §5 |
| N9 default flip / wrapper | folded | §7 |
| N11 wording | §4 plans from a template profile; the per-session file differs only by token/id | |

## 11. Mashūra round 2 — disposition (full diff 5d738b9..HEAD)

Only claude-fable-5.1 completed (gpt-6-astra's reply was VOID: it did not cite the end of the
document, so it is not a review round). No third round, per the brief.

| finding | verdict | action |
|---|---|---|
| B1 staging unreachable (lazy session vs eager client) | BLOCKING, real, folded | client built from the *planned* socket; its connect guard starts the session, requires the service up, vets the socket (`SessionEnsurer`, `KVRPlanner`); test drives App-side client over a lazily started executor, mutation-checked |
| B2 fixed socket + unconditional unlink across processes | BLOCKING, real, folded | per-process socket `kvr-<inst>.sock`; never unlink a socket that answers PING; sweep dead `kvr-*.sock` |
| B3 state dir reachable via grantable_rw / mounts_rw / home_root | BLOCKING, real, folded | `StateGuard` at backend construction + on every rw grant; docs soften the `/dev/tty` claim |
| B4 contract only checked against a hand-written fake | BLOCKING as stated; folded as far as offline can | `TestRealHisn*` run Wakil's generated profiles through the real 0.3.1 `profile validate`/`explain` (ack matrix incl. superfluous acks, full surface, mutation-checked). The *session* tests still need a host (`hisn check` fails here) — listed in the report |
| non-blocking: worktree executor guards, symlink resolution on docker/backend, doctor ignores project-file reasons, vacuous tests, `Load` Lstats leaf only, reconcileNamed with SupPid 0, hisn check run twice, `$HISN_PROFILES` in workspace | not folded | docs/follow-ups.md where relevant |
