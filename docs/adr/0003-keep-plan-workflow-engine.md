# 0003 — Keep the `/plan` workflow engine

- **Status:** Accepted
- **Date:** 2026-11-07
- **Origin:** docs/DECISIONS-2026-11.md §C3 (removed in this cleanup).

## Context

`/plan` is a built-in engine with state, gated steps, an oracle review, a
verify step, and `/plan approve`. Skills such as `task-workflow` describe a
similar procedure in prose. Usage evidence on the local host was thin: 0
`/plan` or `/workflow` input lines, and the workflow event kinds that appeared
came from test fixture texts. The `plan gc` command exists because `plan/current`
memory entries were written by the model following a skill in 0.4.2–0.4.3.

## Decision

Keep the `/plan` engine (`internal/agent/workflow_engine.go`,
`internal/workflow`). Do not replace it with a skill: a skill cannot enforce the
oracle and verify gates. Keep `plan gc` while old `plan/current` entries may
still exist in user stores. Revisit when there is real usage data.

## Consequences

- The engine stays on the turn path (`hostturn.go`) and keeps its event kinds.
- If the engine is removed later, `plan gc` goes only after 0.4.x
  `plan/current` entries are gone from user stores. Removing it earlier would
  leave orphaned entries that accumulate silently.
