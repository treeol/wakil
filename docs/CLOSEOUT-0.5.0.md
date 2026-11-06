# Wakil 0.5.0 — close-out

Status: **not tagged, not pushed.** The host sequence below must pass before
`v0.5.0` is tagged. Branch `master`, local commits on top of `34c29be`.

## Commits

Identity and legacy (C):
- `3413900` C1: ProjectDir is the single chat store key source
- `38ae10c` C2: read-only legacy store read-through
- `77ea5de` C3: `wakil state migrate --from-legacy`
- `f15496d` C4: first-start legacy notice, legacy fields in `--print-workspace-key`
- `f78c1d8`, `8d176ec`, `605b017` C review fixes
- `3f1fbd8` C migrate: never overwrites a newer project value; keeps anchors and provenance; refuses a legacy destination; sidecar after each copy

Rootfs, home, ssh, git (P1–P3):
- `996a42c` P1 persistent home; `01187f4` P1 verify (real hisn >= 0.3.2)
- `8b54bd4` P2 rootfs
- `5215e34` P3 part 1 (ssh gating); `575cfbd` P3 part 2 (fingerprints, session gitconfig, identity, known_hosts)
- `374dd66` P3 review fixes: signing key from global/system scope only; named profile refuses identity or signing
- `9eb53f8` self-ack for Wakil's own home-persist default
- `d246246` P1+P2 review fixes: named profile refuses an effective tmpfs home; approval summary lists rootfs and home

Browser (P4):
- `bb505eb` P4 browser under hisn via `CLIExecer` ownership transport
- `32f34f5` P4 review fixes: exec client outlives the launch context; stop signals only a matching process group; probe requires python3

## Review status

- C: reviewed (one reader). Findings fixed in `3f1fbd8`.
- P1+P2: reviewed (one reader). Findings fixed in `d246246`.
- P3: **partly reviewed.** One reader returned a usable review; its two blocking items are fixed in `374dd66`. Not obtained: `[ssh]` rendering in `buildPolicy`, `ssh_keys` entry validation, `EnvPass` vs `EnvSet` precedence. Two attempts failed (content filter, peak load).
- P4: reviewed (one reader). Findings fixed in `32f34f5`. Q1 found no spurious start.

## Verification run here

- `go vet ./...`: clean.
- `golangci-lint run`: 0 issues.
- `go test -count=1` on the touched packages: pass.
- `go test -race` on the touched packages: pass (`internal/browser`, `internal/exec`, `internal/sandbox`, `internal/sandbox/hisn`, `ctl`, `project`, `wiring`, `config`, `agent`).
- Not run here: the German (de_CH) locale. Not installed in this sandbox.

## Open items

- **Flake, `ctl`:** `TestSignalRequestJSONByteForByte` failed once under full-suite load. Passes on reruns. Cause not found.
- **Flake, `internal/config`:** one run wrote a temp dir into the real `repo-state` directory; the testutil guard caught it. Did not reproduce. Cause not found.
- **P4 option 2 (0.5.x):** control-socket ownership protocol to replace the CLI exec transport for the browser.
- **P3 review gaps:** see Review status above.
- **0.5.1 list:** anything the P3 review gaps turn up; named-profile `[ssh]` behaviour (`hisn profile explain`) not yet confirmed.

## Host sequence (run on rubin, in order)

1. Install the built binary via `mv`, not `cp`.
2. Integration tests with hisn 0.3.2 and `wakil-dev` in the store:
   `go test -count=1 -v -timeout 10m -run TestIntegration ./internal/sandbox/hisn/`
   Covers the real-hisn cmdline test, the `[rootfs]` and `[ssh]` validate tests, and the browser launch.
3. Locale runs: `LC_ALL=C go test ./...` and `LC_ALL=de_CH.UTF-8 go test ./...`.
4. `go test -race ./...`.
5. `golangci-lint run`.
6. `wakil --print-workspace-key`.
7. `wakil state migrate --from-legacy` dry run (no `--apply`).
8. Config lines to set for the hisn backend: `sandbox.hisn.rootfs`, `sandbox.hisn.home`, `sandbox.hisn.ssh_agent`, `sandbox.hisn.ssh_keys`, `sandbox.hisn.git_identity_name`, `sandbox.hisn.git_identity_email`, `ssh_signing`.
9. Real-chat checklist:
   - Banner labels show, including "(wakil default)" on the home mount.
   - A second chat reuses a warm `GOMODCACHE`.
   - A signed commit verifies.
   - Browser screenshot works.
   - `hisn exec <session> pgrep -fa chrom` prints nothing after `/new`.
10. Only if all pass: push, then tag `v0.5.0`.

No tag and no push have been made.
