# Write-Capable Subagents — Feasibility Discovery Report

**Scope:** READ-ONLY investigation. No code changes. No design recommendations.
Maps every touch point for an opt-in capability parameter on `dispatch_subagent`
(default = today's read-only discovery; opt-in = workspace writes).

All file:line references are against the current worktree.

---

## 1. Tool Sets — DiscoveryTools vs. Parent's Full Toolset

### Parent's full toolset

Assembled by `BuildTools` (`mcp_manager.go:301-319`), called at `run.go:476`
and `main.go:148` to populate `App.Tools`. The assembly order:

| # | Tool set | Source | Gate | file:line |
|---|----------|--------|------|-----------|
| 1 | `DefaultTools(cwd)` | `tools.go:12-194` | always | `mcp_manager.go:303` |
| 2 | `SearxngTools()` | `tools/searxng.go` | `cfg.SearXngURL != ""` | `mcp_manager.go:304-306` |
| 3 | `GoogleTools()` | `tools/google.go` | `cfg.GoogleAPIKey + GoogleCX` | `mcp_manager.go:307-309` |
| 4 | `mcp.OpenAITools()` | `mcp_manager.go` | `mcp != nil` | `mcp_manager.go:310-312` |
| 5 | `mashuraToolDefs()` | `mashura.go:73-147` | `OracleEnabled + API key` | `mcp_manager.go:313-315` |
| 6 | `lsp.LSPTools(cwd)` | `lsp/tools.go:17-75` | `cfg.LSPEnabled` | `mcp_manager.go:316-318` |

`DefaultTools` (`tools.go:12-194`) returns exactly these 16 tools in order:

1. `dispatch_subagent` — `tools.go:35-46`
2. `dispatch_subagents` — `tools.go:48-60`
3. `run_shell` — `tools.go:62-67`
4. `read_file` — `tools.go:69-77`
5. `read_file_full` — `tools.go:79-87`
6. `search_files` — `tools.go:89-98`
7. `find_files` — `tools.go:100-107`
8. `list_dir` — `tools.go:109-114`
9. `edit_file` — `tools.go:116-127`
10. `open_url` — `tools.go:129-134`
11. `write_file` — `tools.go:136-142`
12. `delete_file` — `tools.go:144-152`
13. `move_file` — `tools.go:154-164`
14. `run_background` — `tools.go:166-175`
15. `kill_process` — `tools.go:177-184`
16. `read_process_log` — `tools.go:186-193`

### Subagent's toolset (DiscoveryTools)

`DiscoveryTools(cwd)` (`tools.go:219-288`) returns exactly 5 tools:

1. `read_file` — `tools.go:239-248`
2. `read_file_full` — `tools.go:250-258`
3. `search_files` — `tools.go:260-269`
4. `find_files` — `tools.go:271-278`
5. `list_dir` — `tools.go:280-286`

Plumbed at `subagent.go:564`:
```go
Tools: tools.DiscoveryTools(a.Exec.Cwd()),
```

### Diff: what the parent has that the subagent does not

| Tool | Category | Parent (DefaultTools) | DiscoveryTools | Notes |
|------|----------|----------------------|-----------------|-------|
| `dispatch_subagent` | dispatch | ✓ | ✗ | nested dispatch |
| `dispatch_subagents` | dispatch | ✓ | ✗ | nested batch dispatch |
| `run_shell` | **exec** | ✓ | ✗ | deliberately absent — see `tools.go:211-218` |
| `edit_file` | **edit** | ✓ | ✗ | file mutation |
| `open_url` | external (host) | ✓ | ✗ | host desktop action |
| `write_file` | **edit** | ✓ | ✗ | file mutation |
| `delete_file` | **edit** | ✓ | ✗ | file mutation |
| `move_file` | **edit** | ✓ | ✗ | file mutation |
| `run_background` | **exec** | ✓ | ✗ | background process |
| `kill_process` | **exec** | ✓ | ✗ | process control |
| `read_process_log` | read | ✓ | ✗ | reads bg process logs |
| `read_file` | read | ✓ | ✓ | |
| `read_file_full` | read | ✓ | ✓ | |
| `search_files` | read | ✓ | ✓ | |
| `find_files` | read | ✓ | ✓ | |
| `list_dir` | read | ✓ | ✓ | |

Plus conditional parent-only tools: `searxng_*`, `google_*`, `mashura__*`,
`lsp_*`, and MCP tools (`{server}__{tool}`). None of these are in
`DiscoveryTools`. `mashura.go:72` explicitly states mashura tools are
"Intentionally absent from discoveryTools so subagents never receive them."

### "Edit" (file mutation) vs "exec" (shell/process) classification

The codebase has two relevant classifiers:

**`GatedTool`** (`tools.go:198-206`) — returns true for tools requiring
human confirmation:

| Category | Tools | file:line |
|----------|-------|-----------|
| **exec** | `run_shell`, `run_background`, `kill_process` | `tools.go:201` |
| **edit** | `write_file`, `edit_file`, `delete_file`, `move_file` | `tools.go:200` |

Note: `open_url` is NOT in `GatedTool` but does call `a.Confirm` at
`app.go:1492`. `read_process_log` is neither gated nor edit/exec.

The tool descriptions themselves mark the same split:
- "Requires user confirmation" appears in: `run_shell` (`tools.go:63`),
  `edit_file` (`tools.go:120`), `write_file` (`tools.go:137`),
  `delete_file` (`tools.go:148`), `move_file` (`tools.go:159`),
  `run_background` (`tools.go:170`), `kill_process` (`tools.go:180`).

**Edit tools** (file mutation): `write_file`, `edit_file`, `delete_file`,
`move_file`.

**Exec tools** (shell/process): `run_shell`, `run_background`, `kill_process`.

`read_process_log` is read-only but tied to the background-process registry
(`bgProcs` on `App`) — it reads from `a.bgProcs[bgID]` (`app.go:1983`), which
is a parent-local map a child would not share (see §3).

### Is the toolset plumbed anywhere besides the Tools field?

Yes, in several places:

1. **`subagentSystemPrompt`** (`subagent.go:90-100`) — the child's system
   prompt hardcodes the available tool names:
   ```
   "Use list_dir and find_files to navigate, search_files to grep,
    and read_file (offset/limit for large files) or read_file_full..."
   ```
   This is a text enumeration of exactly the DiscoveryTools set. Adding
   write/exec tools would require rewording this prompt (see §2).

2. **Tool-result handling (`CapOrStub`)** (`app.go:1047-1070`) — exempts
   `mashura__*` and `dispatch_subagent(s)` results from capping via
   `IsMashuraTool` / `IsSubagentResult` (`tools.go:295-310`). This is
   tool-name-based, not toolset-based, so it does not assume read-only —
   but `CapOrStub` runs inside the child's `Send` loop (the child has its
   own `App` with its own `CapOrStub`), so any new tool results would flow
   through the same cap path. No assumption break here.

3. **`ToolCache`** (`app.go:1142-1161`, `subagent.go:568`) — the child gets
   a fresh `map[string]bool{}`. The dedup logic is tool-agnostic (normalized
   `name|args` key). No read-only assumption.

4. **`handleToolCall` / `ExecuteToolCall`** (`app.go:1131-2138`) — the
   child's `Send` calls `a.handleToolCall` which dispatches via a switch on
   tool name. The switch has cases for ALL tools (edit, write, delete, move,
   run_shell, etc.) regardless of what's in `a.Tools`. The toolset only
   controls what the model is *offered*; the dispatch switch is universal.
   So if a child were given `write_file` in its `Tools` and the model called
   it, `ExecuteToolCall` would handle it — the plumbing exists, it's just
   not advertised.

5. **`wfPhaseBlock`** (`app.go:1394-1430`) — workflow phase enforcement
   checks `write_file`/`edit_file`/`run_background`. The child has
   `Workflow: nil` (`subagent.go:567`), so this is inert in children today.
   [inferred — confirm] If a write-capable child were given a workflow
   context, this gate would activate; as-is, it's a no-op.

---

## 2. dispatch_subagent Surface — Schema, Description, Task Prompt

### Tool schema (what the parent model sees)

Defined in `tools.go:35-46`:
```go
{Type: "function", Function: proxy.ToolFunction{
    Name: "dispatch_subagent",
    Description: "Dispatch a read-only discovery subagent for a bounded, " +
        "single-objective task. The subagent navigates and reads code " +
        "(list_dir, find_files, search_files, read_file), then returns a " +
        "structured JSON summary (findings with file:line locations, " +
        "checked/skipped files, uncertainty). Use when gathering " +
        "information from many files without loading all raw content into " +
        "the main context. Multiple independent dispatch_subagent calls " +
        "emitted in the same turn run in parallel (bounded); for several " +
        "related discovery tasks prefer dispatch_subagents (plural) which " +
        "runs them concurrently by design.",
    Parameters: obj(map[string]interface{}{
        "task": strProp("Specific discovery objective, e.g. 'find where " +
            "ToolResultCap is configured across the repo'."),
    }, "task"),
}},
```

**Read-only is stated three times** in the description:
- "read-only discovery subagent"
- "navigates and reads code"
- enumerates only read tools: "list_dir, find_files, search_files, read_file"

The `dispatch_subagents` batch variant (`tools.go:48-60`) mirrors this:
"Dispatch several read-only discovery subagents CONCURRENTLY..."

### Where the task prompt for the child is built

`subagentSystemPrompt` (`subagent.go:90-100`) is the child's system prompt,
injected at `subagent.go:577`:
```go
sub.Conv = []proxy.Message{{
    Role: "system",
    Content: StrPtr(subagentSystemPrompt),
    Pinned: true,
}}
```

The prompt **states read-only** and **implies it structurally**:
- States: "Use list_dir and find_files to navigate, search_files to grep,
  and read_file... then respond with ONLY a valid JSON object"
- Implies: the output contract is a `SubagentSummary` (`subagent.go:31-39`)
  with fields `Findings`, `Checked`, `Skipped`, `Uncertainty`, `SpillRefs` —
  a discovery report schema with no field for "files changed" or "actions
  taken."

The task itself is passed as the user message at `subagent.go:579`:
```go
raw, err := sub.Send(ctx, task)
```
The `task` string is the raw `args.Task` from the parent's tool call
(`app.go:2012` for sequential, `subagent_parallel.go:171` for parallel).

### What would need rewording so the model knows delegation of action is possible

1. **`dispatch_subagent` description** (`tools.go:36-42`) — "read-only
   discovery subagent" and the tool enumeration would need to reflect the
   capability parameter. The parent model needs to know it can delegate
   writes.

2. **`subagentSystemPrompt`** (`subagent.go:90-100`) — the hardcoded tool
   enumeration ("Use list_dir and find_files...") and the instruction to
   "respond with ONLY a valid JSON object" with the discovery-only schema.
   A write-capable child would need either a different prompt or a
   parameterized one.

3. **`SubagentSummary` schema** (`subagent.go:31-39`) — has no field for
   reporting what was written/changed. `Finding.Kind` allows
   "match|pattern|error|fact|ref" but not "edit" or "write". The `Render()`
   method (`subagent.go:71-87`) trims to 4000 chars — a write-capable child
   reporting changed files would fit, but the schema has no natural home for
   it (see §6).

4. **`subagentRetryPrompt`** (`subagent.go:103`) — sent on parse failure
   to request clean JSON retry. Tool-agnostic; no rewording needed for
   writes, but if the child did writes before the parse failure, the retry
   would ask for JSON only (no tool re-execution). [See §5 on replay.]

---

## 3. Confirmation Flow

### The child's Confirmer

The child gets `readOnlyConfirmer()` at `subagent.go:565`:
```go
Confirm: readOnlyConfirmer(),
```

`readOnlyConfirmer` (`subagent.go:141-143`):
```go
func readOnlyConfirmer() Confirmer {
    return func(_, _, _ string, readAction bool) bool { return readAction }
}
```
Auto-approves reads (`readAction=true`), silently declines everything else.

### Which tools consult Confirm?

In `ExecuteToolCall` (`app.go:1432-2138`), these tools call `a.Confirm`:

| Tool | file:line | readAction passed |
|------|----------|-------------------|
| `run_shell` | `app.go:1468` | `IsReadOnlyShell(cmd)` — dynamic |
| `open_url` | `app.go:1492` | `false` |
| `write_file` | `app.go:1745` | `false` |
| `edit_file` | `app.go:2287` | `false` |
| `delete_file` | `app.go:1844` | `false` |
| `move_file` | `app.go:1875` | `false` |
| `run_background` | `app.go:1919` | `false` |
| `kill_process` | `app.go:1955` | `false` |
| `mashura__*` | `app.go:190` (mashura.go) | `false` |
| `external_backend` (egress) | `app.go:466`, `subagent.go:164` | `false` |

Plus `MCP.CallTool` consults `a.Confirm` at `app.go:2129`.

### Can a child tool require confirmation today?

**No, by construction.** `DiscoveryTools` contains only `read_file`,
`read_file_full`, `search_files`, `find_files`, `list_dir`. None of these
call `a.Confirm` — they are all ungated read-only tools in
`ExecuteToolCall` (see `app.go:1504-1686` for their cases: none call
`a.Confirm`). So `readOnlyConfirmer` is indeed "belt-and-suspenders"
as its comment says (`subagent.go:140`).

The confirmation gate is unreachable from `DiscoveryTools`. If a child were
given edit/exec tools, the gate would become reachable — and
`readOnlyConfirmer` would **silently decline every write/exec** (returns
`false` for `readAction=false`). The child would see `[declined by user]`
results and have no way to escalate.

### How unattended mode interacts

There are three Confirmer implementations:

1. **`tuiConfirmer`** (`agent_async.go:827-858`) — interactive TUI. Checks
   `AutoApprove` first; if true, calls `SuspendAuto` to check carve-outs
   (external_backend always, destructive unless `AllowDestructive`,
   pre-IMPLEMENT non-read shell). If suspended, posts a `ConfirmReqMsg`
   to the TUI and blocks on a response channel. Handles `ChoiceAllowReads`
   (sets `AllowReads=true`), `ChoiceApprove`, `ChoiceDecline`.

2. **`headlessConfirmer`** (`run.go:178-212`) — headless mode. Without
   `--auto`, declines everything. With `--auto`: allows mashura, allows
   `run_shell`/`run_background` unless destructive (without
   `--allow-destructive`), allows `external_backend` only with
   `--allow-external`, allows all other tools by default.

3. **`readOnlyConfirmer`** (`subagent.go:141-143`) — subagent only.
   Approves reads, declines everything else.

### What "child inherits session standing consent, never prompts" would require

**Where session-level consent state lives (on `App`):**

| Field | Type | file:line | Purpose |
|-------|------|-----------|---------|
| `consentedBackends` | `map[string]bool` | `app.go:284` | external backend egress consent |
| `AllowReads` | `bool` | `app.go:56` | auto-approve read-only shell for session |
| `AutoApprove` | `bool` | `app.go:60` | skip all confirm prompts for session |
| `AllowDestructive` | `bool` | `app.go:69` | auto-approve destructive shell (requires AutoApprove) |

**Is it readable at dispatch time?**

`dispatchSubagent` (`subagent.go:489-677`) reads these parent fields:
- `a.consentedBackends` — **snapshotted** at `subagent.go:555-558` (copy)
  and passed to the child at `subagent.go:573`. This is the **only**
  consent field currently propagated to the child.
- `a.AutoApprove` — **not read, not propagated** to the child.
- `a.AllowReads` — **not read, not propagated** to the child.
- `a.AllowDestructive` — **not read, not propagated** to the child.

The child App struct literal (`subagent.go:560-576`) sets:
- `Confirm: readOnlyConfirmer()` — hardcodes the confirmer, ignoring
  parent's `Confirm`.
- `consentedBackends: consentSnapshot` — the only inherited consent state.
- Does NOT set `AutoApprove`, `AllowReads`, `AllowDestructive` (all default
  to `false`).

**What "inherits session standing consent" would require:**
- Propagating `AutoApprove`, `AllowReads`, `AllowDestructive` from parent
  to child in the struct literal at `subagent.go:560-576`.
- Replacing `readOnlyConfirmer()` with either the parent's `Confirm`
  function or a child-specific confirmer that consults the inherited
  state.
- The existing `readOnlyConfirmer` pattern (no prompts, pure function of
  the consent flags) is the natural shape for "never prompts" — but it
  would need to consult the inherited flags instead of always returning
  `readAction`.
- **Concurrency constraint:** the existing snapshot pattern for
  `consentedBackends` (`subagent.go:555-558`) exists because the parent
  map could be written concurrently. `AutoApprove`/`AllowReads`/
  `AllowDestructive` are bools (not maps), but they are mutable on the
  parent — the same snapshot-or-copy discipline would apply.
- **The egress gate is already handled:** `ensureSubagentConsent`
  (`subagent.go:153-174`) runs on the main goroutine before dispatch,
  and the child receives the snapshot. This pattern is the template for
  tool-call consent propagation.

---

## 4. Shared Exec

### What state does Exec carry?

`Executor` interface defined at `exec.go:17-85`. Two implementations:

**`DockerExecutor`** (`exec.go:104-114`):
```go
type DockerExecutor struct {
    container     string    // container name
    image         string    // docker image
    workspaceRoot string    // project root (immutable)
    hostMount     string    // host path bind-mounted at workspaceRoot
    dockerSock    bool      // host docker socket mounted
    signing       bool      // SSH signing passthrough
    sandboxTools  string    // cached probe result
    toolsOnce     sync.Once // guards probe
    generation    int       // increments on restart
}
```

**`DirectExecutor`** (`exec.go:395-400`):
```go
type DirectExecutor struct {
    root         string    // project root (immutable)
    sandboxTools string
    toolsOnce    sync.Once
    generation   int
}
```

**State summary:**
- `workspaceRoot`/`root` — **immutable**, set at construction.
- `sandboxTools` + `toolsOnce` — lazily-probed, `sync.Once`-guarded cache.
- `generation` — increments on container restart.
- No mutable env, no mutable cwd, no process handles stored on the
  executor itself. Each `RunShell` call composes a fresh command via
  `runFromRoot` (`exec.go:91-93`) — no shell session, no persistent cwd.
- Background processes (`StartBackground`) return `pid`/`pgid` and a
  `logPath`; the executor does NOT store the process handle. The caller
  (`App.bgProcs` at `app.go:162`) owns the registry.

### Which tools go through Exec?

| Tool | Exec method(s) used | file:line |
|------|---------------------|-----------|
| `run_shell` | `RunShell` | `app.go:1472` |
| `read_file` | `ConfinePath`, `StatFile`, `ReadFile` | `app.go:1539-1558` |
| `read_file_full` | `ConfinePath`, `StatFile`, `ReadFile` | `app.go:1598-1614` |
| `list_dir` | `ConfinePath`, `ListDir` | `app.go:1646-1650` |
| `find_files` | `ConfinePath`, `RunShell` (composed `find` cmd) | `app.go:1669-1679` |
| `search_files` | `ConfinePath`, `RunShell` (composed `grep` cmd) | `app.go:1705-1719` |
| `write_file` | `ConfinePath`, `WriteFile` | `app.go:1735-1748` |
| `edit_file` | `ConfinePath`, `ReadFile`, `WriteFile` | `app.go:2265-2290` |
| `delete_file` | `ConfinePath`, `DeletePath` | `app.go:1838-1847` |
| `move_file` | `ConfinePath` (src), `ConfinePath` (dst), `MovePath` | `app.go:1863-1878` |
| `run_background` | `StartBackground` | `app.go:1923` |
| `kill_process` | `IsProcessAlive`, `KillPgid` | `app.go:1958-1972` |
| `read_process_log` | `IsProcessAlive`, `ReadFileTail` | `app.go:1991-1998` |
| `open_url` | **none** (runs on host via `wtools.OpenOnHost`) | `app.go:1495` |
| `mashura__*` | `ReadFile`, `ListDir`, `RunShell` (for source reading) | `mashura.go:520, 591-616` |
| `lsp_*` | `HostPathToURI` (via LSP Manager, which holds `exec`) | `lsp/tools.go:169` |

### Is the Exec shared between parent and children?

**Yes.** `subagent.go:563`:
```go
Exec: a.Exec,
```
The child App gets the **same pointer** to the parent's Executor. This is
documented at `subagent.go:456`: "shares the parent's Executor (same
filesystem, read-only toolset only)".

### Is concurrent use by parent + children safe today?

The concurrency audit in `subagent_parallel.go:82-100` states:
> "Executor: shared with workers. RunShell/ReadFile/ListDir compose fresh
> commands per call (runFromRoot); the one lazily-written cache
> (SandboxTools probe) is sync.Once-guarded. Discovery tools are
> read-only, so no workspace write races from workers."

**Safe today because:** all DiscoveryTools are read-only. Multiple
goroutines reading the same filesystem concurrently is safe. The
`SandboxTools` cache is `sync.Once`-guarded (`exec.go:112`, `exec.go:398`).
Each `RunShell` call spawns an independent `docker exec` process
(`exec.go:242`) or `sh -c` process (`exec.go:421`) — no shared shell
state.

### What breaks if a child gets write/exec tools while sharing the instance?

1. **File write races:** two children (or child + parent) writing the
   same file concurrently would have no coordination. `WriteFile`
   (`exec.go:280-289` Docker, `exec.go:469-478` Direct) has no locking.
   The `runFromRoot` pattern means each write is a separate `docker exec`
   or `sh -c` — there's no atomicity across calls.

2. **`App.bgProcs` registry isolation:** `run_background`, `kill_process`,
   and `read_process_log` all use `a.bgProcs` (`app.go:162-163`), which is
   a map on the **child's** App (the child is a separate `*App` with its
   own `bgProcs: nil` at construction — `subagent.go` does not copy
   `bgProcs`). So a child starting a background process would register it
   in its own `bgProcs`, which is discarded when the child finishes
   (`dispatchSubagent` returns at `subagent.go:677` with no cleanup of
   `sub.bgProcs`). The parent would not know about child-started
   processes, and `StopAllBackgroundProcs` (`app.go:2144-2162`) on the
   parent would not kill them. **Ambiguity flag:** the child does not
   call `StopAllBackgroundProcs` anywhere — `dispatchSubagent` has no
   cleanup for `bgProcs`. Started processes would be orphaned.

3. **`generation` check:** `bgProcs` entries carry `entry.generation`
   (`app.go:327`), checked against `a.Exec.Generation()`. Since the
   parent and child share the same `Exec` pointer, `Generation()` is
   consistent. But `kill_process` / `read_process_log` look up entries in
   `a.bgProcs` (the child's map), not the parent's — so a child cannot
   manage parent-started processes and vice versa.

4. **`SandboxTools` probe:** already `sync.Once`-guarded, safe for
   concurrent access. No break.

5. **LSP Manager:** the child has `LSP: nil` (`subagent.go` does not set
   it). If a child had LSP tools, it would need the LSP manager — and
   `lsp.Manager` holds the `exec.Executor` reference (`lsp/tools.go:169`
   `m.exec.HostPathToURI`). LSP file-sync notifications (`NotifyChange`,
   `MarkOpenFilesDirty`, `BatchNotifyWatchedFiles`) write to the LSP
   server's internal state — concurrent parent + child LSP usage would
   need the manager to be thread-safe. [Not investigated — flagging as
   ambiguity.]

---

## 5. Write-Safety Mechanics

### Workspace-boundary guard (path validation)

**Yes, every file-writing tool calls `a.Exec.ConfinePath` before acting:**

| Tool | ConfinePath call | file:line |
|------|-----------------|-----------|
| `write_file` | `a.Exec.ConfinePath(ctx, args.Path)` | `app.go:1735` |
| `edit_file` | `a.Exec.ConfinePath(ctx, args.Path)` | `app.go:2265` |
| `delete_file` | `a.Exec.ConfinePath(ctx, args.Path)` | `app.go:1838` |
| `move_file` | `a.Exec.ConfinePath(ctx, args.Src)` + `(ctx, args.Dst)` | `app.go:1863, 1867` |
| `read_file` | `a.Exec.ConfinePath(ctx, args.Path)` | `app.go:1539` |
| `read_file_full` | `a.Exec.ConfinePath(ctx, args.Path)` | `app.go:1598` |
| `list_dir` | `a.Exec.ConfinePath(ctx, args.Path)` | `app.go:1646` |
| `find_files` | `a.Exec.ConfinePath(ctx, findPath)` | `app.go:1669` |
| `search_files` | `a.Exec.ConfinePath(ctx, args.Path)` | `app.go:1705` |

`ConfinePath` (`exec_ops.go:113-146`) resolves the path relative to
`workspaceRoot`, resolves symlinks, and verifies the canonical path is
inside `workspaceRoot` via `isInsideWorkspace` (`exec_ops.go:107-111`).
Returns an error if outside.

`run_shell` does NOT call `ConfinePath` — it passes the raw command to
`RunShell`, which wraps it in `runFromRoot` (`exec.go:91-93`) that `cd`s
to the workspace root first. But the command itself can `cd` anywhere or
reference absolute paths outside the workspace. This is documented in
`readonly.go:8-15`: the shell classification is "friction, NOT a security
boundary."

### Where would a per-child writer lock naturally live?

**Existing semaphore in `runSubagentJobs`:**

`subagent_parallel.go:107`: `sem := make(chan struct{}, maxPar)` — this
semaphore bounds **concurrency** (how many children run at once), not
**file-level writes**. It does not serialize writes to the same path.
A child holding a semaphore slot can write any file while another child
in another slot writes the same file.

Two natural homes for a writer lock:

1. **On the shared `Executor`** — since `DockerExecutor` and
   `DirectExecutor` are the single shared objects across parent + all
   children, a mutex on the executor instance would serialize all writes.
   But this would serialize ALL writes (parent + child) globally,
   including the parent's own writes while a child is running — a coarse
   lock. The executor has no existing mutex (only `sync.Once` for the
   probe).

2. **Tool-level** — a lock checked in `ExecuteToolCall` for
   `write_file`/`edit_file`/`delete_file`/`move_file`. This would need
   to live on a shared object (the executor or a new shared struct) to
   be visible across child boundaries. The `App` struct is per-child, so
   a lock on `App` would not coordinate across children.

[inferred — confirm] Neither location exists today. There is no
file-level write coordination mechanism in the codebase.

### Does anything in the parallel path assume children are side-effect-free?

**Yes, explicitly:**

1. **`subagent_parallel.go:86-87`:** "Discovery tools are read-only, so
   no workspace write races from workers." — This is the documented
   safety assumption.

2. **Retry logic — IS a failed child ever re-dispatched?**
   **No.** There is no re-dispatch/retry logic for subagent jobs.
   `runSubagentJobs` (`subagent_parallel.go:101-145`) runs each job once;
   on panic, it produces `panicJobResult` (`subagent_parallel.go:61-68`);
   on cancellation, `cancelledJobResult` (`subagent_parallel.go:51-57`).
   Results are collected and returned — no re-queue.

   The only "retry" in `dispatchSubagent` is the **JSON parse retry**
   (`subagent.go:610-625`): if the first response fails to parse as
   `SubagentSummary` JSON, the child is re-prompted with
   `subagentRetryPrompt` (`subagent.go:103`) and `sub.Tools` is set to
   `nil` (`subagent.go:613`) so the model can only emit text, not call
   tools. This retry is **after** the first `Send` completed — so if the
   first Send did writes (in a hypothetical write-capable child), the
   writes already happened. The retry does NOT replay tool calls. But
   if the first Send's tool loop did writes and then the model's final
   text wasn't valid JSON, the retry asks for JSON only — the writes
   are already on disk. **No replay risk in the current code**, but if
   a future change re-dispatched the *task* (not just the parse), writes
   would replay.

3. **`dispatchSubagentGated`** (`subagent.go:447-452`) — the sequential
   path. No retry; one dispatch, one return.

4. **`runParallelSubagentBlock`** (`subagent_parallel.go:154-226`) — no
   retry of any job. Phase C finalizes results in order; a failed job
   gets its error summary.

**Summary:** No re-dispatch logic exists today, so no replay-of-writes
risk exists today. The assumption "children are side-effect-free" is
explicit in the concurrency audit comment but not enforced by any
mechanism — it's true by construction because `DiscoveryTools` has no
mutating tools.

---

## 6. Result Contract

### What flows back on completion?

`dispatchSubagent` (`subagent.go:489`) returns 5 values:
```go
func (a *App) dispatchSubagent(...) (SubagentSummary, []proxy.GroundingEntry,
    int, string, []proxy.CostRow)
```

1. **`SubagentSummary`** (`subagent.go:31-39`) — the structured return:
   - `Objective string`
   - `Status string` — "incomplete" or empty
   - `Findings []Finding` — `{Summary, Location, Kind, Weight}`
   - `Checked []CheckedItem` — `{Path, SizeK, Status}`
   - `Skipped []SkippedItem` — `{Path, Reason}`
   - `Uncertainty []string`
   - `SpillRefs []SpillRef` — `{ToolName, Path, SizeK}`

2. **`[]proxy.GroundingEntry`** — grounding entries from `sub.Client.Grounding()`
   (`subagent.go:580`).

3. **`int`** — `ctxSize` = `TranscriptSize(sub.Conv)` (`subagent.go:581`).

4. **`string`** — `usedBackend` = `sub.Client.LastUsedBackend()`
   (`subagent.go:582`).

5. **`[]proxy.CostRow`** — child's cost rows from `sub.Costs.Snapshot()`
   (`subagent.go:575, 675`), folded into parent via `foldSubagentCost`
   (`subagent.go:431-440`).

### Summary construction

The summary is produced by the child model's final text response, parsed
as JSON (`subagent.go:609`). If parsing fails, a retry is attempted
(`subagent.go:614`), then a fallback `SubagentSummary` with a
`parse-error` finding (`subagent.go:619-624`).

`Render()` (`subagent.go:71-87`) marshals to JSON, trimming findings
from the tail until ≤4000 chars.

Exhaustion/confinement status overrides (`subagent.go:637-669`) set
`Status="incomplete"` and append `Skipped`/`Uncertainty` entries.

### SubagentDoneMsg fields

`SubagentDoneMsg` (`msgs.go:63-77`):
```go
type SubagentDoneMsg struct {
    ChatID       string
    Grounding    []proxy.GroundingEntry
    CtxSize      int
    HardMaxBytes int
    UsedBackend  string
    CostUSD      float64
}
```

Sent at `app.go:2041-2048` (sequential) and `subagent_parallel.go:206-213`
(parallel). Note: the `SubagentSummary` itself is NOT in `SubagentDoneMsg`
— it's returned as the tool result string (rendered JSON) to the parent
model. The `SubagentDoneMsg` is a TUI event for the sidebar.

### Spill files

The rendered summary is spilled to disk at `app.go:2057` (sequential)
and `subagent_parallel.go:216` (parallel):
```go
if spillPath := wtools.SpillToCache(a.chatID(), "dispatch_subagent", fullJSON); spillPath != "" {
    result = fullJSON + fmt.Sprintf("\n[subagent summary at: %s]", spillPath)
}
```
The spill path is under the parent's `chatID` toolcache directory
(`toolcap.go:103-109, 230-232`). The child's ephemeral `chatID`
(`subagent.go:490-493`) is used only for the child's own internal
toolcache during `Send` — it does not appear in the final result.

### Is there any existing record of which files a child's tool calls touched?

**No.** There is no bookkeeping of files touched by a child's tool calls.

What exists:
- **`Checked []CheckedItem`** in `SubagentSummary` — but this is
  model-self-reported (the child model fills it in as part of its JSON
  response, `subagent.go:98`). It's a coverage signal for files *read*,
  not files *written*. And it's model-generated, not mechanically tracked.
- **`ToolTraceEntry`** (`app.go:305-317`) — captures tool call evidence
  for the step log. But `captureToolTrace` (`app.go:1167-1172`) and
  `recordRecentTrace` (`app.go:1261-1271`) run on the **child's** `App`
  during the child's `Send` loop. These traces live in
  `sub.WorkflowStepTrace` and `sub.recentTraces` — both on the child
  `App`, which is local to `dispatchSubagent` and **discarded** when the
  function returns. They are NOT propagated to the parent.
- The parent's `finalizeToolResult` (`app.go:622-660`) records traces for
  the `dispatch_subagent` tool call itself (not the child's internal
  calls), but that trace entry (`app.go:679`) captures the final summary
  string, not individual file paths the child wrote.
- `ToolCache` (`app.go:1142-1161`) — records `(tool, args)` dedup keys
  on the child's `ToolCache` map (`subagent.go:568`). This is discarded
  with the child. And it records ALL tool calls (reads included), not
  just writes, and it stores only a boolean, not file paths.

**Bottom line:** There is no existing mechanical record of which files
a child's tool calls touched that could populate a `files_changed` list
on `SubagentDoneMsg` or `SubagentSummary` without new bookkeeping. The
child's `WorkflowStepTrace` and `recentTraces` are the closest existing
artifacts, but they are (a) discarded with the child, (b) not file-write-
specific, and (c) not structured for file-path extraction (they're
`ToolTraceEntry` with `Command`/`FirstLine`/`LastLine` text fields,
`app.go:305-317`).

To populate a `files_changed` list without new bookkeeping, one would
need to either:
- Scrape the child's `recentTraces` before discarding the child `App`
  (but `ToolTraceEntry.Command` holds the path arg, not a structured
  "written file" marker — see `MakeTraceEntry` `app.go:1174-1253`, the
  default case extracts `Path` from args, `app.go:1191-1200`).
- Or add a write-path tracker on the child (new bookkeeping).

**No existing field on `SubagentDoneMsg` or `SubagentSummary` is named
or structured for files_changed.** The `Finding.Location` field
(`subagent.go:46`) holds "file:line or path" but is model-generated and
semantically about *findings*, not *changes*.

---

## Summary of Ambiguity Flags

1. **§3 — LSP in children:** Not investigated whether `lsp.Manager` is
   thread-safe for concurrent parent + child usage. The child has
   `LSP: nil` today (`subagent.go` does not set it), so this is only
   relevant if a future write-capable child were also given LSP tools.

2. **§4 — `bgProcs` orphaning:** `dispatchSubagent` has no cleanup of
   `sub.bgProcs` (`subagent.go:560-677`). If a child started background
   processes, they would be orphaned on child completion. Not a current
   issue (DiscoveryTools has no `run_background`), but relevant for
   exec-capable children.

3. **§5 — Writer lock location:** Neither a file-level lock on the
   executor nor a tool-level lock exists today. The natural home is
   [inferred — confirm] the shared `Executor` instance, but this is a
   design choice not a mechanical finding.

4. **§5 — Re-dispatch risk:** No re-dispatch logic exists today. The
   only retry is the JSON-parse retry (`subagent.go:610-625`), which
   does not replay tool calls. Any future change that re-dispatches a
   failed task would replay writes — but this is a hypothetical, not
   current behavior.

5. **§6 — `ToolTraceEntry.Command` as file-path source:** The `Command`
   field (`app.go:1191-1200`) is populated from the `path` arg for
   non-`run_shell` tools, but it's a text field populated by best-effort
   JSON parsing, not a structured "written file" record. Whether it's
   reliable enough to populate a `files_changed` list is a design
   question, not a mechanical fact.
