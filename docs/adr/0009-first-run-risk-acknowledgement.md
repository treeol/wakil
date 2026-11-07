# 0009 — First-run risk acknowledgement

- **Status:** Accepted
- **Date:** 2026-11-07
- **Origin:** CHANGELOG 0.5.2 (first-run acknowledgement); tag v0.5.2.

## Context

A project's `.wakil/sandbox.json` records a `risk_ack` value for the hisn sandbox
labels that the project grants. Writing that JSON by hand was error-prone, and
the first-run path had no clear prompt.

## Decision

- `wakil sandbox trust`, with no `.wakil/sandbox.json`, shows the capability
  labels and the resulting risk. It writes `{"hisn":{"risk_ack":"<risk>"}}` only
  after an explicit `yes`. Any other answer deletes the file it wrote.
- A bootstrap-form file whose ack no longer matches is updated the same way.
- A hand-written file is never rewritten. The command names the key to change.
- In the interactive TUI, a missing or mismatched ack is offered inline
  (`acknowledge now? [y/N]`). Non-interactive runs (`wakil run`, `--print-*`,
  scheduled) never prompt and keep the hard error.
- Home-persist under Wakil's own root is self-acknowledged.
- No ack is written without an explicit `yes` or `y` in that invocation.

## Consequences

- The acknowledgement is an explicit act by the user, recorded in the project
  file.
- Fail-closed behaviour (ADR 0005) is unchanged for unacknowledged projects.
