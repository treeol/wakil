# LSP Integration Proposal for wakīl — v3 (locked)

> **Status:** v3 locked. Resolutions R1–R4 and locks L1–L3 folded in. Frozen
> baseline (10 tasks + measured grep numbers) committed. Implementation proceeds
> in phases after Phase 0 approval. Diagnostics scope dropped — six tools only,
> MVP ships four.

## Scope

Six tools total: `lsp_definition`, `lsp_references`, `lsp_hover`, `lsp_symbols`,
`lsp_document_symbols`, `lsp_rename`. MVP ships four (`lsp_definition`,
`lsp_references`, `lsp_hover`, `lsp_symbols`). No diagnostics tool, no post-edit
diagnostics hook, no `publishDiagnostics` handling.

## Pre-registered decisions

### Decision 1 — TOOL INPUT SCHEMA: hybrid, line-anchored PRIMARY  *(R4)*

**Chosen:** Hybrid schema — line-anchored as the **primary** path, not a
concession. The model passes `(file, line, symbol_name)` where `line` is the
primary anchor and `symbol_name` disambiguates within the line.

```
lsp_definition(path, line, symbol?)         — line required (primary anchor)
lsp_references(path, line, symbol?)
lsp_hover(path, line, symbol?)
lsp_rename(path, line, symbol?, new_name)    — new_name required
lsp_symbols(query)                            — workspace-wide, no position
lsp_document_symbols(path)                    — no position needed
```

**Primary resolution path (line-anchored, hot path):**
1. The client reads the file via the executor.
2. Locates the line (1-based → 0-based conversion).
3. Finds the `symbol` substring on that line.
4. Computes the character offset in the **negotiated** encoding (UTF-8 if
   negotiated, else UTF-16), targeting the **middle of the identifier** (cursor-
   bias guard — landing at a token boundary resolves left-vs-right incorrectly).
5. Issues the positional LSP request.

**Fallback (rarely hit):** If only `symbol` is provided (no line), the client
falls back to `documentSymbol` → find the symbol whose `name` matches → use its
`selectionRange.start`. This is for the "known name, unknown line" case only.

**Why line-anchored is PRIMARY (not a concession — R4 reframing):**
- Fewer round-trips than name-only: no `documentSymbol` pre-query on the hot
  path. The model already has the line from `read_file`'s `cat -n` output
  (verified: `formatFileView` in app.go emits `%6d\t%s`, 1-based, no columns).
- Removes the locals/params ceiling: `documentSymbol` is an outline of
  declarations — it excludes local variables and parameters. Line-anchoring
  covers usage sites and locals for one optional field.
- There is no "simpler" name-only variant. The hybrid is the primary design.

**Multiple-match ambiguity:**
1. Prefer `container`/`receiver` hint if provided (optional parameter).
2. If still ambiguous, return a compact candidate list (qualified name + kind +
   line + snippet), not raw ranges.
3. With line-anchoring, most ambiguity disappears — the line pins the occurrence.

**Cursor-bias guard:** The offset targets the middle of the identifier, not the
start byte. This is a test-worthy edge case — landing at a token boundary can
resolve to the wrong symbol.

**Rejected:**
- Positional `(file, line, character)`: the model never sees character offsets;
  LSP `character` defaults to UTF-16 — an unobservable, encoding-ambiguous
  integer. [panel consensus]
- Pure name-based without line: `documentSymbol` excludes locals/parameters,
  creating a functional ceiling. [panel consensus]

### Decision 2 — POSITION ENCODING: negotiate UTF-8, convert if needed

**Chosen:** Send `general.positionEncodings: ["utf-8", "utf-16"]` in the
`initialize` client capabilities. Read `capabilities.positionEncoding` from the
response. If `utf-8`, all positions are byte offsets — no conversion. If
`utf-16` (or absent, defaults to UTF-16 per spec), convert at the boundary using
`unicode/utf16` for surrogate-pair handling.

**Correctness invariant:** wakil's codebase contains non-ASCII (Arabic in
docstrings, README). LSP `character` defaults to UTF-16 code units — NOT a byte
offset, NOT a rune index. Negotiating UTF-8 lets the server own the offset math.

**Do not hard-assume gopls returns utf-8.** Implement the conversion path; assert
at runtime from the actual `initialize` response.

**Rejected:** Always-UTF-16 with conversion (off-by-N bugs for non-BMP).
Ignoring encoding (breaks silently on Arabic/CJK).

### Decision 3 — FILE-SYNC: didOpen + didChange + run_shell lazy invalidation  *(R3)*

**Chosen:** `textDocument/didOpen` on first touch, `textDocument/didChange`
(full sync) after `edit_file`/`write_file`, and **lazy invalidation with mtime
guard** for `run_shell` external edits.

**Hook points:**
- **`handleEditFile`** (~app.go:1876, after `WriteFile` succeeds): fire `didChange`.
- **`write_file` case** (~app.go:1440, after `WriteFile` succeeds): fire
  `didChange`.
- **First query on a file:** `didOpen` with full content (read via executor)
  before any positional request.
- **`run_shell` — lazy invalidation (R3):**
  - After any `run_shell` NOT provably read-only (coarse `isReadOnlyShell` gate;
    err toward dirty — it's free), mark all currently-open files **dirty**: a
    flag, no I/O.
  - On the NEXT LSP query touching a dirty file: `stat` it; if mtime+size
    changed since last sync, `didChange` from disk before issuing the request;
    if unchanged, clear the flag and skip.
  - For unopened files the shell created/deleted: one batched
    `workspace/didChangeWatchedFiles` after the non-read-only shell — no contents
    read, gopls just invalidates its snapshot.
  - This pays resync only when queried, only for the file queried, only when it
    actually changed. Strictly correct and cheaper than eager-all or heuristic.

**Panel-confirmed contract:** Once you `didOpen` a file, the LSP spec makes client
content authoritative and the server **stops reading that URI from disk**. So
pushing `didChange` is mandatory, not stylistic.

**Implementation specifics:**
- `didOpen` carries **full text** in `TextDocumentItem.text`.
- **Per-URI monotonic version counter.** `didOpen` v1, each `didChange` bumps.
  Version is the state *after* the change.
- **Full sync is fine for gopls** (accepts full content even when advertising
  incremental). But read `capabilities.textDocumentSync.change` and obey it.
- **Docker URI translation (silent killer):** `file://` URI must be the
  *container-visible* path (`/mnt/<dirname>/...`), not the host mount path.
  Mismatch → empty/wrong results that look like "no references."
- **`didClose` ≠ discard changes.** Reverts server to disk (= truth).

**Rejected:**
- `workspace/didChangeWatchedFiles` as primary: redundant (wakil knows when it
  writes). Corrected from v1: not rejected for "requires inotify" (false — it's
  client→server).
- No sync: gopls caches all content in snapshots; doesn't re-read disk per query.
  [panel consensus, gopls issue #78859]
- Eager-all resync after every `run_shell`: correct but slow (R3 rejects).
- Command-heuristic resync: wrong-shaped (R3 rejects).

### Decision 4 — INDEX READINESS: gate on $/progress

**Chosen:** Register for `window/workDoneProgress`. First query blocks until:
1. Indexing `$/progress` end notification arrives, or
2. Configurable timeout (default 30s) elapses, or
3. Server sends a successful (non-empty) response to the first query.

Timeout → `[lsp: server still indexing — try again in a moment. Fell back to
text search: no]`.

**Async caveat:** JSON-RPC over stdio is ordered, so a query after `didChange`
is processed after it. But gopls computes type-checking asynchronously —
`definition`/`hover` handled synchronously against the snapshot are fine. Noted
for generalization.

**Rejected:** No gate (silent empty = worst failure mode). Polling (wastes a
request; empty may be genuine).

### Decision 5 — CAPABILITY-DRIVEN GATING

Read `ServerCapabilities` from `initialize` result. Each tool enabled
dynamically:

| Tool | Capability check |
|---|---|
| `lsp_definition` | `definitionProvider != nil` |
| `lsp_references` | `referencesProvider != nil` |
| `lsp_hover` | `hoverProvider != nil` |
| `lsp_symbols` | `workspaceSymbolProvider != nil` |
| `lsp_document_symbols` | `documentSymbolProvider != nil` |
| `lsp_rename` | `renameProvider != nil` |

On top of config gate (`lsp_enabled`).

### Decision 6 — SERVER LIFECYCLE & RESILIENCE  *(R1)*

**Chosen:**
- Lazy spawn: one warm instance per `(workspace, language)`.
- Idle timeout: **30 min** (L3 — re-index is non-blocking, 30 min avoids
  thrashing on a session that pauses for lunch).
- Crash recovery: mirror `HandleStreamError` (agent_async.go:347).
  - **Fatal** (binary not found, `initialize` failed): no retry. Mark
    `unavailable` with reason. Tool returns failure contract (Decision 8).
  - **Transient** (process exited after successful `initialize`): auto-restart
    once with 1s backoff. If restart's `initialize` fails → fatal. If succeeds,
    re-issue pending query.
- `Generation()` check: if executor generation changed (container restarted),
  server is stale — re-spawn on next call.

**StartInteractive (R1 — APPROVED, amended):**

```go
// StartInteractive spawns a long-running process with stdin/stdout/stderr
// pipes for bidirectional communication (e.g., an LSP server speaking JSON-RPC).
// The caller owns the pipes and must close them on completion.
StartInteractive(ctx context.Context, command string) (
    stdin  io.WriteCloser,
    stdout io.ReadCloser,
    stderr io.ReadCloser,  // R1: crash classification needs stderr — gopls writes traces there
    pid    int,
    err    error,
)
```

**Shutdown contract (R1, document it):** graceful `shutdown` → `exit` exchange,
**THEN** close stdin. LSP servers exit on stdin EOF; this is the reliable
hard-stop because killing the host-side `docker exec -i` does not reliably reap
the in-container gopls child.

- **DockerExecutor:** `docker exec -i <container> sh -c <command>` with
  `cmd.StdinPipe()`, `cmd.StdoutPipe()`, `cmd.StderrPipe()`.
- **DirectExecutor:** `sh -c <command>` with pipes.

**R2 — MCP SDK framing reuse (timeboxed inspection, bright line):** Inspect
`modelcontextprotocol/go-sdk` transport subpackages for ~30 min. Reuse ONLY the
raw Content-Length read/write loop + id↔response correlation, and ONLY if it is
exported AND accepts arbitrary `json.RawMessage` payloads AND has zero coupling
to MCP message types or MCP lifecycle. The moment it imposes MCP structs or
handshake, STOP and hand-roll. Framing is ~150 lines; do not trade a
version-skew liability on the critical path to save it. If the boundary isn't
obviously clean, hand-roll.

**Rejected:** Bypassing executor (container name is private to DockerExecutor,
loses generation tracking). `StartBackground` (no pipes — redirects to log file).

### Decision 7 — RESULT SHAPING / TOKEN BUDGET

- **`lsp_references`:** cluster by file. Cap 5/file, 50 total. Render:
  ```
  3 files, 12 references:

  internal/agent/app.go:
    512:6   a.handleToolCall(ctx, tc)
    ...
  ```
- **`lsp_definition`:** cap 10 locations. If more, first 5 + count.
- **`lsp_hover`:** truncate to `ToolResultCap` (8k). Strip markdown to plain text
  (reuse `stripHTML` from `searxng.go:161`).
- **`lsp_symbols` / `lsp_document_symbols`:** cap 50 symbols. Tree with kind +
  name + range.
- **Tool-line summary:** `· lsp_references app.go → 12 refs in 3 files`.

### Decision 8 — FAILURE CONTRACT

Every failure returns an explicit, truthful status. Never silent empty. Mirrors
subagent's `Status:"incomplete"` (subagent.go:30):

| Failure | Tool returns |
|---|---|
| Server not configured | `[lsp: no server configured for language "go". Configure lsp_servers or install gopls.]` |
| Binary not found | `[lsp: server binary "gopls" not found in PATH. Install it or use --exec direct.]` |
| Crashed, restart failed | `[lsp: server "go" crashed and could not restart — <error>. Fell back to text search: no]` |
| Still indexing (timeout) | `[lsp: server still indexing after 30s — try again in a moment. Fell back to text search: no]` |
| Capability not supported | `[lsp: "gopls" does not support rename. Fell back to text search: no]` |
| No results (genuine) | `[lsp: no references found for "Foo" in app.go — the symbol exists but has no callers.]` |
| Name not found | `[lsp: symbol "Foo" not found in app.go. Symbols in file: Bar, Baz, Quux — check the name.]` |

`Fell back to text search: no` always present in failure cases. Fail-closed.

### Decision 9 — LIBRARY: hand-rolled  *(POST-PANEL, UNANIMOUS)*

Hand-rolled JSON-RPC 2.0 framing + hand-rolled LSP type structs (~500–700
lines, re-scoped from v1's undercount of 250).

**Decisive finding (verified against source):** `go.lsp.dev/protocol` lacks
`positionEncoding` fields entirely (LSP 3.16-era). So the library can't
represent the one thing Decision 2 depends on. Also pulls `go.uber.org/zap`,
`multierr`, `segmentio/encoding` (with SIMD asm).

**Struct sourcing:** Copy the ~7 relevant type shapes from **gopls's own
`internal/protocol`** (generated from LSP meta-model, definitive for what gopls
emits). Better than spec memory or the dormant `go.lsp.dev` package.

**High-hazard struct shapes (ranked):**
1. `WorkspaceEdit` (rename): `changes` vs `documentChanges`. gopls emits
   `documentChanges` with versioned IDs + resource ops. Wrong → **silent partial
   renames.**
2. `Hover.contents`: `MarkupContent | MarkedString | MarkedString[]`.
3. `documentSymbol`: `DocumentSymbol[]` (hierarchical) vs `SymbolInformation[]`
   (flat). Advertise `hierarchicalDocumentSymbolSupport`.
4. `definition`: `Location | Location[] | LocationLink[]`. Don't advertise
   `linkSupport`.
5. `references`: `context.includeDeclaration` mandatory.
6. `workspace/symbol`: `SymbolInformation[]` vs `WorkspaceSymbol[]` (3.17).

**Nil-slice→null hazard:** Initialize slices explicitly; `nil` marshals to
`null`, strict servers may reject where they expect `[]`.

**Server→client requests to handle:** `window/workDoneProgress/create` (must
answer), `window/showMessage`, `workspace/configuration`, `client/registerCapability`.

**Rejected:** `go.lsp.dev/protocol` (lacks positionEncoding, pulls heavy deps).
`go.lsp.dev/protocol` + `jsonrpc2` (version-skew trap, full framework overkill).

### Decision 10 — MVP CUT + PRE-REGISTERED OUTCOME

**MVP slice:** gopls only, 4 tools (`lsp_definition`, `lsp_references`,
`lsp_hover`, `lsp_symbols`). Done right: hybrid schema (D1), UTF-8 negotiation
(D2), didOpen/didChange + lazy invalidation (D3), index readiness (D4),
capability gating (D5). Deferred: `lsp_document_symbols`, `lsp_rename`, other
languages.

**Pre-registered claim:** On the 10 frozen tasks below, LSP reaches correct
definition/callers in fewer tool-call round-trips and fewer total tokens than
grep baseline.

**Generalization gate:** extend to rust-analyzer/pyright only if gopls slice
beats grep.

## Architecture

### New package: `internal/lsp/`

```
internal/lsp/
  manager.go      — Manager: lazy spawn, initialize, capability check, shutdown
  jsonrpc.go      — JSON-RPC 2.0 over stdio (or go-sdk transport per R2)
  protocol.go     — LSP type structs (~7 methods, sourced from gopls internal/protocol)
  resolve.go      — line+symbol→position resolution (R4: middle-of-identifier)
  render.go       — result shaping (clustering, capping, formatting)
  manager_test.go — mock LSP server + golden JSON tests
```

### Config — `internal/config/config.go`

```go
LSPEnabled bool                  `json:"lsp_enabled,omitempty"`
LSPServers map[string]LSPServer  `json:"lsp_servers,omitempty"`
LSPIdleTimeoutSeconds int         `json:"lsp_idle_timeout_seconds,omitempty"` // default 1800 (30 min, L3)
LSPIndexTimeoutSeconds int        `json:"lsp_index_timeout_seconds,omitempty"` // default 30
```

### Executor — `StartInteractive` (R1)

```go
StartInteractive(ctx context.Context, command string) (
    stdin  io.WriteCloser,
    stdout io.ReadCloser,
    stderr io.ReadCloser,
    pid    int,
    err    error,
)
```

### Request serialization (L1)

The Manager owns **per-server write serialization**: one writer, mutex the write
side, route responses by id. Designed for parallel subagents sharing one gopls
connection NOW (per-server request queue), so it's not a retrofit when parallel
dispatch lands. Interleaved frames / id-correlation corruption is the failure
this prevents.

### Tool assembly — `BuildTools` (mcp_manager.go:301)

```go
if cfg.LSPEnabled {
    t = append(t, wtools.LSPTools()...)
}
```

### File-sync hooks — `handleEditFile`, `write_file`, `run_shell` (R3)

After successful `WriteFile`:
```go
if a.LSP != nil { a.LSP.NotifyChange(args.Path) }
```

After non-read-only `run_shell`:
```go
if a.LSP != nil { a.LSP.MarkOpenFilesDirty() }  // lazy: stat on next query
```

## Decisions table

| # | Decision | Chosen | Rejected | Rationale |
|---|---|---|---|---|
| 1 | Tool input schema | **Hybrid: file + line + symbol?** (line-anchored PRIMARY, middle-of-identifier offset) | Pure positional; pure name-based | Line is observable from read_file; documentSymbol excludes locals — line-anchoring covers both; fewer round-trips than name-only |
| 2 | Position encoding | **Negotiate UTF-8**, convert if UTF-16 | Always-UTF-16; ignore | Arabic/CJK makes UTF-16 a correctness hazard |
| 3 | File-sync | **didOpen + didChange + lazy invalidation with mtime guard (R3)** | didChangeWatchedFiles (primary); no sync; eager-all; command-heuristic | Once didOpen'd, server stops reading disk. run_shell bypasses hooks → lazy dirty-flag + mtime stat on next query |
| 4 | Index readiness | **Gate on $/progress** with 30s timeout | No gate; polling | Silent empty = worst failure mode |
| 5 | Capability gating | **Dynamic per-tool from ServerCapabilities** | Static | Not every server supports every operation |
| 6 | Lifecycle | **Lazy spawn, 30-min idle (L3), crash recovery mirroring HandleStreamError, StartInteractive with stderr (R1), R2 go-sdk framing inspection** | StartBackground; eager spawn | StartBackground has no pipes; LSP needs bidirectional stdio + stderr for crash classification |
| 7 | Result shaping | **Cluster by file, cap 5/file + 50 total** | Dump all | Same compaction sensibility as CapOrStub |
| 8 | Failure contract | **Explicit truthful status, never silent empty** | Silent empty; auto-fallback | Fail-closed; model makes explicit choice to use grep |
| 9 | Library | **Hand-rolled** (~500–700 lines, structs from gopls) | go.lsp.dev/protocol; +jsonrpc2 | Library lacks positionEncoding (3.16-era); pulls zap+asm |
| 10 | MVP cut | **gopls only, 4 tools** | Full 6-tool multi-language | Prove value before generalizing; gate on beating grep |

## Frozen baseline (L2)

### The 10 navigation tasks

Each task has a precise success criterion. Tasks 1–5 are in wakil's repo
(ground truth verified by reading source). Tasks 6–10 are in the ZDB repo
(ground truth defined from ZDB's public command structure; the ZDB repo must be
available at eval time — **see OPEN ITEM below**).

#### Wakil repo tasks (ground truth verified)

**Task W1 — Find the definition of `ExecuteToolCall`**
- Task: "Find where `ExecuteToolCall` is defined."
- Success criterion: `internal/agent/app.go:1203` — the `func (a *App)
  ExecuteToolCall(ctx context.Context, tc proxy.ToolCall) string` declaration.
- Grep challenge: 5 matches total (2 in non-test code); agent must distinguish
  the `func` definition from the call site at app.go:922.

**Task W2 — Find all callers of `dispatchSubagent` (production code only)**
- Task: "Find all production (non-test) call sites of `dispatchSubagent`."
- Success criterion: exactly **1 call site** — `internal/agent/app.go:1712`.
- Grep challenge: 35 matches across 9 files; 16 are non-comment/non-def; only 1
  is a real call in production code (the rest are tests, comments, or the func
  definition). Agent must filter test files and comments.

**Task W3 — Find the type of the `Workflow` field on `App`**
- Task: "What type is the `Workflow` field on the `App` struct, and where is
  that type defined?"
- Success criterion: type is `*workflow.WorkflowState`, field at
  `internal/agent/app.go:144`, type defined at
  `internal/workflow/workflow.go:47`.
- Grep challenge: 89 matches for "Workflow" across internal/; 16 in app.go
  alone. Most are `a.Workflow` usage, not the field decl or type def. Two-hop
  navigation (field → type definition).

**Task W4 — Find the definition of `handleMashura` and its signature**
- Task: "Find where `handleMashura` is defined and its full signature."
- Success criterion: `internal/agent/mashura.go:152` —
  `func (a *App) handleMashura(ctx context.Context, name string, tc proxy.ToolCall) string`.
- Grep challenge: 13 matches; agent must distinguish the definition from the 11
  call sites (test files + app.go:1744 dispatch).

**Task W5 — Find the definition of `BuildTools` and what it returns**
- Task: "Find where `BuildTools` is defined and what it returns."
- Success criterion: `internal/agent/mcp_manager.go:301` —
  `func BuildTools(cfg config.Config, cwd string, mcp *MCPManager) []proxy.Tool`.
- Grep challenge: only 3 matches; but agent must find the `func` definition, not
  the call sites in agent_async.go.

#### ZDB repo tasks (ground truth defined; repo required at eval time)

> **OPEN ITEM:** The ZDB repo is not on this host (`/mnt/wakil` is the only
> mount). These 5 tasks are designed from ZDB's command structure (grounding
> docs). At Phase 5 eval, the ZDB repo must be cloned/mounted so the baseline
> can be measured. If unavailable, the eval runs on 5 wakil tasks only and the
> generalization claim is weakened.

**Task Z1 — Find the definition of the ZDB `get` command handler**
- Task: "Find where the `get` command is implemented in ZDB."
- Success criterion: the function handling `zdb get <key>`, printing
  `"Value: <value>"` or `"Key not found."`
- Grep challenge: "get" is a common substring; agent must navigate to the
  command dispatcher and find the specific handler.

**Task Z2 — Find all callers of the snapshot creation function**
- Task: "Find all call sites of the function that creates snapshots (the `snap`
  command handler)."
- Success criterion: all sites that call the snapshot creation path, including
  the CLI handler and any internal replication path.

**Task Z3 — Find the type definition for the CAS operation result**
- Task: "What type represents a CAS (compare-and-swap) result, and where is it
  defined?"
- Success criterion: the struct/type for CAS success/failure, which prints
  `"CAS succeeded"` / `"CAS failed"`.

**Task Z4 — Find the definition of the `scrub` command handler**
- Task: "Find where `scrub` is implemented."
- Success criterion: the function handling `zdb scrub`, printing
  `"[Status] Scanned: <N> | Errors: <N> | Msg: Scrub Completed..."`.

**Task Z5 — Find all callers of the VDEV listing function**
- Task: "Find all call sites of the function that lists VDEVs
  (`admin list-vdevs`)."
- Success criterion: all production call sites, including CLI dispatch and any
  admin/monitoring paths.

### Measured grep baseline (wakil tasks)

Methodology: simulate the agent's grep-only path (search_files + read_file).
For each task, count tool-call round-trips and estimate tokens from output byte
counts (~4 chars/token). The agent must grep, read matches, and disambiguate
(definition vs call site, production vs test).

| Task | Grep round-trips | Read round-trips | Total round-trips | Grep output (bytes) | Read output (bytes) | Est. tokens (~4 ch/tok) | Correct? |
|---|---|---|---|---|---|---|---|
| W1 (def ExecuteToolCall) | 1 | 2 | 3 | 531 | 957 | ~372 | yes |
| W2 (callers dispatchSubagent) | 1 | 3 | 4 | 4223 | ~2000 | ~1556 | yes (with effort) |
| W3 (type of Workflow field) | 2 | 2 | 4 | 925 + ~500 | ~1000 | ~606 | yes |
| W4 (def handleMashura) | 1 | 2 | 3 | 1285 | ~800 | ~521 | yes |
| W5 (def BuildTools) | 1 | 1 | 2 | 339 | ~400 | ~185 | yes |

**Baseline totals (wakil, 5 tasks):** 16 round-trips, ~3,240 tokens estimated.

**Where grep struggles (and LSP should win):**
- **W2 is the hardest for grep:** 35 matches, 9 files, 16 non-comment lines, but
  only 1 production call site. The agent must read multiple files to filter
  tests and comments. LSP `references` returns exactly the 1 call site in one
  round-trip.
- **W3 is a two-hop navigation:** grep finds the field, then a second grep/find
  for the type definition. LSP `hover` or `definition` on the field returns the
  type and its definition location in one round-trip.
- **W1/W4/W5 are definition lookups:** grep returns both definition and call
  sites mixed; agent must read to disambiguate. LSP `definition` returns only
  the definition.

**ZDB baselines:** not measured — ZDB repo not on this host (see OPEN ITEM
above). Will be measured in Phase 5 when the repo is available, or the eval
runs on 5 wakil tasks only.

### LSP target (for comparison in Phase 5)

| Task | LSP tool | Expected round-trips | Expected tokens |
|---|---|---|---|
| W1 | lsp_definition | 1 | ~100 |
| W2 | lsp_references | 1 | ~200 |
| W3 | lsp_hover or lsp_definition | 1 | ~200 |
| W4 | lsp_definition | 1 | ~100 |
| W5 | lsp_definition | 1 | ~100 |

**LSP target totals (wakil, 5 tasks):** 5 round-trips, ~700 tokens estimated.

**Win condition:** LSP wins if median round-trips and median tokens are both
lower than grep, with no correctness regressions. Predicted: 5 vs 16 round-trips,
~700 vs ~3,240 tokens.

## Implementation phases

### Phase 0 — Doc lock + frozen baseline (THIS PHASE)
- Fold R1–R4 + L1–L3 into v3 ✓
- 10 frozen tasks with success criteria ✓
- Measured grep baseline (wakil) ✓
- Commit: "docs(lsp): lock proposal v3 — resolutions, serialization, frozen baseline"
- **>>> HARD STOP.** Wait for Valon's approval before writing any implementation code.

### Phase 1 — Transport + protocol types (after approval)
- 30-min go-sdk inspection per R2.
- `jsonrpc.go` (framing + id-correlated read loop), `protocol.go` (~7 method
  shapes sourced from gopls `internal/protocol`), `StartInteractive` on the
  Executor interface (Direct + Docker, -i flag, stderr, shutdown contract per R1).
- **mashūra VERIFY gate (full panel):** framing correctness; high-hazard struct
  shapes (WorkspaceEdit, Hover.contents union, definition union, references
  includeDeclaration, documentSymbol hierarchical vs flat); nil-slice→null;
  server→client requests answered. Fold objections, re-verify if structural.
- Commit: "feat(lsp): json-rpc stdio transport + protocol types + StartInteractive"

### Phase 2 — Manager + lifecycle + serialization (L1)
- **mashūra PLAN gate first:** lifecycle state machine, serialization model,
  crash classification mapping to HandleStreamError, positionEncoding
  negotiation, $/progress readiness gate. Then implement.
- Lazy spawn, initialize, capability read, UTF-8 negotiation + UTF-16 fallback,
  index-readiness gate, idle timeout (L3: 30 min), crash recovery, per-server
  request serialization (L1).
- **mashūra VERIFY gate** before commit.
- Commit: "feat(lsp): server manager — lifecycle, serialization, readiness, encoding"

### Phase 3 — Resolution + file-sync (HIGHEST bug density)
- `resolve.go` (R4: line-anchored primary, middle-of-identifier offset,
  documentSymbol fallback, multi-match candidate list), file-sync hooks
  (didOpen on first touch, didChange at handleEditFile + write_file), run_shell
  lazy invalidation + mtime guard + didChangeWatchedFiles batch (R3), Docker URI
  translation (container path, not host) with explicit assertion/test.
- **mashūra VERIFY gate (full panel)** — silent false-negatives hide here: URI
  translation, dirty-flag/mtime guard correctness, didOpen→didChange obligation.
- Commit: "feat(lsp): name+line resolution, file-sync hooks, run_shell resync"

### Phase 4 — Tools + rendering + wiring
- 4 tools (def/refs/hover/symbols), `render.go` (cluster-by-file, cap 5/file +
  50 total, hover strip-to-plain, tool-line summary), ExecuteToolCall dispatch,
  BuildTools capability+config gating, config schema, failure-contract strings
  (Decision 8), context preamble, Dockerfile gopls install.
- **mashūra VERIFY gate** before commit (failure-contract completeness; gating
  correctness).
- Commit: "feat(lsp): 4 MVP tools, result shaping, dispatch + config wiring"

### Phase 5 — Eval against frozen baseline
- Run the 10 frozen tasks with LSP tools; record round-trips + tokens per task;
  compare to L2 baseline.
- **mashūra gate:** interpret results HONESTLY — no overclaiming, report
  regressions and ties, state per-task deltas. Fail-closed on any task where
  LSP did worse.
- Commit: "test(lsp): MVP eval vs grep baseline — <summary>"
- **>>> STOP.** Present comparison. Generalization gate is Valon's to call.
