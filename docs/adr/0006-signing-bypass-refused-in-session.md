# 0006 — Signing cannot be bypassed from inside an enforced session

- **Status:** Accepted
- **Date:** 2026-11-07
- **Origin:** docs/CLOSEOUT-0.5.0.md F5/F6 (removed); commits 2249d4a, 480ed65, 826b7ca.

## Context

In a signing-enforced session (the sandbox profile carries signing), an agent
or a user command could disable signing with `git -c commit.gpgsign=false`,
`--no-gpg-sign`, or a config write, so that an unsigned commit was made without
any notice. Review found several gaps: `run_background` skipped the guard, git
config keys are case-insensitive, `--null` and similar output modifiers could
hide a write, and `--git-dir` and `--work-tree` forms were not walked back.

## Decision

In a signing-enforced session, the shell tool and every git tool refuse commands
that would disable or bypass signing. The guard is applied to `run_shell`,
`run_background`, and the git tools. Config keys are compared case-insensitively.
Reads of signing config are allowed. `--no-gpg-sign` is refused only on git
commands. A prompt rule backs the tool guard.

Out of scope for a direct-command guard: shell quoting and indirection, `env -u`,
script files, `git commit-tree`, and `fast-import`. The audit trail (an unsigned
commit that a host `git log` can show) catches these.

## Consequences

- The guard is a control against accidental and prompted bypass, not a
  complete sandbox against a deliberately hostile script.
- Open, recorded in CLOSEOUT-0.5.0: `git config set/unset/edit` subcommand
  syntax (git ≥ 2.46) is not handled.
