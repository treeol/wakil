# Test Coverage Gap Analysis

Generated from `go test -coverprofile` + `go tool cover -func` across all packages.

## Current Coverage Summary (sorted ascending)

| Package | Coverage | Priority |
|---|---|---|
| `internal/remote` | 24.9% | 🔴 Critical |
| `internal/auth` | 43.5% | 🔴 High |
| `internal/browser` | 46.3% | 🔴 High |
| `internal/exec` | 55.3% | 🟡 Medium |
| `internal/wiring` | 57.4% | 🟡 Medium |
| `internal/diag` | 58.3% | 🟡 Medium |
| `internal/tools` | 66.3% | 🟡 Medium |
| `internal/workflow` | 67.3% | 🟡 Medium |
| `internal/agent` | 70.7% | 🟢 OK |
| `internal/config` | 81.9% | 🟢 OK |
| `internal/memory` | 80.3% | 🟢 OK |
| `internal/tui` | 78.6% | 🟢 OK |
| `internal/proxy` | 82.0% | 🟢 OK |
| `internal/staging` | 84.8% | 🟢 OK |
| `internal/orregistry` | 81.7% | 🟢 OK |
| `internal/sessionhistory` | 79.5% | 🟢 OK |
| `internal/crypto` | 77.5% | 🟢 OK |
| `internal/trace` | 92.5% | ✅ Good |
| `internal/counsel` | 93.2% | ✅ Good |
| `internal/core` | 94.1% | ✅ Good |
| `internal/verify` | 96.1% | ✅ Good |
| `internal/protoconv` | 97.0% | ✅ Good |
| `internal/policy` | 98.4% | ✅ Good |
| `internal/safe` | 100.0% | ✅ Perfect |
| `internal/scrub` | 100.0% | ✅ Perfect |

---

## Priority 1: Critical Gaps (sub-50% coverage)

### internal/remote (24.9%) — 9 gaps identified

**Files with zero test coverage:**
- `bootstrap.go` — BootstrapRemote, StartEventPump, SubscribeLive
- `manager.go` — NewConversation, ResumeConversation, HandoffConversation, Close, CloseManager

**Partially tested files with major gaps:**
- `pump.go` — Run() loop untested: reconnect on error/EOF, 2s/500ms backoff, Stop/Done, ephemeral seq-0 passthrough, lastSeq+1 cursor. Only dedup is tested.
- `facade.go` — SessionService/EventService delegation untested: CreateSession, SubmitInput, RespondToApproval (outcome mapping + invalid-outcome error), Interrupt, CloseSession, ListEvents, SessionSnapshot

**Suggested tests:**
1. **pump.go Run() lifecycle** — Test reconnect-on-error with a mock connection that returns EOF, verify backoff timing and that Stop() cleanly drains via Done channel
2. **pump.go seq-0 passthrough** — Verify ephemeral events (seq=0) bypass the dedup/lastSeq+1 cursor check
3. **facade.go RespondToApproval** — Test outcome mapping (approve/deny/invalid) with a mock SessionService, verify invalid outcome returns error
4. **facade.go refreshState() stale-overwrite** — Test ticket logic: closed/nil-client/empty-sid guards, ticket >= refreshSeq ordering
5. **manager.go** — Test NewConversation/ResumeConversation with a mock dialer, verify connection code mapping

### internal/auth (43.5%) — 3 gaps identified

**File with zero test coverage:**
- `context.go` — WithHTTPHeaders, HTTPHeadersFromContext (0% each)

**Untested exported functions:**
- `principal.go:85` NewLocalResolver (0%)
- `principal.go:110` NewMultiResolver (0%)
- `principal.go:115` MultiResolver.Resolve (0%)

**Suggested tests:**
1. **context.go HTTP headers round-trip** — `WithHTTPHeaders` + `HTTPHeadersFromContext` set/get cycle, verify absent returns (nil, false)
2. **MultiResolver.Resolve chain ordering** — Test that first success wins, ErrCredentialAbsent falls through to next, ErrInvalidCredential hard-fails (doesn't fall through)
3. **MultiResolver.Resolve all-absent** — All resolvers return ErrCredentialAbsent → returns ErrUnauthenticated
4. **NewLocalResolver** — Verify it captures os.Geteuid() and accepts matching UID, rejects non-matching

### internal/browser (46.3%) — 6 gaps identified

**Critical issue:**
- `manager_integration_test.go` does not compile — calls `NewManager()` with zero args but signature is `NewManager(exe SandboxExecutor, browserPath string)`. The entire browser-tagged suite is dead code.

**Suggested tests:**
1. **Fix manager_integration_test.go** — Update `NewManager()` call to match current signature or remove the dead test file
2. **newDockerManager error paths** — Test CDPPort()==0 and chromium-binary-not-found preflight (unit-testable with fake SandboxExecutor, no browser needed)
3. **cdpReady HTTP probe** — Test with httptest.NewServer returning 200 vs 404 vs a refusing listener
4. **EvalJS result-type formatting** — Test nil→"null", string passthrough, object→JSON marshal, marshal-failure fallback
5. **GetHTML 50KB truncation** — Test that truncation cap kicks in and appends the hint string
6. **NewManager routing** — Test ContainerName()!="" → docker vs "" → local with a fake executor

---

## Priority 2: Medium Gaps (50-70% coverage)

### internal/lsp (53.4%) — 7 gaps identified

**Untested exported functions (0%):**
- `render.go`: RenderHover, RenderSymbols, stripMarkdown, stripInlineMarkdown, stripPair
- `tools.go`: handleDefinition, handleReferences, handleHover
- `manager.go`: Call, CapabilitySupported, Shutdown, initialize, handleNotification, resetIdleTimer, drainStderr
- `filesync.go`: EnsureManagerForFile, NotifyChange, MarkOpenFilesDirty, BatchNotifyWatchedFiles, resolveToPosition, batchNotifyWatchedFiles, notifyChange

**Suggested tests:**
1. **render.go RenderHover** — Test rendering of MarkupContent (markdown + plain), Hover with range, nil hover
2. **render.go RenderSymbols** — Test DocumentSymbol hierarchy rendering, nested children, various symbol kinds
3. **render.go stripMarkdown / stripInlineMarkdown** — Test stripping of `**bold**`, ``code``, `[link](url)`, headers, lists
4. **tools.go handleDefinition/handleReferences/handleHover** — Test with a mock manager returning canned LSP responses, verify formatted output
5. **filesync.go resolveToPosition** — Test UTF-16 → byte offset conversion, multi-byte characters, out-of-range line/column

### internal/exec (55.3%) — focus on unit-testable logic

**Note:** Docker integration tests exist but require a running daemon. Focus on pure logic.

**Suggested tests:**
1. **signing.go** — Verify signature generation/verification for container configs
2. **exec_ops.go** — Test argument sanitization, path resolution edge cases
3. **exit_marker.go** — Test exit code parsing/marking logic
4. **seccomp_iouring.go** — Test seccomp filter generation

### internal/wiring (57.4%) — 6 gaps identified

**Untested exported functions (0%):**
- `coordinator.go`: NewTransitionCoordinator, WithTransition, WithTurnStart, ClearTurnActive, WithIdleMaintenance
- `conversation_manager.go`: transferDestructive, SetAutoUserOverridden, SetModelUserOverridden, ResumeConversation (12.5%)
- `facade.go`: CreateSession, Interrupt, CloseSession, Consent, CompletionSource, SetAllowDestructive, RevokeAuto, AppendSystemMessage, SaveSession, SaveRepoState, ListSessions, LoadSession, Models, Backends, Sessions
- `headless.go`: EmitEvent, HeadlessDecision, RunHeadless, RunHeadlessApp, runPlanTask

**Suggested tests:**
1. **coordinator.go** — Test transition coordinator state machine: WithTransition/WithTurnStart/ClearTurnActive lifecycle
2. **conversation_manager.go ResumeConversation** — Test resume path with mock remote, verify repo state restoration
3. **conversation_manager.go transferDestructive** — Test that destructive-allow flag transfers correctly between old and new conversations
4. **facade.go DispatchCommand sub-commands** — Test /backend, /auto, /mode commands with mock session client
5. **facade.go Consent/CompletionSource** — Test consent state queries with mock app state

### internal/diag (58.3%) — 2 gaps identified

**Untested functions (0%):**
- `diag.go:70` OpenSessionLog
- `diag.go:99` dataDir (40%)

**Suggested tests:**
1. **OpenSessionLog** — Test that it creates the log file and directory structure, handles existing files, returns a valid writer
2. **dataDir** — Test path resolution with various config scenarios (XDG, custom, default)

### internal/tools (66.3%) — 5 gaps identified

**Untested functions (0%):**
- `mentions.go`: UserQueryText, ResolveMentions, resolveMention, listDirCapped, ChipsLine, HumanSize, langForExt
- `toolcap.go`: MakeEvictionStub, StubToolResult, SpillFullResult
- `google.go`: GoogleFetchURL (9.1%)

**Suggested tests:**
1. **mentions.go ResolveMentions** — Test @path parsing, file content injection, directory listing, binary detection, truncation, dedup of duplicate tokens, no-match passthrough
2. **mentions.go UserQueryText** — Test extraction of original text from outgoing message with mention blocks
3. **mentions.go langForExt / HumanSize** — Test all file extension mappings and byte size formatting (KB/MB/B boundaries)
4. **toolcap.go SpillFullResult / StubToolResult** — Test spill-to-disk path, eviction stub creation, cap enforcement
5. **google.go GoogleFetchURL** — Test URL fetching with mock HTTP server, content extraction, error handling

### internal/workflow (67.3%) — 5 gaps identified

**Untested functions (0%):**
- `workflow.go`: StatusString, Directive, truncate, CountStepLogEntries, LastAssistantText, IsPlanFilePath, BuildFinalReviewBriefing, GapGist, WFEverystepCritical

**Suggested tests:**
1. **Directive()** — Test all phase directives (GATHER, PLAN, IMPLEMENT, REVIEW, DONE), verify plan-format-invalid path, verify oracle review injection on step 1
2. **StatusString()** — Test format with various phase/step combinations
3. **BuildFinalReviewBriefing** — Test with full step log (k=9999), verify all entries included, maxBytes=16KB default
4. **GapGist** — Test extracting first non-VERDICT line, truncation at 120 chars, empty/whitespace input
5. **WFEverystepCritical** — Test keyword matching ("incorrect", "wrong", "error", etc.), case-insensitive, no-match returns false
6. **IsPlanFilePath** — Test relative/absolute path matching, suffix logic for /work/.wakil/plan.md vs .wakil/plan.md
7. **LastAssistantText** — Test finding last assistant message, skip non-assistant, empty conversation
8. **CountStepLogEntries** — Test counting "Step " prefixed entries in ## Step log section

---

## Summary: Highest-Impact Test Additions

If only a few tests could be added, these would give the most coverage bang for the buck:

| # | Package | Test | Est. Coverage Gain |
|---|---|---|---|
| 1 | `internal/remote` | pump.go Run() lifecycle + facade.go delegation | +15-20% |
| 2 | `internal/auth` | MultiResolver.Resolve + context.go round-trip | +25-30% |
| 3 | `internal/tools` | mentions.go ResolveMentions full suite | +10-15% |
| 4 | `internal/workflow` | Directive() all phases + helper functions | +15-20% |
| 5 | `internal/lsp` | render.go RenderHover/RenderSymbols + stripMarkdown | +10-15% |
| 6 | `internal/browser` | Fix dead integration test + unit-testable error paths | +15-20% |
| 7 | `internal/wiring` | coordinator.go + conversation_manager.go resume path | +10-15% |
