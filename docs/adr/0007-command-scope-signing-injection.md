# 0007 — Signing settings injected at command scope

- **Status:** Accepted
- **Date:** 2026-11-07
- **Origin:** docs/CLOSEOUT-0.5.0.md F9 (removed); commit 14c386e.

## Context

The session gitconfig carried the signing identity. A repo-local `.git/config`
with its own `user.signingkey` beats global scope, so it could shadow the
session's key. A repo-local `gpg.ssh.program` could point at a wrapper that
signs with a different key.

## Decision

Signing settings are injected at command scope on every exec, through
`GIT_CONFIG_COUNT` / `GIT_CONFIG_KEY_n` / `GIT_CONFIG_VALUE_n`. Command scope
beats local and global scope. The injected keys are `user.signingkey`,
`gpg.format`, `gpg.ssh.allowedSignersFile`, `gpg.ssh.program` (pinned to
`ssh-keygen`), `commit.gpgsign`, and `tag.gpgsign`. The `GIT_CONFIG_*` names are
added to `env_pass` when signing is on.

For git older than 2.31, which does not support `GIT_CONFIG_COUNT`, the same
settings also stay in the session gitconfig as a fallback. On git 2.31 and
newer, the command-scope values win.

A repo-local signing key is reported in the startup banner.

## Consequences

- A repo-local `.git/config` cannot change the signing choice for agent commands.
- Known, non-blocking gaps recorded in CLOSEOUT-0.5.0: `validateExecEnv`
  accepts any `GIT_CONFIG_KEY_*` value (wider than needed; internal callers
  only), and the pair count is hard-coded in `signingEnvPass()`.
