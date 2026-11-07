# 0011 — Cleanup-pass invariants (A1–A3) and what was found

- **Status:** Accepted
- **Date:** 2026-11-07
- **Origin:** design-note-a1-a6.md (removed in this cleanup) and the A1–A3 items of
  docs/CLEANUP-2026-11.md (removed). The note's sections are A.1 and A.6; A2 and
  A3 are in the cleanup plan.

## Context

The cleanup plan set three invariants for store identity and path containment.
This ADR records each one with its status as found in source at a18447e, so the
removed records are not the only description.

## Decision

Keep these as the invariants the code must hold:

- **A1 — One key source for the workspace store.** Intended: one resolver gives
  both the chat store key and `plan gc` the same identity. **Status: not met.**
  `ResolveProject` exists only in `cmd/wakil/plan_gc.go:201`. Chat resolves
  through `WorkspacePath()` (see ADR 0004). Tracked in ADR 0010 item 9 and ADR 0004.
- **A2 — One containment check.** Intended: every path-containment comparison uses
  one helper. **Status: partial.** `internal/exec/exec_ops.go:170`
  (`isInsideWorkspace`) delegates to `sandbox.WithinLexical`, so there is a shared
  helper, but call sites that compare unresolved roots are not all audited. The
  B1 item in ADR 0010 covers that work.
- **A3 — Open does not create.** Intended: opening a missing store returns an error and
  creates nothing. **Status: met for `memory`.** `memory.OpenExisting`
  (`internal/memory/store.go:205`) returns before `MkdirAll` and before `sql.Open`.
  The `sessionhistory` package has no `OpenExisting`; the invariant does not
  cover it yet.

Also kept from the A.1 and A.6 design note, as built:

- **A.1 (timeout output).** When a shell command times out or is cancelled, the
  result is the last `TimeoutOutputTail` bytes (`internal/exec/exec.go:44`).
  Normal output keeps the `DefaultShellOutputCap` of 1 MB.
- **A.6 (taint).** `computeTainted` (`internal/agent/memory_tools.go:602`) marks
  a session tainted when external grounding was added. `/taint` exists
  (`internal/agent/commands.go:851`). The note's "no `/taint` command exists"
  line is out of date.

## Consequences

- A1 is an open invariant, not a finished one. Any change to store keys is a
  migration decision (ADR 0004), so A1 is not finished in this pass.
- A3 is enforced for memory only. A `sessionhistory` equivalent is a follow-up.
