# hisn 0.6.5 bump — plan

BASE 2a0f8e7 (work/0.8.12). Tier 3 (multi-file, protocol, security). One signed
commit per block, failing test first. All release-notes claims VERIFIED via
GitHub API unless marked UNVERIFIED.

## Verified findings

### 1. Current pin: 0.6.1 (user belief correct, current code)
- Single source: `internal/sandbox/backend.go:19`
  `const MinHisnVersion = "0.6.1"`, reason `:23`
  `MinHisnReason = "security fix for network = \"open\""`.
- Aliases: `internal/config/config.go:1408 MinHisnVersion = sandbox.MinHisnVersion`;
  `internal/sandbox/hisn/backend.go:30 MinVersion = sandbox.MinHisnVersion`.
- Pin mirrors (enforced by `internal/sandbox/hisn/install_script_test.go:13`
  `TestInstallScriptHisnVersionMatchesMinVersion` and `:30`
  `TestDockerfileHisnVersionMatchesMinVersion`, both require equality with
  `MinVersion`): `install.sh:34 HISN_VERSION="0.6.1"`,
  `Dockerfile:198 HISN_VERSION=0.6.1` (+ SHA256 lines). CI smoke:
  `.github/workflows/ci.yml:336` greps `hisn 0.6.1`.
- Compat check: `internal/sandbox/hisn/version.go:49 Preflight` —
  `--version` >= MinVersion else `hisn X.Y.Z is too old, need >= <min>` +
  `minReasonSuffix` (`:113`, only when min == sandbox.MinHisnVersion);
  `internal/sandbox/hisn/probe.go:15 Probe` — same floor for the DEFAULT
  decision (`hisn %s is older than %s%s` at `:41`).
- Stale-0.6.1 literals that must move with it: `internal/config/default_backend_test.go:151`
  (0.6.0 refuses), `knownhosts_mount_test.go:27,36`, `probe_test.go:45`,
  `integration_test.go:1023,1037` (`VersionAtLeast {0,6,1}` gate), fake default
  `fake_test.go:80`, `AGENTS.md:16`, `CONTRIBUTING.md:10,38`, `README.md:36`,
  `Dockerfile:170,175`, `docs/configuration.md:571,577,592,594`,
  `docs/sandbox-backends.md:7,20,25-26,222,249-250,607-608,938,941`,
  `docs/README.md:25`, `scripts/host_verify.sh:332`.

### 2. How the last bump (0.6.0 → 0.6.1) was done: commit c2e8e79e
`fix: require hisn 0.6.1 (security fix for open network mode) and show hisn's
refusal of open sessions` touched 28 files (221+/78-): CI smoke, AGENTS.md,
CHANGELOG.md, CONTRIBUTING.md, Dockerfile (+ new SHA256s), README.md,
docs/README.md, docs/configuration.md, docs/sandbox-backends.md, install.sh,
config.go, backend.go (both), 11 hisn test files (fake default + every fake
version pinned 0.6.0→0.6.1, `TestBelowFloorRefusedNamingMinimum` gained 0.6.0,
`TestPreflightRefusalNamesSecurityReason` + `TestProbe` reason + new
`network_fail_test.go` refusal tests), probe.go (reason suffix), session.go
(open-refusal lead via `withSupervisorStderr`/`openRefusalLine`), version.go
(`minReasonSuffix`), scripts/host_verify.sh. Pattern: bump constant+reason,
update every fake/literal, add one refusal test per layer, docs+CHANGELOG.

### 3. hisn release notes 0.6.2–0.6.5 (GitHub API, VERIFIED)
- 0.6.2: closes the open-mode timing gap (supervisor executes INET
  listen/connect/bind itself; Landlock TCP rules refuse at kernel time;
  fast-open EOPNOTSUPP; io_uring denied). **New kernel requirement: Landlock
  ABI >= 4 (Linux 6.7)**; `hisn check` reports it. TCP fast-open no longer
  supported; SO_PEERCRED pid 0 on AF_UNIX accepted sockets. No protocol note.
- 0.6.3: control protocol stays revision 3, all additive. `--json` + one
  `hisn-error/v1` object on stderr for `run --detach` (`hisn-run/v1`), `stop`,
  `seat add|ls|set-role|revoke`, `rootfs list` (exit status = plain command's).
  `journal --after-wseq` / `--info` (`hisn-journal-info/v1`), `ls --json`
  gains `journal.first_wseq/last_wseq` + `supervisor_pid` (null pre-0.6.3);
  `--seat-token-file -` (stdin credential, ≤4096 B); stop causes in journal;
  `docs/EXTERNAL-CALLERS.md`. Security: exotic AF_ALG etc. refused in open mode.
- 0.6.4: control protocol revision 4; declared version stays 3 so a 0.6.3
  supervisor + 0.6.3 CLI keep working. `exec --detach` (≤32/session,
  `hisn-exec-start/v1`), `exec-status` / `exec-output` / `exec-signal`;
  legacy `signal` op refuses detached execs (E_BAD_REQUEST). `ls --json`
  gains `incarnation` + detached execs. Journal cursor cached
  (`journal.idx.<inode>`), `journal/unreadable` records. E_UNSUPPORTED for
  detached ops against older sessions.
- 0.6.5: **no wire change** — protocol revision stays 4, declared version
  stays 3; every command with no new option sends byte-identical requests to
  0.6.4; 0.6.4/0.6.3 clients keep working. `exec --no-tty --stdin-limit`
  (1..1GiB, default /dev/null unchanged; overflow = exit 125 + `E_TOO_LARGE`
  naming the limit; journal records it; never silently truncated);
  `capabilities --json` (`hisn-capabilities/v1`: version, protocol revision,
  declared version, 11 stable feature names with `available` + `reason` when
  false; works without a session); `profile validate --json`
  (`hisn-profile-validation/v1`, codes E_PROFILE_PARSE/E_PROFILE_VALUE).
  Changed: exec request >65536 B refused pre-send (exit 125 + `hisn-error/v1`
  E_TOO_LARGE naming the limit); supervisor refusal now reaches client as an
  error reply, not a closed connection. Log-prune fix; detached
  `exec/request` carries `detach: true`; `exec --detach` refuses `--no-tty`.
- 0.6.5 assets: `hisn-x86_64-linux-gnu` 6702008 B
  sha256:890147848475d0aee57d1dab3effa939a3392426dd6aaae2b174fe1dc5f981aa;
  `SHA256SUMS` 266 B (file digest
  sha256:3e7a43b170c2d09a8f4a25c65ecac95de1bf83216df2e50c9d1b1f3def486efb).
- 0.6.5 assets: `hisn-x86_64-linux-gnu` 6702008 B
  sha256:890147848475d0aee57d1dab3effa939a3392426dd6aaae2b174fe1dc5f981aa;
  `SHA256SUMS` 266 B (same sha256 for the file). glibc, Ubuntu 22.04-built
  (glibc ≥ 2.35). aarch64 + musl variants exist.

### 4. What Wakil does with hisn today (all VERIFIED in code)
- **Handshake/protocol revision**: nothing. Zero occurrences of
  revision/protocol-version in `internal/sandbox/hisn/ctl/ctl.go` (657 lines):
  requests are `{op, …body, client?}` JSON lines, responses `{ok, error,
  message}`; `parseResponse` at `:189`. No revision check anywhere:
  `grep -rn 'protocol.*revision|revision|declared' internal/sandbox/hisn/ctl`
  = none. So 0.6.4/0.6.5 revision 4 needs **no client change** — Wakil just
  sends declared-version-3-shaped bytes and the 0.6.5 supervisor accepts them
  (E_UNSUPPORTED only for *newer* ops from an older client/older session,
  which Wakil never sends). To be proven, not assumed: integration Block 3
  runs the full Start/exec/stop cycle against real 0.6.5.
  Note: ctl exec request line has NO size cap today (only `maxLine` 64 KiB
  read cap at `ctl.go:49`). A >65536 B exec request under 0.6.5 gets error
  reply E_TOO_LARGE instead of a closed connection — strictly better; the
  `*ctl.Error` surfaces via `execCtl` return, not a panic. Wakil never
  sends a 64 KiB request today (argv+cwd+env JSON).
- **Error reporting**: all call sites use `b.output()` (`cmd.go:92`: stdout on
  success, stdout+stderr combined on error) then `fmt.Errorf("…: %s",
  strings.TrimSpace(out))` — i.e. raw combined text, never parsed.
  Mapped: `version.go:53 --version`, `:71 check`; `probe.go:24,43`;
  `session.go:207 ls --json`, `:231 inspect --json`, `:572 profile validate`,
  `:779 ls --session`, `:301 killSession steps` (discarded!), `:1003/:1085
  stop` (discarded), `:660-681 runSession → withSupervisorStderr`
  (`session.go:691`: leads with line containing `network = "open"`, else
  generic `(supervisor <status>; log <path>); stderr: <ring>` stripped of
  nothing); `exec.go:177` exec-CLI streams stderr through `diagWriter`
  (`exec.go:234`: exit 125 + diag seen → BackendError, 125 without → Lost).
  Code-typed only: `classifyControlError` (`backend.go:849`,
  E_NOT_GRANTABLE/E_PATH_FORBIDDEN substring match), `validateError`
  (`session.go:582`, E_RISK_ACK_ substring + network/open substring),
  ctl `--json` parse in `verifyInstance`/`listSessions`.
  The 0.6.3 `hisn-error/v1` shape: `{"schema":"hisn-error/v1","code":"E_…",
  "message":"…",…}` — exact key spellings UNVERIFIED (need a real `--json`
  error sample from the 0.6.5 binary in Block 3; parse defensively:
  extract schema/code/message if present, else fall back to today's text).
  Commands Wakil calls that gained `--json` error objects in 0.6.3/0.6.5:
  `run --detach` (Wakil calls it WITHOUT `--json` today, but supervisor
  stderr goes through the ring buffer → `withSupervisorStderr` path),
  `stop` (discarded output), `rootfs list` (Wakil never calls it),
  `seat` (never called), plus E_TOO_LARGE on `exec` (CLI) and on the ctl
  path (error reply). Warga's surface: every `b.output()` on-error site.
- **Open-mode kernel gate today**: none in Wakil. No kernel/Landlock read
  anywhere (`grep Landlock/uname/osrelease` in `internal/sandbox/hisn`:
  comments only). The only gate is hisn's own validate/run refusal text
  (`validateError` network/open branch `session.go:590`;
  `withSupervisorStderr`/`openRefusalLine` `session.go:691/:707`), surfaced
  at Start. Preflight runs `hisn check` (`version.go:71`) but today does NOT
  parse its output — Probe surfaces `firstLine` of the `hisn check` tail
  (`probe.go:44`).
- **Landlock ABI source**: no `/proc/sys/kernel/` landlock file (checked on
  this host: only `io_uring_disabled` etc.). The ABI probe is via the
  Landlock syscall (`LANDLOCK_CREATE_RULESET` returns abi for a requested
  version) or `/proc/self/mountinfo`. Plan: kernel version from
  `/proc/sys/kernel/osrelease` (release string) as the 6.7 floor indicator;
  Landlock ABI detection itself via a tiny `golang.org/x/sys/unix`
  `LandlockCreateRuleset` call — if that needs cgo/x-sys churn, fall back to
  kernel-release-only with reason text and note Landlock ABI as UNVERIFIED.
  `hisn capabilities --json` (0.6.5) reports `available`+`reason` for
  open-mode prerequisites *including* the Landlock requirement — Block 2
  may let the preflight name the missing kernel feature from that output
  when the binary supports it.
- **Open-mode docs today**: "kernel 5.14+" (`network_fail_test.go:17`,
  `docs/sandbox-backends.md:607`, `docs/configuration.md:592`). Under 0.6.2+
  the floor is kernel 6.7 / Landlock ABI 4. The 5.14 number is HISN 0.6.1's
  (SECCOMP_ADDFD); Wakil must update its text to 6.7/ABI 4 and the refusal
  message mapping (`openRefusalLine` matches `network = "open"` substring —
  hisn 0.6.2+ refusal text is UNVERIFIED, need a real sample).
- **WAKIL_HISN_BIN**: `integration_test.go:47` (`realBackend`), `:289`,
  `:1026` (open-network test), plus profile/realprofile tests. Integration
  gate: `--version` ≥ 0.2.1 → `Preflight` (which enforces MinVersion and
  runs `hisn check`, skip on failure). Host here: kernel 7.3.4 (≥ 6.7),
  user namespaces present (`/proc/sys/user/max_user_namespaces` =
  2147483647), real `hisn` on PATH at `/usr/local/bin/hisn` (live — must not
  touch). No Landlock ABI file in /proc, but kernel 7.3 ⇒ ABI ≥ 11 per hisn
  0.6.2 release notes.

## Mashūra round 1 (2026-11-25) — folded in
BLOCKING (binding): B1 every real-binary test name actually runs (no silent
SKIP; list above: 22 TestIntegration* + 6 TestRealHisn* + TestIntegration* in
integration_profile_test.go — grep WAKIL_HISN_BIN); require mode via env
(WAKIL_HISN_REQUIRE=1 → t.Skip becomes Fail, to add in realBackend); log
resolved binary + --version; isolation: Block 3 runs with isolated
XDG_RUNTIME_DIR (NOT the live $XDG_RUNTIME_DIR/hisn — startLocked/Reconcile
run `ls --prune` there). B2 order: capture the real 0.6.5 outputs FIRST
(download + --version + `ls --json` + `capabilities --json` + a `--json`
error sample + supervisor.pid/inspect shape) before the Block 1 pin commit;
no invented fixtures. B3: readyMode drop discriminant + `pasteMode` variants
capture realities (raw stderr, leaked HOST note): every on-error output site
gets the parser (not just run/stop/seat/rootfs); `stop`/`killSession` keep
discard-and-verify (waitEnded decides; parsed stop error only logged, never
returned early); ctl: explicit-error-reply semantics change (0.6.5 refusal
arrives as error reply, not closed conn) — audit all pre-acceptance choices;
exec error contract: BackendError (not plain fmt.Errorf) for E_TOO_LARGE on
both ctl and CLI paths; real-binary oversized-argv test on both paths. B4:
`ls --json` schema string captured from 0.6.5 in the pre-capture step; hard
gate if v3. B5: on-error parse must run BEFORE E_NOT_GRANTABLE/etc.
substring matchers? NO — run AFTER: existing code-typed matchers keep first
claim; hisn-error parse is the fallback for unmapped codes; whole-string
JSON parse of combined stdout+stderr on error (no -v multi-line issue:
multi-line output won't parse, falls through). B6: `hisn check` on sub-6.7
host — UNVERIFIED on this machine (kernel 7.3.4); record as Open risk; docs
must state the intended host-mode outcome as unverified-pre-6.7. Byte-identical
regression test for non-error-object failure required.
Order after round 1: (0) capture 0.6.5 in .tmp (read-only, isolated
XDG_RUNTIME_DIR) → Block 1 (pin+parse+kernel) → Block 2 (capabilities if
simple, now Block 2 = capture-dependent) → Block 3 (integration, isolated,
zero-skip acceptance) → Block 4 (docs/CHANGELOG).

## Blocks (one commit each, failing test first)

### Block 1 — pin + compat (required for compatibility)
1. Failing tests first: `TestPreflightRefusalNamesSecurityReason`-style
   (0.6.4 refused, message names 0.6.4/0.6.5 found+required), Probe floor
   test (0.6.4 → `older than 0.6.5`), `hisn-error/v1` parse tests on the new
   helpers (recorded 0.6.5 outputs), `TestOwnHisnError` strip test,
   E_TOO_LARGE → tool-error test, kernel-floor parse test (6.7/ABI 4 wording).
2. Bump `MinHisnVersion` 0.6.1 → 0.6.5 + reason text (new security framing:
   0.6.2 closes the open-mode timing gap; 0.6.2+ needs Landlock ABI 4 /
   kernel 6.7 for open). Update every literal listed in §1 (fake default,
   fake versions, `integration_test.go` version gates 0.6.1→0.6.5, install.sh,
   Dockerfile + SHA256 from §3, CI smoke, AGENTS/CONTRIBUTING/README/docs).
2. Add `--json` ONLY where Wakil already parses stdout on success (ls --json,
   inspect --json, profile explain): those calls already request JSON, so an
   error object today (if hisn ever emits one) reaches the same parse path.
   Do NOT add `--json` to run/stop/grant/revoke/exec (changes stdout
   contract: run prints "<id> <name|-> ready" on stdout; exec streams program
   output). For the plain-argv sites, a whole-string `hisn-error/v1` parse of
   the combined error output is the FALLBACK for unmapped codes ONLY
   (existing E_RISK_ACK_/E_NOT_GRANTABLE/network-open substring matchers keep
   first claim; multi-line -v output won't parse and falls through).
   E_TOO_LARGE (code from an error object, ctl reply, or `hisn exec`): clear
   tool error naming the limit from hisn's message where present, via
   BackendError on the exec paths (not plaintext fmt.Errorf).
   `stop`/`killSession` keep discard-and-verify (waitEnded decides; parsed
   stop error only logged, never returned early).
3. Version.go Preflight message names found + required + reason (already
   does; verify wording covers "hisn 0.6.4 is too old, need >= 0.6.5").
4. Error-object parsing: new `hisnError` parse helper in `cmd.go`/`diag.go`
   applied at every `b.output()` on-error site listed in §4 (NOT just
   run/stop/seat/rootfs: validate, inspect, ls, grant/revoke, exec-CLI):
   if the combined output parses as `hisn-error/v1` with a code, surface
   `code: message` (E_TOO_LARGE → a clear `hisn: request too large (E_TOO_LARGE,
   limit …)` tool error naming what was too large — the message itself names
   it; do not fabricate a limit the output doesn't give). All other sites keep
   today's raw text byte-identical (this is the DO-NOT-ALTER guard: parse-only,
   no behavior change on non-error-object output).
   `stop`/`killSession` discard output — parse there too so a stop refusal
   isn't swallowed (defensive: only surface code/message when the error
   object is present).
5. ctl `E_TOO_LARGE`: `ctl.Error` already surfaces; add `IsTooLarge()` +
   a Backend wrapper naming the limit (same treatment as CLI path).
6. Open kernel requirement: in Preflight, when the session config selects
   `network = "open"`, check host kernel release (osrelease) ≥ 6.7 up front
   and fail HERE naming kernel 6.7 / Landlock ABI 4 instead of at Start. If
   the release parse fails or the kernel is too old, name what was found.
   (Landlock ABI direct probe only if trivial via x/sys; else kernel floor
   + hisn's own refusal still leads.) HOST mode path unchanged.
7. Recorded-0.6.5 unit fixtures: handshake not a fixture (no revision bytes
   today) — the Block 3 real-binary run IS the handshake test; unit tests
   cover error object + E_TOO_LARGE + refusal text + kernel parse.
8. Commit (signed) after `go build`, `gofmt`, package tests.

### Block 2 — capabilities (only if simple)
- Try `hisn capabilities --json` on the real 0.6.5 binary (Block 3 download
  first, read-only). If it's a stable list of the 11 features with
  `available`/`reason`, wire it INTO Preflight/Probe as an *additional*
  name-the-missing-feature reason (open-mode prerequisite unavailable →
  reason names the feature + hisn's reason). Keep version floor as primary
  (hisn < 0.6.5 has no `capabilities` → skip silently). If the output is
  unstable/undocumented, do NOT wire it: list under Open points.
- Everything else new → Open points (§ below), one line each. No impl.

### Block 2 verdict (2026-11-25): NOT wired — kept Open point
Observed `capabilities --json` on the real 0.6.5 binary: stable schema
`hisn-capabilities/v1` with `version`, `protocol_revision` (4), `declared_protocol_version`
(3), and 11 named features (detached-exec, journal-cursor, journal-unreadable,
exec-stdin, seat-credential-stdin, network-none/host/open with a `reason` when
`available:false`, rootfs, ssh-agent, services), each with `available` + `reason`.
The version gate already names the open-mode floor (0.6.2+, Landlock ABI v4 /
kernel 6.7) and Preflight now fails closed for open below 6.7, which covers the
only Wakil-relevant capability — wiring `network-open.available` as an
*additional* reason adds a second source of truth for the same requirement
without a second behavior. Pre-0.6.5 binaries lack `capabilities` and would need
silent skip. Decision: version + kernel floor stay canonical; `capabilities`
stays an Open point (would only add value if Wakil ever gates a *new*
optional feature on hisn support).

### Block 3 — integration vs real 0.6.5
- Download `hisn-x86_64-linux-gnu` + `SHA256SUMS` to `.tmp/hisn-0.6.5/`,
  `sha256sum -c` both; report asset + checksum (expect
  890147…981aa; mismatch = stop and report). CHMOD +x in .tmp only. Never
  PATH-install, never touch `/usr/local/bin/hisn`.
- Run: `WAKIL_HISN_BIN=$PWD/.tmp/hisn-0.6.5/hisn-x86_64-linux-gnu go test
  -count=1 -v -timeout 10m -run TestIntegration ./internal/sandbox/hisn/`
  (+ realprofile + planned_risk tests if they gate on binary). Report exact
  command + per-test PASS/FAIL/SKIP. Host kernel 7.3.4 (≥ 6.7) so open-mode
  tests should run, not skip; namespaces present.
- Capture: `--version` output, `capabilities --json` sample, one real
  `hisn-error/v1` sample (e.g. a `--json` failing command — stop on a
  non-existent session? choose a non-mutating failure against .tmp session),
  open-refusal text shape. Feed back into Block 1 fixtures if Block 1 landed
  with defensive parse (reconcile, no flag churn).
- If they cannot run (kernel/ns/permission): name the exact failure, write
  the exact host command for the user, mark assertive claims UNVERIFIED.

### Block 3 result (2026-11-25, agent sandbox): SKIPS, not failures
- Asset: `.tmp/hisn-0.6.5/hisn-x86_64-linux-gnu` 6702008 B, sha256
  `890147848475d0aee57d1dab3effa939a3392426dd6aaae2b174fe1dc5f981aa`
  (matches `SHA256SUMS`).
- Command: `XDG_RUNTIME_DIR=$PWD/.tmp/rt-065 WAKIL_HISN_BIN=$PWD/.tmp/hisn-0.6.5/hisn-x86_64-linux-gnu go test -count=1 -v -timeout 10m -run 'TestIntegration|TestRealHisn' ./internal/sandbox/hisn/`
  (log `.tmp/b3-integration.log`, exit 0).
- Result: 31 tests matched. All 28 `TestIntegration*`/`TestRealHisn*` that need
  a session SKIP with `hisn unusable on this host: sandbox backend hisn: hisn
  check failed — this host cannot run hisn sessions` (unshare(NEWUSER)
  Operation not permitted — user namespaces disabled in this sandbox), i.e. the
  `hisn check` gate fired, not test failures. What DID run against the real
  0.6.5 binary: 5 `TestRealHisn*` PASS (generated base profile, full surface,
  ack matrix, persistent home, SSH table) + `TestIntegrationToolPath` PASS.
  `TestRealHisnAcceptsRootfs` SKIP (no rootfs built). The full session cycle
  against 0.6.5 is UNVERIFIED here and must run on the host:
  `WAKIL_HISN_BIN=$PWD/.tmp/hisn-0.6.5/hisn-x86_64-linux-gnu go test -count=1 -v -timeout 10m -run 'TestIntegration|TestRealHisn' ./internal/sandbox/hisn/`
  on a host where `hisn check` passes (userns enabled).
- Captured from the real 0.6.5 binary (read-only, isolated runtime dir):
  `--version` → `hisn 0.6.5`; `capabilities --json` (see Block 2);
  `hisn check` shows `landlock ABI v11` and `landlock net rules (open mode)
  ABI v11 (>= v4, Linux 6.7)` — confirms the kernel-floor text semantics.
  Error-object sample shape confirmed for the parser via the committed
  `hisn_error_test.go` fixture (recorded from real 0.6.5 output).

### Block 4 — docs + CHANGELOG
- README/docs/configuration.md/docs/sandbox-backends.md: 0.6.5 minimum,
  open-mode kernel 6.7 / Landlock ABI 4 (was "5.14+"), E_TOO_LARGE note,
  capabilities note if Block 2 wired it. The `0.6.1 → 0.6.5` example banner
  line in sandbox-backends §Choosing. `AGENTS.md:16`, `CONTRIBUTING.md`.
- CHANGELOG `[Unreleased]`: new minimum 0.6.5, what changed for users (older
  hisn refused; open mode needs kernel 6.7/Landlock ABI 4; E_TOO_LARGE is a
  clear error; no wire/protocol change — 0.6.4/0.6.3 clients interoperate),
  kernel note for open mode.
- `scripts/host_verify.sh:332` label 0.6.1 → 0.6.5.
- Commit (signed).

## Touched paths (expected)
- `internal/sandbox/backend.go` (pin + reason) — one line + comment.
- `internal/sandbox/hisn/version.go` (message + kernel preflight for open).
- `internal/sandbox/hisn/probe.go` (message; capabilities add-on if Block 2).
- `internal/sandbox/hisn/cmd.go` or `diag.go` (new `hisnError` parse helper).
- `internal/sandbox/hisn/session.go` (`validateError`, `withSupervisorStderr`,
  `stop`/`killSession` error parse, `listSessions`/`verifyInstance`).
- `internal/sandbox/hisn/exec.go` (CLI E_TOO_LARGE surface).
- `internal/sandbox/hisn/ctl/ctl.go` (`IsTooLarge()`).
- Tests: `internal/sandbox/hisn/*_test.go` (fake default versions, refusal
  tests, `hisn_error_test.go` new, `kernel_floor_test.go` new), `internal/config/default_backend_test.go`.
- Pins: `install.sh`, `Dockerfile`, `.github/workflows/ci.yml`,
  `AGENTS.md`, `CONTRIBUTING.md`, `README.md`, `docs/*`, `scripts/host_verify.sh`.
- `CHANGELOG.md`, this plan (docs/plans/hisn-0.6.5.md).

## Tests per block
- Block 1: new refusal/parse/E_TOO_LARGE/kernel tests (fail first, then pass);
  full `internal/sandbox/hisn` + `internal/config` packages.
- Block 2: capabilities unit test only if wired (fake outputs canned
  capabilities JSON).
- Block 3: real-binary integration suite (above) — the handshake proof.
- Block 4: `TestInstallScriptHisnVersionMatchesMinVersion`,
  `TestDockerfileHisnVersionMatchesMinVersion`, `scripts/check_config_docs.sh`.

## Open points (adopting what 0.6.2–0.6.5 added — NOT implementing)
- `exec --detach` (+ `exec-status`/`exec-output`/`exec-signal`): long-running
  tool calls that outlive their client; currently a cancelled call kills the
  program (TERM→KILL). Would need a lost-status redesign.
- `journal --after-wseq`/`--info` cursor + `ls --json supervisor_pid`:
  cheap "did the session stop, why" answers without `ls` polling.
- `--seat-token-file -`: seat credential never in argv/logs (multi-user hosts).
- `--stdin-limit` stdin passthrough for CLI execs (browser, `hisn exec` path):
  tool-call stdin currently /dev/null.
- `profile validate --json`: machine-readable profile errors (TOML path+line)
  instead of substring matching in `validateError`.
- `capabilities --json` as the compat basis (only if Block 2 says simple).
- `run --detach --json` / `stop --json` / `rootfs list --json` structured
  success output (Wakil parses streams today).
- `docs/EXTERNAL-CALLERS.md` patterns (setsid long-command, linger note):
  relevant if Wakil ever hosts sessions for other users.

## Open risks
- `hisn-error/v1` exact key spellings (schema/code/message vs alternatives)
  UNVERIFIED until Block 3's real capture; Block 1 parse must be defensive
  (try schema, else code/message, else fall through).
- 0.6.2+ open-refusal wording changed vs 0.6.1's `network = "open"` line;
  `openRefusalLine` substring match may miss — Block 3 captures real text.
- Landlock ABI detection: no clean Go-stdlib probe; kernel-release floor is
  the cheap 80%. If x/sys adds weight, stop at kernel floor and say so.
- `capabilities --json` feature names/keys "11 stable feature names"
  UNVERIFIED until Block 3 runs it.
- `scripts/host_verify.sh` full run + `scripts/bench.sh --self-test` are
  final gates, not per-block.
- DO-NOT-ALTER: raw stderr text must stay byte-identical when no error object
  is present; `context_modules` off = tool array/prompt byte-identical to
  v0.8.11 (this bump touches no prompt/tool text — verify via the gates);
  live hisn binary untouched; bench files unchanged.
