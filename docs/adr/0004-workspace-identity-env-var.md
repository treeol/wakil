# 0004 — `WAKIL_WORKSPACE_PATH` and workspace store identity

- **Status:** Accepted
- **Date:** 2026-11-07
- **Origin:** docs/DECISIONS-2026-11.md §C4 (removed in this cleanup); CHANGELOG 0.4.3.

## Context

`WAKIL_WORKSPACE_PATH` overrides the workspace path that determines the store
identity (`internal/config/config.go`, `WorkspacePath()`). Containers set it so
that a container and its host agree on the same key (`docker-compose.yml`).
With the variable unset, chat and `plan gc` resolve the same store for the same
directory, including a symlinked cwd. This is locked by
`TestResolveProjectMatchesChatForCwd`. With the variable set to another
directory, they differ.

Harm observed: on the user's host the variable pointed `plan gc` at another
workspace's store until 0.4.3 made `plan gc` ignore it. Frozen store keys
(for example `/mnt/wakil` → `f3fa3243c6fc07e5`) must not change without a
migration. `docker-compose.yml` also documents a `--print-workspace-key` flag
that no Go code implements. (Corrected 2026-11-08: `docker-compose.yml` no longer
exists, and `--print-workspace-key` is implemented in `cmd/wakil/main.go`; see
`docs/configuration.md`.)

Options considered: (1) keep both sources; (2) make `plan gc` honour the
variable (rejected: it reintroduces the 0.4.3 failure); (3) use cwd identity
only (a behaviour release that orphans stores of users who set the variable);
(4) move identity to `WAKIL_WORKSPACE_KEY`, which already exists, and keep the
path variable for file operations only.

## Decision

Option 4, staged. First, add a startup warning when `WAKIL_WORKSPACE_PATH` is
set and differs from the cwd identity. Then deprecate it for identity in favour
of `WAKIL_WORKSPACE_KEY`. Do not do option 3 without a store migration. Remove
or correct the `--print-workspace-key` reference in `docker-compose.yml`.
(Corrected 2026-11-08: that file is gone, so the reference no longer exists.)

The staged warning is not part of the 0.6.0-dev cleanup. The cleanup changes no
identity behaviour.

## Consequences

- Store keys do not change in this step. No migration is needed for option 4
  when the key is set once.
- Open point: no file-operation-only reader of `WAKIL_WORKSPACE_PATH` exists
  today. Option 4 needs one, or the compose claim about file operations must be
  dropped.
- Open point: `TestResolveProjectGoldenKeys` hard-codes `/mnt/wakil`, so it
  depends on the environment.
