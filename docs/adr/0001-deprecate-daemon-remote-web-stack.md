# 0001 — Remove the daemon, remote, and web stack

- **Status:** Accepted
- **Date:** 2026-11-07
- **Origin:** docs/DECISIONS-2026-11.md §C1 (removed in this cleanup).

## Context

Wakil had a Connect-RPC daemon (`wakil daemon`), a remote facade that let the
TUI attach to it, token and OIDC auth, and a web UI. Usage evidence from the
local store (about 2,900 sessions in `~/.local/share/wakil`, read from copies
of the databases):

- every session was created by the local user; 0 API tokens; 0 web sessions;
  1 join token; no backends, workspaces, or agents rows;
- the only multi-message histories came from repeated test fixture texts.

The daemon and remote code was roughly 10,600 non-test lines plus about 13,000
test lines, a 13,800-line generated `api/gen`, and a `web/` UI. The local TUI
path does not depend on it. The session host and staging packages are shared
with the local path and are not part of this decision.

## Decision

Remove the daemon/remote/web transport and its auth in one commit:
`internal/remote`, `internal/server/connect`, the auth packages that only
served them, `api/` (proto and generated code), `web/`, the `wakil daemon`,
`serve`, `remote`, and `web` commands and their config keys, and the
Dockerfile and compose services that existed only for the daemon.

Where a type is shared with the local path (`sessionhost`, `staging`, `auth`
pieces used locally), the type stays and only the transport is deleted.

## Consequences

- Users who ran `wakil daemon` or a remote attach lose that path. No stored
  data is migrated, because there were no tokens to migrate.
- The protobuf/buf CI steps and the daemon Docker image go with it.
- Reverting is one commit per removal step.
