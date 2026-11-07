# 0000 — ADR format and convention

- **Status:** Accepted
- **Date:** 2026-11-07
- **Origin:** Convention chosen in the 0.6.0-dev cleanup (no prior ADR existed).

## Context

Wakil's design history was kept in per-release plan and close-out documents
(`docs/PLAN-*.md`, `docs/CLOSEOUT-*.md`, decision sheets). Those describe how
the work was done, go stale, and are hard to find for a decision. Decisions
that still matter need a stable home that describes the decision, not the
session that made it.

## Decision

- Architecture decisions live in `docs/adr/`, one Markdown file per decision,
  named `NNNN-short-slug.md`. Numbers are never reused; a superseded ADR is
  kept and marked `Superseded by NNNN`.
- Each ADR has: title, `Status` (Proposed / Accepted / Superseded / Deprecated),
  `Date`, `Context`, `Decision`, and `Consequences`. Evidence pointers
  (commit hashes, file paths, CHANGELOG entries) go in Context.
- `docs/adr/README.md` is the index. Add a row when you add an ADR.
- Process records (plans, close-outs, decision sheets, cleanup logs, session
  narratives) are not kept in `docs/`. Any decision they contain is folded into
  an ADR first, then the record is removed. Git history keeps the original.
- Do not add "deprecated" stubs for removed code. Record the removal in an ADR
  and in the CHANGELOG.

## Consequences

- Contributors find decisions by number or the index, not by release.
- A decision change is a new ADR that supersedes the old one, so the history
  of the decision stays visible.
