# 0008 — Per-repository signing-key resolution

- **Status:** Accepted
- **Date:** 2026-11-07
- **Origin:** CHANGELOG 0.5.1 (per-repo signing resolution); commit bfd7175.

## Context

In 0.5.0 the signing key had to be listed in `sandbox.hisn.ssh_keys`, and it was
resolved once, at global scope. A repository that set its own `user.signingkey`
in `.git/config` was therefore either rejected or resolved to the wrong key.

## Decision

With `ssh_signing = auto` under `sandbox.hisn`, the key is resolved per
repository on the host, at plan time, by reading the repo's effective git config
with the repo as the working directory. A repo-local `.git/config` wins over
global config, as it does for a host commit.

- `commit.gpgsign` not true: signing is off for the session. Nothing is
  injected and no allowed-signers file is written.
- `commit.gpgsign` true: the repo's `user.signingkey` is resolved (a `key::`
  literal, a `.pub` path, or a private path with a `.pub` sibling; relative
  paths resolve against the repo). Its fingerprint must be loadable by the host
  ssh-agent. `ssh-add` is attempted once when a private path is known.
- The plan fails loudly, naming the repo, the key, and the fingerprint, when the
  key is unset or cannot be loaded. A failing git invocation also fails the plan.
  An unsigned start is never silent.

The resolved key is injected at command scope (ADR 0007). `ssh_keys` is now only
the agent-forwarding allowlist. The resolved signing fingerprint is appended to
it automatically.

## Consequences

- Repositories that do not sign start unsigned instead of demanding a global key.
- A signing repository needs `sandbox.hisn.ssh_agent = true`, as before.
