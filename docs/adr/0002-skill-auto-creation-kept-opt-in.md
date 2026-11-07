# 0002 — Skill auto-creation is kept; suggestions are off by default

- **Status:** Accepted
- **Date:** 2026-11-07
- **Origin:** docs/DECISIONS-2026-11.md §C2, and the scope decision made
  during the 0.6.0-dev cleanup pass (the removal originally planned for that
  pass was dropped).

## Context

Skill auto-creation has a mode (`skill_creation.mode`), a draft pipeline
(`skill_creation*.go`, `skill_secret_screen.go`), and an end-of-turn suggestion
(`skill_suggestion.go`, at most two per session). In earlier releases the
suggestion was appended to tool results; that injection was removed in 0.4.4.

The November review of the feature found:

- the local store held no skills created through this path;
- the suggestion fires on the length of a tool sequence, not on any sign that
  the work is reusable, so it is noisy;
- the pipeline itself was judged sound enough to keep, and the owner wants the
  capability available.

## Decision

Keep the feature as it is: the pipeline, the `skill_creation.*` config keys,
the per-repo and per-session `/skill-creation` modes, and every `/skills`
subcommand.

The default is unchanged: `skill_creation.mode` is `off`, and suggestions only
appear in mode `on`. A user opts in with `/skill-creation on` (for the repo, or
`session` for one session) or with the config key. The 0.4.4 changelog once
listed the feature under "Deprecated"; a dated correction in that entry withdraws
it (2026-11-07). The feature is kept.

Stored auto-created skills keep their existing gating (unconfirmed skills are
listed but not injected or loaded until `/skills confirm`). That gating is part
of the feature, not a separate removal.

## Consequences

- No behaviour change in this pass: the feature, its defaults and its tests
  stay as they were.
- No model-visible text is injected into tool output by skill logic (removed in
  0.4.4).
- Users who want a skill without the pipeline use `/skill` or `save_skill`.
- Revisit only if the review data changes; a future removal would need its own
  ADR and must still record this decision.
