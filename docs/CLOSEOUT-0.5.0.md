# Wakil 0.5.0 — close-out

Status: **not tagged, not pushed.** The host sequence below must pass before
`v0.5.0` is tagged. Branch `master`, local commits on top of `34c29be`.

## Commits

Identity and legacy (C):
- `3413900` C1: ProjectDir is the single chat store key source
- `38ae10c` C2: read-only legacy store read-through
- `77ea5de` C3: `wakil state migrate --from-legacy`
- `f15496d` C4: first-start legacy notice, legacy fields in `--print-workspace-key`
- `f78c1d8`, `8d1767f`, `605b017` C review fixes
- `3f1fbd8` C migrate: never overwrites a newer project value; keeps anchors and provenance; refuses a legacy destination; sidecar after each copy

Rootfs, home, ssh, git (P1–P3):
- `996a42c` P1 persistent home; `01187f4` P1 verify (real hisn >= 0.3.2)
- `8b54bd4` P2 rootfs
- `5215e34` P3 part 1 (ssh gating); `575cfbd` P3 part 2 (fingerprints, session gitconfig, identity, known_hosts)
- `374dd66` P3 review fixes: signing key from global/system scope only; named profile refuses identity or signing
- `9eb53f8` self-ack for Wakil's own home-persist default
- `d246246` P1+P2 review fixes: named profile refuses an effective tmpfs home; approval summary lists rootfs and home
- `ab6899d` test: default profile carries home-persist data label since 0.5.0

Browser (P4):
- `bb505eb` P4 browser under hisn via `CLIExecer` ownership transport
- `32f34f5` P4 review fixes: exec client outlives the launch context; stop signals only a matching process group; probe requires python3

Host-verified defects (F1–F4):
- `80fdaa2` F1: project file errors fail closed (startup error, not docker fallback)
- `7bf319f` F4: pass SSH_AUTH_SOCK to the host-side hisn supervisor
- `d95e5b7` F2: browser tools on under hisn (inside the session, not on the host)
- `69bc01b` F3: preflight SSH_AUTH_SOCK when sandbox.hisn.ssh_agent is true
- `a8c2ffa` F4+F2 review fixes: slice aliasing, SSH_AUTH_SOCK reserved in env_pass, stale docs

## Review status

- C: reviewed (one reader). Findings fixed in `3f1fbd8`.
- P1+P2: reviewed (one reader). Findings fixed in `d246246`.
- P3: **partly reviewed.** One reader returned a usable review; its two blocking items are fixed in `374dd66`. Not obtained: `[ssh]` rendering in `buildPolicy`, `ssh_keys` entry validation, `EnvPass` vs `EnvSet` precedence. Two attempts failed (content filter, peak load).
- P4: reviewed (one reader). Findings fixed in `32f34f5`. Q1 found no spurious start.
- F1+F4+F2: reviewed (two readers). Two BLOCKING findings fixed in `a8c2ffa`: slice aliasing in `command()` (copy `EnvPass` before appending), and `SSH_AUTH_SOCK` rejected in `env_pass` validation (would leak the host socket path into the sandbox). Open (non-blocking): reader 1 notes the F1 docstring overstates the guarantee scope (a broken/unapproved sandbox.json is an error only when hisn would have been chosen — i.e. when hisn is usable and the backend is defaulted); reader 2 notes `workspaceFitsState` became fatal (intended — a workspace containing the state dir is a startup error, not a docker fallback). Both are documented in the F1 commit message.

## Verification run here (sandbox)

- `go vet ./...`: clean.
- `golangci-lint run`: 0 issues.
- `go test ./... -count=1`: all packages pass except `internal/agent` (pre-existing test timeout in `ListSessions` unmarshal fixture — unrelated to F1–F4).
- `LC_ALL=C go test ./... -count=1`: same result as above.
- `go test -race -count=1` on `internal/sandbox`, `internal/sandbox/hisn`, `internal/sandbox/hisn/ctl`, `internal/sandbox/project`, `internal/browser`, `internal/wiring`, `internal/config`: all pass.
- Real-hisn integration tests (`TestIntegration*` in `internal/sandbox/hisn`) skip in this sandbox (hisn 0.2.2, needs >= 0.3.2 for `ssh_agent`/`rootfs`/`home`/`[ssh]`).

## Host results (rubin, hisn 0.3.2, wakil-dev rootfs, real ssh-agent)

Steps 1–13 of the host sequence: **green.** Steps 14–16 were blocked by F4
(SSH_AUTH_SOCK not reaching hisn), now fixed.

- Steps 1–13: pass (including integration tests, locale runs, race, lint,
  `--print-workspace-key`, `wakil state migrate --from-legacy` dry run,
  config lines, real-chat checklist items 9a–9d).
- Step 14: browser screenshot under hisn — blocked by F2 (browser reported off
  under hisn). Fixed in `d95e5b7`.
- Step 15: `hisn exec <session> pgrep -fa chrom` after `/new` — blocked by F2
  (browser never started). Fixed.
- Step 16: signed commit — blocked by F4 (SSH_AUTH_SOCK not forwarded). Fixed
  in `7bf319f`.

Stale-test commit: `ab6899d` (TestIntegrationProfileStart: Risk is "data" with
home-persist since 0.5.0).

F1–F4 with commit hashes:
- F1 `80fdaa2`: broken/unapproved `.wakil/sandbox.json` is now a startup error
  (no docker fallback, no browser). `ResolveSandbox` returns `(Config, error)`;
  all three entry points (TUI, `wakil run`, daemon) check and exit before the
  banner.
- F4 `7bf319f`: `SSH_AUTH_SOCK` passed to the host-side hisn supervisor
  (`withPass=true` + `SSHAgent`); exec clients never get it. In-sandbox env
  stays allowlisted; `SSH_AUTH_SOCK` rejected in `env_pass`.
- F2 `d95e5b7`: `browser_enabled` stays true under hisn; banner reports
  "on (inside hisn session)"; old force-off and `newHisnExecutor` guard
  removed. P4 CLI transport handles the browser.
- F3 `69bc01b`: `SSH_AUTH_SOCK` checked at preflight when `ssh_agent=true`
  (set + socket); error names `sandbox.hisn.ssh_agent`.

## Open items

- **Flake, `ctl`:** `TestSignalRequestJSONByteForByte` failed once under full-suite load. Passes on reruns. Cause not found.
- **Flake, `internal/config`:** one run wrote a temp dir into the real `repo-state` directory; the testutil guard caught it. Did not reproduce. Cause not found.
- **Flake, `internal/agent`:** `ListSessions` unmarshal test times out under full-suite load (pre-existing, unrelated to F1–F4).
- **P4 option 2 (0.5.x):** control-socket ownership protocol to replace the CLI exec transport for the browser.
- **P3 review gaps:** see Review status above.
- **0.5.1 list:** anything the P3 review gaps turn up; named-profile `[ssh]` behaviour (`hisn profile explain`) not yet confirmed.

## Remaining host steps (14–16)

After the F1–F4 fixes, re-run on rubin:

14. Browser screenshot under hisn (was blocked by F2 — now fixed in `d95e5b7`).
15. `hisn exec <session> pgrep -fa chrom` after `/new` (was blocked by F2 — now fixed).
16. Signed commit (was blocked by F4 — now fixed in `7bf319f`).

If all pass: push, then tag `v0.5.0`.

No tag and no push have been made.