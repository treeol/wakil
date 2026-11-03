# Design note: A.1 (timeout tail + notice format) and A.6 (taint guard + /taint)

## A.1 — Tool timeout/cancel returns the LAST 16 KB (normal cap unchanged at 1 MB)

### Scope
- **Timeout/cancel only.** When a tool call's context ends and the command
  did not finish normally, the result is the **last 16 KB** of its output
  (`exec.TimeoutOutputTail`) with a truncation notice.
- **Normal output is unchanged.** `DefaultShellOutputCap` stays `1 << 20`
  (1 MB); `ShellOutputCap` still overrides it per executor, and the agent
  layer's `tool_result_cap` still applies on top. A completed command's
  output is never cut to 16 KB: a 200 KB `cat` or `go test ./...` returns
  whole.

### Current state
- `boundedTailBuffer` retains the TAIL up to the cap (correct direction).
- The old notice `[... first N bytes of output omitted (showing last M) ...]`
  read as if the FIRST bytes were shown.

### Behaviour
1. **Timeout tail.** `shellResult(out, omitted, timedOut)` (exec.go) is the
   one place all four `RunShell` implementations (docker, direct,
   docker-worktree, backend) render their result. `timedOut` is
   `ctx.Err() != nil && err != nil`: the context ended AND the command
   failed. When set, the retained tail is cut to the last 16 KB, on a rune
   boundary.
2. **Notice format.** `[truncated, N bytes total — showing last M]`, where
   `M` is what is shown and `N = bytes dropped before it + M`. Emitted only
   when bytes were dropped: by the cap (normal path), by the timeout cut, or
   both.
3. **Config.** No new key. `ShellOutputCap` keeps its meaning; `<= 0` means
   the 1 MB default.

### What does NOT change
- `boundedTailBuffer` tail-retention semantics.
- `WaitDelay` (5 s) and `runShellCapped`'s signature.

### Tests (A.1) — `internal/exec/shell_cap_test.go`
1. `shellResult` table: no notice when nothing was dropped; a large normal
   result is not cut; a capped normal result keeps its cap notice; a timed-out
   result is cut to 16 KB; earlier omissions are added to the total.
2. The 16 KB cut never starts mid-rune.
3. Direct and backend executors: 200 KB of normal output passes through
   untruncated with no notice; a flood cut off by a context deadline returns
   exactly 16 KB with the notice; an explicit small cap still applies to
   normal output.

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