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


## host_verify.sh signature fix (d9e57d1 + 58df571)

- Moved the `signed-commits` check (check 2) to run BEFORE `HOME` is redirected, so git reads the user's global config where `gpg.ssh.allowedSignersFile` is set.
- Captured `git log --format='%G?'` stderr to `$outdir/sig.err`; if it contains `allowedSignersFile`/`gpg.ssh`/`no signing key`/`secret key needed`, SKIP with a reason instead of FAIL.
- Normal run: `PASS signed-commits` (G signatures present).
- Empty-HOME run: `SKIP signed-commits (signing setup unavailable)` — not FAIL.
- `tracked-tree-clean` check also runs pre-HOME (repo-local, no global config needed).
- Final `--fast master`: 15 PASS, 0 FAIL, 1 SKIP (race skipped).
