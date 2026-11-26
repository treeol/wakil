# hisn 0.6.7 bump — plan

BASE 102f5b5 (v0.8.12 tag, work/0.8.13). Tier 3 (multi-file, protocol-adjacent,
signing/security). One signed commit per block, failing test first. All
release-notes claims re-read from hisn's CHANGELOG; the file wins where the
task brief differs.

## Verified findings

### 1. Every place the 0.6.5 pin lives (from the 0.6.5 bump commits)

The 0.6.5 bump between v0.8.11 and v0.8.12 was four commits: `24af2d7` (plan),
`c891549` (red tests), `de1ec46` (feats: pin + error objects + open-mode
kernel floor), `a8f1f24` (docs/CI/CHANGELOG). `git show de1ec46`/`a8f1f24`
enumerated the surface; the current tree was then re-verified with
`search_files "0.6.5"` plus targeted reads (the discovery dispatch that was
supposed to do this came back `failed — budget/cancelled`, so every finding
below was re-checked inline or by the second/third subagents that did
complete; nothing below rests on the failed child).

Production pin (must move to 0.6.7):
- `internal/sandbox/backend.go:24` `const MinHisnVersion = "0.6.5"` — the
  single source. Comment `:12-23` explains why 0.6.5 is the floor (first
  sound open-mode release: 0.6.1 bypass, 0.6.2 timing gap, Landlock ABI v4 /
  kernel 6.7).
- `internal/sandbox/backend.go:28` `MinHisnReason = "open network mode needs
  hisn 0.6.2+ (Landlock ABI v4 / kernel 6.7)"` — follows `need >= <version>`
  in the refusal text. For 0.6.7 the reason becomes the seat-role signing
  gate (per the task instruction: "Reason string: the signing gate by seat
  role"). Decision: the new reason string names the seat-role gate, and the
  open-mode/kernel floor is still named by the preflight kernel-floor error
  (`checkOpenKernelFloor`, `kernel_floor.go`) — the old reason's kernel claim
  now lives in its own dedicated error, so replacing the reason does not lose
  it. Candidate: `signing is gated by seat role — observer seats are refused
  (hisn 0.6.7)`. Exact wording settled in Block 1, tests pin it.
- Aliases (no numeric literal, no edit needed): `internal/sandbox/hisn/
  backend.go:31 MinVersion = sandbox.MinHisnVersion`;
  `internal/config/config.go:1408 MinHisnVersion = sandbox.MinHisnVersion`;
  `internal/config/config.go:3430 MinHisnDefaultVersion = MinHisnVersion`.
- Refusal wording: `internal/sandbox/hisn/version.go:67` preflight
  `hisn %d.%d.%d is too old, need >= %s%s` and `:56` missing-binary
  `install hisn >= %s`; `internal/sandbox/hisn/probe.go:40-41` probe
  `hisn %s is older than %s%s`. `minReasonSuffix` (`version.go:118-125`)
  appends `MinHisnReason` whenever the floor is `sandbox.MinHisnVersion` —
  it flows from the constant, no literal to update. The `minReasonSuffix`
  comment at `:118` still says "security fix for network = open", stale
  since the 0.6.5 bump; Block 1 fixes the comment too.
- `install.sh:34 HISN_VERSION="0.6.5"` — comment: keep equal to
  `MinHisnVersion`, enforced by
  `TestInstallScriptHisnVersionMatchesMinVersion`.
- `Dockerfile:170-203` — `HISN_VERSION=0.6.5` with per-arch SHA256
  (amd64 `890147848475d0aee57d1dab3effa939a3392426dd6aaae2b174fe1dc5f981aa`,
  arm64 `4bd4db05247e17d61ecea6dc90f00ef813b71cf9182cda8809ec428bae4bf18a`),
  comment `pinned to v0.6.5`. Enforced by
  `TestDockerfileHisnVersionMatchesMinVersion` (version only, not the
  checksums). The 0.6.7 checksums are copied from the downloaded
  `SHA256SUMS` in the capture step (amd64
  `d438caaedbd5b56d0b7e7cc34614fa36d5ff1eecf6b3cabd644a67b7be17cb0e`,
  arm64 `e28b2342332b87269f3b03048dd9d49c16bd375632cec6735ea204b7d4fbe7cd`)
  — full 64-hex strings, never transcribed from memory. Note on
  `install.sh`: a `0.6.5` hisn earlier on PATH no longer counts as "keep",
  so the installer drops 0.6.7 into `$DIR/hisn` and the old binary can
  still shadow it on PATH — the CHANGELOG entry notes the upgrade path.
  Docs/CI: `.github/workflows/ci.yml:336` greps `hisn 0.6.5`;
  `AGENTS.md:16` (`config.MinHisnVersion`); `CONTRIBUTING.md:10,38`;
  `README.md:36`; `docs/README.md:25`;
  `docs/configuration.md:572,578,595`; `docs/sandbox-backends.md:7,20,25-26,
  222,252-254,950,953`; `scripts/host_verify.sh:332` label `(hisn 0.6.5)`.
  CHANGELOG: the 0.6.5 entries (`CHANGELOG.md:15` under `[Unreleased]` and
  the released `:22`) stay as history; Block 3 adds a NEW `[Unreleased]`
  entry for the 0.6.7 floor.

Test literals that must move with it (Block 1):
- `internal/config/default_backend_test.go:151,155`
  (`HisnAvailability{Version: "0.6.4", Reason: "hisn 0.6.4 is older than 0.6.5
  (open network…)"}` → 0.6.6 refused / 0.6.7; note hisn 0.6.6 was never
  released — a `0.6.6` version string in a test literal is still a valid
  "older than" example, but prefer `0.6.5` as the refuses-an-older-hisn
  example since 0.6.5 is the real previous release; decide in Block 1 and
  keep found+required naming).
- `internal/sandbox/hisn/backend_test.go:90,102,117` (fake `version` file
  `[]byte("0.6.5")` → `0.6.7`).
- `internal/sandbox/hisn/fake_test.go:80` (fake `--version` default
  `v = []byte("0.6.5")` → `0.6.7`).
- `internal/sandbox/hisn/integration_test.go:1023,1036-1037`
  (`VersionAtLeast(v, [3]int{0,6,5})` gate + skip text → 0.6.7). The
  open-network gate is a feature gate, not the minimum; it moves because
  0.6.5 no longer meets the floor, not because open mode changed in 0.6.7.
- `internal/sandbox/hisn/knownhosts_mount_test.go:37` (want list
  `need >= 0.6.5`, reason substrings); `internal/sandbox/hisn/probe_test.go:
  45`; `internal/sandbox/hisn/regress_test.go:212,218`
  (`"hisn 0.6.5"` → `0.6.7`).
- `internal/sandbox/hisn/profile_test.go` (`:51,257,292,305,330,350,375,
  380,391,419,444,482,499,516`, `:90` comment), `kvr_test.go:188`,
  `selfack_test.go:88`, `session_v2_test.go:179,466` — `setVersion(t, fe,
  "0.6.5")` / version-file `0.6.5` literals → `0.6.7`.
- Completeness check (round 1): the second literal sweep
  (`grep -rn 'VersionAtLeast\|0, 6,\|0\.6\.4\|0\.6\.5\|0\.6\.6' internal/
  --include='*.go'`) found no other `[3]int` or version literal in
  production or tests — every remaining hit is either an IP literal
  (`10.6.5.4`), a feature comment (E_TOO_LARGE / hisn-error), a
  sub-floor feature gate (`0,2,1`, `0,3,1`, `0,3,2`, `0,4,1`), or already
  listed above. No `capabilities` parser exists to break.
- `internal/sandbox/hisn/profile_test.go:90` and `fake_test.go:175`
  comment lines ("hisn 0.6.1 style refusal of network = open" at
  `fake_test.go:175` was already updated to 0.6.5 in the last bump) — the
  comments move with the literals.
- The `0.6.5 E_TOO_LARGE` / `hisn-error/v1 recorded from real 0.6.5`
  comments (`exec.go:170,211,238,275,340,414,416`, `ctl/ctl.go:67`,
  `hisn_error.go:10,48`, `hisn_error_test.go:9-17`, `fake_test.go:175`) are
  FEATURE comments, not min pins: 0.6.5 remains the version that introduced
  E_TOO_LARGE, and 0.6.7 does not change its wire shape (CHANGELOG: no wire
  change). They stay as-is.

Not pin sites (verified, no change): `internal/config/config_test.go:874,894`
(`10.6.5.4` is an IP literal); `docs/long-session-mode.md:330` and
`CHANGELOG.md:502-503,525` (Wakil 0.6.5 release history);
`bench/results/*.log` (DO-NOT-ALTER run logs); `.tmp/**` worktrees.

### 2. What hisn's CHANGELOG says for 0.6.7 (re-read via tools dispatch)

Fetched `https://raw.githubusercontent.com/treeol/hisn/master/CHANGELOG.md`
(`## [0.6.7] - 2026-11-26`), plus the tag's `SHA256SUMS` asset from the
GitHub API. "0.6.6 was not released; 0.6.7 follows 0.6.5." Verbatim/close:
- No wire change: no new control request, `PROTOCOL_REVISION` stays 4;
  `compat_063.sh` passes; 0.6.3 and 0.6.5 clients keep working against a
  0.6.7 supervisor. (Matches the brief.)
- Added: `hisn journal --after-wseq N --wait [SECS]` (with `--json` and
  `--session`); new exit codes 6 (`E_WAIT_TIMEOUT`) and 7
  (`E_WAIT_SESSION_ENDED`); exit 5 unused/documented (exit 4 = gap).
- Added: `hisn run --ssh-agent-socket PATH` and the `[ssh] socket` profile
  key (CLI wins over profile, default off; add/remove refusal unchanged).
- Added: journal records `ssh/sign` and `ssh/sign-summary`. "Every sign
  request journals one ssh/sign record (key fingerprint, seat, exec id,
  result — never the data being signed, never the signature)."
- Added: `hisn capabilities --json` gains `journal-wait` and
  `caller-agent-socket` after the existing 11 names (13 total).
- Changed: "The ssh-agent proxy now gates signs by seat role (owner/member
  sign, observer is refused with SSH_AGENT_FAILURE), on both the env path
  and the caller-socket path. This reverses the old allowlist-only behaviour
  for observers."
- Changed: "hisn profile validate --json now reports **all** post-parse
  errors (deterministic, dedup'd order) instead of only the first." Text
  path stays fail-fast; ok/exit unchanged; TOML parse failure still exactly
  one error.
- Changed: "check_host_agent error text now names the path actually checked
  (a caller socket is no longer reported as SSH_AUTH_SOCK)."
- Fixed: late exec-signal for a detached exec's drain could be missed in
  the journal cause (finalised under the exec_table lock).

Release assets (GitHub API `/repos/treeol/hisn/releases/tags/v0.6.7`):
`hisn-x86_64-linux-gnu` 6768280 B sha256
`d438caaedbd5b56d0b7e7cc34614fa36d5ff1eecf6b3cabd644a67b7be17cb0e`;
`hisn-aarch64-linux-gnu` 6187136 B sha256
`e28b2342332b87269f3b03048dd9d49c16bd375632cec6735ea204b7d4fbe7cd`;
`hisn-x86_64-linux-musl` 6734504 B sha256
`8c2acd94dcc6ef9498a8d3e2adacc695fc379952dd91a29cf61ef436a7d3fc4f`;
`SHA256SUMS` 266 B (same sha256 for the file). The Block 1 Dockerfile
checksums come from this SHA256SUMS (amd64 `d438caaa…7cb0e`, arm64
`e28b2342…f7cd`). Wakil's `install.sh` downloads per-arch from the same
release; nothing to change there beyond `HISN_VERSION`.

### 3. Where Wakil uses the ssh-agent proxy and signing; seat role

- Mechanism: the hisn `[ssh]` profile table, never an explicit socket
  forward. `profile.go:447-449` (`profileFor`) emits
  `pf.SSH = &sshTOML{Agent: true, Keys: …}` only when `s.SSHAgent` and at
  least one key fingerprint is allowed; `sshTOML` (`:328-331`) serialises
  `[ssh] agent = true` + `keys`. Global config
  `sandbox.hisn.ssh_agent` (bool, default false) + `ssh_keys` allowlist
  (`internal/config/config.go:1440-1449`); a project file cannot enable it.
- The host supervisor receives the agent through the env path:
  `cmd.go:67-77` — `command(withPass)` appends `SSH_AUTH_SOCK` to the
  passed env, and only `startSupervisor` uses `withPass=true`
  (`cmd.go:274`). `SSH_AUTH_SOCK` itself is reserved: `config.go:3604-3605`
  rejects it in `env_pass` so the host path cannot leak into the session.
  `preflightSSHAgent` (`version.go:84-108`) requires `SSH_AUTH_SOCK` set
  and a socket before a session starts. Backend walls:
  `backend.go:287-298` (agent with empty allowlist refused; signing
  key without agent refused).
- Session start: `session.go:488-505` (`sessionArgs`) builds
  `hisn run --detach -v --session-id <id> --profile <p> --cwd <cwd>
  --ready-fd 3 --name <n> [--grant …]` — **no seat/role flag, no
  `--seat-token-file`, no per-exec role**. Wakil starts the session itself,
  as the owner; the execs Wakil runs inside it ride the owner seat the
  session was started from.
- The seat-role verdict is PROVISIONAL (Mashūra round 1, unanimous).
  No Wakil path is assigned an observer seat — `sessionArgs` carries no
  seat flag (`session.go:488-505`), and the only seat-shaped data in the
  tree is the additive `seats` count in `ls --json` (an ignored field, not
  parsed, `ls_060_test.go:12-33`). The session is started by Wakil itself,
  so it is the seat originator. BUT which role hisn 0.6.7 assigns a
  tokenless client — and whether a *later* exec client rides the owner's
  seat or arrives as its own client (the adopted-session/reconcile path
  runs `hisn run` again under a restarted client) — is decided inside
  hisn, not verifiable from Wakil's code. So the claim "Wakil signs as
  owner" is **unverified** until a real 0.6.7 `ssh/sign` journal record
  for a Wakil-started session shows an owner/member seat with
  `result: ok`. That record is the Block 2 acceptance proof (and the
  early warning: without it, a seat assignment change would only surface
  as `SSH_AGENT_FAILURE` on the user's first signed commit, with nothing
  in Wakil naming the cause). Block 1's pin step therefore records the
  finding as provisional, not verified.
- Existing coverage: `TestIntegrationSSHAgentForwarded`
  (`integration_test.go:206-485`) already registers a throwaway ed25519 key +
  throwaway `ssh-agent -s -a <temp socket>`, allowlists only its fingerprint,
  starts a session via the backend (`New` + `Preflight` + `Start`, the way
  Wakil starts it), and performs a signed `git commit -S`-equivalent
  (command-scope `commit.gpgsign` via `irunSign`) verified on the host side
  with `%G?`/`%GK`. Block 2 extends THIS test instead of writing a second
  near-duplicate: it gates at `>= 0.6.7` (today's gate is `>= 0.3.3` at
  `:299`) and asserts the resolved binary's `--version` exactly before
  `Start`. **The journal is not currently readable in the test**:
  `ls_060_test.go:11,106` documents additive `ls --json` fields Wakil does
  NOT parse, and no `hisn journal` reader exists in the tree. The Block 2
  assertion invokes journal through `b.output(..., "journal", "--session",
  <id>, "--json")` so it inherits the session's `XDG_RUNTIME_DIR` from
  `b.baseEnv()` — `clientEnvAllow` (`cmd.go:27-33`) carries
  `XDG_RUNTIME_DIR`, so an isolated runtime dir set by the host command
  reaches hisn. **On `>= 0.6.7` a journal-read failure or a missing/
  malformed `ssh/sign` record is `t.Fatalf`, never `t.Skip`** — once
  `Preflight` passes, 0.6.7 always supports journal. Skip stays legitimate
  only for the host-capability gate (`hisn check` / user namespaces).
  Assertions on the `ssh/sign` records: at least one record with
  `result: ok`, the throwaway fingerprint, and a seat of owner/member
  (the direct evidence for the provisional seat verdict above); no record
  with a failed result; and the raw journal bytes must not contain the
  signature blob or the commit payload. "Exactly one" is NOT asserted —
  git may request a sign more than once, and `ssh/sign-summary` records
  also exist. Field/key names and the exact seat value are pinned from a
  real captured sample, not invented here. If the session tests skip here
  on the `hisn check` gate, the assertions skip with them and the host
  command covers them — the final report then states the seat verdict and
  the journal-visibility claim as **unverified**, not covered-implied.
- The task-brief note: the brief says "via `--ssh-agent-socket` or the
  existing `SSH_AUTH_SOCK` path, whichever Wakil uses". Wakil uses the
  `SSH_AUTH_SOCK` env path (`cmd.go:73`); the new caller-socket flag is NOT
  adopted (Open point). The test uses the existing path.

### 4. Error-text matches (esp. `check_host_agent`)

- **No `check_host_agent` / `E_HOST_AGENT` matcher exists in Wakil.**
  `search_files "check_host_agent"` and `"E_HOST_AGENT"` over the whole
  tree (and `internal/`): no matches. The 0.6.7 text change therefore has
  NO matcher to update. Its real surface is `hisn run` supervisor stderr —
  `preflightSSHAgent` (`version.go:84-108`) checks `SSH_AUTH_SOCK` in Go
  before any run, and `hisn check` (`version.go:71`) never validates it;
  hisn's own check surfaces through `withSupervisorStderr`
  (`session.go:696-716`) at Start. The Block 1 test therefore records the
  real 0.6.7 failure bytes and runs them through the actual surfaces that
  could carry them — `withSupervisorStderr` and the `hisn check` tail —
  asserting the recorded wording (path actually checked, never
  `SSH_AUTH_SOCK` when a caller socket was set) reaches the user intact.
  It captures only with `SSH_AUTH_SOCK` unset/`--ssh-agent-socket` pointed
  at a nonexistent path under `.tmp/`, never a real user agent socket.
  It is a fixture-and-surface test, not a matcher test.
- Wakil matches error TEXT only through: `parseHisnError`
  (`hisn_error.go:31` — whole-output `hisn-error/v1` schema/code match,
  fixtures recorded from real 0.6.5, `hisn_error_test.go:9-15`);
  `classifyControlError` substrings `E_NOT_GRANTABLE`/`E_PATH_FORBIDDEN`
  (`backend.go:852-865`); `validateError` first-claim matchers `E_RISK_ACK_`
  and `network`+`open` (`session.go:590-602`); `rootfsError` `rootfs.path` +
  `No such file` (`session.go:614`); `openRefusalLine` `network = "open"`
  (`session.go:722`); `isConnRefused` case-insensitive `connection refused`
  (`diag.go:76-78`); `E_TOO_LARGE` via `parseHisnError` (`exec.go:224,282,
  413`) and `ctl.Error.IsTooLarge` (`ctl/ctl.go:66-70`).
- None of these texts changed in 0.6.7 (no wire change; `profile validate
  --json` all-errors only affects the `--json` path Wakil does not call —
  confirmed: `prepareLaunch`/`profile validate` is invoked without
  `--json`; `validateError`'s `ownHisnError` fallback parses a whole-output
  `hisn-error/v1` object and is indifferent to how many errors hisn reports
  internally). So Block 1's error-text work is: recorded-0.6.7-outputs-parse-
  as-before tests (the existing `hisn_error_test.go` fixtures re-recorded
  against the 0.6.7 binary in the pre-capture and re-checked) plus the
  `check_host_agent` recorded-output/surface test above. No matcher code
  changes.
- Strict-parse surfaces to pre-capture (Mashūra round 1): `ls --json` on an
  empty runtime dir, `inspect --json`, and the exact `--version` format
  (`install.sh`'s `sed 's/^hisn \(...\)'` depends on it) — `lsSchemas`
  rejects unknown schemas, so a silent schema bump would break every start.
  Wakil parses no `capabilities --json` (repo-wide search: only LSP /
  Docker / exec-capability hits; no hisn `capabilities` caller), so the
  11→13 feature-name growth has no parser to break.

## Mashūra round 1 (2026-11-26) — folded in

BLOCKING (binding): seat verdict provisional until a captured `ssh/sign`
record shows owner/member + `result: ok` (§3, Block 2); the signing test
gets a hard `>= 0.6.7` gate + exact-version assert and fail-not-skip on the
journal step; the `ssh/sign` assertions include the seat value; the
`check_host_agent` test is redirected to `withSupervisorStderr`; a
capture-first step precedes Block 1; this plan states how its own commits
get signed (host git + the user's own ssh key, outside any Wakil session —
never through the himsn-wrapped Wakil; the bumped preflight is never on
that path); the seat verdict is reported unverified if the host run is out
of reach. Non-blocking: full 64-hex checksums only (abbreviations dropped);
both arch assets downloaded and `sha256sum -c`-verified; `SHA256SUMS`
never self-verifies; `MinHisnReason` keeps both reasons (seat-role gating
AND the open-mode/kernel floor) as one string; CHANGELOG states the 0.6.7
floor is a policy floor with no wire change; the `backend.go:12-23` and
`Dockerfile:170-191` rationale comments are rewritten; the plan's commit
sequence is one red-test commit, then per-block commits; a second literal
sweep (`VersionAtLeast`, `[3]int`) closes the `[3]int{0,6,5}` gap;
pre-capture includes `ls --json`/`inspect --json`/`--version` shapes; the
signing test's host-side `git log` will run under
`GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1` so it never reads the
real user gitconfig.

### Pre-capture (DONE 2026-11-26, before Block 1 — read-only, uncommitted)

Download `hisn-x86_64-linux-gnu` + `hisn-aarch64-linux-gnu` +
`SHA256SUMS` to `.tmp/hisn-0.6.7/`, `sha256sum -c` both binary assets
(`SHA256SUMS` cannot verify itself — it is the check source, not a
checked file). CHMOD +x in `.tmp` only. Never PATH-install, never touch
`$XDG_RUNTIME_DIR/hisn`, `/usr/local/bin/hisn` or `~/.local/bin` — and
use an isolated `XDG_RUNTIME_DIR=$PWD/.tmp/rt-067` (mode 0700, short
path) so `ls --prune` and the TestMain sweep never touch the live
runtime dir (`clientEnvAllow` carries `XDG_RUNTIME_DIR`, so it does
reach hisn). Record `--version`, `capabilities --json` (13 names),
`ls --json` on an empty runtime dir, `inspect --json`, one real
`hisn-error/v1` sample, the `check_host_agent` failure text
(`SSH_AUTH_SOCK` unset, caller socket at a nonexistent `.tmp/` path),
and a `journal --json` sample. The Dockerfile checksums, the Block 1
fixtures, and the `ssh/sign` schema all come from this capture — never
from plan text.
- Capture results (VERIFIED): all three binary assets `sha256sum -c`
  OK (`hisn-x86_64-linux-gnu` 6768280 B `d438caae…7cb0e`,
  `hisn-aarch64-linux-gnu` `e28b2342…f7cd`,
  `hisn-x86_64-linux-musl` `8c2acd94…fc4f` — full strings in §2;
  the musl asset was downloaded too, since `SHA256SUMS` lists it and
  `-c` fails on a listed-but-missing file).
  `--version` → `hisn 0.6.7` (the format `install.sh`'s sed expects).
  `capabilities --json` → `hisn-capabilities/v1`, 13 features:
  the 11 existing names plus `journal-wait` and `caller-agent-socket`,
  `protocol_revision: 4`, `declared_protocol_version: 3`,
  `version: "0.6.7"` — network-* features `available: false` with
  `unshare(NEWUSER): Operation not permitted (os error 1) (errno 1)` in
  this sandbox (user namespaces blocked here).
  `ls --json` on an empty runtime dir →
  `{"schema":"hisn-ls/v2","sessions":[]}` (works without namespaces).
  `profile validate --json` probes: the all-errors change could NOT be
  exercised — every multi-defect profile (bad network variant + unknown
  key, bad type + unknown key, two unknown sections) still returns
  exactly ONE `E_PROFILE_PARSE` error because the TOML deserializer
  fails fast; `ok: false`, `exit 125`, schema
  `hisn-profile-validation/v1`. A fully valid profile (incl. an
  `[ssh] socket` key, which validates — the new key is real) returns
  `ok: true`, `exit 0`, `errors: []`. The text path likewise fails fast
  and names the first defect. So in 0.6.7 as shipped, a second
  post-parse error to report was never produced in my probes — the
  multi-error behaviour remains changelog-trust, and Wakil's
  first-claim `validateError` matchers are unaffected.
- Not captured (needs a running session; user namespaces blocked here):
  `inspect --json` on a live session, `journal --json` / `ssh/sign`
  record shape, the `check_host_agent` failure text, a real
  `hisn-error/v1` sample. The first three are Block 2 host-run
  captures; the `check_host_agent` text is attempted under an isolated
  `XDG_RUNTIME_DIR` with the caller socket pointed at a nonexistent
  `.tmp/` path (it may still fail earlier at namespace setup — if it
  cannot be captured, the Block 1 test is recorded as fixture-only).
  No `hisn run` was attempted against the live runtime dir at any
  point.

### Block 1 — pin and compatibility

Red commit first, then the pin (one signed commit each, per the task
procedure: failing test first).

1. Red tests: a 0.6.5-found refusal test naming found `0.6.5` + required
   `0.6.7` (Preflight + Probe layers, same style as
   `TestPreflightRefusalNamesSecurityReason`/`TestProbe`), the reason-string
   test (new `MinHisnReason`), `check_host_agent` recorded-output/surface
   test (see §4), recorded-0.6.7 `hisn-error/v1` parse tests on the existing
   helpers, the signing test's `>= 0.6.7` gate + journal assertion (Block 2
   code, red here only insofar as it needs the pin to be meaningful — it
   lands as green in the Block 2 commit).
2. Pin commit: `MinHisnVersion` 0.6.5 → 0.6.7 + `MinHisnReason` rewritten —
   the seat-role signing gate AND the open-mode/kernel floor in one string
   (a 0.6.0 user must still read why: the open-mode bypass and the kernel
   6.7 / Landlock ABI v4 floor stay named; the new gate is the 0.6.7
   reason). Rewrite the `backend.go:12-23` and `Dockerfile:170-191`
   rationale comments for 0.6.7; fix the stale `minReasonSuffix` comment
   (`version.go:118`). `install.sh` `HISN_VERSION`; `Dockerfile`
   `HISN_VERSION` + checksums from the capture; CI smoke grep; every test
   literal in §1; `scripts/host_verify.sh` label.
3. Seat-role finding: **provisional** (see §3). Report with the file:lines
   there; no seat logic change.
4. Error texts: no matcher changes (none match `check_host_agent`); the
   recorded-output/surface tests above are the "update the match + test"
   step — the match list is empty, the tests prove it.
5. `go build ./...`, `gofmt`, package tests; signed commits.

### Block 2 — integration vs real 0.6.7 + signing test

- The binary is the pre-capture's `.tmp/hisn-0.6.7/` asset (checksums
  already verified there; re-run `sha256sum -c` for the record). Never
  PATH-install, never touch the live hisn binary, no sudo.
- Run: `XDG_RUNTIME_DIR=$PWD/.tmp/rt-067
  WAKIL_HISN_BIN=$PWD/.tmp/hisn-0.6.7/hisn-x86_64-linux-gnu go test
  -count=1 -v -timeout 10m -run 'TestIntegration|TestRealHisn'
  ./internal/sandbox/hisn/` — report exact command + per-test
  PASS/SKIP/FAIL. When `WAKIL_HISN_BIN` is set, a Preflight failure is a
  test failure, not a skip (new `WAKIL_HISN_REQUIRE=1` switch or an
  unconditional hard fail when the env var is set — settle in the block).
  In this sandbox user namespaces are blocked: expect the session-level
  `TestIntegration*` to SKIP at the `hisn check` gate (same as the 0.6.5
  Block 3 result) — say so and give the host command. A signed commit
  whose signing/journal assertions never executed here is reported as
  **unverified** in the final report, not covered-implied.
- Extend `TestIntegrationSSHAgentForwarded` (see §3): `>= 0.6.7` gate,
  exact `--version` assert, `ssh/sign` journal assertions with the seat
  value, fail-not-skip on the journal step. The host-side `git log`
  verification runs with `GIT_CONFIG_GLOBAL=/dev/null
  GIT_CONFIG_NOSYSTEM=1` (pre-existing gap: today it reads the real user
  gitconfig).
- One signed commit (test-only changes + the assertions).

### Block 3 — docs + CHANGELOG

- `README.md:36`, `docs/configuration.md`, `docs/sandbox-backends.md`,
  `docs/README.md`, `AGENTS.md:16`, `CONTRIBUTING.md`: pin 0.6.7, the
  seat-role signing gate as the reason (both reasons, per §4/round 1),
  `ssh_agent` still env-path only (caller socket NOT adopted;
  `--ssh-agent-socket` stays an Open point). `scripts/host_verify.sh:332`
  label → 0.6.7.
- `CHANGELOG.md [Unreleased]`: new minimum 0.6.7, why (signing gated by
  seat role — owner/member may sign, observer refused `SSH_AGENT_FAILURE`),
  no wire change (protocol revision 4; 0.6.5 clients keep working against a
  0.6.7 supervisor — this is a policy floor, not a protocol break), the
  `install.sh` PATH-shadow note for 0.6.5 users upgrading. One signed
  commit.

### Open points (NOT implementing — one line each)

- `hisn journal --after-wseq N --wait [SECS]` (exit 6/7): a blocking "wait
  for the next journal record" poll — Wakil could wait for session-stop
  causes and `mgr/exec` drain events without `ls` polling.
- `hisn run --ssh-agent-socket PATH` / profile `[ssh] socket` (off by
  default): the caller-socket path, where the session's agent is reached
  without exporting `SSH_AUTH_SOCK` into the supervisor's env — Wakil
  currently uses the env path only.
- Journal `ssh/sign` / `ssh/sign-summary` records (fingerprint, seat, exec
  id, result; never signed data): auditable signing provenance per session,
  readable after the fact without a proxy sidecar.
- `capabilities --json` (`journal-wait`, `caller-agent-socket`): feature
  detection for the two new behaviours, if Wakil ever gates them.
- `profile validate --json` all-errors: machine-readable multi-error
  profile validation, replacing `validateError`'s first-claim substring
  matchers.
- Late exec-signal journal-cause fix: no action — detached execs still
  aren't used by Wakil (0.6.5 Open point).

### Open risks

- The seat-role verdict is provisional (§3): hisn 0.6.7's seat assignment
  for a tokenless exec client is binary-side and unproven from Wakil's
  code. The Block 2 `ssh/sign` journal assertion (seat = owner/member)
  is the early warning; until it passes on a userns-capable host, report
  the claim as unverified. `inspect --json` on a live session, the
  `ssh/sign` record shape, and the `check_host_agent` text are all
  session-dependent captures this sandbox prohibits — the pre-capture
  could not produce them (namespaces blocked), so all three are
  Block 2 host-run work.
- `profile validate --json` all-errors: pre-capture probes show the
  0.6.7 binary still fails fast on every dual-defect profile tried
  (TOML parse errors preempt post-parse checks), so the multi-error
  behaviour is unchanged in practice — recorded from real output, not
  assumed, and Wakil's `validateError` is unaffected either way. If a
  future hisn ever does report two errors, Wakil does not call `--json`
  and its first-claim matchers keep working.
- `check_host_agent` past-workflow scripts/do-not-alter verification: the
  recorded-0.6.7-output test pins the new wording at passthrough level; if
  hisn renames it again, the test fails loudly (by design).
- `TestIntegrationStopKillsSleeper` (`integration_test.go:560`) runs bare
  `pgrep -f "sleep 300"` + SIGKILL on the host: on a host run it can kill
  an unrelated `sleep 300` process. Pre-existing, not touched by this
  bump — recorded here as an Open risk, not fixed in this session.
- The self-signing question (Mashūra round 1): after Block 1, Wakil
  refuses hisn < 0.6.7. This session's own commits are signed with host
  git + the user's own ssh key outside any Wakil session; the bumped
  preflight is never on that path, and the live hisn binary is never
  upgraded or touched by this session.
- The full session-suite run against 0.6.7 needs a host with user
  namespaces: exact host command in Block 2 / final report.
- DO-NOT-ALTER targets: live hisn/wakil binaries untouched (binary only in
  `.tmp/`, explicit path); signing test uses throwaway key + throwaway
  agent only (`TestIntegrationSSHAgentForwarded` generates both in temp
  dirs); `context_modules` stays off — tool array + preamble
  byte-identical to v0.8.12 (verified by
  `TestContextModulesOff_ByteIdentical` /
  `TestContextModulesOff_PreambleByteIdentical`); no real user config, key,
  or `SSH_AUTH_SOCK` ever read or forwarded (the test unsets/sets a
  throwaway agent); bench files unchanged; no real model requests except
  Mashūra; staged by explicit path; no force-push/rebase/tags; untracked
  notes and `.tmp/**` stay untracked; domain `wakiil.pro`.
