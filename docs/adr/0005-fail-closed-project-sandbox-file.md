# 0005 — Project sandbox file fails closed

- **Status:** Accepted
- **Date:** 2026-11-07
- **Origin:** docs/CLOSEOUT-0.5.0.md F1 (removed in this cleanup); commit 80fdaa2.

## Context

`.wakil/sandbox.json` configures the hisn sandbox for a project. Before 0.5.0,
a broken or unapproved file could fall through to the Docker executor or to a
browser-less start, which let a project-controlled file silently weaken the
sandbox.

## Decision

A broken or unapproved `.wakil/sandbox.json` is a startup error when hisn would
be chosen. There is no Docker fallback and no browser fallback. `ResolveSandbox`
returns `(Config, error)`. A workspace that contains the state directory is a
fatal error, not a Docker fallback.

Scope, as stated in the F1 commit: the guarantee applies when hisn is usable and
the backend is defaulted. Parsing errors and unapproved files both fail closed.

## Consequences

- Users see a startup error naming the file. They fix or approve it with
  `wakil sandbox trust`.
- The fallback is not silent. Non-interactive runs keep the hard error.
