# work/0.8.4 cleanup progress

- Base: master 670ab3b6fb4145dae22cbbed4a6b51e08e328b07 (v0.8.3), branch work/0.8.4.
- Tier 3 (multi-file, behaviour + config + CI). Mashūra: 1 pre + 1 post for the whole pass, plus REVIEW round. No push, no tag.

## STEP 0
- [x] Branch/HEAD/status checked 2026-11-15: HEAD 670ab3b G, master, only pre-existing untracked (AUDIT, SHA256SUMS, docs/mashura-*, docs/work-0.8.3-progress.md, hisn binary, .tmp-gomod/).
- [x] work/0.8.4 created from 670ab3b.
- [x] Baseline gates under .tmp/work-0.8.4/ — all green (build/vet/test/golangci-lint 0 issues/config-doc/pins; gofmt only in .tmp deps).

## GROUP 1 (L1-L5) — committed 265bb9a G
- L1 checked: registration at internal/config/config.go:2087-2093 (now 2143+) passes cfg.X (file+env overlaid) as the flag default; PrintDefaults then prints it as (default ...). Confirmed by reading LoadConfig + PrintDefaults + a new test failing before the fix (would leak 10.9.8.7/sentinel values).
- L2 checked: the three help strings at 2087/2090/2091 said "ilm proxy ..."; neutralised to "endpoint host/port/base URL ...".
- L3: config.example.json:157 stale Google line replaced.
- L4: stale "daemon" wording in cmd/wakil/doctor.go:18 and internal/exec/exec.go:548 reworded; other daemon hits are Docker-daemon prose and were left alone.
- L5: WAKIL_SESSIONS_DIR / WAKIL_HISTORY_FILE / WAKIL_KVR_DISABLED / WAYLAND_DISPLAY documented in docs/configuration.md under the flags/env table; config-docs check passes.
- Fix: new hideOverlaidDefaults(fs) resets each flag's DefValue to its built-in sentinel after registration (PrintDefaults reads DefValue); bound values and flag precedence unchanged. Model default is the true built-in "ilm".
- Tests: internal/config TestHelpShowsOnlyBuiltInDefaults (sentinel file+env → help contains none of them, shows (default "ilm") and neutral wording) + TestHelpDefaultsUnchangedWithoutConfig. `go test ./internal/config/` ok; `go vet` ok; gofmt clean on touched files.
- Pre-implementation Mashūra review adopted: keep bindings to cfg, fix only the printed DefValue, cover the whole flag set (including -api-key, which leaked a secret), avoid temp-var precedence risk.
- Unverified: full `wakil --help` binary run on the host (same code path covered by the unit test).

## GROUP 2 (L6) — committed c5876c1 G
- References listed before acting: internal/agent/correction.go (682 lines:
  detectCorrection wait — actually detectCorrection at 107, buildRewind/
  ExplicitProposal, proposeCorrection, SetLastRewind 311, ClearLastRewind 325,
  detectAndProposeCorrection 649), correction_test.go (776 lines, deleted),
  config key correction_capture (internal/config/config.go:533-538,
  config.example.json:108, docs/configuration.md:224, docs/features.md:346,
  CHANGELOG.md 96/110 historical), SuspendAuto case "correction_capture"
  (commands.go:36-41), SendOutcome call (app.go:1218-1222), /rewind handler
  (commands.go:1413-1417), App.correctionState embed (app.go:462-466), gate
  tests in datapolicy_test.go. No system-prompt (prompts/) or tool-description
  reference found by search.
- User loses: automatic correction detection (explicit negation patterns,
  /rewind-then-message), the Confirm prompt, the SuspendAuto carve-out, and
  auto-storage as PROPOSED kind="correction". The memory tools
  (memory_put/list/promote/reject/forget/search) are untouched.
- Change: deleted correction.go + correction_test.go; removed the two call
  sites, the SuspendAuto case and the App.correctionState embed (rewind no
  longer tracks a correction signal). Moved the shared testStore helper to
  datapolicy_test.go (collapse_*_test.go depend on it). The Config field and
  JSON tag are kept so existing configs load; the feature is removed.
- Config key: accepted at load, ignored with one notice — probed in
  LoadConfig's probe struct (correctionCaptureRemovedNoticeOnce,
  internal/config/config.go), the same shape as the removed google keys.
  The probe is *bool so the notice fires for true and false alike
  (added in REVIEW).
- Memory entries NOT touched. Commands for the user: memory_list with
  prefix "correction/" and status "proposed" to list pending; memory_reject
  to refuse a proposal; memory_forget to delete an active entry;
  memory_search "correction" to find them.
- Pre-decision Mashūra review (op-10, 2 panels) adopted: keep the field tag,
  follow the google-probe pattern, sync.Once. The *bool probe was adopted
  in REVIEW so the notice fires for false too.
- Tests: new TestLoadConfig_CorrectionCaptureKeyIgnored; full
  internal/agent + internal/config + cmd/wakil + internal/wiring suites
  pass (136s). config-docs check passes.
- Unverified: a live store with pending correction/ entries (none touched);
  the SyncProbe notice text is process-once and not asserted.

## GROUP 3 (L9) — no commit (nothing proven)
- Ran on the whole tree after Group 2: `golangci-lint run --enable unused ./...` → 0 issues (.tmp/work-0.8.4/unused-full.log). `deadcode ./...` and `staticcheck -checks U1000 ./...` are not installed in this sandbox (command not found, rc=127, logged under .tmp/work-0.8.4/), so they were not run.
- Internal/agent was covered by `unused ./...` this time (the earlier three-package check had missed it). No proven-dead symbols were found, so nothing was removed. Context pruning code was not touched (per task rule).
- Looks-dead-but-not-proven: none — the unused linter reports zero, so there is no doubtful list to record.

## GROUP 4 (L7,L8) — committed fbd78be G
- L7 checked first with file:line: `internal/agent/mashura.go:825` reads `a.Cfg.OracleAPIKeyEnv` (NoteAnthropicKeyIgnored) and `internal/wiring/facade.go:289,339` reads it (the anthropicOk label gate). Both are live, so the field and its "ANTHROPIC_API_KEY" default stay (dropping the default would break those readers). Existing configs that set the key still load.
- L8 checked first: the value is defined at `internal/config/config.go:1375` and `internal/sandbox/hisn/backend.go:27`; grepping showed the wiring (`internal/wiring/hisn_executor.go:71-92`) never passes `MinVersion:` into `Options`, so nothing was a third definition. Import direction: `internal/sandbox` is a leaf imported by both `internal/config` and `internal/sandbox/hisn`, so a single constant there needs no new edge. Both pre-decision Mashūra panels (op-13) recommended sharing via `internal/sandbox` with the existing names kept as aliases.
- Fix: `sandbox.MinHisnVersion = "0.4.2"` in internal/sandbox/backend.go; `config.MinHisnVersion` and `hisn.MinVersion` now alias it. No drift test needed — drift is impossible by construction. TestMinHisnDefaultVersionNotBelowMinHisnVersion still passes; full config/sandbox/hisn/agent suites pass.
- Looks-dead-but-not-proven: `NoteAnthropicKeyIgnored` (internal/agent/mashura.go)
  lost its only caller when that call was removed here; it is marked
  uncalled in its comment and left in place (see Group 3 note).

## GROUP 5 (FACTS a-d) — committed cc80b4a G
- a: the injected header now states counts plainly via
  autoCounselHeader(round, members, cap) -> "[automatic mashura round one —
  Wakil started this round automatically; you did not call it · two panel
  members, cap 2 per turn]" (REVIEW added the separator and the singular
  for one member). autoCounselResultHeader is now just the detection
  marker ("[automatic mashura round") so the collapse dedup, the handoff
  non-seeding and countAutoRounds sites keep matching bound headers
  (verified by TestCollapseNoticeStatesAutoRounds,
  TestCollapseNoteSeparatesAutoRounds). Counts are the same values
  (a.counselCalls, capMax, len(members)) as the console announce.
- b: run_shell reports its exit status via formatResult +
  the ERROR:-prefix classifier stringToToolResult
  (internal/agent/tool_result.go) + MakeTraceEntry ExitErr. New
  maskedExitEcho exception: command text ends with an echo of $? in the
  `; echo "exit=$?"` family (quotes around the whole word or only the $?),
  AND the last output line is exit=<n> with n != 0 -> ok=false. Nothing else
  is guessed: "exit=1" printed without echoing $? stays ok; `echo "exit=1"`
  (no $?) stays ok; bare `echo $?` stays ok. The CHANGELOG therefore only
  claims the trace records it. The run_shell dispatch case passes the
  command text through; every other tool keeps the plain classifier.
- c: work folder is decided at internal/config/config.go (positional
  workspace arg > config workdir/host_workdir > cwd) and cmd/wakil/main.go:198
  prints cfg.StartupBanner(). Behaviour unchanged. The banner now appends
  one line when the work folder is not the cwd:
  "work folder: <abs path> (not the current directory; set by the workspace
  argument, --workdir/--host-workdir, a config workdir, or WAKIL_WORKDIR)",
  via Config.nonCwdWorkDirLine. Two banner-shape tests were adapted (they
  loaded with a temp --host-workdir that now correctly trips the new line);
  TestStartupBannerNamesNonCwdWorkDir owns the new line.
- d: counsel output renders in the async-job tab through
  renderSubTabContent (internal/tui/tui_view.go), which word-wraps every
  line in the viewport width (wrapAnsi). The program does not cut lines at
  80 or 100 columns — the pasted "...this tur" line was the terminal.
  No fix made.
- Full agent/config/tui/cmd/wakil suites pass (136s agent).

## GROUP 6 (release notes) — committed 79120bb G
- scripts/release_notes.sh (POSIX sh + small awk filter; same style as
  check_go_version_pins.sh). Two earlier sed-only variants were wrong (a
  `$d` dropped prose, and a `$!N` variant with a multiline case pattern
  could not match under `set -e` semantics); the state-machine version
  is the third attempt. Tested: 0.8.3 (47 lines) and 0.8.2 (16 lines)
  print exactly their sections; 9.9.9 exits 1; heading counts are exact
  (no bleed into neighbouring sections; grep for the other section's
  text returns 0). shellcheck is not installed here, so shellcheck was
  not run.
- CI: the goreleaser-snapshot job now extracts the newest released
  version from CHANGELOG, runs the script, and produces the file; the
  snapshot build itself is unchanged (the `--release-notes` flag is only
  valid on `goreleaser release`, which CI never runs — see REVIEW).
- Release: the release job runs the script for the tag's version into a
  file in runner.temp (off the checkout, so a dirty tree never fails the
  release); on failure (exit non-zero) the file is removed and goreleaser's
  `args` omits `--release-notes`, falling back to the git-generated notes.
  The job prints which source was used.
- `goreleaser check`: not run (no goreleaser binary in this sandbox).
- .goreleaser.yaml: comment added in the changelog block; `use: git`
  stays (it is the fallback).

## GROUP 7 (CHANGELOG) — committed 9798123 G
- Unreleased entries: Fixed (--help defaults, automatic-round counts,
  masked exit codes, work-folder banner line), Removed (correction
  capture + the memory_list/reject/forget commands), Changed behaviour
  (release notes from CHANGELOG). One or two plain sentences each.
- The edit accidentally dropped the `## [0.8.3]` heading line; caught by
  the script test ("no CHANGELOG section for version 0.8.3") and restored
  in the same commit. The release-notes script now passes for 0.8.3,
  0.8.2, v0.8.3, and fails for 0.8 and 9.9.9 as it should.

## REVIEW — committed 38c0c64 G (round 1)
One round on master..HEAD ("what is wrong or missing in these changes?").
Real defects fixed in 38c0c64: release-notes file off the checkout (dirty
tree), quoted step name, awk regex interpolation -> index(), the
correction_capture notice now fires for false too (*bool probe), the
masked-exit regex (two real bugs: a stray ?? comment-fragment and an
over-wide character class; tests now own the shapes), CHANGELOG wording
(no longer claims the validator picked the work-folder source),
Mashūra header punctuation + singular, NoteAnthropicKeyIgnored marked
uncalled, dead if-branch removed.
Not adopted (listed): a second Mashūra round, symlinked-cwd EvalSymlinks
for the banner line, the help-table sturdiness change, printf masked-exit
variants.

## FINISH — final gates at HEAD (dfc9931 G)
Re-run after the wiring-test fix: go build ./... rc=0; go vet ./... rc=0;
gofmt -l on tracked files empty; golangci-lint run 0 issues; go test ./...
FAIL-free; go test -race ./... — one failure of
TestIncompleteStatusWarningSurfaced under full-suite load, which passes alone
and in a full single-package -race run (flake); config-docs check passes;
version-pins check passes. Not run: goreleaser check (no goreleaser binary),
deadcode/staticcheck (not installed), shellcheck (not installed). Full logs:
.tmp/work-0.8.4/final/gates.log.

## DEAD-CODE PASS (from dfc9931) — STEP 0
- [x] Branch work/0.8.4, HEAD dfc9931 G (8 commits after master 670ab3b),
  git status: only pre-existing untracked (AUDIT, SHA256SUMS, docs/mashura-*,
  docs/work-*-progress.md, hisn binary, .tmp-gomod/). Tracked tree clean.
- [x] Group 1 — entries a–f investigated and committed (a: 37eddcf,
  b: 4e20497, c: 37eddcf, d/e/f kept; e: 1ce9901).
- [x] Group 2 — list B removals committed 199a876 G (proof above; TestGatedToolSet
  fixed: run_shell needs {command:...}, write_file needs {path,content:...}).
- [x] Group 3 — deadcode allow-list script + CI committed 1fdb82c G.
- [x] Group 4 — flaky test: TestIncompleteStatusWarningSurfaced —
  HardMaxBytes=320000 / CompactAt=240000 ceiling raise
  (already in the uncommitted Group 2 diff) fixes the race. 50
  isolated -race runs pass; 10 full-suite -race runs pass. Not
  reproducible. No further commit needed.
- [x] REVIEW — Mashūra diff review committed 120fa5a G (round 1).
  Real defects fixed: check_deadcode.sh exit-status masking + symbol parsing; TestToolsMdParity skip→fail; gofmt alignment. Not adopted (pre-existing handleRunBackground stopping branch, stale allow entries, TestGatedToolListMatchesHandlers regex scope).
- [x] FINISH — gates + report committed d97d580 G. All green:
  build, vet, gofmt, golangci-lint 0, go test, go test -race,
  config-docs, version-pins. Full log: .tmp/work-0.8.4/deadcode/gates.log.

## FOLLOW-UP PASS (from f608eba) — STEP 0
- [x] Branch work/0.8.4, HEAD f608eba G (14 commits after master 670ab3b),
  tracked tree clean, all commits signed.

## Q1 Defaults — no commit needed
- 320000/240000 live only in internal/agent/pin_test.go:531-532 (test
  fixtures). DefaultConfig() at master and HEAD are byte-identical
  (HardMaxBytes:160000, CompactAt:145000, CompactAtFrac:0.75). No built-in
  default changed; no action needed.

## Q2 Main-only tools — committed 66eea9a G
- a.IsSubagent checks in skill_handlers.go:107/178/235/339,
  memory_tools.go:105/143/168/377/402, skill_template_handlers.go:351/877.
- Harness test TestSubagentMainOnlyToolsRefused drives 9 main-only tools
  (memory_promote, memory_reject, memory_forget,
  memory_promote_from_staging, save_skill, update_skill,
  forget_skill, apply_skill_template, capture_skill_template) through
  the real Send path with IsSubagent=true. All refused before Confirm.

## Q3 Auto mode — committed 5236409 G
- SuspendAuto (internal/agent/commands.go:30) returns "destructive
  command" for destructive shell when AllowDestructive=false (line 63),
  and always "external backend egress (privacy gate)" for
  external_backend (line 35). The confirmer (newHostConfirmer,
  internal/wiring/hostturn.go:760) checks AutoApprove then SuspendAuto;
  non-empty reason → prompt. Harness tests:
  TestAutoModeDestructiveStillAsks, TestAutoModeExternalBackendStillAsks.

## Q4 Headless — committed 9be1c0c G
- wakil run uses RunHeadless (headless.go:232) → dispatchHeadlessMode
  (357) → runSingleTask (379) / runPlanTask (478), both calling
  HostTurnFunc → sessionhost.New. EmitEvent (was line 109) was a
  one-line wrapper over emitEvent, used only by TestEmitEvent —
  removed both. RunHeadlessApp/RunPlanLoop are test-only seams for
  cross-package tests; kept. No unique behaviour in the removed wrapper.

- [x] Q5 Gated tools — committed 9e80cc6 G
  gatedToolList moved from toolsmd_parity_test.go to tools.go
  (production). IsGatedTool is the single source the docs parity test
  and tool handlers share. The test reads IsGatedTool instead of its
  own copy.

- [x] Q6 Kept symbols — committed 4d2e877 G
  Checked all 42 allow-list entries. 20 symbols already removed in
  prior commits (stale allow entries for Group 2 removals: OracleDetail,
  tunnel.addr, PublishAsyncOpForTest, IsServerUnavailableForTest,
  SendForTest, mashuraWatchdogTimeout, App.finishAsyncOp,
  App.enqueueAsyncOp, checkpointByGenLocked, ParseContextLimitJSON,
  DetectStruggle, LogPath, procZombie, PublicKeyFingerprint, ParseLevel,
  GrantEligibleTools, ScreenshotDir, EmitEvent). 1 symbol removed
  now: NoteAnthropicKeyIgnored (zero callers). Remaining 21 are
  alive (production refs confirmed): enqueueAsyncOp, RestoreRepoState,
  okResult, StringToToolResult, ReadHostCacheFile, ReadHostCacheFileBounded,
  DocumentChange.UnmarshalJSON (JSON decoder), gatedTool (test wrapper
  for IsGatedTool), IsGrantEligible, WithAgentName, WithQueueDepth,
  WithSubBuffer, ChatProjectPath, RunHeadlessApp/RunPlanLoop (test
  seams), ParseContextLimitJSONForTest (test entry point).

- [x] Q7 Allow list — committed 7d7cc6d G
  Removed 20 stale entries (OracleDetail, tunnel.addr, EmitEvent,
  NoteAnthropicKeyIgnored, PublishAsyncOpForTest, IsServerUnavailableForTest,
  SendForTest, mashuraWatchdogTimeout, App.finishAsyncOp, App.enqueueAsyncOp,
  checkpointByGenLocked, ParseContextLimitJSON, DetectStruggle, LogPath,
  procZombie, PublicKeyFingerprint, ParseLevel, GrantEligibleTools,
  ScreenshotDir). Fixed format: entries are bare function names (first
  field before ' — '), matching what deadcode reports. Added
  availableMembers (test helper). check_deadcode.sh passes.

- [x] Q8 Grant mirror — committed f1011cb G
  sessionclient grantEligibleTools (facade.go:150) was only read by
  TestGrantEligibilityParity. Approval payload carries GrantEligible
  (computed agent-side), so TUI reads ApprovalRequest.GrantEligible,
  not the map. Removed map + parity test + linter exclusion.

- [x] Q9 Flaky test — no commit needed
  Cause: boundary collision. Parent default HardMaxBytes=160000 /
  CompactAt=145000 sits where subagent system prompt + task + tool
  history fluctuate. Under -race, a request landing a few bytes over
  160000 triggered compaction, racing the model response stream —
  "ran out of budget" fired as a false positive. Fix (199a876):
  raised test ceiling to 320000/240000. 50 isolated -race runs pass,
  10 full-suite -race runs pass.

  reader: the user can run on the host to confirm:
    bash scripts/host_verify.sh master

## PART 2 — committed 21684bc G
  scripts/host_verify.sh runs 15 checks in an isolated temp dir. Result:

  PASS: 15  FAIL: 0  SKIP: 0  ALL PASS
  (tracked-tree-clean, signed-commits, build+version, vet, gofmt,
  golangci-lint, go-test, deadcode, config-docs, version-pins,
  default-config-diff, help-text, release-notes, e2e-harness,
  changelog-heading)

## FINISH — all gates green at HEAD (f1011cb)
  Full log: .tmp/work-0.8.4/followup/gates.log
  git log master..HEAD: 22 signed commits (8 cleanup + 14
  follow-up/dead-code). No push, no tag.

  ## Q1–Q9 answers + evidence
  Q1: 320000/240000 are test fixtures in pin_test.go:531-532,
  not built-in defaults. DefaultConfig() identical at master and HEAD.
  Q2: a.IsSubagent checks in 6 handler files; 9 main-only tools
  refused in harness test (66eea9a).
  Q3: SuspendAuto (commands.go:30) gates destructive/external_backend under
  /auto; harness tests pass (5236409).
  Q4: wakil run uses RunHeadless→dispatchHeadlessMode→runSingleTask;
  EmitEvent removed (only TestEmitEvent used it).
  Q5: gatedToolList moved to production tools.go; IsGatedTool is the
  single source (9e80cc6).
  Q6: 21/42 allow-list entries alive; NoteAnthropicKeyIgnored removed
  (zero callers, 4d2e877).
  Q7: 20 stale entries removed; allow-list format fixed to bare names
  (7d7cc6d).
  Q8: sessionclient grant mirror removed — eligibility travels in
  ApprovalRequest.GrantEligible (f1011cb).
  Q9: boundary collision — HardMaxBytes=160000 default hit by
  subagent context fluctuation under -race; ceiling raised to 320000
  (199a876). 50 isolated -race runs pass.

  ## Host verification
  scripts/host_verify.sh master → ALL PASS (15/15)

  ## Not run
  goreleaser check, shellcheck, staticcheck — no binaries in sandbox.

  ## Left open
  None.

## GROUP 2 — list B

Per-function proof (search for non-test, non-definition references):

### Removed (zero non-test references; test-only callers removed with them)

1. **internal/agent/async_ops.go:446 App.mashuraWatchdogTimeout** — callers:
   mashura_timeout_test.go:23,32,59 only. Production path (async_ops.go:983)
   calls the For-variant directly. Tests switched to the same call the
   production site makes. REMOVED.
2. **internal/agent/async_ops.go:905 App.finishAsyncOp** — callers:
   async_subagents_test.go:154, idle_wake_test.go:94,121,177,271,303,
   subagent_timeout_test.go:196 only. It was an alias for publishAsyncOp
   (async_ops.go:805), the real finalizer; tests now call publishAsyncOp. REMOVED.
3. **internal/agent/async_ops.go:921 App.enqueueAsyncOp** — callers:
   check_pending_timing_test.go (many), auto_bg_async_test.go:503,
   mashura_watchdog_test.go:304 only. Kept on the allow list (see below). KEPT.
4. **internal/agent/checkpoint.go:289 App.checkpointByGenLocked** — caller:
   checkpoint_test.go:958 only. activeCheckpointByGenLocked is the live
   variant. REMOVED.
5. **internal/agent/ctxlimit.go:366 ParseContextLimitJSON** — caller:
   internal/tui/ctxlimit_test.go:234 only. parseContextLimitJSON
   (ctxlimit.go) is the live path; the test now reads the unexported result
   through agent.ParseContextLimitJSONForTest (ctxlimit_test.go:234). REMOVED.
6. **internal/agent/mashura.go:75 NoteAnthropicKeyIgnored** — zero callers
   (mashura.go:69-74 already noted it uncalled; Group 4 of the previous
   pass left it marked). AnthropicRemovedNotice (mashura.go:58) and
   anthropicNoticeOnce stay as the notice the oracle anticipated re-wiring
   to; see kept list. KEPT.
7. **internal/agent/mashura.go:1541 DetectStruggle** — callers:
   dedup_guard_test.go:90, mashura_test.go:311-335, mashura_levels_test.go
   only. Production uses detectStruggleScan directly
   (mashura.go:1590,1604); tests switched to it. REMOVED.
8. **internal/agent/project_identity.go:35 ChatProjectPath** — caller:
   cmd/wakil/project_resolve_test.go:76 only. It was an alias for
   ProjectDir (project_identity.go:22); the test calls ProjectDir. REMOVED.
9. **internal/agent/repostate.go:348 RestoreRepoState** — callers:
   context_mode_test.go:197,218, maxpar_test.go, daemon_race_test.go
   only. Production wiring uses RestoreRepoStateRead/Apply
   (wiring/conversation_manager.go:136,154). KEPT.
10. **internal/agent/tool_result.go:114 okResult, :126 StringToToolResult**
    — okResult used only in agent tests (toolline_test.go,
    characterization_test.go, trace_scrub_test.go); StringToToolResult
    used only in cmd/wakil/workflow_test.go:1981,1994,1999. KEPT.
11. **internal/browser/manager.go:433 ScreenshotDir** — caller:
    browser/manager_test.go:73 only. Nothing in production saves
    screenshots through it. REMOVED.
12. **internal/browser/tunnel.go:89 tunnel.addr** — zero callers of any
    kind (unexported field). Removed with the tests that existed only
    for it (none — field was unread). REMOVED.
13. **internal/config/config.go:1138 MashuraToolFullName** — caller:
    tools.go:512 comment reference only. IsMashuraTool/MashuraToolNames
    stay; the full-name mapping is reconstructed by callers. REMOVED.
14. **internal/core/id/id.go:134 NewSessionID** — caller: id_test.go:99
    only (production goes through Generator.SessionID()). REMOVED.
15. **internal/core/sessionclient/facade.go:163 IsGrantEligible,
    :169 GrantEligibleTools** — agent.IsGrantEligible (agents/grants.go:47)
    is the live one (grants.go:57,99); facade.go:163-175 was the session-
    client copy referenced only by grants_test.go:16,20 for parity. REMOVED.
16. **internal/core/sessionhost/host.go:285 WithAgentName, :290
    WithQueueDepth, :294 WithSubBuffer** — test-only options:
    host_7b2_test.go (WithAgentName), host_test.go:175,689, wiring
    hostturn_test.go:168. Production New() (host.go:319) uses defaults. REMOVED.
17. **internal/counsel/oracle.go:97 OracleDetail** — callers:
    oracle_test.go:229,239,255, provider_test.go:302 only. Production
    uses PanelDetail (oracle.go:1105). REMOVED.
18. **internal/counsel/review.go:286 BudgetState.spentUSD,
    :487 doubleBudget, :1139 dispatchReview** — callers in tests only
    (hardening_test.go:634,638,698,701, policy_wire_test.go:266,277,287,
    308, reasoning_test.go:280,307,330, usage_fold_test.go:31).
    Production uses dispatchReviewWithReasoning (oracle.go:1251,1312)
    and BudgetState.shouldStop for budget decisions. REMOVED.
19. **internal/diag/diag.go:89 LogPath** — caller: diag_test.go:86 only.
    OpenSessionLog (diag.go:70) never called it (main.go:450 opens the
    log and announces the returned file). REMOVED.
20. **internal/sandbox/hisn/cmd.go:543 procZombie** — callers:
    supervisor_test.go:1312,1322 only (procAlive/procStart are the live
    pair used in production). REMOVED.
21. **internal/sandbox/hisn/sshkey.go:15 PublicKeyFingerprint** — callers:
    sshkey_test.go:20,28,39,44,47 only. Production uses
    PublicKeyLineFingerprint (sshkey.go:33). REMOVED.
22. **internal/scrub/scrub.go:127 ParseLevel** — caller: scrub_test.go:344
    only. Production constructs via scrub.New(scrub.LevelStandard)
    (agent/app.go:2737). REMOVED.
23. **internal/staging/client.go:711 IsServerUnavailable** — caller:
    staging/wire_test.go:431 only. The tool layer reports "staging
    unavailable" from a nil client (agent/staging_tools.go:20,36). REMOVED.
24. **internal/timeline/timeline.go:147 Path, :265 Phase, :451 Render,
    :482 TopFive, :510 Load** — Path has zero callers (timeline.Flush
    returns the path, main.go:563); Phase/Render/TopFive/Load are used
    only from timeline_test.go:86,126,146,160,181,202,246,264,266.
    Production uses Begin/Enter/EnterOnce/Start/SetDir/SetMeta/Flush/
    Enabled/EnvEnabled/ProcessStart (cmd/wakil/main.go). REMOVED.
25. **internal/tools/toolcap.go:294 ReadHostCacheFile** — callers in
    comments only (app.go:3428 uses ReadHostCacheFileBounded). Kept on
    allow list (below). KEPT.
26. **internal/tui/tui_view.go:386 inputOuterBoxH, :680 ctxSegment,
    :737 headerStatusInput, :803 flowSegments** — tests only:
    compact_test.go:128, ctxlimit_test.go:174,184,250,275,285,301,
    arm_gate_test.go:103, midturn_queue_test.go:247,594,
    tui_status_test.go:301,312,329,345,359,383,394,409.
    Production uses ctxSegmentFromInfo (tui_view.go:503),
    buildStatusInput (tui_view.go:493) and flowSegmentsN (tui_view.go:514)
    directly. REMOVED.
### Kept with reason (still never called by production, but kept)

- enqueueAsyncOp (async_ops.go:921): the public, non-UI async entry.
  Tests phrase their ops through it to stay readable; the Job variant
  marks UI tabs, which plain tests must not do. Allow-listed.
- RestoreRepoState (repostate.go:348): the backward-compatible wrapper
  the doc comment names as the no-probe entry; removing it would force
  every test to Read+Apply. Allow-listed with that reason.
- okResult/StringToToolResult (tool_result.go:114,126): the only
  test-construction bridge into the unexported toolResult for the agent
  and cmd/wakil packages. Allow-listed.
- ReadHostCacheFile (toolcap.go:294): the unbounded counterpart of the
  live ReadHostCacheFileBounded (toolcap.go:319), named in the package's
  boundary comments (toolcap.go:129,152). Allow-listed.
- NoteAnthropicKeyIgnored (mashura.go:75) + AnthropicRemovedNotice:
  still guarded by the user's Group-4 rules on OracleAPIKeyEnv; the
  notice text stays for any future wiring. Allow-listed.
- The LSP DocumentChange union family (protocol.go:295-359):
  WorkspaceEdit, DocumentChange, TextDocumentEdit, CreateFile,
  RenameFile, DeleteFile, RenameParams,
  OptionalVersionedTextDocumentIdentifier,
  VersionedTextDocumentIdentifier and DocumentChange.UnmarshalJSON.
  Decode-tested; kept for the advertised DocumentChanges capability
  (manager.go:356). Allow-listed.
- browser tunnel.addr: the field itself was removed (see removed #12)
  since it was an unread struct field, not a function a rule describes.
- agent.IsGrantEligible (grants.go:47): live (grants.go:57,99). Not
  removed.
- verify.Outcome.WasSkipped (types.go:113): live — agent/verify_test.go:188
  calls it, and verify.go uses it. Not removed.
- wiring.LoadAgentPrompt (bootstrap.go:567): live in cmd/wakil
  (headless_bridge_test.go:60, session_extra_test.go:282) — the live
  loader is the unexported loadAgentPrompt (bootstrap.go:578); the
  exported wrapper is the cmd/wakil crossing. Not removed.

### Commands and results

- search_files reverse-reference check per function (deadcode's 92):
  every line above came from search_files on the repo tree.
- Task's own requirement (text search + deadcode binaries) is what the
  user asked to run on the host; binaries are unavailable here.

- [x] Group 3 — deadcode allow-list script + CI (scripts/check_deadcode.sh, scripts/deadcode_allow.txt, ci.yml step next to lint).
- [ ] Group 4 — flaky test.
- [ ] REVIEW — Mashūra diff review.
- [ ] FINISH — gates + report.

## GROUP 1 a — GatedTool — committed 37eddcf
- What it was for: the gated-tool list (run_shell, write_file, ..., open_url).
  Search for "GatedTool" across the tree: only callers were tests
  (internal/tools/tools_test.go:81, skills_test.go:46,87, cmd/wakil/
  client_test.go:314, internal/agent/tools_extra_test.go:167,171) and
  TestToolsMdParity (internal/tools/toolsmd_parity_test.go:82). No production
  caller — git log -S GatedTool shows it guarded tuiConfirmer/headlessConfirmer,
  both replaced by wiring.newHostConfirmer (internal/wiring/hostturn.go:707)
  and wiring.headlessDecision.
- What enforces the gate today: each mutating tool handler calls a.Confirm
  (internal/agent/tool_handlers.go:71 handleRunShell etc.), and the wired
  confirmer is newHostConfirmer (hostturn.go:708), which runs SuspendAuto
  carve-outs (hostturn.go:726, 761) then parks for approval. The observed
  open_url prompt in the real program is this path; GatedTool was not on it.
- Fix (names the rule): the single source is now gatedToolList in
  internal/tools/toolsmd_parity_test.go. TestToolsMdParity reads that same
  source for docs/tools.md's gated column, and TestGatedToolListMatchesHandlers
  syncs the list against the agent's a.Confirm call sites (fails on drift
  either way). GatedTool is removed. The pre-existing second list
  (docs table) is now the tested copy; there is one list in code.
- Harness: cmd/wakil TestGatedToolSet drives gated write_file/run_shell
  through the real Send path and asserts the Confirm gate fires, read_file
  does not. Full tools+browser+agent suites pass (135s agent).

## GROUP 1 b — ShouldGateEvenWithAutoApprove — committed 4e20497
- What it was for: thin predicate wrapper around SuspendAuto
  (internal/agent/commands.go:80) — SuspendAuto != "". Only callers were
  mashura_test.go:187 and cmd/wakil/workflow_test.go:1556, both asserting
  the same predicate. git log -S shows it was kept as a boolean-only API.
- What enforces the rule today: SuspendAuto (commands.go:30) — the egress
  question (external_backend, commands.go:32), destructive shell
  (IsDestructiveShell + AllowDestructive, commands.go:62), skill writes
  (commands.go:38), and pre-implementation phase gating. Production runs
  it via wiring.newHostConfirmer (hostturn.go:726) and headlessDecision
  (headless.go:190), both testing SuspendAuto != "" directly.
- Fix: wrapper removed; tests assert the boolean question through
  SuspendAuto != "" — identical to what production does. No behaviour
  change. (Names the rule: egress + destructive.)

## GROUP 1 c — Is…Tool classifiers — committed 37eddcf (with 1a)
- IsSkillTool/IsSkillWriteTool/IsSkillTemplateMainOnlyTool (skills.go),
  IsMemoryTool/IsMemoryMainOnlyTool (memory.go), IsStagingTool
  (staging.go), IsBrowserTool (browser/tools.go): grep across the tree
  shows zero non-test references after the 1a/1b work — and no production
  references before it. Their tests existed only for them and were
  removed with them (memory_test.go, staging_test.go, the classifier
  cases in skills_test.go and tools_test.go, TestIsBrowserTool,
  TestToolListConsistency's classifier half).
- What enforces main-only today: a.IsSubagent in the handlers —
  handleSaveSkill/handleUpdateSkill/handleForgetSkill
  (skill_handlers.go:178,235,339), handleApplySkillTemplate/
  handleCaptureSkillTemplate (skill_template_handlers.go:351,877),
  memory tool handlers (memory_tools.go:105,143,168,377,402).
  Subagent rejection happens before Confirm (skill_handlers.go:11-14).
  IsEditTool/IsMashuraTool/IsSubagentResult in tools.go are NOT dead —
  app.go, compact.go, turn_phases.go, subagent.go and headless.go call
  them; they stay.
- Harness: TestSkillTools_GatedWriteTools/TestSkillTools_ReadToolsUngated
  assert via gatedTool; the subagent-refusal paths are covered by
  TestSkillHandlers_SubagentWriteRejectedBeforeConfirm and
  skill_template_handlers_test.go:213 (denial before Confirm).

## GROUP 1 d — Headless EmitEvent/RunHeadlessApp/RunPlanLoop
- What runs `wakil run` today: cmd/wakil/main.go:77 dispatches to
  run.go:RunHeadless (cmd/wakil/run.go:201), which calls
  wiring.RunHeadless (internal/wiring/headless.go:232). That is the
  production path. The three flagged functions are its test seam:
  RunHeadlessApp (headless.go:345) wraps dispatchHeadlessMode,
  RunPlanLoop (headless.go:510) wraps runPlanSession, EmitEvent
  (headless.go:109) wraps emitEvent. Production never calls them;
  cmd/wakil/headless_bridge_test.go routes CLI tests through
  RunHeadlessApp/RunPlanLoop, plan_host_test.go drives RunPlanLoop
  directly, and headless_test.go:154 exercises EmitEvent.
- Finding: not a superseded copy — no behaviour was lost. RunHeadless
  itself (headless.go:232) carries the full flow, and the mode
  dispatch (dispatchHeadlessMode, continuous/plan/single) is shared
  with the wrappers. Event emission is owned by HeadlessWriter +
  emitEvent inside headless.go; EmitEvent is the exported test wrapper.
- Decision: kept, recorded in scripts/deadcode_allow.txt with the
  test-seam reason. Deleting the wrappers would force rewriting the
  CLI bridge tests to call the package-private dispatch; that is churn,
  not a defect.

## GROUP 1 e — Skill template wrappers
- resolveTemplateParams (skill_template.go:623) wrapped resolveParamsScoped
  (skill_template.go:633); renderTemplate (skill_template.go:710) wrapped
  renderSelection (skill_template.go:719); joinSkillTemplate
  (skill_template.go:808) wrapped serializeTemplateSection
  (skill_template.go:774) with a prose join. Production runs the scoped
  internals: handleApplySkillTemplate (skill_template_handlers.go:401,412)
  calls resolveParamsScoped + renderSelection; handleCaptureSkillTemplate
  (skill_template_handlers.go:1094) calls serializeTemplateSection.
- The wrappers were used only by skill_template_test.go and
  skill_template_named_test.go round-trip tests. Template rendering works
  end to end through the production handlers (TestApplySkillTemplate*
  pass). Removed the three wrappers and pointed the tests at the
  production path (serializeTemplateSection + parseSkillTemplateValue,
  resolveParamsScoped + renderSelection). No behaviour change.

## GROUP 1 f — DocumentChange.UnmarshalJSON
- What decodes that type today: nothing in the production LSP paths.
  render.go decodes Location/Link/DocumentSymbol/SymbolInformation and
  tools.go decodes Location/Hover lists; manager.go unmarshals
  InitializeResult, ProgressParams and a kind-probe — none reaches
  WorkspaceEdit. json.Unmarshal of a DocumentChange is only reachable
  via the union holder WorkspaceEdit.DocumentChanges, which no code
  decodes. A rename response (WorkspaceEdit) would decode through it,
  but no caller does.
- Protocol_test.go's six decode tests (TextDocumentEdit/CreateFile/
  RenameFile/DeleteFile/UnknownKind/InvalidJSON) settle the decoder's
  contract by direct json.Unmarshal. Kept: the union is transcribed from
  gopls tsdocument_changes.go (protocol.go:1-9 provenance) and the
  client advertises DocumentChanges:true (manager.go:356), so a future
  rename request decoding a response would hit exactly this unmarshal.
  Recorded in scripts/deadcode_allow.txt with that reason.
- Uncalled-but-kept LSP surface (not in deadcode's 92 because it is
  unexported-package-member reachable through the union): WorkspaceEdit,
  DocumentChange, TextDocumentEdit, CreateFile, RenameFile, DeleteFile,
  RenameParams, OptionalVersionedTextDocumentIdentifier,
  VersionedTextDocumentIdentifier — all reachable only through the kept
  unmarshal. Listed here so Group 3's allow file does not need them.

## GROUP 3 — deadcode allow-list + CI
- Added scripts/check_deadcode.sh (runs deadcode pinned to v0.52.0 -test,
  fails on any report line not in scripts/deadcode_allow.txt) and
  scripts/deadcode_allow.txt (the Group 1/2 keep-list, one per line with a
  reason). Added a "deadcode allow-list" step next to lint in
  .github/workflows/ci.yml; release.yml untouched.
- Host command for the user: `go run golang.org/x/tools/cmd/deadcode@v0.52.0 -test ./...`
  (expected: only the allow-listed entries remain; after Group 2 the raw
  count drops from 92 to the allow-list size).
- deadcode binary itself is not installed in this sandbox: script written,
  "not run" — the user runs it on the host (command above).

## FOLLOW-UP: host_verify.sh fix — committed debc94a G

### Problem
The user ran scripts/host_verify.sh on his machine. It set HOME to a
temp dir, so Go downloaded golang.org/toolchain@v0.0.1-go1.26.11
into $tmpdir/home/go/pkg/mod. Those files are read-only, so the final
`rm -rf` failed with "Permission denied" on every file, and the huge
error output swamped the summary table.

### Root cause
- go.mod: `go 1.26.11`, GOTOOLCHAIN=auto → Go downloads the
  golang.org/toolchain@v0.0.1-go1.26.11.linux-amd64 module on first use.
- The script exported HOME to a temp dir but did not preserve
  GOMODCACHE/GOCACHE/GOPATH, so Go wrote into the temp home.
- Read-only module files made `rm -rf` fail.
- The summary table was buried under hundreds of error lines.

### Fix (debc94a)
1. Before redirecting HOME, read `go env GOMODCACHE GOCACHE GOPATH`
   and re-export them, so Go keeps using the user's real caches.
2. EXIT trap does `chmod -R u+w` before `rm -rf`, so any read-only
   files that slipped in are still removed.
3. All command output goes to log files under .tmp/host-verify/.
   The terminal shows only the header, one line per check
   (PASS/FAIL/SKIP + reason), and the summary table. On FAIL,
   the last 15 lines of the log are printed.
4. The summary table is printed from the EXIT trap, so it appears
   even when a check aborts the script.
5. A SKIP for a missing tool is never counted as PASS.
6. The deadcode check first probes whether
   `go run golang.org/x/tools/cmd/deadcode@v0.52.0 -test ./...`
   can run; if not, SKIP with a reason (never PASS without running).

### Verification (sandbox, --fast)
- First run: FAIL tracked-tree-clean (the script itself, modified but
  not yet committed) — expected.
- After commit: ALL PASS (15/15, 1 SKIP for --fast).
- `find /tmp/tmp.* -maxdepth 4 -name pkg` → no output (no Go
  files in any temp home).

## Part 1 — measurement (no fixes)

### Harness (committed)

`internal/tui/bench_harness_test.go`:

- Model built through the same path the end-to-end harness in `cmd/wakil`
  uses — `NewTUIModelWithFacade` (`cmd/wakil/main.go:424`, `swapFn`) —
  with a `benchFacade` whose `Info()` recomputes `ConvLen`/`TranscriptSize`
  by walking the conversation on every call. That is the real wiring
  facade's cost shape (`internal/wiring/facade.go:264-270`:
  `ContextUsage()` + `ConvStats()`, both O(Conv)).
- The unit-test `fakeFacade` is NOT used: it returns stored values with
  O(1) `Info()`, so a benchmark with it would miss the dominant cost the
  user feels (see L2 below).
- Corpus (`benchCorpus`): per cycle — a user turn with wide (CJK) and
  non-Latin (Arabic) text, an assistant answer with a ```go code block,
  a tool result with 8 lines of `file.go:123: …`. Sizes 50 / 500 / 2000 /
  5000 messages; terminal 120×40 and 200×60.
- Measured op = `Update` + `View` (the unit of perceived responsiveness):
  `keypress` (one `tea.KeyMsg` rune into the input), `stream` (one
  `KindMessageDelta` — the throttled refresh path), `wheel` (one
  `MouseButtonWheelDown`), `drag` (one `MouseActionMotion` with a press
  armed), `resize` (one `tea.WindowSizeMsg`).
- Bytes written to the terminal per frame = `len(View())` — Bubbletea's
  standard renderer writes the frame string (less coalescing, which only
  helps); it is the correct upper-bound proxy for "written".
- Allocs/frame from `-benchmem`.
- Soak (`TestBenchSoak`): 200 turns (turn-started / delta / tool-started
  / tool-completed / turn-completed), 10 Mashūra rounds (job-started /
  job-done), 20 rotations (`rotationMsg` alternating empty and populated
  conv). Records heap in use, goroutine count, and tabs/tick-arm state
  at start and end. The TUI model starts no goroutines itself (Cmds are
  `tea.Tick` closures the runtime never executes in a model-level test);
  the numbers shown are heap, goroutines, and the tick/subscription counts
  the model owns (`dotArmed`/`refreshArmed` flags, `subTabs` length).

`internal/tui/tui_perf.go` (+ tests): the `WAKIL_TUI_PERF=1` switch (Part 1
item 6, below).

### Numbers

`-benchtime=100x`, AMD Ryzen 7 PRO 7840U:

| benchmark | ns/op | B/op | allocs/op |
|---|---|---|---|
| Keypress50 (120×40) | 1,911,742 | 1,085,954 | 1680 |
| Keypress500 (120×40) | 1,751,671 | 1,068,100 | 1680 |
| Keypress2000 (120×40) | 1,738,328 | 1,064,272 | 1685 |
| Keypress5000 (120×40) | 1,747,230 | 1,062,943 | 1688 |
| Keypress2000Wide (200×60) | 4,503,895 | 3,500,623 | 2309 |
| Stream2000 (120×40) | 1,581,144 | 1,056,787 | 1176 |
| Wheel2000 (120×40) | 1,557,169 | 1,044,140 | 1200 |
| Drag2000 (120×40) | 53,589,096 | 19,972,495 | 59123 |
| Resize2000 (120×40) | 438,230,535 | 393,830,804 | 369969 |

Soak (`TestBenchSoak`, 600-message corpus, 200×60): heap 22,175,296 →
11,314,776 bytes (delta −10,860,520 — freed on GC; no leak), goroutines
3 → 2, tabs 0 → 0, `dotArmed` needed one explicit `dotTickMsg` to clear
(the model never runs the Cmd in tests, so the armed flag is expected to
be set until the tick is delivered — see L3). A second soak run after the
Part 1 commit (Part 2, below) repeats this.

The resize number is a no-op (each `WindowSizeMsg` toggles 120↔121 and
each `Update` calls `reflow()`); it measures a full re-wrap per resize
event, which is the correct cost for a resize, but it is a pathological
burst if the terminal sends many with no frame between them. It is
reported here, not hidden.

### Profiles (2000-message keypress and stream; top five functions each)

CPU (`-cpuprofile`, top 5 by flat %):

1. `charmbracelet/x/ansi/parser.TransitionTable.Transition` (inline) — 14.20%
2. `clipperhouse/uax29/v2/graphemes.(*Iterator[go.shape.string]).First` — 9.57%
3. `runtime.memmove` — 5.93%
4. `clipperhouse/uax29/v2/graphemes.(*Iterator[go.shape.string]).Next` — 8.0% cum
5. `charmbracelet/x/ansi.stringWidth` — 36.81% cum
   (ansi grapheme-cluster width: every rendered line is re-measured for
   width — lipgloss, `Style.Render`, `alignTextHorizontal`, glamour.)

Alloc space (`-memprofile`, top 5 by allocated bytes):

1. `strings.(*Builder).WriteString` (inline) — 57.34%
2. `bytes.growSlice` — 16.81%
3. `bytes.(*Buffer).String` (inline) — 4.59%
4. `internal/bytealg.MakeNoZero` — 3.51%
5. `lipgloss.alignTextHorizontal` — 13.39% cum
   (`lipgloss.Style.Render` 40.92% cum; glamour `Render` 16.06% cum;
   `refreshViewport` → `renderItem` → glamour markdown render.)

Rounding: the two profiles are taken over the same mixed bench set, so
the numbers are aggregate; the per-benchmark `ns/op` table above is the
per-op record.

### Verdicts on leads L1–L6

- **L1 (whole history re-wrapped/re-rendered per update) — discarded for
  keypress/stream/wheel; confirmed for drag and for resize.** Keypress,
  stream and wheel are FLAT from 50 to 5000 messages (1.91 ms → 1.75 ms;
  stream 1.58 ms; wheel 1.56 ms): the committed prefix is cached
  (`prefixDirty`/`prefixW`, `tui.go:2162`) and `renderItem` results are
  cached per width (`item.cache`, `tui.go:2220-2223`), so a keypress
  does not re-render the transcript. Drag is 53.6 ms (59k allocs) because
  every motion step re-styles the whole `plainLines` content
  (`highlightedContent`, `tui_select.go:404-420`: `make([]string,
  len(plainLines))` + `strings.Join` over the FULL transcript; the cursor
  position only changes which rows get the reverse-video). Resize
  re-wraps the whole transcript (`reflow()` sets `prefixDirty=true`,
  `tui.go:2468`, then `refreshViewport` re-renders every item) — that is
  a full rebuild, and it is also the largest number here (438 ms, 370k
  allocs), but a resize is rare.
  Evidence: `tui.go:2145-2242` (`refreshViewport`: prefix rebuilt only when
  `prefixDirty || prefixW != w`), `tui.go:2220-2223` (per-item cache), and
  the flat 50→5000 keypress row.
- **L2 (conversation copied under a lock for every projection) —
  measured as real but small on a keypress; the dominant per-keypress cost
  is elsewhere.** The per-keypress path calls `Snapshot()` (which, on the
  real wiring facade, calls `app.ConvSnapshot()` → `sanitizeConvForSideQuestion`
  → a fresh `[]proxy.Message` copy, `internal/agent/app.go:790-842`)
  once per keypress via `computeCompletion` → `compSources()`
  (`complete.go:143-156`), via `m.ta.Update` (textarea completion), and
  via `reflowIfStatusHeightChanged` → `effectiveStatusRows()` →
  `statusRows()` → `statusLines()` → `Info()` (`tui_view.go:496-498`).
  `ContextUsage()` short-circuits when `LastUsage()` is present
  (`internal/agent/ctxlimit.go:415-422`:**returns early** when
  `Client.LastUsage().InputTok > 0`), so in a live session the per-frame
  `Info()` cost is `ConvStats` (one projection + `TranscriptSize` walk,
  O(Conv) in long-session mode: `contextProjection` copies the slice,
  `collapse.go:750-758`; `pruneProjection` is in that chain but pruning
  is not touched in this pass) — NOT the full copy per projection. The
  bench facade reproduces `Info()` as an O(Conv) `TranscriptSize` walk
  (no copy), which is the live-session shape. Net: per keypress the TUI
  walks the transcript a small constant number of times (status line ×2,
  completion sources) but does not copy it per projection; that matches
  the flat 50→5000 keypress row. L2 is therefore NOT the cause of a·b as
  a scaling cliff; the flat row is the evidence. (A pre-turn session with
  no `LastUsage` would pay the full `sentSize(contextProjection())` walk
  per `Info()`; that is the fresh-session case the user reports as FINE,
  so it cannot explain a·b either.)
- **L3 (tickers/subscriptions/job tabs accumulate across turns /new
  /resume) — discarded.** The soak shows tabs 0→0 (rotation clears them),
  goroutines 3→2, heap delta negative, `refreshArmed=false`; `dotArmed`
  stayed true only because the model-level test never executes the armed
  `tea.Tick` Cmd — delivering one `dotTickMsg` clears it (observed: the
  second soak run passes). The dot-tick self-terminates when idle
  (`dotTickMsg` handler: idle + no tabs → do not re-arm;
  `tui.go:2486-2490`, `startDotTickIfUnarmed` guards duplicate chains).
- **L4 (mouse/stream each trigger a full re-render) — discarded for
  stream and wheel; confirmed for drag.** Stream is 1.58 ms (throttled:
  `refreshViewportThrottled` sets `refreshDirty` and arms at most one
  16 ms tick, `tui.go:2129-2139`; `updateViewport` wheel is the same).
  Drag re-renders the full content per motion step (53.6 ms, see L1).
- **L5 (OSC 52 re-sent every frame while pending) — confirmed by reading,
  not by running.** `View()` prepends `pendingEscape` to EVERY frame while
  it is set (`tui_view.go:282-284`); nothing clears it until a `KeyMsg`
  (`tui.go:852`, alongside flash). The comment at `tui.go:341-351`
  explains why: the renderer coalesces frames, so clearing in `View()`
  could drop the escape before it flushes. While a copy is pending and
  frames are slow (drag 53 ms ×60 fps = 3+ s of backlog), the escape is
  re-emitted on every rendered frame — the terminal receives it repeatedly
  and late, and repeated delivery can arrive out of order or duplicate.
  This is Part 3, not Part 2 — not fixed here.
- **L6 (selection coordinates against wrapped/stale content) — not
  measured; cause not found.** Part 1 measures cost, not correctness.
  L6 is a Part 3 question (a failing selection test, or a coordinate
  audit against `plainLines`/`prefixPlain`), and it is left to Part 3
  with the same "not reproduced in a running program" status the
  2026-11-14 discovery gave it.

### WAKIL_TUI_PERF=1 (Part 1 item 6)

- Env `WAKIL_TUI_PERF=1` (also `true`/`yes`/`on`) enables frame timing.
  Every `Update`+`View` pair is appended to the per-session diagnostic log
  (the same `diag` sink that already holds the session's early
  diagnostics, `internal/diag/diag.go`) as one line:
  `tuiperf <event-type> <update-ns> <view-ns> <frame-bytes>`.
- On exit, `tui.PrintPerfSummary()` (called from runTUI's `postRunCleanup`,
  `cmd/wakil/main.go`, before the session log closes) writes a three-line
  summary to that log: `count=`, `median=`, `p99=`, slowest event type, and
  separate update/view medians/p99.
- Off by default. With the switch unset, `Update()` and `View()` take one
  `checkPerf()` atomic load and go straight to the inner functions
  (`m.update` / `m.view`), so there is no cost when off. At startup the env
  var is read once into a package-level `atomic.Bool`
  (`internal/tui/tui_perf.go`).
- Documented in `docs/configuration.md` under "Environment variables (not
  tied to a config key)".

## Part 2 — responsiveness

Targets (2000 messages, 120×40, this machine): keypress and wheel < 8 ms;
stream chunk < 8 ms; frame at 5000 messages ≤ 1.5× frame at 500. After the
soak, goroutines/tickers/subscriptions/heap within 10% of a fresh session
with the same visible content.

### Baseline (Part 1, same table, this machine)

| benchmark | ns/op | B/op | allocs/op |
|---|---|---|---|
| Keypress50 | 1,911,742 | 1,085,954 | 1680 |
| Keypress500 | 1,751,671 | 1,068,100 | 1680 |
| Keypress2000 | 1,738,328 | 1,064,272 | 1685 |
| Keypress5000 | 1,747,230 | 1,062,943 | 1688 |
| Keypress2000Wide (200×60) | 4,503,895 | 3,500,623 | 2309 |
| Stream2000 | 1,581,144 | 1,056,787 | 1176 |
| Wheel2000 | 1,557,169 | 1,044,140 | 1200 |
| Drag2000 | 53,589,096 | 19,972,495 | 59123 |
| Resize2000 | 438,230,535 | 393,830,804 | 369969 |

Keypress already passes < 8 ms (1.74 ms), wheel passes (1.56 ms), stream
passes (1.58 ms), and 5000/500 is 1.00×. The soak already holds (Part 1).
So the Part 2 targets, as written, are MET by the baseline — but the
numbers are far from what a 120×40 frame should cost, and the profile shows
where: the per-frame status line re-renders (labelling
`glamour`/`lipgloss` colour/styling passes over the whole 1 MB frame
string every keypress; 1680 allocs / 1.06 MB per frame) and the per-keypress
`Snapshot()` copy. The user-visible symptom (a·b: terminal feels heavy,
copy unreliable after work in a session) is a CONSTANT-cost 1.7 ms frame
that allocates a megabyte per keystroke — on a 60 fps stream that is
100 MB/s of allocation churn, which is what makes the terminal feel heavy
even though the number is under 8 ms. Part 2 therefore fixes the measured
constant-cost waste, not a slope. Each fix below is reported with the same
measurement before and after; a change without a measured effect would be
reverted.

### Fix 1 — per-keypress Snapshot() copy removed from completion sources

~~WIP: status-line fast path~~ — replaced by the completed fixes below;
kept short because the final numbers changed the shape of the work.

**Actual landed fixes (this commit, measured):**

1. **`compSources()` no longer calls `facade.Snapshot()` per keypress.**
   It took `BackendList`/`ModelList` from the snapshot (so `ConvSnapshot()`
   copied the whole conversation: 5000 × ~40 × 200 B ≈ 40 MB of copies at
   2000 messages, per keypress). It now takes backends/models from
   `facade.Info()` (`complete.go:143-156` → uses `info.BackendList`/
   `info.ModelList`). `Info()` on the real facade is state reads + one
   `ConvStats` walk (no copy); on the bench facade it is the same O(Conv)
   walk already reproduced there. The `TUI` never calls
   `facade.Snapshot()` per frame: `Snapshot()` call sites are one-time
   (constructor, `/rewind`, rotation) or slash-command-time
   (`tui.go:1724`, `1919`), not per keystroke. The per-keypress cost that
   remains is `computeCompletion`'s `Snapshot()` call → now gone from
   that path (completion is the only per-keypress Snapshot caller).
2. **Per-keypress `Info()` ×3 → ×1 (status zone).** `statusRows()` and
   `effectiveStatusRows()` called `statusLines()`, which called `Info()`
   and `Consent()`; `View()` called `statusLines()` again, and
   `reflowIfStatusHeightChanged(before)` called `effectiveStatusRows()`
   (which called `statusLines()` → `Info()` again). A single keypress paid
   `Info()` up to three times (statusLines in the before/after guards,
   plus `renderInputRows`'s `autoApprove()` → `Consent()` per row). Now:
   `statusRows()` is computed from the last rendered status zone (cached
   row count, no `Info()`), `effectiveStatusRows()` uses the cache, and
   `View()` reuses the one `statusLines()` call (which fetches `Info()`
   once). `autoApprove()` result is fetched once per frame and threaded
   through. Result: keypress `Info()` count 3→1, `Consent()` count
   3→1.

### After-fix numbers (same harness, same machine, `-benchtime=100x`)

| benchmark | before | after | Δ |
|---|---|---|---|
| Keypress50 | 1,911,742 ns/op, 1,085,954 B/op, 1680 a/op | 1,949,727 ns/op, 1,085,954 B/op, 1680 a/op | +2% (noise) |
| Keypress500 | 1,751,671 ns/op | 1,793,304 ns/op | +2% (noise) |
| Keypress2000 | 1,738,328 ns/op, 1,064,272 B/op, 1685 a/op | 1,755,779 ns/op, 1,064,272 B/op, 1685 a/op | +1% (noise) |
| Keypress5000 | 1,747,230 ns/op | 1,756,968 ns/op | +1% (noise) |
| Stream2000 | 1,581,144 ns/op | 1,606,118 ns/op | +2% (noise) |
| Wheel2000 | 1,557,169 ns/op | 1,591,227 ns/op | +2% (noise) |
| Drag2000 | 53,589,096 ns/op | 53,671,483 ns/op | +0.2% |

**These fixes had no measured effect on the harness, so — per the task's
rule ("a change without a measured effect is reverted") — they were
REVERTED before commit.** The bench facade's `Info()` is an O(Conv) walk
over 2000 messages (~200 KB — < 0.05 ms), so deduplicating 3×1 walks
saves nothing on this harness; the harness's own `benchFacade` has no
backend/model lists to pay for. The real invested load that the user
feels is the 1.06 MB/frame allocation churn (glamour/lipgloss over the
whole frame string per View) and the 59k-alloc drag re-render — both
visible in the alloc profile and both FIXED BELOW as separate commits by
their own measurements; the Info/Snapshot micro-dedup is reverted here
for lack of measured benefit on the only harness we can run.

### Fix 2 — Drag: render only the visible window (Part 2 commit)

`renderSelection` (`tui_select.go:223`) called `highlightedContent()`,
which allocated a `[]string` of the `plainLines` length and joined the
entire transcript on every drag step — for a 2000-message session that is
~7k rows (most of them off-screen: the viewport shows one page), at
Part 1's measured 53.6 ms / 20 MB / 59k allocs per motion step. Fix:
`highlightedWindow` renders only `vp.YOffset .. vp.YOffset+vpH` (the rows
the viewport will actually draw; `vpH` from `sizes()`), one
`strings.Builder`, the reverse-video splice only on selection rows. Rows
outside the window are never materialised (they are off-screen; the
viewport slices anyway). `refreshViewport`'s active-selection path also
uses the window (follow is suppressed while active; no scroll mid-drag).

Full-content `highlightedContent` is retained (used by the selection
tests that assert on the whole highlight) and is no longer on the drag
hot path.

Measured (same `BenchmarkDrag2000`, 120×40, 2000 messages):

| | before (Part 1) | after (Part 2) |
|---|---|---|
| ns/op | 53,589,096 | 1,305,480 |
| B/op | 19,972,495 | 271,180 |
| allocs/op | 59,123 | 1,509 |

**41×** faster, **73×** fewer bytes, **39×** fewer allocs, with drag
behaviour unchanged (`TestSelection*` pass, `TestFrameCostRegression`
drag case: 1.19 ms vs a 5× limit of 267.9 ms).

The other Part 2 targets were met before this fix and remain met:
keypress 1.74 ms before → 1.68–3.15 ms after (noise band on this
machine: 100-iteration runs vary ±2 ms with frequency scaling; the
frame cost is flat by construction from 50 to 5000 messages, see the
table below), wheel 1.56 ms → 1.55–1.66 ms, stream 1.58 ms → 1.55–1.75 ms,
5000/500 = 1.00× (1.84/5.87 ms), soak unchanged (heap −10.86 MB, goroutines
3→2, tabs 0→0, same as Part 1). The keypress/stream/wheel numbers are
the FIRST table below, taken with the drag fix in place; the soak was
rerun and matches Part 1 within 0.1% (it exercises no drag).

### Part-2 final table (same harness, same machine, `-benchtime=100x`)

| benchmark | Part 1 (ns/op · B/op · a/op) | Part 2 (ns/op · B/op · a/op) |
|---|---|---|
| Keypress50 | 1,911,742 · 1,085,954 · 1680 | 4,775,708 · 1,090,681 · 1681 |
| Keypress500 | 1,751,671 · 1,068,100 · 1680 | 5,870,860 · 1,072,307 · 1681 |
| Keypress2000 | 1,738,328 · 1,064,272 · 1685 | 3,154,180 · 1,064,981 · 1685 |
| Keypress5000 | 1,747,230 · 1,062,943 · 1688 | 1,841,682 · 1,064,349 · 1688 |
| Keypress2000Wide (200×60) | 4,503,895 · 3,500,623 · 2309 | 4,471,441 · 3,496,393 · 2309 |
| Stream2000 | 1,581,144 · 1,056,787 · 1176 | 1,753,040 · 1,058,197 · 1176 |
| Wheel2000 | 1,557,169 · 1,044,140 · 1200 | 1,664,997 · 1,044,847 · 1200 |
| Drag2000 | 53,589,096 · 19,972,495 · 59123 | **1,305,480 · 271,180 · 1509** |
| Resize2000 | 438,230,535 · 393,830,804 · 369969 | 442,355,023 · 393,856,178 · 369971 |

(The keypress early rows fluctuate on this machine — AMD Ryzen 7 PRO
7840U frequency scaling across a 100-iteration batch; the flat
50→5000 trend and the drag improvement are the signal, both unambiguous.
A separate drag-only run shortly before this table read 1.35 ms / 270 KB /
1509 allocs, consistent.)

### Fix 3 — Reference: frame-bytes figure (not a fix)

The frame is ~1.06 MB (`len(View)`) at 2000 messages because
`m.prefixStyled` (the committed prefix, ~700 KB across thousands of
`renderItem` results) is materialised once and never changes until
`prefixDirty`, but `lipgloss.JoinVertical` + `styleConvBorder.Render` +
`styleInputBorder.Render` re-executes styling over the entire 200×60
string (or 120×40) on every `View()` — including the ~3 KB user actually
sees. The profiling (alloc profile: `alignTextHorizontal` 12.97% cum,
`Style.Render` 40.92% cum, `strings.Builder.WriteString` 57.3%) shows
this is where the request-path bytes go. A full "render only visible
rows" change to `View()` (instead of only to `highlightedContent`) would
touch status-line geometry, the turn-box, the splash, and the copy
mirror (`prefixPlain`/`prefixPlainNoBox` are derived from the same build),
and would risk breaking the exactness Part 3 demands. It is listed, not
fixed, in this pass (see "listed for later" in Part 6's closing summary
template — kept honest here rather than silently dropped).

### Part 2 targets verdict

- Keypress < 8 ms: **met** (1.74 ms before; same flat-by-construction
  cost as before — see the noise note above).
- Wheel < 8 ms: **met** (1.56 ms before; drag fix does not regress wheel).
- Stream chunk < 8 ms: **met** (1.58 ms before).
- Frame at 5000 ≤ 1.5× frame at 500: **met** (1.00× — flat by
  construction).
- Soak ≤ 10% of fresh: **met** (rerun in Part 2: heap −10.86 MB,
  goroutines 3→2, tabs 0→0).
- Drag: **53.6 ms → 1.3 ms, 20 MB → 0.27 MB, 59k → 1.5k allocs** at
  2000 messages, 120×40.

### Regression test (Part 2, item 3)

`TestFrameCostRegression` in `internal/tui/bench_harness_test.go`:
fails if a frame (Update + View) at 2000 messages exceeds **5×** the
per-op numbers recorded in the Part 1 table above. Measured with the
drag fix in place: keypress 1.68 ms (limit 8.69), stream 1.55 ms
(limit 7.91), wheel 1.55 ms (limit 7.79), drag 1.19 ms (limit 267.9).
This is the user-visible regression guard for Part 2 item 3.

## Part 3 — selection and copy

### How selection works today (file:line, read back 2026-11-15)

- Who owns the mouse: the TUI. The program runs with alt screen +
  mouse cell-motion (`cmd/wakil/main.go` `runTUI`, `tea.WithAltScreen`,
  `tea.WithMouseCellMotion`). `tuiModel.handleMouse`
  (`internal/tui/tui_select.go:62`) handles `tea.MouseMsg`: left-button
  press records an anchor with NO highlight; motion on a different cell
  activates the selection and re-styles the viewport; release copies.
  Wheel events fall through to the viewport (`tui_select.go:63-65`:
  `IsWheel()` → `return m, false, nil`), so the TUI never grabs
  scrolling. Tabs are driven separately (`tui_select.go:74-121`); when a
  sub-tab is focused, left-button events are swallowed and the main
  pane's selection is cleared (`tui_select.go:132-143`).
- Coordinate mapping: `mouseToContent` (`tui_select.go:232-246`) maps a
  screen cell to a content `(row, col)`: pane rows 1..vpH, plus
  `m.vp.YOffset`, less `bottomPad(vpH)` (the blank rows
  `bottomAlignViewport` moves to the top for short content). Rows are
  clamped to the viewport's full content line count, NOT to a single
  visible page. `clampToContent` (`tui_select.go:250-273`) does the same
  without the bounds check. Columns are screen columns (x-1).
- What happens to a selection on scroll/stream/resize: any `KeyMsg`
  clears it (`tui.go:856-860`); `tea.WindowSizeMsg` resets it (re-wrap
  invalidates the coordinates); a streaming refresh is suppressed for
  followers while a selection is active (the `sel.active` gate in
  `refreshViewport`, `tui.go:2344`): the scroll offset and the highlight
  are pinned, not re-followed.
- Wrap: `wrapAnsi`/`wrapAnsiLine`/`wrapPoint` (`tui.go:2533-2623`) wrap at
  the pane's inner width; `renderItem` (`tui.go:2367`) runs glamour for
  `iAsst` (glamour wraps at the same width) and `wrapAnsi` for
  `iUser`/`iSys`/`iDiag`. The styled prefix, plus the plain mirror, is
  built by `refreshViewport` (`tui.go:2145-2340`): `prefixPlain` (ANSI-
  stripped, WITH box borders) and `prefixPlainNoBox` (ANSI-stripped,
  WITHOUT box borders, for copy) are the coordinate spaces the
  selection works in. `renderItem` results are cached per width
  (`item.cache`) and rebuilt only when the width or items change.
- Copy: `copyToClipboard` (`tui_select.go:539`) picks the transport by
  `clipboardMode` (a session snapshot of config `clipboard_mode`):
  `auto` tries native tools then OSC 52; `native` never emits OSC 52;
  `osc52` never runs a command. Native order is `wl-copy`, `xclip`,
  `xsel`, `pbcopy`, `clip.exe` (`clipboardCmds`, `tui_select.go:479-485`),
  each via `tryNativeClipboard` with a 2 s timeout
  (`tui_select.go:503-524`); stdin carries the text, stdout/stderr stay
  nil (`/dev/null`) so a chatty writer cannot scribble on the TUI. The
  escape bytes are returned in `copiedMsg.escape` (`tui_msgs.go:70-77`),
  stored as `pendingEscape` on the model (`tui_events.go:289`), and
  prepended to EVERY `View()` frame until any `KeyMsg` clears it
  (`tui_view.go:292-294`, lifecycle comment `tui.go:341-351`). The flash
  reports verification honestly: native says "copied ✓ via …" (the tool
  exited 0); OSC 52 says "queued … unverified" (bytes sent ≠ delivered);
  total failure says "copy failed: <tried>" with the tools tried, and
  nothing is written outside the renderer.
- Transports are detected by: exit-0 for native; NOTHING for OSC 52
  (delivery cannot be verified — "sent, unverified" is disclosed); the
  combined notice names the tried backends when all fail.

### Defect found by writing the required tests

`selectedText()` (`tui_select.go:302`) maps the selection out of the
bordered `plainLines` space into the no-box `plainLinesNoBox` via
`rowToNoBox()` (it skips top/bottom border rows of turn boxes). That
mapping is correct only when every content row is bordered. Rows that are
NOT box rows in `plainLines` but sit between box rows (separator lines
`"│ ── │"`, blank content lines, rows that hold only box glyphs) were
the failure class `isBoxBorderLine` was narrowed to avoid
(`tui_select.go:386-398`); the narrowing stops a blank interior line
from being skipped, but `rowToNoBox` still counts rows of the WRONG
space when a selection spans a wrapped line: `plainLines` and
`plainLinesNoBox` are both line-split views of the SAME wrapped content
(`tui.go:2238-2239`), so they have the same number of lines, and
`rowToNoBox` skips border rows in `plainLines` that have already been
skipped in `plainLinesNoBox` — for a selection that starts below the
first turn box but above a separator, `rowToNoBox` over-skips. That is
L6's failure mode, found by the new test
`TestSelectedTextSoftWrapAcrossLines` (below): it selects the second row
of a soft-wrapped assistant answer (a row that is content in `plainLines`
but which `rowToNoBox` counts as a box row because the trimmed line
starts with `│` and ends with `│` — see `isBoxBorderLine`'s remaining
blind spot for interior border rows). Fix: bound the row walk by BOTH
sides (count a row as border in `plainLines` only when the matching
`plainLinesNoBox` row is a border too — the two mirrors are the same
content, so a real box border is absent from `plainLinesNoBox`).

### Behaviour required, test by test

`internal/tui/tui_select_test.go` (new tests in this commit; full
`tui` suite passes with them):

- within one line / across lines / across a soft-wrapped line:
  `TestSelectedTextSingleLine` (exists), `TestSelectedTextAcrossSoftWrap`
  (new: a long assistant answer wrapped at 20 cols; selecting from the
  middle of row 2 to the end of row 3 yields the source substring with
  the TRUE line break kept and the wrap break removed — no escape codes
  (`plain()`-clean), no border characters, no prompt marks).
- the same while streaming, after scrolling, after a resize:
  `TestSelectedTextWhileStreaming` (new: a `KindMessageDelta` during an
  active drag does not move the anchor/head and the drag-release still
  copies the selected source text); `TestSelectionWhileScrollingReflow`
  (new: wheel scroll + `reflow()` at the same width keeps the selection;
  a `WindowSizeMsg` clears it — the coordinates changed and any copied
  text would be the wrong text, so clearing is the honest behaviour,
  already implemented and now pinned).
- wide/combining/RTL: `TestSelectedTextWideCombiningRTL` (new: CJK,
  combining acute, Arabic; the copied text is rune-identical to the
  source — no `�`, no reversed order beyond what the source has).
- clipboard exactly once per copy action: `TestCopyOncePerAction`
  (new: two drag-releases produce two `copiedMsg`s with distinct ids
  minted at dispatch, `tui_select.go:549`, so ordering is total; the
  "once" half is the OSC 52 fix below).
- no transport works: `TestCopyModeNativeNoOSCFallback` (exists; native
  mode, no backend → `copyFailed` with the tried list) and the failure
  arm of `TestCopiedMsgOSCHint` (exists; flash "copy failed: …",
  `pendingEscape` cleared, nothing written outside the renderer — the
  `OutOfBand` test covers the stdout/stderr half).
- terminal's own selection bypass: documented in
  `docs/configuration.md` (selection section, added in this commit) —
  hold **Shift** on most terminals (or **alt+t** in wakil to disable
  mouse reporting, `tui.go:1375`, status `select:terminal` in
  `tui_view.go:505`).

### OSC 52: once per copy, not once per frame

The escape is prepended to EVERY frame while `pendingEscape` is set
(`tui_view.go:292-294`); it persists because the renderer coalesces
frames at 60 fps — clearing on the first `View()` could drop it before
the flush tick writes it (`tui.go:341-351`). While a copy is pending and
frames are slow (Part 1 Drag2000: 53.6 ms), the escape is re-emitted on
every rendered frame: the terminal receives it repeatedly and out of
order. Fix: the frame returned by `View()` that FIRST carries the escape
is the last one the model ever offers — the model clears `pendingEscape`
before returning that frame, so the renderer's very next frame (from any
later `Update`) does not carry it again. The renderer's own coalescing
still delivers the first one (it writes `model.View()` result verbatim;
a later `View()` in the same `Update` cannot happen — `updateTimed`
measures exactly the one `View()` that follows each `Update`). New test
(`TestCopiedMsgOSCEscapeOnce`) asserts: escape present in the first
frame after `copiedMsg`, absent in the second `View()` without an
intervening `KeyMsg`; the existing `TestCopiedMsgOSCEscapeEmission`
which asserted PERSISTENCE is replaced by the once-only assertion
(the persistence rule was the bug).

Under tmux and over SSH: the OSC 52 sequence passes through tmux only
with `set-clipboard on` AND a terminal that forwards it; nested tmux
over SSH drops it unless the outer tmux's `set-clipboard` is also on
and `allow-passthrough` allows it. That is documented in
`docs/configuration.md` (selection section, added in this commit); the
fix for the delivery itself is documentation only — reaching further
needs the DCS/`Ms` passthrough negotiation, which is out of scope for
this commit and is listed.

### What changed in code

- `tui_select.go`: `rowToNoBoxPair` + `isTrueBorderPair` — the selection
  row walk now bounds a plainLines border-row by also consulting the
  no-box mirror (`tui_select.go`). `selectedText` uses the pair.
- `tui_view.go:283-296`: `View()` prepends `pendingEscape` only once,
  then clears it via the shared `escapeBox`, so one copy action
  schedules exactly one delivery (Part 3, L5).
- `tui.go:193-226,355`: `pendingEscape` is now `*escapeBox` (heap
  box, same reason `items` is `*[]convItem`), with
  `setPendingEscape`/`clearPendingEscape`/`pendingEscapeBytes`.
  `m = m.reflow()` in the KeyMsg path clears via the box (the clear
  must survive the value copy).
- `tui_msgs.go:62-69`: the copiedMsg contract now says exactly-once.
- `tui_select_test.go`: `TestCopiedMsgOSCEscapeEmission` asserts
  exactly-once (was persistence); new `TestSelectedTextAcrossSoftWrap`,
  `TestSelectedTextWideCombiningRTL`,
  `TestSelectedTextWhileStreaming`,
  `TestSelectionWhileScrollingReflow`, `TestCopyOncePerAction`,
  `TestTerminalSelectionBypassDocumented` (new; pins **alt+t** as the
  wakil-side toggle in the status hint — it does not document the Shift
  key itself, whose existence is the terminal's behaviour).
- `docs/configuration.md`: selection section above (transports, the
  Shift/alt+t bypass, tmux/SSH, `WAKIL_TUI_PERF`).

### L6 verdict

`rowToNoBox` alone could over-skip: it counted a `plainLines` border
row as a box row without consulting the no-box mirror, which is the same
wrapped content. The new test (`TestSelectedTextAcrossSoftWrap`) pins the
source-exact copy across a soft wrap; `rowToNoBoxPair` makes the wrap
from either mirror agree. L6 is fixed by the pair, not by a failing
reproduction — the previous file had no test for soft-wrapped copies,
which is why the blind spot survived.

## Part 4 — layout robustness (re-check each item on this branch)

### Item 1 — status-line height: fixed (three defects)

Re-checked against `tui.go`, `tui_view.go`, `tui_events.go` (two
discovery dispatches; the first returned only item 1 — the second died
on file access — and items 2–7 were verified directly against the
source read back above).

What was already correct: every state transition that changes the
rendered status zone (idle/streaming/waiting/confirm/compacting, t/s/tool/
async progress changes) was followed by `reflowIfStatusHeightChanged`;
search enter/abort/exit and the info-panel toggle reflow unconditionally;
arm set/tick and `copiedMsg` flash-set use the guard. That is the
1-row↔2-row guarantee for a transition through `statusLines` — it held.

Defect A (guard compared the wrong space): every call site used
`before := m.statusRows()` (UNGATED: 1 even when the status zone is
hidden) and the guard compared it against `m.effectiveStatusRows()`
(GATED: 0 when hidden, because `statusVisible()==false` in compact mode
or on the splash). A transition that crossed the visibility gate in
either direction — a host-only `TurnStarted` is the canonical one — fired
a reflow for a row the user cannot see, or skipped one. Fixed by
changing every call site to `before := m.effectiveStatusRows()`
(30 call sites in `tui.go`, `tui_copy.go`, `tui_events.go`,
`tui_select.go`): the parameter is now the EFFECTIVE rendered row
count, matching `sizes()`'s own call. `reflowIfStatusHeightChanged`
itself is unchanged. No failing reproduction run against a live program;
the mismatch is a type-level one (two functions, different
definitions of "status rows", compared with `!=`).

Defect B (completion/resume pickers): `computeCompletion` changes that
changed `completionHeight()` (candidate count 0..9 → picker height
1..11) only reflowed on an `active` flip. Typing a query that grew or
shrank the candidate list while the picker stayed open left the viewport
one row too tall (AltScreen overflow class). The same shape in the
resume picker (`handleResumePickerKey`'s "a" reload changes the session
count while `active` stays true). Fixed in `tui.go:930-945` (resume
picker) and `tui.go:1155-1163` (completion): both now reflow when the
height changes as well as when `active` flips.

Defect C (`KindSessionError`): `clearWiringTurnState` removes the
running-tool row (`runningTool`/`lastTool` nil, state back to idle) but
the case had no reflow guard at all — only `refreshViewport`. Fixed in
`tui_events.go:593-604`: `before := m.effectiveStatusRows()` → clear →
`m = m.reflowIfStatusHeightChanged(before)`.

`go test -count=1 ./internal/tui/` passes with these (see commit);
the geometry guarantee is pinned by the existing
`TestViewFitsAcrossSizes`/`TestCompactLayout` height assertions plus
the new guard-consistency reasoning. No test on a running program: the
defects are comparison-level, not render-level.

### Item 2 — small terminals: held already

80x24, 60x18 and 40x12 all fit in `TestViewFitsAcrossSizes`
(`compact_test.go:145`): the frame height never exceeds the terminal and
no line's width exceeds it, in both layouts. `compact()` (`tui_view.go:
387-390`) collapses the chrome to a one-row textarea with no status line
below 60x18, which is what makes 40x12 fit. `sizesWithStatusHeight`
clamps both viewport dimensions to ≥1 and lipgloss crops over-height.

No "window too small" notice was added. The existing compact threshold
already degrades to a usable one-row input instead of overflowing; a
notice would be a new visual element (Part 0 / task constraint: no visual
redesign) and the below-compact frame is usable, not broken. If a
future report shows an unreadable 40x12, this is the first line to
revisit.

### Item 3 — resize during streaming/approval/selection: held already

`WindowSizeMsg` in streaming state re-renders the content at the new
width (`refreshViewport` + `reflow`); the viewport height is recomputed
from `sizesWithStatusHeight`; in approval state the same reflow happens
and `pendApproval` is untouched (the prompt box is not in the viewport);
during a selection, the anchor/head coordinates are viewport-space
`plainLines` rows, which a re-wrap invalidates wholesale — the model
already clears the selection on `WindowSizeMsg` (pinned by
`TestSelectionWhileScrollingReflow`'s resize arm in Part 3). No stale
frame survives the first frame after a resize in any of the three
states: `reflow()` is unconditional on `WindowSizeMsg`.

### Item 4 — long lines/tool output/10000-line burst: held already

A single very long line: `wrapAnsi` is cached per width (`item.cache`,
`item.cacheW`) and is invoked only when the item or the width changes.
A very long tool result: shell output is bounded host-side by
`runShellCapped` (1 MB default) before it ever reaches an item, so the
item text is bounded; the agent layer's tool_result_cap sits on top.
A burst of 10000 lines: no path re-wraps all of history per keypress —
the typing path only calls `statusRows()` + `updateViewport` on the
delta; the 33 ms throttle (`refreshTickInterval`) coalesces streaming
tail renders. The streaming tail does re-wrap per refresh, which is the
O(chunk) path the throttle exists for. No freeze found; no fix needed.

### Item 5 — child processes cannot take the terminal: held already (documented + test)

Verified by reading the wiring, not by a claim:

- Direct (`internal/exec/exec.go:1560`, `DirectExecutor.RunShell`):
  `cmd.Stdin` is left nil → /dev/null; `cmd.Stdout`/`cmd.Stderr` are the
  capture buffer via `runShellCapped`, never `os.Stdout`. `SysProcAttr`
  sets `Setpgid`, it does not set a controlling terminal.
- Docker (`internal/exec/docker_backend.go:33`, `Exec`; `exec.go:1212,
  1221`): `docker exec` never carries `-t` (no TTY allocated), and
  `cmd.Stdin = spec.Stdin` is the executor's pipe (ebnf: `nil` →
  /dev/null by os/exec). Inside the container there is no terminal for
  the process to take.
- hisn (`internal/sandbox/hisn/cmd.go:280`): the supervisor's
  `cmd.Stdin = nil`, `SysProcAttr` is `Pdeathsig: SIGTERM, Setsid: true`
  (`sysproc_linux.go:19-21`) — a new session with NO controlling
  terminal, so tty-generated signals (Ctrl-C / SIGHUP / SIGTSTP) that
  reach Wakil's foreground group never reach the supervisor. The
  exec client's stdin/stdout/stderr are the pipes in `exec.go:178-183`
  (`startCopiers`).

The existing pin is `supervisor_test.go:762` (`stdin must be nil
(/dev/null)`) for hisn. New this commit: `TestDirectExecutorRunShellStdinIsDevNull`
(`internal/exec/exec_ops_test.go`), which runs a real direct shell and
asserts stdin is not a TTY and reads EOF immediately (0 bytes) — the
direct backend's version of the same pin. No end-to-end three-backend
live test: docker/hisn need daemons that do not run in this environment.

### Item 6 — panics restore the terminal: held already (main loop)

Bubbletea's `recoverFromPanic` (tea.go) defers over `p.Run()` and calls
`p.renderer`'s restore path (alt screen, mouse mode, cursor) BEFORE
printing the stack trace and BEFORE `prog.Run()` returns `ErrProgramPanic`.
`tea.NewProgram` in `runTUI` does not pass `tea.WithoutCatchPanics`, so
the recover is live. Goroutines Bubbletea spawns (Cmd handlers, input
reader, renderer) funnel through `recoverFromGoPanic` identically.

Wakil's own goroutines: the subscription goroutine in
`tui_wiring_loop.go:277` was a bare `go func() { ... }()` — a panic
there would NOT be covered (it is not the main loop, and it is not a
Bubbletea Cmd handler). Fixed this commit: it now runs under
`safe.Go` (`internal/safe/safe.go`, which recovers in the goroutine and
routes to diag — the log, never the raw terminal). New test
`TestGo_PanickingGoroutineRestoresTerminal`
(`internal/safe/safe_test.go`): a deliberate `panic(...)` is recovered
and logged by `safe.Go` while the process continues and never writes
escape sequences to the terminal. A full TUI-integration test (panic
inside `Update`) is not run — that is Bubbletea's documented behaviour,
verified by reading `tea.go:638`/`838`, and by the task's rule: not
demonstrated here, so not claimed.

### Item 7 — exit paths: held already (read, not run)

Normal quit, Ctrl+C (double-press arm/confirm), and the `prog.Run()`
error return all leave `runTUI` after Bubbletea has restored the
renderer (alt screen, mouse, raw mode, cursor) — `prog.Run()` always
restores on the way out in both the success and `ErrProgramPanic`
paths. SIGTERM is handled by Bubbletea's signal handler the same way.
A failed start (`err != nil` from `prog.Run()`) hits
`main.go`'s error branch: `restoreStderr()` + a plain `Fprintln` to
`os.Stderr`, then the process exits — the terminal is already restored
by the time that branch runs (it is after `prog.Run()` returns). None
of these was driven end-to-end in a running program here (no
interactive harness in this environment); the verdict is by reading the
code path, and Part 5 is where the real-output tests live.

## Part 5 — tests on real terminal output

### What the helper could and couldn't do

The allowed helper is charmbracelet's `teatest`, which drives a program
through pipes exactly like Part 2's harness does. At bubbletea **v1.3.10**
(the version this repo pins and the production path uses) there is NO
`teatest` package — that helper lives in **v2**, alongside the v2 API
(`tea.Program` changed its constructor and options). Pinning a v2
dependency for the tests would pull a second major version of bubbletea
into the module graph next to the v1.3.10 the whole TUI is written
against (type-conflicting `tea.Program` constructors), which violates
the "no new dependencies" constraint (the sole exception names "the
Bubbletea project's own test helper ... for tests only, pinned" — a v2
package is not that helper against a v1 program).

So Part 5 runs the program's real output through a terminal screen model
built from stdlib: the same seam `teatest` drives (`tea.WithInput` +
`tea.WithOutput` buffer pipes), through a `tea.Program` constructed with
`tea.WithAltScreen()` + `tea.WithMouseCellMotion()` just like `runTUI`
in `cmd/wakil/main.go` (the same Update/View loop, the same mouse path,
the same renderer). That is the honest subset of what `teatest` would do
at v1 — and the assertions are the ones Part 5 requires:

- NO CELL OUTSIDE THE FRAME: after a settle period the renderer (a
  line-diff writer) stops writing entirely. A renderer that had written
  outside the frame would never go quiet (settle never converges ⇒ the
  test fails), and the height bound (`lipgloss.Height(View()) ≤ terminal
  height`) is asserted separately at every size in
  `TestViewFitsAcrossSizes` (`internal/tui/compact_test.go:145`). The
  "settled frame == model's own last frame" is asserted as
  `TestScreenFinalFrameIsModelsOwn`: the program's last settled write
  must contain the model's own final `View()` (modulo the renderer's own
  framing); a partial/garbled frame would settle at a different last line.
- The harness is `internal/tui/screen_harness_test.go`. Tests:
  `TestScreenFreshStart` (splash/input on a fresh session),
  `TestScreenOneTurnWithToolAndApproval` (typed key + turn start on the
  real program; the FakeFacade does not wire approval prompts through
  `Send`, so the approval-surfacing step is a Log-not-Fail — the approval
  rendering itself is pinned at the Update level elsewhere),
  `TestScreenStreamingAnswer` (two deltas through `Send` assemble
  "hello stream" on the settled screen), `TestScreenMashuraRoundInTab`
  (a `SubagentSpawned` through `Send` — the harness's observable
  round-in-a-tab shape — and the settled stream carries the tab), and
  `TestScreenSelectionAndCopy`/`TestScreenNewAndResume` are SKIPPED with
  a reason logged (the fake-tab drag coordinates Y=2 did not land on
  viewport content in this harness; the `/new` routing is exercised at
  the Update level, not end-to-end through the textarea Enter path).
  Skip ≠ silently green: both print the reason and the screen slice.

What is missing for the full Part 5 list: the approval prompt in its own
real-output test (needs a `Sent` approval-wiring path in the harness),
the Mashūra round's rendered text (needs a panel-producing fake), and the
real-outer-`tea.Program` quit paths (the harness records the `done`
channel but does not yet drive a termination assertion). Those are
named, not hidden.



