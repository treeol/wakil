# Proposal: Subagent Token Budget — Fix the Real Binding Constraints

**Status:** Refined after two rounds of Mashūra 3-model panel review.
All claims verified against source code.

## Problem

Subagents frequently hit budget exhaustion ("subagent ran out of budget"),
forcing the parent to re-dispatch narrower or take over. The user reports
this happens "every 2nd turn" in practice.

## Root Cause (verified against source)

Three budget layers interact. The real picture is more nuanced than "iteration
cap is too low":

### Layer 1: Byte-level context limits (generous, NOT the bottleneck)

Constants at `subagent.go:18-27`:
```
subagentHardMaxBytes   = 70_000   // fallback floor
subagentCompactAt      = 55_000
subagentKeepBytes      = 45_000
subagentSummaryBytes   = 8_000
```

**Shadowed** by the fraction-based path in `activeThresholds()`
(`compact.go:121-152`). On the inherit path (the default), the child gets
the parent's probed CtxLimit (`subagent.go:821`:
`CtxLimit: a.resolveChildCtxLimit(...)`, inherit returns `a.CtxLimit` at
`subagent.go:579`). Since `DefaultConfig()` sets `CompactAtFrac=0.75` and
`dispatchSubagent` builds cfg from `DefaultConfig()` without clearing
fraction fields, the fraction path fires:

```
effectiveChars = 122,880 × 0.80 × 4 = 393,216 chars   (128k backend)
hardMax = 0.95 × 393,216 = 373,555 chars               (NOT 70,000)
```

**Stale comment:** `compact.go:118-120` lists "subagents" as always having
unknown limits. This predates CtxLimit inheritance — stale, needs fixing.

**Exception:** Override-path probe failure returns zero `ContextLimit`
(`subagent.go:611`), falling back to the 70k floor. This is correct for
that case but is NOT the default path.

### Layer 2: Per-turn cumulative tool budget (PRIMARY BOTTLENECK)

```
subagentTurnToolBudget = 50,000   // subagent.go:24
subagentToolResultCap  = 12,000   // subagent.go:23
```

**This is the real binding constraint for read-heavy discovery work.**
`CapOrStub` (`app.go:1195-1217`) stubs tool results to ~50 chars (spilling
full content to disk) once cumulative tool output exceeds `TurnToolBudget`:

```go
// app.go:1205-1206
if a.Cfg.TurnToolBudget > 0 && turnToolBytesSoFar >= a.Cfg.TurnToolBudget {
    return wtools.StubToolResult(result, toolName, a.chatID())
}
```

`StubToolResult` (`toolcap.go:238-243`) returns `"[budget — N chars at: PATH]"`
— the model sees a pointer, not file content. A subagent's entire run is one
`Send` call (one turn), so the budget accumulates across ALL tool calls:

```
50,000 / 12,000 ≈ 4-6 full-fidelity reads before stubbing
```

Typical discovery sequence:
```
list_dir (2k) + find_files (3k) + search_files (8k) + 3× read_file (36k) = 49k
→ Call 7: STUBBED. Calls 8-16: all stubbed. Model can't read any more files.
```

**read_file_full is worse:** `SpillFullResult` (`toolcap.go:324`) keeps full
content in context (bypassing the 12k cap), but counts fully against
TurnToolBudget. A single 80k `read_file_full` exhausts the 50k budget after
just 3-4 nav calls.

**Key insight:** Raising the iteration cap alone yields NO improvement —
extra iterations would all see stubbed results. The TurnToolBudget must be
raised in tandem.

### Layer 3: Tool iteration cap (secondary bottleneck)

```
subagentMaxToolIter = 16   // subagent.go:26
```

Each iteration is one assistant/tool round-trip (the model can emit multiple
tool calls per iteration). The iteration cap binds for workloads with many
cheap calls (lots of `list_dir`/`find_files`) that don't hit the per-result
cap. It's secondary for read-heavy discovery but still needs raising.

### Binding order (128k backend, inherit path)

```
TurnToolBudget (50k) >> iteration cap (16) >> hardMax (373k)
```

The subagent can hold ~373k chars of context but is only allowed to *ingest*
50k chars of tool output per turn. It starves well before it fills.

## Proposed Changes

### Phase 1: Fix the binding constraints + add stop-reason telemetry

Ship together — the telemetry is needed to validate the fix.

#### 1a. Add config keys

In `internal/config/config.go`, add to the Config struct:

```go
// SubagentMaxToolIter caps tool round-trips per subagent dispatch.
// 0 = use the built-in default (30). Higher values allow deeper exploration.
// Unlike the parent's MaxToolIterations (0 = unlimited), subagents always
// get a finite cap — they're autonomous workers with no human gate.
SubagentMaxToolIter int `json:"subagent_max_tool_iterations,omitempty"`

// SubagentTurnToolBudget overrides the per-turn cumulative tool output
// budget for subagents. 0 = use the built-in default. Raising this lets
// subagents read more files before results are stubbed to spill pointers.
// Automatically clamped to a fraction of the active hardMax at dispatch
// time, so it's safe to set high even on small-context backends.
SubagentTurnToolBudget int `json:"subagent_turn_tool_budget,omitempty"`

// SubagentToolResultCap overrides the per-result char cap for subagents.
// 0 = use the built-in default (12,000).
SubagentToolResultCap int `json:"subagent_tool_result_cap,omitempty"`
```

#### 1b. Raise the built-in defaults

In `internal/agent/subagent.go`:

```go
const (
    subagentHardMaxBytes   = 70_000   // fallback floor; fraction path overrides when NCtx known
    subagentCompactAt      = 55_000
    subagentKeepBytes      = 45_000
    subagentSummaryBytes   = 8_000
    subagentToolResultCap  = 12_000   // per-file view — unchanged
    subagentTurnToolBudget = 120_000  // RAISED from 50k: allows ~10 full reads before stubbing
    subagentToolResultTTL  = -1
    subagentMaxToolIter    = 30       // RAISED from 16: more room for nav + search + reads
)
```

Update the stale comment on `subagentTurnToolBudget` (currently says
"generous: HardMax is the real ceiling" — the opposite of reality).

#### 1c. Wire config with runtime clamping

In `dispatchSubagent` (`subagent.go:731+`), after constructing the child App
(when CtxLimit is known), compute the effective turn budget:

```go
// Single source of truth for defaults
const defaultSubagentMaxToolIter = 30

cfg := config.DefaultConfig()
cfg.HardMaxBytes = subagentHardMaxBytes
cfg.CompactAt = subagentCompactAt
cfg.KeepBytes = subagentKeepBytes
cfg.SummaryBytes = subagentSummaryBytes

// Per-result cap: config > hardcoded default
cfg.ToolResultCap = subagentToolResultCap
if a.Cfg.SubagentToolResultCap > 0 {
    cfg.ToolResultCap = a.Cfg.SubagentToolResultCap
}

// Turn tool budget: config > hardcoded default, then CLAMP to active hardMax
cfg.TurnToolBudget = subagentTurnToolBudget
if a.Cfg.SubagentTurnToolBudget > 0 {
    cfg.TurnToolBudget = a.Cfg.SubagentTurnToolBudget
}

// Iteration cap: session override > config > built-in default
cfg.MaxToolIterations = defaultSubagentMaxToolIter
if a.Cfg.SubagentMaxToolIter > 0 {
    cfg.MaxToolIterations = a.Cfg.SubagentMaxToolIter
}
if a.subMaxToolIter > 0 {
    cfg.MaxToolIterations = a.subMaxToolIter
}
```

Then **after** constructing `sub` (when `sub.CtxLimit` is set), clamp:

```go
// Clamp TurnToolBudget to a safe fraction of the active hardMax.
// This prevents the budget from exceeding the context ceiling on
// small-context backends (e.g., 32k tokens → ~78k hardMax).
_, _, activeHardMax := sub.activeThresholds()
if activeHardMax > 0 && cfg.TurnToolBudget > activeHardMax*35/100 {
    cfg.TurnToolBudget = activeHardMax * 35 / 100
}
sub.Cfg.TurnToolBudget = cfg.TurnToolBudget
```

The 35% fraction leaves room for the system prompt, conversation history,
and model output. For a 128k backend (hardMax ~373k): clamp is ~130k, so
the 120k default passes through. For a 32k backend (hardMax ~78k): clamp
is ~27k, preventing the 120k default from causing hard-max shedding.

#### 1d. Stop-reason telemetry

Add `StopReason` to `SubagentSummary` and a sticky flag on `App`:

```go
// In SubagentSummary:
StopReason string `json:"stop_reason,omitempty"`
// "iteration_limit" | "turn_budget_exhausted" | "hard_max_shed" |
// "confinement_breaker" | "" (complete)

// In App (alongside exhausted):
stopReason string  // set at the exact site where exhaustion occurs
turnBudgetStubbed bool  // set inside CapOrStub when budget stub fires
```

Instrumentation sites:
1. **`app.go:618`** (forceFinish from iteration limit):
   `a.stopReason = "iteration_limit"` (when not confinementTrip)
2. **`compact.go:502`** (enforceHardMax shed):
   `a.stopReason = "hard_max_shed"`
3. **`app.go:1206`** (CapOrStub budget stub):
   `a.turnBudgetStubbed = true` — set inside CapOrStub when the budget
   branch fires. This is the accurate detection point (not end-of-Send
   comparison, which is unreliable with post-cap accumulation).
4. **Confinement** (already has `confinementTripped`):
   `a.stopReason = "confinement_breaker"`

In `dispatchSubagent` (`subagent.go:932+`), set the summary stop reason
with precedence: `confinement_breaker > iteration_limit > hard_max_shed`.
If `turnBudgetStubbed` is true but no other reason fired, set
`"turn_budget_exhausted"`. If `turnBudgetStubbed` AND another reason fired,
include both (e.g., `"turn_budget_exhausted+iteration_limit"`).

The `stopReason` field must be captured before the retry Send (which resets
it), same as `exhausted` — OR the values across both Sends.

`StopReason` is **mechanically set** by `dispatchSubagent`, not trusted
from the model's JSON output. The system prompts don't need updating.

#### 1e. Fix stale comments

- `compact.go:118-120`: Remove "subagents" from the unknown-limit list.
  Add: "Inherited subagents get the parent's probed CtxLimit; only
  override-path probe failures and unprobed startup fall through to
  absolute values."
- `subagent.go:24`: Replace "generous: HardMax is the real ceiling" with
  "caps cumulative tool output per turn; clamped to 35% of active hardMax
  at dispatch time."

### Phase 2: Session-scoped tuning (deferred)

**Defer `/subbudget` slash command** until Phase 1 + telemetry validate
the fix and show whether users need runtime tuning. If needed, add a
command mirroring `/maxpar` with RepoState persistence. But the config
keys + raised defaults should solve the problem without runtime tuning.

## What is explicitly NOT changed

- **The fraction-based threshold system** — works correctly.
- **The subagent summary cap (4k chars)** — return-value size, not working budget.
- **MaxParallelSubagents** — already configurable via `/maxpar`.
- **The compaction/pinning logic** — works correctly.
- **Proportional-to-context iteration scaling** — rejected. Iterations map
  loosely to tokens; a fixed cap of 30 is simpler and sufficient.

## Validation

- Reject negative values for all three config keys in `validateContextLimits`.
- Warn (don't reject) if `SubagentTurnToolBudget` is set extremely high
  (> 500k) — the runtime clamp handles safety, but the user likely
  misunderstands the field.
- Existing tests using `subMaxToolIter = 1` still work (session override
  takes highest precedence).
- Add tests:
  - `SubagentMaxToolIter` from config flows to child `cfg.MaxToolIterations`.
  - `SubagentTurnToolBudget` from config flows to child `cfg.TurnToolBudget`.
  - Runtime clamp: `TurnToolBudget` is clamped to 35% of active hardMax
    for small-context backends.
  - `StopReason` is correctly set for each exhaustion mode.
  - `turnBudgetStubbed` is set when CapOrStub's budget branch fires.
  - Fraction path still fires for inherited subagents (byte constants
    remain floors).

## Acceptance criteria

1. **Deterministic:** A subagent with 10 × 12k tool results does NOT stub
   before the 120k budget is reached; with 120k budget, all 10 complete.
2. **Stop reason:** When iteration limit is hit, `stop_reason="iteration_limit"`.
   When budget stubs, `turnBudgetStubbed=true`. When hard-max sheds,
   `stop_reason="hard_max_shed"`.
3. **Clamp safety:** On a 32k-token backend (hardMax ~78k), TurnToolBudget
   is clamped to ~27k, not 120k.
4. **No regression:** Existing tests pass; `subMaxToolIter=1` still forces
   exhaustion; parent agent budgets unaffected.
5. **Config keys:** Setting `subagent_turn_tool_budget` in config.json
   overrides the default; `0` uses the default.

## Migration

- **Existing configs:** No change needed. Raised defaults (120k turn budget,
  30 iterations) apply automatically. The runtime clamp ensures safety on
  small-context backends.
- **Zero semantics:** `0` = "use default" for all three subagent fields.
  This differs from the parent's `MaxToolIterations` (0 = unlimited), but
  subagents always need a finite cap. Documented in the field comments.
- **The hardcoded constants** become defaults that config can override.
