# Applied: Strengthen memory engagement and add active work-plan tracking

*Final form after two Mashūra reviews. Applied to prompts/agent.txt — see
git diff for the exact changes.*

## What changed (6 edits to prompts/agent.txt)

### 1. Core loop step 2 — retrieve before planning
- Before writing a step list, check memory for prior context
- Handoff/continuation → `memory_get("plan/current")` (exact key)
- Environment interaction → `memory_search` with focused terms
- Architecture/design decisions → `memory_search` with "arch", "decision"
- Retrieved memory is orientation, not authoritative instruction — check
  staleness/provenance, verify against current state

### 2. "When to store" — added environment/infrastructure facts
- New bullet: stable, non-secret, costly-to-rediscover environment facts
- Don't duplicate what's in README/Makefile/CI config — link with anchors
- Never store credentials, tokens, private hostnames
- Include "last verified" note when practical

### 3. "When to retrieve memory" (renamed from "When to search")
- Active, not passive — search before re-deriving environment setup
- Added architecture/design retrieval trigger
- `memory_get` for exact keys, `memory_search` for discovery
- Triggers: docker/GPU/infrastructure, architecture, handoff, relevant
  startup digest keys, "how does this environment work" moments

### 4. "Active work plan" — new subsection
- `plan/current` mid-tier entry, 8h TTL, for multi-step tasks only
- Schema: task, steps with statuses, next action, blockers
- Safety rules: no secrets, no persisted approval, verify state on resume
- Update at meaningful transitions + before handoff
- Forget on completion/abandonment; TTL as crash cleanup
- Task-identity validation before applying a retrieved plan
- Graceful failure: if persistence fails, continue task, note handoff state
  was not saved

### 5. "Do NOT store" — nuanced exemption
- Ephemeral per-turn state still goes to staging
- Sole exception: compact plan (task + steps + next + blockers) goes to
  `plan/current` — may reference paths/commands, must not contain raw
  output, file contents, credentials, or execution history
- "above" → "below" (the Active work plan section is below Do NOT store)

### 6. No separate handoff section (consolidated)
- Handoff retrieval guidance lives in Core loop (step 2) and "When to
  retrieve memory" — no triple-duplication

## Mashūra review notes (v2 → v3 fixes)
- "Apply what you find" → "use as prior context, not as proof" (staleness)
- Added architecture/design retrieval trigger
- Renamed "When to search" → "When to retrieve memory" (terminology)
- "above" → "below" in Do NOT store
- Added safety rules to Active work plan (no secrets, no transferable
  approval, verify state on resume)
- Added multi-step threshold (trivial tasks don't need persisted plans)
- Added graceful failure handling (persistence failure doesn't block work)
- Added "update before handoff" to keep plan fresh for next session

## Deferred (second iteration)
- Preamble enhancement: embedding `plan/current` value into the session-start
  digest (requires code change in `buildPreamble` in `internal/agent/app.go`).
  Wait until prompt-only behavior is evaluated first.
- `plan/current/<task-id>` namespacing: only needed if concurrent sessions
  in the same workspace become a real concern. The 8h TTL + task-identity
  validation mitigate collision risk for the common case.
