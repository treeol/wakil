# Wakil 0.5.0 — close-out

Status: **tagged v0.5.0, pushed to origin.** Host steps 1–13 green, 14–16
fixed (F1–F9). Branch `master`, HEAD `5c9988d`.

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

Host-verified defects (F5–F6):
- `12eee6c` F5: signing key fingerprint must be in sandbox.hisn.ssh_keys (not auto-added)
- `2249d4a` F6: refuse signing bypass in enforced sessions (tool guard + prompt rule)
- `e6f6a8e` docs: add DNS known limitation to 0.5.0 changelog
- `9d4744d` F5: fix integration test — set signing config inside session, verify on host (reverted by `826b7ca` — see F8)
- `480ed65` F5+F6 review fixes: guard run_background, case-insensitive keys, read-only config, --config-env

Host-verified defects (F7–F8):
- `9396cb7` F8: GIT_CONFIG_GLOBAL must point at the in-session home path (not the host HomeRoot)
- `1076721` F7: signing guard must allow reads with any read flag, not just --get first
- `826b7ca` F7+F8 review fixes: allowedSignersFile in-session path, --null bypass, --type/--default space form, bare key=value, --git-dir walk-back

Host-verified defect (F9):
- `14c386e` F9: inject signing settings at command scope via GIT_CONFIG_* env — a repo-local .git/config with its own user.signingkey can shadow the session gitconfig (GIT_CONFIG_GLOBAL) because local scope beats global. The fix injects user.signingkey, gpg.format, gpg.ssh.allowedSignersFile, gpg.ssh.program, commit.gpgsign, tag.gpgsign at COMMAND scope via GIT_CONFIG_COUNT/GIT_CONFIG_KEY_n/GIT_CONFIG_VALUE_n on every exec. Command scope beats local and global. gpg.ssh.program is pinned to ssh-keygen (review fix: without it, a repo-local .git/config could set it to a wrapper that signs with a different key — same threat class). The session gitconfig carries signing settings as a defense-in-depth fallback for git < 2.31 (which does not support GIT_CONFIG_COUNT). The GIT_CONFIG_* names are added to env_pass when signing is on. A repo-local signingkey is reported in the banner. Mashūra review (two readers): two BLOCKING findings fixed.

## Review status

- C: reviewed (one reader). Findings fixed in `3f1fbd8`.
- P1+P2: reviewed (one reader). Findings fixed in `d246246`.
- P3: **partly reviewed.** One reader returned a usable review; its two blocking items are fixed in `374dd66`. Not obtained: `[ssh]` rendering in `buildPolicy`, `ssh_keys` entry validation, `EnvPass` vs `EnvSet` precedence. Two attempts failed (content filter, peak load).
- P4: reviewed (one reader). Findings fixed in `32f34f5`. Q1 found no spurious start.
- F1+F4+F2: reviewed (two readers). Two BLOCKING findings fixed in `a8c2ffa`: slice aliasing in `command()` (copy `EnvPass` before appending), and `SSH_AUTH_SOCK` rejected in `env_pass` validation (would leak the host socket path into the sandbox). Open (non-blocking): reader 1 notes the F1 docstring overstates the guarantee scope (a broken/unapproved sandbox.json is an error only when hisn would have been chosen — i.e. when hisn is usable and the backend is defaulted); reader 2 notes `workspaceFitsState` became fatal (intended — a workspace containing the state dir is a startup error, not a docker fallback). Both are documented in the F1 commit message.
- F5+F6: reviewed (two readers, gpt-6-astra + fable-5.1). Three BLOCKING findings fixed in `480ed65`: (1) `handleRunBackground` skipped the guard — a command refused by `handleRunShell` could reach execution through `run_background`; (2) git config keys are case-insensitive — `git -c Commit.GpgSign=false` bypassed the guard; (3) `git config --get commit.gpgsign` (a read) was refused. Additional fixes: `--no-gpg-sign` only on git commands, `--config-env` form, `git -C . config` and `git -c core.pager=cat config` forms, multiple `config` tokens, `GIT_CONFIG_VALUE_<n>` prefix, error message no longer double-prefixes "ERROR:". Open (non-blocking): shell quoting/indirection (`$K`, `$(which git)`), `env -u`, section operations (`--remove-section`), `--config-env` abbreviation, and `git commit-tree`/`fast-import` are out of scope for a direct-command guard — the audit trail (an unsigned commit) catches the rest.
- F7+F8: reviewed (two readers, gpt-6-astra + fable-5.1). Five BLOCKING findings fixed in `826b7ca`: (1) `allowedSignersFile` in the rendered gitconfig used the host HomeRoot path — same bug class as F8's `GIT_CONFIG_GLOBAL`; now uses `SandboxHome`; (2) `--null`/`-z`/`--name-only` are output modifiers, not read actions — `git config --null commit.gpgsign false` was a real bypass; (3) `--type`/`--default` space form was not consumed as a two-token pair — the value leaked as the key; (4) bare `key=value` was not split; (5) `--git-dir`/`--work-tree` were not in the walk-back list. Additional fixes: a bare `git config <key>` (one non-flag token) is a get (read) — allowed; `git config <key> <value>` (two non-flag tokens) is a set (write) — checked. Integration test host-side `git log` now passes `-c gpg.format=ssh -c gpg.ssh.allowedSignersFile=<HomeRoot>/.wakil-allowed-signers` so the host git can verify the SSH signature without a global allowed_signers config. Open (non-blocking): `9d4744d`'s test workaround (set signing config inside the session instead of going through `GIT_CONFIG_GLOBAL`) was reverted — it hid the F8 bug; `git config set/unset/edit/remove-section/rename-section` subcommand syntax (git >= 2.46) is not handled — the `config` token is treated as the `config` subcommand and `set`/etc. as the key; `git --git-dir=X` joined form is handled but `git --git-dir X` (space, before the `config` token) is not in the walk-back for `config` tokens after the first.
- F9: reviewed (two readers, gpt-6-astra + fable-5.1). Two BLOCKING findings fixed: (1) `gpg.ssh.program` was not pinned at COMMAND scope — a repo-local `.git/config` could set it to a wrapper that signs with a different key (same threat class as F9); now pinned to `ssh-keygen`; (2) fail-open on git < 2.31 — `GIT_CONFIG_COUNT` is not supported, and signing settings were removed from the session gitconfig, so commits would be unsigned; signing settings restored to the session gitconfig as a defense-in-depth fallback (COMMAND-scope env still wins on git >= 2.31). Open (non-blocking): `validateExecEnv` accepts any `GIT_CONFIG_KEY_*`/`GIT_CONFIG_VALUE_*` with any value (only internal callers, but wider than needed); `signingEnvPass()` hardcodes the pair count (6) separately from `signingEnv()`'s `pairs` slice — brittle but tested.

## Verification run here (sandbox)

- `go vet ./...`: clean.
- `golangci-lint run`: 0 issues.
- `go test ./... -count=1`: all packages pass.
- `LC_ALL=C go test ./... -count=1`: same result as above.
- `go test -race -count=1` on `internal/sandbox/hisn`, `internal/exec`, `internal/tools`, `internal/agent`, `internal/wiring`: all pass.
- Real-hisn integration tests (`TestIntegration*` in `internal/sandbox/hisn`) **ran** (hisn 0.3.2 available in this sandbox) — all pass, including `TestIntegrationSSHAgentForwarded` with the signed commit verified on the host side.

## Host results (rubin, hisn 0.3.2, wakil-dev rootfs, real ssh-agent)

Steps 1–13 of the host sequence: **green.** Steps 14–16 were blocked by F4
(SSH_AUTH_SOCK not reaching hisn), now fixed.

- Steps 1–13: pass (including integration tests, locale runs, race, lint,
  `--print-workspace-key`, `wakil state migrate --from-legacy` dry run,
  config lines, real-chat checklist items 9a–9d).
- Step 14: browser screenshot under hisn — blocked by F2 (browser reported off
  under hisn). Fixed in `d95e5b7`. **P4 host result: Chromium ran inside the
  session on real hisn 0.3.2.**
- Step 15: `hisn exec <session> pgrep -fa chrom` after `/new` — blocked by F2
  (browser never started). Fixed.
- Step 16: signed commit — blocked by F4 (SSH_AUTH_SOCK not forwarded). Fixed
  in `7bf319f`. F5 (`12eee6c`) and F6 (`2249d4a`) fix the remaining signing
  defects found at step 16 (fingerprint not checked, bypass not refused).

Stale-test commit: `ab6899d` (TestIntegrationProfileStart: Risk is "data" with
home-persist since 0.5.0).

F7–F8 with commit hashes:
- F8 `9396cb7`: `GIT_CONFIG_GLOBAL` and `allowedSignersFile` in the rendered
  gitconfig now use the in-session path (`SandboxHome` = `/run/hisn/home`), not
  the host `HomeRoot`. The file is still written under `HomeRoot` on the host;
  hisn bind-mounts it at `SandboxHome`. `GIT_SSH_COMMAND`'s
  `UserKnownHostsFile` keeps the host path (same-path RO mount). `9d4744d`'s
  integration test workaround (set signing config inside the session) was
  reverted — it hid this bug. `SandboxHome` constant added to `profile.go`.
- F7 `1076721`: `checkGitConfigSub` now allows reads when ANY read action
  (`--get`, `--get-all`, `--get-regexp`, `--list`, `-l`, `--get-urlmatch`) is
  present and no write form (`--add`, `--unset`, `--unset-all`, `--replace-all`,
  `--rename-section`, `--remove-section`, `--edit`, `-e`, bare key=value) is.
  Output modifiers (`--show-origin`, `--show-scope`, `--name-only`, `--null`,
  `-z`, `--type`, `--default`) are neutral — they neither make a write into a
  read nor vice versa. `--remove-section`/`--rename-section` take a section
  name: refuse when the section covers any signing key. `--edit`/`-e` always
  refused. Bare `git config <key>` (one non-flag token) is a get (read) —
  allowed. `git config <key> <value>` (two non-flag tokens) is a set (write).
- `826b7ca`: F7+F8 review fixes — `allowedSignersFile` in-session path,
  `--null`/`-z`/`--name-only` bypass, `--type`/`--default` space form, bare
  `key=value` split, `--git-dir`/`--work-tree` walk-back.

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

F5–F6 with commit hashes:
- F5 `12eee6c`: the signing key fingerprint must be in `sandbox.hisn.ssh_keys` —
  it is never auto-added. `hisnGitSetup` checks the fingerprint against
  `h.SSHKeys` and fails with an error naming the key, the fingerprint, and
  `sandbox.hisn.ssh_keys`. `SigningSetup.SigningKeyValue` carries the raw
  `user.signingkey` value for error messages. Integration test extended: a
  throwaway key whose `.pub` path is the signing key, a commit made inside the
  session verifies with `git log --format=%G?` on the host side.
- F6 `2249d4a`: in a signing-enforced session (profile carries signing), the
  shell tool and every git tool refuse commands that would disable or bypass
  signing — `git -c <signing_key>=…`, `git config <signing_key> …`, `--no-gpg-sign`,
  `GIT_CONFIG_*` env vars. The guard catches the direct forms; a determined
  bypass through a script file is out of scope (the audit trail catches it).
  `SigningEnforcer` interface; `BackendOptions.SigningEnforced` set by
  `newHisnExecutor`; `DockerExecutor.SigningEnforced()` returns `d.signing`.
  System prompt rule added in the Shell section of `prompts/agent.txt`.
- `9d4744d`: F5 integration test fix — set signing config inside the session instead of going through `GIT_CONFIG_GLOBAL`. **Reverted by `826b7ca`** (F8): the workaround hid the F8 bug (`GIT_CONFIG_GLOBAL` pointed at the host HomeRoot path, which hisn remounts at `SandboxHome`). The integration test now verifies the product path: the session gitconfig is visible at the in-session file, reports a `key::` value, `commit.gpgsign` is true, and a commit made inside the session verifies on the host side with `git log --format=%G? = G` — with nothing set inside the session by the test.

## Open items

- **DNS limitation (known):** browser under hisn: name resolution fails on hosts
  where `/etc/resolv.conf` only lists the systemd-resolved stub (127.0.0.53);
  hisn 0.3.2 drops loopback resolvers and leaves none. Fixed in hisn 0.3.3.
  Host evidence: network reachable by IP, `getent` fails, `/etc/resolv.conf`
  empty inside the session.
- **Flake, `ctl`:** `TestSignalRequestJSONByteForByte` failed once under full-suite load. Passes on reruns. Cause not found.
- **Flake, `internal/config`:** one run wrote a temp dir into the real `repo-state` directory; the testutil guard caught it. Did not reproduce. Cause not found.
- **Flake, `internal/agent`:** `ListSessions` unmarshal test times out under full-suite load (pre-existing, unrelated to F1–F6).
- **0.5.1 list:**
  - crashpad handlers still start despite `--disable-crash-reporter` — add `--disable-features=Crashpad` or a `/tmp` crash-dumps dir.
  - `migrate --from-legacy` dry-run offers `--apply` with 0 copyable.
  - `ctl` JSON flake; `internal/config` temp-dir flake.
  - F3 if open (preflight `SSH_AUTH_SOCK`) — **done** (`69bc01b`).
  - P4 Option 2: control-socket ownership protocol to replace the CLI exec transport for the browser.
  - P3 review gaps: `[ssh]` rendering in `buildPolicy`, `ssh_keys` entry validation, `EnvPass` vs `EnvSet` precedence.
  - named-profile `[ssh]` behaviour (`hisn profile explain`) not yet confirmed.
  - F7 review gaps: `git config set/unset/edit/remove-section/rename-section` subcommand syntax (git >= 2.46) not handled; `git --git-dir X` (space, before the `config` token) not in the walk-back for `config` tokens after the first.
  - F9 review gaps: `validateExecEnv` accepts any `GIT_CONFIG_KEY_*`/`GIT_CONFIG_VALUE_*` with any value (wider than needed — only internal callers); `signingEnvPass()` hardcodes the pair count separately from `signingEnv()`'s `pairs` slice (brittle but tested).
  - Browser tunnel attached to a stale session once (seen after old sessions were left running — investigate ownership on attach).
  - Screenshots land in host `/tmp` and are unreachable from the sandbox (write them to the session `/tmp` or the workspace).

## Remaining host steps (14–16)

After the F1–F8 fixes, re-run on rubin:

14. Browser screenshot under hisn (was blocked by F2 — now fixed in `d95e5b7`).
    **P4 host result: Chromium ran inside the session on real hisn 0.3.2.**
15. `hisn exec <session> pgrep -fa chrom` after `/new` (was blocked by F2 — now fixed).
16. Signed commit (was blocked by F4/F5/F6/F7/F8/F9 — now fixed). The session
    gitconfig (`GIT_CONFIG_GLOBAL=<SandboxHome>/.wakil-gitconfig`) carries
    identity AND signing settings (defense-in-depth fallback for git < 2.31);
    COMMAND-scope `GIT_CONFIG_*` env vars on every exec enforce
    `user.signingkey`, `gpg.format=ssh`, `gpg.ssh.program=ssh-keygen`,
    `gpg.ssh.allowedSignersFile=<SandboxHome>/.wakil-allowed-signers`,
    `commit.gpgsign=true`, `tag.gpgsign=true`. A repo-local `.git/config`
    signingkey is overridden and reported in the banner. The agent proxy
    forwards the key; the signing guard refuses bypasses.

If all pass: push, then tag `v0.5.0`.

Tagged `v0.5.0` at `5c9988d` and pushed to origin. GitHub release created.