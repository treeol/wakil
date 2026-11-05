# Wakil 0.4.2 — Subagent discipline, collapse line, plan/current quarantine

Version target: 0.4.2. No tag, no push, no release — the user does those.

## Behaviour contracts (DO NOT ALTER)

1. Ledgers are harness-derived from actual tool calls, never from subagent text.
2. "Ran out of budget" is never a result by itself; the reserve turn always yields the structured partial.
3. Parent never re-dispatches the identical scope, and never silently takes over after a rejection; both end in a user-visible notice.
4. No changes to sandbox backends, profiles, .wakil/sandbox.json, kvr service, or deploy profile.
5. No tag, no push, no release.

---

## ITEM 1 — Subagent budget by task class + structured partial

### Tool schema changes (`internal/tools/tools.go`)

Add `class` to `dispatch_subagent` and `dispatch_subagents` parameter schemas:

```go
"class": EnumProp("Task class for budget sizing: \"read\" (code reading), \"search\" (pattern/location search), \"edit\" (file mutation), \"verify\" (build/test/lint). Determines the subagent's token budget.", "read", "search", "edit", "verify"),
```

`class` is **required** (add to SchemaObj required list). No default — callers must pass it.

Add `files` (optional array of strings) to both schemas:
```go
"files": map[string]interface{}{
    "type":        "array",
    "items":       StrProp("A file path the subagent should read."),
    "description": "Explicit file list for the dispatch. The harness records which listed files were actually read and rejects off-task dispatches (zero listed files read).",
},
```

### Constants (`internal/agent/subagent.go`)

```go
const (
    subagentBudgetRead   = 40 // % of context window
    subagentBudgetSearch = 25
    subagentBudgetEdit   = 60
    subagentBudgetVerify = 40
    subagentBudgetReserve = 8  // held back for the final report
)
```

Budget = token budget for the subagent's own turns + tool results, measured with the existing accounting (CostTracker / transcript size). **Do not add a second counter.** The budget percentage is applied to `activeThresholds().hardMax` (same as the current TurnToolBudget clamp at subagent.go:1421-1428) to derive a per-class limit. Reserve is subtracted first: `workBudget = hardMax * (classPct - reservePct) / 100`, `reserve_budget = hardMax * reservePct / 100`.

### Structured partial result type (`internal/agent/subagent.go`)

New struct, replacing the current `SubagentSummary` for the dispatch return path:

```go
// SubagentResult is the structured return from a dispatch. Both complete and
// partial results use this shape.
type SubagentResult struct {
    Status        string     `json:"status"`          // "complete" | "partial"
    FilesCovered  []string   `json:"files_covered"`    // from the harness ledger (item 2)
    FilesNotRead  []string   `json:"files_not_read"`    // explicit-list dispatches only
    Findings      []Finding  `json:"findings"`
    RemainingScope string    `json:"remaining_scope"`  // subagent's own words; may be empty
    Budget        BudgetInfo `json:"budget"`
    // Legacy fields carried for back-compat with existing callers:
    Objective     string     `json:"objective,omitempty"`
    Uncertainty   []string   `json:"uncertainty,omitempty"`
    FilesChanged  []string   `json:"files_changed,omitempty"`
    ExternalCalls []ExternalAction `json:"external_calls,omitempty"`
}

type BudgetInfo struct {
    Used  int    `json:"used"`
    Limit int    `json:"limit"`
    Class string `json:"class"`
}
```

`SubagentSummary` is kept as the internal model-produced summary; `dispatchSubagent` wraps it into a `SubagentResult` at the return boundary. The `Render()` method renders `SubagentResult` (not just `SubagentSummary`).

### Exhaustion + forced final turn (`internal/agent/subagent.go`)

When the subagent's work budget is exhausted (the existing `sub.exhausted` / `sub.stopReason` mechanism), the harness stops dispatching tool calls and forces exactly one final turn with the reserve budget. The final turn uses a fixed instruction:

```
const subagentReservePrompt = `Your tool budget is exhausted. Respond with ONLY the JSON object — no prose, no tool calls. Report what you established (findings), what files you did and did not read, and your remaining scope in your own words.`
```

A subagent that fails to produce the structure in the reserve turn still returns a partial with `findings` empty and the raw tail in `remaining_scope`. "Ran out of budget" alone is never the result.

Implementation: in `dispatchSubagent`, after the first `Send` detects exhaustion (`sub.exhausted`), if the parsed summary has no findings and no files_covered, inject the reserve prompt and run one more `Send` with tools stripped (`sub.Tools = nil`). The reserve turn's output is parsed into `SubagentResult` with `status="partial"`. If the reserve turn also fails to produce valid JSON, set `remaining_scope` to the truncated raw output.

### Parent rule on partial (`internal/agent/subagent_parallel.go` + `tool_handlers.go`)

After receiving a partial result, the parent may dispatch again **only** with a strictly smaller scope: a proper subset of `files_not_read` (or a split derived from `remaining_scope`), and a different task text. One split level only.

**Strictly smaller scope check** (harness-enforced, not prompt-enforced):

```go
// isStrictlySmallerScope reports whether the re-dispatch's file set and task
// text are strictly smaller than the original. Compares file sets (proper
// subset) and a hash of the task text (must differ).
func isStrictlySmallerScope(origFiles, newFiles []string, origTaskHash, newTaskHash string) bool {
    if origTaskHash == newTaskHash {
        return false // identical task text → not smaller
    }
    orig := stringSet(origFiles)
    new := stringSet(newFiles)
    for f := range new {
        if !orig[f] {
            return false // new file not in original → not a subset
        }
    }
    return len(new) < len(orig) // proper subset
}
```

A second partial on the split, or a partial with no `files_not_read` and empty `findings`, is surfaced to the user as a one-line notice: `"subagent <id> partial: N/M files, K findings — split or take over?"` and the parent stops auto-dispatching for that task.

### Config keys (`internal/config/config.go`)

Add to `Config`:

```go
// SubagentBudgetClass overrides the per-class budget percentage. 0 = use the
// built-in default. Valid for class "read", "search", "edit", "verify".
SubagentBudgetRead   int `json:"subagents.budget.read,omitempty"`
SubagentBudgetSearch int `json:"subagents.budget.search,omitempty"`
SubagentBudgetEdit   int `json:"subagents.budget.edit,omitempty"`
SubagentBudgetVerify int `json:"subagents.budget.verify,omitempty"`
SubagentBudgetReserve int `json:"subagents.budget.reserve,omitempty"`
```

Defaults (in `DefaultConfig()`): 40, 25, 60, 40, 8. Validation: reject negative; reject > 100.

### Call site updates

All callers of `dispatchSubagent` / `dispatchSubagentGated` / `runSubagentJobs` must pass `class` and `files`:

- `subagentJob` struct: add `Class string` and `Files []string` fields.
- `prepareSubagentBlock`: parse `class` (required) and `files` (optional) from tool-call args. Reject missing class with `"ERROR: class is required (read|search|edit|verify)"`.
- `dispatchSubagent`: add `class string, files []string` parameters; compute budget from class.
- `handleDispatchSubagent` (sequential): parse `class` and `files` from args; pass through.
- `handleDispatchSubagents` (batch): same.
- `runSubagentJobs`: pass `class` and `files` to `dispatchSubagent`.
- `dispatchSubagentGated`: add `class, files` to signature.

### Tests

- Budget sizing per class incl. reserve arithmetic (work = hardMax × (classPct − reservePct) / 100, reserve = hardMax × reservePct / 100).
- Forced final turn produces a partial even with garbage output (empty findings, raw tail in remaining_scope).
- Parent split check rejects identical scope (same file set + same task hash) and accepts a proper subset.
- "Ran out of budget" alone is never the result.

---

## ITEM 2 — Per-file read ledger, off-task rejection

### Ledger type (`internal/agent/subagent.go`)

```go
// FileLedger records which files a subagent actually read during its run,
// derived from actual tool calls (read_file, read_file_full, search_files
// with a path). Never from the subagent's text.
type FileLedger struct {
    Listed   []string  // files the dispatch explicitly asked for
    Read     []string  // listed files that were actually read
    NotRead  []string  // listed files that were NOT read
    Extra    []string  // files read that were NOT in the listed set
}
```

### Ledger derivation

The `App` already has a `filesChangedRecorder` for edit-tier. Add a parallel `fileReadRecorder` that is populated during the child's tool-execution loop:

- For each `read_file`, `read_file_full` call: extract the `path` argument, record it as read.
- For `search_files` with a `path` argument: record the path (or its parent file if it points to a specific file).
- The recorder is carried on the child `App` (like `filesChangedRecorder`).

After the child finishes, the harness computes:
- `Read` = listed files that appear in the recorder's read set.
- `NotRead` = listed files NOT in the recorder's read set.
- `Extra` = recorded reads NOT in the listed set.

`FilesCovered` in `SubagentResult` = `Read`. `FilesNotRead` = `NotRead`.

### Off-task rejection

When the dispatch carries an explicit `files` list AND zero listed files were read → the result is marked `rejected: off-task`:

```go
if len(files) > 0 && len(ledger.Read) == 0 {
    result.Status = "rejected:off-task"
    result.Findings = nil // discarded (kept in trace, not fed to parent)
}
```

The parent re-dispatches **once** with the ledger appended to the task text:
```
"you read none of: <listed files>; actual reads: <extra files>"
```

A second rejection is surfaced to the user as a notice and the parent does **NOT** silently take over.

Without an explicit `files` list: no rejection; the ledger is still returned (Read/Extra may be populated, NotRead is empty).

### TUI display

The live child-completion line in the TUI shows "files 7/9" (read/listed). This goes in the `SubagentFinishedMsg` / `SubagentDoneMsg` event:

```go
// In SubagentFinishedMsg and SubagentDoneMsg:
FilesRead   int  `json:"files_read,omitempty"`
FilesListed int  `json:"files_listed,omitempty"`
```

The TUI renders this as "files 7/9" when FilesListed > 0.

### Tests

- Ledger derivation from a recorded tool-call sequence (read_file, read_file_full, search_files with path).
- files_extra: reads outside the list are recorded.
- Off-task rejection: explicit list, zero listed reads → rejected, findings discarded.
- Once-only re-dispatch: first rejection → re-dispatch with ledger; second rejection → user notice, no takeover.
- Without explicit list: no rejection; ledger still returned.

---

## ITEM 3 — Collapse line

### Format change (`internal/agent/collapse.go`)

**Desired format**:
```
collapsed 73 tool results (read_file ×40, run_shell ×20, grep ×9, …) — 412 KB dropped
```

Rules:
- Aggregate by tool name.
- Sort by count descending.
- Show the top 4 tools, then "…" with the remaining tool count.
- Show total bytes dropped (in KB, rounded).
- **Never print call_… ids** in any user-facing output (TUI, daemon web console, logs at info level).

### `buildCollapseNote` (collapse.go:152-229)

The tool-count part currently uses insertion order. Change to sort by count descending:

```go
// Sort tool counts by count descending, then by name for ties.
type toolCount struct {
    Name  string
    Count int
}
var counts []toolCount
for name, c := range toolCounts {
    counts = append(counts, toolCount{name, c})
}
sort.Slice(counts, func(i, j int) bool {
    if counts[i].Count != counts[j].Count {
        return counts[i].Count > counts[j].Count
    }
    return counts[i].Name < counts[j].Name
})

// Top 4, then "…" with remaining count.
const maxCollapseTools = 4
var parts []string
shown := 0
for _, c := range counts {
    if shown >= maxCollapseTools {
        break
    }
    parts = append(parts, fmt.Sprintf("%s ×%d", c.Name, c.Count))
    shown++
}
toolSummary := strings.Join(parts, ", ")
remaining := len(counts) - shown
if remaining > 0 {
    toolSummary += fmt.Sprintf(", … (+%d more tools)", remaining)
}
totalCalls := sumCounts(toolCounts)
droppedKB := renderWorkingSetBytes(w) / 1024
```

Header line becomes:
```go
fmt.Fprintf(&b, "collapsed %d tool results (%s) — %d KB dropped\n", totalCalls, toolSummary, droppedKB)
```

### `formatCollapseNotice` (collapse.go:1040-1075)

Remove tool-result **ids** (call_…) from the notice. Replace with the tool-name aggregation. The notice becomes:

```go
func formatCollapseNotice(turn, priorBytes, sentBytes int, dropped []proxy.Message, first int, archive string) string {
    priorTokens := priorBytes / charsPerToken
    sentTokens := sentBytes / charsPerToken
    // Aggregate tool results by name from dropped.
    toolAgg := aggregateToolResults(dropped)
    var b strings.Builder
    fmt.Fprintf(&b, "· collapsed request %d: %d tokens → %d sent", turn, priorTokens, sentTokens)
    if len(dropped) > 0 {
        fmt.Fprintf(&b, " (msgs %d–%d", first, first+len(dropped)-1)
        if len(toolAgg) > 0 {
            fmt.Fprintf(&b, ", %s", formatToolAggregation(toolAgg))
        }
        b.WriteString(")")
    }
    if archive != "" {
        fmt.Fprintf(&b, " → %s", archive)
    }
    return b.String()
}
```

`aggregateToolResults` and `formatToolAggregation` are shared helpers that both the collapse note and the compact note use.

### `compact.go` lastCompactNote (compact.go:541-574)

The `lastCompactNote` currently lists tool result ids (maxNoticeToolIDs cap). Replace with tool-name aggregation using the same helpers.

### TUI / daemon web console / logs

Audit all user-facing surfaces for call_… ids:
- TUI: the collapse notice is rendered from `formatCollapseNotice` output (via `a.Out`).
- `lastCompactNote` is rendered in the compact notice line.
- Any other location that prints tool result ids in user-facing output.

Remove or replace all of them with tool-name aggregation. **ids stay in the trace** (trace store, debug-level logs).

### Tests

- Collapse line formatting: ordering (count desc), top-4 cutoff, "…" with remaining count, no ids.
- `formatCollapseNotice`: no call_ ids, tool aggregation present.
- `lastCompactNote`: no call_ ids, tool aggregation present.

---

## ITEM 4 — Plan/current quarantine, memory_preflight_recall

### Plan quarantine (`internal/agent/memory_tools.go`)

In `handleMemoryPut`, when the key is `plan/current` (or starts with `plan/current`) and the session is tainted (`computeTainted() == memory.TaintTrue` or `/taint` is set), redirect the key to `plan/tainted/<chatID>/current`.

```go
if strings.HasPrefix(args.Key, "plan/current") {
    if a.computeTainted() == memory.TaintTrue || a.isTaintForced() {
        args.Key = "plan/tainted/" + a.chatID() + "/current"
    }
}
```

The redirect applies to both mid-tier (TTL) and durable writes.

**Untainted sessions never read plan/tainted**: `handleMemoryGet` and `handleMemorySearch` must filter out `plan/tainted/` keys when `computeTainted() != memory.TaintTrue`.

### Handoff reader (`internal/agent/handoff.go`)

`RunHandoffPipeline` or the handoff reader must ignore `plan/tainted/` entirely. The handoff reader uses `MemoryStore.Get(ctx, "plan/current")` — that key will not exist in a tainted session (it was redirected), so the handoff naturally finds nothing. But to be safe, add an explicit filter: the handoff reader must never read `plan/tainted/`.

### `/plan` status output (`internal/agent/commands.go`)

In `HandlePlanCommand` → status subcommand, when the session is tainted, the status output says:

```
tainted — not handed off
```

This goes after the normal status output, as an additional line.

### `wakil plan gc` CLI

Add a new CLI subcommand: `wakil plan gc [--apply]`.

- **Dry-run** (default): lists entries in `plan/current` with no living chat/session behind them. Uses the same liveness check `/resume` uses (session file exists and is loadable). Prints counts: "plan gc: N entries, K dead, J alive".
- **`--apply`**: deletes the dead entries via `MemoryStore.Forget`.

Implementation: in `cmd/wakil/` (the CLI main), add a `plan gc` subcommand that:
1. Lists all `plan/current*` entries via `MemoryStore.List(ctx, "plan/current", ...)`.
2. For each entry, check if the chat/session it belongs to is alive (session file exists on disk).
3. Print dead entries (dry-run) or delete them (--apply).

### `memory_preflight_recall` (`internal/config/config.go` + `internal/agent/retrieval.go`)

**Confirmed no-op**: `MemoryPreflightRecall *bool` at config.go:688 is never read outside its declaration. `retrieveMemoryContext` at retrieval.go:51 runs unconditionally — there is no `if cfg.MemoryPreflightRecall` gate anywhere.

**Action**: delete the field from `Config`, the config key from `config.example.json`, the docs entry from `docs/configuration.md`, and the call site at `app.go:1164` (the `retrieveMemoryContext` call itself stays — it always runs regardless of the field).

Add one line to `docs/inventory-decisions.md` under the `memory_preflight_recall` entry: `"removed 0.4.2 (no-op, confirmed)"`.

### Tests

- Plan quarantine routing: tainted session → key redirected to `plan/tainted/<chatID>/current`.
- Handoff filtering: untainted session never reads `plan/tainted/`.
- `/plan` status: tainted session shows "tainted — not handed off".
- `wakil plan gc` liveness: dead entries identified, alive entries kept; `--apply` deletes dead only.

---

## Phase 5 — Tests, docs, verification

### Unit tests
- Budget sizing per class incl. reserve arithmetic.
- Forced final turn produces a partial even with garbage output.
- Parent split check rejects identical scope, accepts proper subset.
- Ledger derivation from recorded tool-call sequence incl. files_extra.
- Off-task rejection and the once-only re-dispatch.
- Collapse line formatting (ordering, top-4 cutoff, no ids).
- `formatCollapseNotice` and `lastCompactNote`: no call_ ids.
- Plan quarantine routing and handoff filtering.
- `wakil plan gc` liveness.

### Integration
- One end-to-end dispatch with an explicit file list against the real tool path (existing integration harness; runs under the default sandbox — not a sandbox change).

### Docs
- `README.md` / subagent docs: `class`, budgets, partial result shape, ledger, rejection notices.
- `CHANGELOG` for 0.4.2.
- `docs/PLAN-0.4.2.md`: this file, with an "Open points — not done" section.
- `docs/inventory-decisions.md`: `memory_preflight_recall` removal note.

### Verification
- `go build ./...`
- `go test ./...`
- `go vet ./...`
- `gofmt -l .`
- `golangci-lint run`
- Mashūra round 2 on diff + results.

---

## Open points — not done

Status after the finishing session. Items 1–6 below are the previous session's list, updated; items 7+ are Mashūra round-1 open points (non-blocking) not fixed in 0.4.2.

1. **`stringSet` vs `toStrSet`**: renamed to `toStrSet`. Closed.
2. **Config JSON tags with dots** (`subagents.budget.read`): literal key names, work as-is. Closed.
3. **`MemoryPreflightRecall`**: confirmed no-op, removed. Closed.
4. **Collapse format**: the per-request and compaction notices aggregate by tool name (top 4, `… (+N more tools)`); `buildCollapseNote` is sorted by count but **not capped** (inline working-set note — different context). Open, by design.
5. **Integration test**: CLOSED — `subagent_dispatch_e2e_test.go` drives the real dispatch handler, async worker, `read_file` tool path (DirectExecutor on a temp repo), ledger, guard and notices; only the model is scripted (httptest SSE double). Exhaustion is forced via the real config key `subagent_max_tool_iterations`, because the class byte budget has a 50 KB floor.
6. **Reserve turn + Anthropic API**: superseded — the separate reserve block was removed (round-1 B1); the single reserve turn reuses the JSON-retry call shape (tools stripped, one `Send`).

### Round 1 (Mashūra) — fixed

- B1 reserve turn fired on every exhaustion and stacked on the JSON retry → one extra turn max, replaces the retry, unparseable → `remaining_scope` (2 KB).
- B2 class budget replaced `TurnToolBudget` (raising it) and the override key was dead → explicit `subagent_turn_tool_budget` is a cap; validation: reserve < every class %; `BudgetInfo` populated.
- B3 silent takeover / harness text recommending it → guard + notices + stop; wording removed.
- B4 `plan gc` listed only ACTIVE entries and `Forget`-ed by key → now lists PROPOSED quarantined plans only, `Reject`s by id, keeps unattributable entries, liveness via `SessionFilePath`.

### Round 1 — open points (not fixed)

7. **Ledger = attempted reads.** `ExecuteToolCall` records before `ConfinePath`/execution (`app.go` ~2649), so a denied or failed read counts as read; `search_files path:"."` records `.`. Off-task detection (zero reads) is still sound for the "never opened anything" case; "covered" can over-count.
8. **Exact-string path match** in `deriveLedger`: `./a.go`, `a.go` and an absolute path are different files. A child that reads by a different spelling is scored NotRead (and can be rejected off-task). Normalize against the workspace root.
9. **`dispatch_subagents` shares one `files` list across all tasks** (`tool_handlers.go` batch form) — per-task ledgers are meaningless for heterogeneous tasks. Use `dispatch_subagent` per task for list-bearing dispatches.
10. **Ledger can crowd the 4 KB render** (`SubagentSummary.Render` does not trim `files_covered/not_read/extra`); a very long list can push the JSON over 4000 and trigger the overflow fallback that drops findings.
11. **Error / cancel / panic results** (`cancelledJobResult`, `panicJobResult`) carry no ledger and no `budget`; they classify as `failed`, never `partial`, so they neither retry nor stop (they cannot be "off-task").
12. **Guard linking is by file set + task-text hash.** A dispatch with no `files` can only be linked to a partial by identical text; a re-worded no-file re-dispatch is treated as new work. The model-visible schema says so; a stronger link needs a task id on the tool.
13. **The guard is in-memory per `App`.** Resume/`/new` start with no lineages (intended: a new conversation), but a daemon that rotates the App mid-task also forgets a stop.
14. **`/taint` is display-only**; "tainted" for the quarantine means `computeTainted()==TaintTrue` (touched external content). A user cannot force quarantine.
15. **`plan/currentfoo`** is no longer redirected (exact key only); the handoff reader still relies on key absence rather than an explicit `plan/tainted/` filter.
16. **Collapse `call_` audit** covered `collapse.go`/`compact.go` only; the daemon web console and info-level logs were not audited.
17. **`aggregateToolResults` keys on `m.Name`.** Verified populated in `turn_phases.go` for production tool results; messages restored from old session files without `Name` aggregate to nothing.
18. **Rejected-run spill** is written before the retry; if the retry also fails the first run's findings exist only in the spill.
19. **Reviewer coverage.** Round 1: `claude-fable-5.1` delivered; `gpt-6-astra` returned 429, retried once (both reviewers returned VOID on the retry — see the close-out). Open points above come from the one reviewer that delivered plus own verification against source.
