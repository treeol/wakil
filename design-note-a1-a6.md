# Design note: A.1 (tail cap default + notice format) and A.6 (taint guard + /taint)

## A.1 — Tool timeout/cancel returns LAST N KB (default 16 KB, configurable)

### Current state
**Scope:** the cap applies to ALL shell output (every `runShellCapped`
call), not only timeout/cancel paths. The heading is the task item label;
the change is to the universal default. A successful `cat largefile` or
`go test ./...` also returns only the last 16 KB — this is intended, and
`ShellOutputCap` remains configurable for users who need more.
- `DefaultShellOutputCap = 1 << 20` (1 MB) — exec.go:42
- `boundedTailBuffer` already retains the TAIL (correct direction)
- Notice format: `"[... first %d bytes of output omitted (showing last %d) ...]\n"`
  (exec.go:1221, backend_executor.go:354) — says "first %d bytes omitted" which
  is misleading: it reads as if the FIRST bytes are shown.
- `ShellOutputCap` config key already exists (DockerExecutor.exec.go:202,
  BackendExecutor.backend_executor.go:27); `<= 0` means default.

### Changes
1. **Default**: change `DefaultShellOutputCap` from `1 << 20` to `16 << 10`
   (16384 bytes = 16 KB). Both backends share this constant (docker path:
   exec.go:1231; hisn/backend path: backend_executor.go via `capOr`).
2. **Notice format**: replace the two format sites with:
   `"[truncated, %d bytes total — showing last %d]\n"`
   where `%d` (total) = `omitted + len(out)` and `len(out)` = retained tail.
   This makes it unambiguous: the line says "truncated" (not "omitted"),
   gives the total output size, and clarifies the tail is shown.
3. **Config**: no new key needed — `ShellOutputCap` already exists. Update
   the doc comment to say "default 16 KB" instead of implying 1 MB.
   **Configurability**: `ShellOutputCap` is set from the config file key
   `shell_output_cap` on both backends. Grep `ShellOutputCap =` in
   `internal/exec/` to confirm the assignment path before implementing.

### Why 16 KB
- The task asked for 16 KB as the default — this is the stated requirement,
  not a "16 KB is enough" claim. O(cap) memory was already true at 1 MB.
- 16 KB is configurable via `ShellOutputCap` for users who need more
  (large test outputs, full compiler error batches, Go panic traces).

### What does NOT change
- `boundedTailBuffer` tail-retention semantics (already correct).
- `WaitDelay` (5 s) — unrelated to the cap.
- `runShellCapped` signature — callers compute total from `omitted + len(out)`.

### Tests (A.1)
1. Notice emitted ONLY when overflow occurs (no notice when output < cap).
2. `total == omitted + len(out)` in the notice string.
3. Both backends produce the identical notice string for the same input.
4. Existing tests asserting the old 1 MB default or the old notice format
   are updated.
5. `omitted` counter: confirm it tracks bytes DISCARDED (not total written)
   so the total formula doesn't double-count. Check `boundedTailBuffer` in
   `internal/exec/bounded_capture.go` before implementing.

---

## A.6 — Memory-taint guard + /taint command

### Current state
- `touchedExternal` (atomic.Bool, app.go:208) is set by:
  - `addExternalGrounding` when type == "web" or "oracle" (memory_tools.go:460)
  - AGENTS.md loading (app.go:1650)
  - async ops delivery (async_ops.go:1128, 1182)
- `computeTainted()` (memory_tools.go:477) returns `TaintTrue` if
  `touchedExternal.Load()`, else `TaintUnknown`.
- Ordinary tool output (ReadFile, RunShell) does NOT call
  `addExternalGrounding` and does NOT set `touchedExternal` — the guard
  already works correctly for the workspace case.
- No `/taint` command exists. No taint-reason tracking exists (just a bool).

### Design

#### Taint reason tracking
Add a mutex-protected slice alongside `touchedExternal`:

```go
taintMu     sync.Mutex
taintReasons []taintReason
type taintReason struct {
    Source string // "web", "oracle", "agentsmd"
    Label  string // URL, model name, or "AGENTS.md"
    When   time.Time
}
```

`addExternalGrounding` appends a reason when it latches `touchedExternal`
(first time only — deduplicate so the list shows distinct exposures, not
every call). The AGENTS.md path (app.go:1650) also records a reason.

**Dedup key**: `(Source, Label)` — one entry per distinct source+label
pair. The `taintReasons` slice is bounded at `maxTaintReasons` (e.g. 64);
on overflow, the oldest entry is dropped (the list is diagnostic, not
authoritative). Ordered by `When` ascending (oldest first).

The sticky `touchedExternal` bool stays as-is — the reasons are purely
diagnostic (for `/taint`), not used by `computeTainted`.

#### Async-ops delivery
The async-ops path (async_ops.go:1128/1182) also sets `touchedExternal`.
Reasons MUST be recorded there too — otherwise a session tainted via async
delivery shows `taint: tainted (0 exposures)` (bool and reasons disagree).
The async delivery path appends a reason with Source="async", Label=<op-id>
or "async-delivery".

#### /taint command
New slash command in commands.go (after /backend, before /model):
- No arguments. Any argument is rejected with "usage: /taint" (no subcommands).
- Output format (tainted):
  ```
  taint: tainted (3 exposures)
    • web: https://example.com — 2026-11-03 14:22
    • oracle: claude-opus-4 — 2026-11-03 14:25
    • agentsmd: AGENTS.md — 2026-11-03 14:20
  ```
- Non-tainted output: `taint: no known external exposure (0 exposures)`
  (NOT "not tainted" — `computeTainted` returns `TaintUnknown`, not
  `TaintFalse`; the command must not assert a state the system doesn't track).

#### AGENTS.md question
AGENTS.md is workspace content but is loaded into the system prompt (not
ordinary tool output). Current code sets `touchedExternal` for it
(app.go:1650).

**Decision: keep AGENTS.md tainting.** Rationale:
- AGENTS.md is advisory system-prompt content that could contain adversarial
  instructions targeting memory writes. The taint signal ensures any
  memory writes influenced by AGENTS.md content are quarantined.
- The user's spec says "file reads, shell output from the workspace" —
  AGENTS.md is neither; it's system-prompt injection, not a tool result.
- The guard the user wants ("ordinary tool output must not taint") already
  works: ReadFile and RunShell never set `touchedExternal`.
- Removing AGENTS.md tainting would be a safety regression with no benefit.

**Consequence acknowledged:** in repos with an AGENTS.md, every session is
tainted at startup before the user types anything → all memory writes are
permanently quarantined (proposed, not active). This is the intended
trade-off: the taint signal errs on the side of quarantine. The memory
system still works (entries are stored as proposed and can be promoted
manually); only automatic mid-tier active writes are blocked. The task
author accepts this — AGENTS.md is system-prompt content, not workspace
tool output, and the taint guard's purpose is exactly this.

**Consistency note:** `ReadFile` on AGENTS.md does NOT taint (it's a
file-read tool result), but loading AGENTS.md into the system prompt at
startup DOES taint (it's prompt injection, not a tool result). This is
correct — the two paths are different exposure surfaces.

#### Tests
1. **Ordinary tool output does not taint**: call the tool-result handling
   path directly (not via Docker — must be a unit test). Assert
   `computeTainted() == TaintUnknown` and `touchedExternal.Load() == false`
   and `len(taintReasons) == 0`.
2. **External content taints**: call `addExternalGrounding` with a web
   entry, assert `computeTainted() == TaintTrue` and reason is recorded.
   Use an injectable clock for deterministic timestamps.
3. **Async delivery taints**: call the async delivery path, assert a reason
   with Source="async" is recorded and `computeTainted() == TaintTrue`.
4. **/taint command**: verify output format for both tainted and
   non-tainted states. Non-tainted says "no known external exposure"
   (not "not tainted").
5. **Reset consistency**: if `touchedExternal` is ever cleared
   (grep `touchedExternal.Store(false)`), `taintReasons` is cleared in the
   same place.

### What does NOT change
- `computeTainted()` return values (`TaintTrue` / `TaintUnknown`).
- The mid-tier write gate (subagent or tainted → quarantined).
- `ResetGrounding` behavior (clears per-turn grounding, not sticky flag).
- AGENTS.md still sets `touchedExternal`.