# Delta Verification — Write-Capable Subagents Scan vs. Current HEAD

READ-ONLY. No code changes. Drift noted explicitly.

---

## Re-verification of prior scan findings

### 1. DiscoveryTools = 5 read-only tools

**Unchanged.** `DiscoveryTools` at `tools.go:219-288` — still returns exactly 5
tools: `read_file` (239), `read_file_full` (250), `search_files` (260),
`find_files` (271), `list_dir` (280). No line drift.

### 2. Edit/exec category separation via GatedTool

**Unchanged.** `GatedTool` at `tools.go:198-206`:
- Edit: `write_file`, `edit_file`, `delete_file`, `move_file` (line 200)
- Exec: `run_shell`, `run_background`, `kill_process` (lines 201)

No new tools added to either category. No line drift.

### 3. Toolset assumed read-only in exactly one place beyond the Tools field

**Substantively unchanged, with one nuance.** The single runtime plumbing site
that hardcodes the read-only tool enumeration is still `subagentSystemPrompt`
(`subagent.go:90-100`), a `const` string assigned at `subagent.go:581`.

A second assumption exists as a **code comment** (not runtime plumbing):
`subagent_parallel.go:86`: "Discovery tools are read-only, so no workspace
write races from workers." This is documentation, not a code path — but it
is an explicit assumption that would need updating if children gained write
tools.

No new plumbing site that assumes read-only was added by the prompt-cache
pass. The `ensurePreamble`/`buildPreamble` changes (see below) are parent-only
— the child has `InjectDate: false` (not set in the struct literal,
`subagent.go:564-580`) and `ensurePreamble` returns early when `!a.InjectDate`
(`app.go:1037-1040`).

### 4. readOnlyConfirmer auto-approves reads, silently declines else; unreachable today

**Unchanged.** `readOnlyConfirmer` at `subagent.go:141-143`:
```go
func readOnlyConfirmer() Confirmer {
    return func(_, _, _ string, readAction bool) bool { return readAction }
}
```
Assigned at `subagent.go:569` (prior scan: 565 — **drift +4** due to
`CachePrompt` field added to `subClient` struct at `subagent.go:521`).

Still unreachable: no tool in `DiscoveryTools` calls `a.Confirm`. The 5
DiscoveryTools (`read_file`, `read_file_full`, `search_files`, `find_files`,
`list_dir`) are all ungated in `ExecuteToolCall` — their cases at
`app.go:1570-1717` never call `a.Confirm`.

### 5. Every file tool calls ConfinePath; no writer lock; only retry is JSON-parse-only

**Unchanged.** ConfinePath calls verified at current lines:

| Tool | ConfinePath call | file:line |
|------|-----------------|-----------|
| `write_file` | `a.Exec.ConfinePath(ctx, args.Path)` | `app.go:1801` |
| `edit_file` (via `handleEditFile`) | `a.Exec.ConfinePath(...)` | `app.go:2331` |
| `delete_file` | `a.Exec.ConfinePath(ctx, args.Path)` | `app.go:1904` |
| `move_file` (src) | `a.Exec.ConfinePath(ctx, args.Src)` | `app.go:1929` |
| `move_file` (dst) | `a.Exec.ConfinePath(ctx, args.Dst)` | `app.go:1933` |
| `read_file` | `a.Exec.ConfinePath(ctx, args.Path)` | `app.go:1605` |
| `read_file_full` | `a.Exec.ConfinePath(ctx, args.Path)` | `app.go:1664` |
| `list_dir` | `a.Exec.ConfinePath(ctx, args.Path)` | `app.go:1712` |
| `find_files` | `a.Exec.ConfinePath(ctx, findPath)` | `app.go:1735` |
| `search_files` | `a.Exec.ConfinePath(ctx, args.Path)` | `app.go:1771` |

`run_shell` does NOT call ConfinePath — passes raw command to `RunShell`
(`app.go:1538`). Still documented as "friction, not a security boundary"
(`readonly.go:8-15`).

No writer lock exists anywhere. No line drift on the "no lock" finding.

Retry: still JSON-parse-only at `subagent.go:610-625` (prior scan: 610-625).
`sub.Tools = nil` at `subagent.go:617`, then `sub.Send(ctx, subagentRetryPrompt)`
at `subagent.go:618`. Does not replay tool calls. No re-dispatch logic exists.

### 6. Child's recentTraces/WorkflowStepTrace discarded with the child App

**Unchanged.** `recordRecentTrace` (`app.go:1327-1337`) writes to `a.recentTraces`
— the child's field. `captureToolTrace` (`app.go:1233-1238`) writes to
`a.WorkflowStepTrace` — also the child's field, and a no-op because the child
has `Workflow: nil` (`subagent.go:567`, now `subagent.go:571` — **drift +4**).

Neither field is propagated to the parent. The parent records only the
`dispatch_subagent` tool call itself (using the summary JSON as the result)
via `a.captureToolTrace`/`a.recordRecentTrace` at `app.go:697-698` (parallel)
and `app.go:1220-1221` via `handleToolCall` (sequential). The child's internal
tool-call traces are discarded when `dispatchSubagent` returns.

---

## New questions

### 5. Consent inventory

**Complete set of session consent fields on `App` at current HEAD:**

| Field | Type | file:line | Purpose |
|-------|------|-----------|---------|
| `AllowReads` | `bool` | `app.go:56` | auto-approve read-only shell for session |
| `AutoApprove` | `bool` | `app.go:60` | skip all confirm prompts for session |
| `AllowDestructive` | `bool` | `app.go:69` | auto-approve destructive shell (requires AutoApprove) |
| `consentedBackends` | `map[string]bool` | `app.go:295` | external backend egress consent |

**No new consent-adjacent field was added by the endpoint/submodel/cache passes.**
Fields added by those passes are routing/cache/feature fields, not consent:

| New field | Type | file:line | Pass | Consent? |
|-----------|------|-----------|------|----------|
| `SubagentEndpointOverride` | `string` | `app.go:258` | endpoint | no — routing override |
| `SubagentModelOverride` | `string` | `app.go:267` | submodel | no — model override |
| `subagentLimitsCachePtr` | `*subagentLimitsCache` | `app.go:287` | endpoint | no — probe cache |
| `cachePrompt` (on `subagentEndpointView`) | `*bool` | `subagent.go:202` | cache | no — prompt-cache hint |
| `CachePrompt` (on child `proxy.Client`) | `*bool` | `subagent.go:521` | cache | no — prompt-cache hint |
| `InjectDate` | `bool` | `app.go:145` | cache | no — feature flag (parent-only) |
| `preambleDay` | `string` | `app.go:155` | cache | no — day-stable cache state |
| `CachedTok` (on `CostRow`/`Record`) | `int` | `app.go:925,945` | cache | no — cost metric |

**Propagation to children:** Only `consentedBackends` is propagated. The child
App struct literal (`subagent.go:564-580`) sets:

```go
consentedBackends: consentSnapshot,   // subagent.go:577
```

The snapshot is built at `subagent.go:559-562` (prior scan: 555-558 — **drift
+4**). `AutoApprove`, `AllowReads`, `AllowDestructive` are **not set** on the
child (default `false`). `Confirm` is hardcoded to `readOnlyConfirmer()`
(`subagent.go:569`), ignoring the parent's confirmer entirely.

**Confirmed: none are propagated to children today except `consentedBackends`.**

### 6. Prompt constness

**`subagentSystemPrompt` is still a single `const` with zero interpolation.**

- Definition: `subagent.go:90` — `const subagentSystemPrompt = ...` (string
  literal, no `fmt.Sprintf`, no parameters, ends at `subagent.go:100`)
- Content: hardcoded tool enumeration ("Use list_dir and find_files to
  navigate, search_files to grep, and read_file... or read_file_full..."),
  the JSON schema, and 5 rules. All static text.

**Assignment site:** Exactly one — `subagent.go:581`:
```go
sub.Conv = []proxy.Message{{Role: "system", Content: StrPtr(subagentSystemPrompt), Pinned: true}}
```

This is inside `dispatchSubagent` (`subagent.go:492-681`). There is no other
reference to `subagentSystemPrompt` anywhere in the codebase. A per-tier const
could be chosen at this single site by replacing the identifier with a
variable — no other code would need touching.

(`subagentRetryPrompt` at `subagent.go:103` is a separate const, referenced
once at `subagent.go:618`. It is tool-agnostic and would not need tiering.)

### 7. recentTraces content

**Per-tool-call data recorded in `ToolTraceEntry`** (`app.go:316-328`):

| Field | Content | Source |
|-------|---------|--------|
| `Abbrev` | short tool name | `toolAbbrev(tc.Function.Name)` (`app.go:1339-1367`) |
| `Command` | run_shell command OR key arg (path, pattern) | `MakeTraceEntry` (`app.go:1240-1267`) |
| `ExitErr` | true if result starts with `ERROR:` | `app.go:1244-1245` |
| `OutputLen` | raw result byte length | `app.go:1243` |
| `FirstLine` | first non-empty output line, ≤80 chars | `app.go:1270-1274` |
| `LastLine` | last non-empty output line, ≤80 chars | `app.go:1296-1303` |
| `ErrorTail` | last ~15 non-empty lines on error | `app.go:1306-1317` |

**`Command` field population by tool** (from `MakeTraceEntry`,
`app.go:1248-1267`):

The `default` case parses `{"path": "...", "pattern": "..."}` from the raw
tool-call arguments JSON. It sets `e.Command = a.Path`, falling back to
`a.Pattern` if path is empty.

| Edit tool | Arg keys in JSON | `Command` captures | Sufficient? |
|-----------|------------------|--------------------|-------------|
| `write_file` | `path`, `content` | `path` ✓ | yes |
| `edit_file` | `path`, `old_string`, `new_string`, `replace_all` | `path` ✓ | yes |
| `delete_file` | `path` | `path` ✓ | yes |
| `move_file` | `src`, `dst` | **empty** ✗ | **no** |

**Gap: `move_file` records an empty `Command`.** The `default` parser at
`app.go:1256-1266` looks for `path` and `pattern` keys. `move_file`'s JSON
uses `src` and `dst` — neither matches, so `e.Command = ""`. The moved
paths are not captured in the trace entry.

**Additional limitation:** `Command` stores the **model-supplied raw path**
(parsed from `tc.Function.Arguments`), not the **canonical/resolved path**
from `ConfinePath`. It could be relative, have trailing slashes, or use `.`.
The canonical path is computed inside `ExecuteToolCall` but is not passed to
`MakeTraceEntry` — only `tc` (the raw tool call) and `result` are passed
(`app.go:1220-1221`).

**Sufficiency for a mechanical files_changed list at the join point:**
Partially. For `write_file`, `edit_file`, `delete_file`: yes — `Command`
holds the path. For `move_file`: no. And the data **does not reach the
join point** regardless — `recentTraces` is on the child `App`
(`sub.recentTraces`), populated during `sub.Send()`, and discarded when
`dispatchSubagent` returns. The parent never sees the child's
`recentTraces`. Deriving a files_changed list from this data would require
both (a) reading `sub.recentTraces` before the child is discarded, and
(b) fixing the `move_file` gap.

The prior scan implied `Command` was less structured than it is — it does
hold the path for 3 of 4 edit tools. But the `move_file` gap and the
discard-with-child issue make it insufficient as-is.

### 8. Parent-write concurrency

**Yes, the parent is strictly blocked during both sequential and parallel
child execution. No parent tool calls can interleave with running children.**

**Sequential path** (`dispatch_subagent` case in `ExecuteToolCall`):
- The parent calls `a.dispatchSubagent(...)` at `app.go:2101`
- This is a synchronous call — `dispatchSubagent` returns before the parent
  continues
- Inside, `sub.Send(ctx, task)` at `subagent.go:583` is also synchronous
- The parent's tool loop (`Send`, `app.go:560-712`) is blocked at the
  `handleToolCall` call (`app.go:704`) until the child completes
- **Parent waits at: `app.go:2101`** (the `dispatchSubagent` return blocks)

**Parallel path** (`runParallelSubagentBlock`):
- Phase B: `a.runSubagentJobs(ctx, jobs, backend)` at `subagent_parallel.go:197`
- Inside `runSubagentJobs`: worker goroutines spawned, then `wg.Wait()` at
  `subagent_parallel.go:143` blocks the main goroutine until all workers join
- Phase C (finalization) runs only after `wg.Wait()` returns
- **Parent waits at: `subagent_parallel.go:143`** (`wg.Wait()`) which is
  inside `runSubagentJobs`, called at `subagent_parallel.go:197`

The parent's `Send` tool loop (`app.go:687-707`) processes tool calls
sequentially — it calls either `a.handleToolCall` (app.go:704) or
`a.runParallelSubagentBlock` (app.go:695) and waits for the result before
proceeding to the next tool call or the next Stream iteration. No parent
tool execution can overlap with child execution.

**Implication:** A writer lock would only need to serialize **children among
themselves** (in the parallel path, where multiple children run concurrently
under the semaphore at `subagent_parallel.go:107`). The parent cannot make
tool calls while children are running. In the sequential path, only one
child runs at a time and the parent is blocked — no lock needed for
parent-child isolation.

### 9. Construction path

**Confirmed: `dispatchSubagent` (`subagent.go:492-681`) is the single child
construction site. No second construction site was added by recent passes.**

The flow through `dispatchSubagent`:

1. `resolveSubagentEndpointName(a)` → `subagent.go:498`
2. `a.resolveSubagentEndpointView(epName)` → `subagent.go:499` (returns
   `subagentEndpointView` including the new `cachePrompt` field,
   `subagent.go:202`)
3. Backend resolution → `subagent.go:505-511`
4. `subClient` construction → `subagent.go:513-528` (now includes
   `CachePrompt: view.cachePrompt` at `subagent.go:521` — the only new
   field added by the cache pass)
5. `cfg` construction → `subagent.go:530-550`
6. Consent snapshot → `subagent.go:559-562`
7. Child `App` struct literal → `subagent.go:564-580`
8. `sub.Conv` initialization with `subagentSystemPrompt` → `subagent.go:581`
9. `sub.Send(ctx, task)` → `subagent.go:583`

Both call paths use this same function:
- **Sequential:** `app.go:2101` calls `a.dispatchSubagent(...)` directly
- **Parallel:** `subagent_parallel.go:132` calls `a.dispatchSubagent(...)`
  from worker goroutines
- **Wrapper:** `dispatchSubagentGated` (`subagent.go:450-455`) adds the
  consent gate then calls `dispatchSubagent` — it does not construct a
  separate child

**Capability is orthogonal to endpoint/model/cachePrompt plumbing.** The
three things that would vary by capability tier are all in the same struct
literal, independent of the endpoint fields:

| What | Assignment site | Depends on endpoint? |
|------|----------------|---------------------|
| `Tools` | `subagent.go:568` (`tools.DiscoveryTools(a.Exec.Cwd())`) | no |
| `Confirm` | `subagent.go:569` (`readOnlyConfirmer()`) | no |
| System prompt (Conv[0]) | `subagent.go:581` (`subagentSystemPrompt`) | no |

The endpoint view (`subagentEndpointView`) is resolved at `subagent.go:499`
before the struct literal — it feeds `subClient` (lines 513-528) and
`CtxLimit` (line 578), but does not touch `Tools`, `Confirm`, or the prompt.
A capability parameter would be a parallel input to the struct literal,
unaffected by the endpoint/model/cachePrompt fields.

---

## Line-drift summary (prior scan → current HEAD)

| Item | Prior line | Current line | Drift | Cause |
|------|-----------|--------------|-------|-------|
| `Tools: DiscoveryTools(...)` | 564 | 568 | +4 | `CachePrompt` field added to `subClient` (subagent.go:521) |
| `Confirm: readOnlyConfirmer()` | 565 | 569 | +4 | same |
| `consentedBackends` snapshot | 555-558 | 559-562 | +4 | same |
| `consentedBackends:` field | 573 | 577 | +4 | same |
| `sub.Conv` assignment | 577 | 581 | +4 | same |
| `Session: nil` | 567 | 571 | +4 | same |
| `foldSubagentCost` | 431-440 | 434-443 | +3 | `CachedTok` param added to `tracker.Record` (subagent.go:437) |
| `RecordInferenceCost` | — | 891-946 | changed | `CachedTok` param in `Costs.Record`/`ExternalInferenceCost` (app.go:925,945) |
| `contextPreamble()` | 954 | **replaced** | — | renamed to `buildPreamble` (app.go:983) + `ensurePreamble` (app.go:1037); parent-only, no child impact |
| `subagentSystemPrompt` | 90-100 | 90-100 | 0 | unchanged |
| `readOnlyConfirmer` | 141-143 | 141-143 | 0 | unchanged |
| `DiscoveryTools` | 219-288 | 219-288 | 0 | unchanged |
| `GatedTool` | 198-206 | 198-206 | 0 | unchanged |
| JSON-parse retry | 610-625 | 610-625 | 0 | unchanged |

**All drift is in `subagent.go` lines 513-581 (+4) and 434-443 (+3), caused
by the `cachePrompt`/`CachedTok` additions from the prompt-cache pass. No
substantive change to the construction flow, tool selection, confirmer, or
prompt.**
