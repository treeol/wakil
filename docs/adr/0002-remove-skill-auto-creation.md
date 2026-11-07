# 0002 — Remove skill auto-creation; keep skill store and templates

- **Status:** Accepted
- **Date:** 2026-11-07
- **Origin:** docs/DECISIONS-2026-11.md §C2 (removed in this cleanup).

## Context

Skill auto-creation had a mode (`skill_creation.mode`, default `on`), a draft
pipeline, and a suggestion hint appended to tool results. The hint read
`[app note — not from the user: …]` and asked the agent to draft a skill from
the current workflow (`internal/agent/skill_suggestion.go`, max two per
session).

Evidence:

- the local store held 0 skills created through this path;
- the hint appeared three times inside ordinary tool output during the cleanup
  work, where it is indistinguishable by position from tool or fetched content;
- it fires on the length of a tool sequence, not on any sign that the work is
  reusable.

Two earlier changes already removed the tool-result injection and turned
suggestions off, but the pipeline code and the config keys remained.

## Decision

Remove the auto-creation pipeline: `skill_creation*.go`, `skill_suggestion.go`,
`skill_create.go`, `skill_secret_screen.go`, the `SkillCreation` repo-state
field (old state files are read and the field is ignored), the three app-note
injection points, the `skill_creation.*` config keys, the `/skill-creation`
command, and the prompt text that describes it.

Keep: the skill store and its tools (`list`, `load`, `search`, `history`),
user-authored skills, the `/skill` and `/skills` commands, and the
`apply_skill_template` / `capture_skill_template` feature. Templates are
explicit tool calls and do not depend on auto-creation.

## Consequences

- No model-visible injected text in tool output from skill logic.
- Users who want a new skill write it with `/skill` or `save_skill` through the
  user-driven path.
- Stored skills with template sections stay readable as plain skills.
