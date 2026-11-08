# Architecture decision records

Decisions that still apply to Wakil. Each file describes one decision, its
context and its consequences. The convention is in
[0000](0000-adr-format.md): Status, Date, Origin, Context, Decision, Consequences. Superseded ADRs stay in place and are marked as
such.

| No. | Title | Status |
|---|---|---|
| [0000](0000-adr-format.md) | ADR format and convention | Accepted |
| [0001](0001-deprecate-daemon-remote-web-stack.md) | Remove the daemon, remote, and web stack | Accepted |
| [0002](0002-skill-auto-creation-kept-opt-in.md) | Skill auto-creation is kept; suggestions are off by default | Accepted |
| [0003](0003-keep-plan-workflow-engine.md) | Keep the `/plan` workflow engine | Accepted |
| [0004](0004-workspace-identity-env-var.md) | `WAKIL_WORKSPACE_PATH` and workspace store identity | Accepted |
| [0005](0005-fail-closed-project-sandbox-file.md) | Project sandbox file fails closed | Accepted |
| [0006](0006-signing-bypass-refused-in-session.md) | Signing cannot be bypassed from inside an enforced session | Accepted |
| [0007](0007-command-scope-signing-injection.md) | Signing settings injected at command scope | Accepted |
| [0008](0008-per-repo-signing-key-resolution.md) | Per-repository signing-key resolution | Accepted |
| [0009](0009-first-run-risk-acknowledgement.md) | First-run risk acknowledgement | Accepted |
| [0010](0010-open-items-carried-forward.md) | Open items carried forward from the process records | Accepted |
| [0011](0011-cleanup-pass-invariants.md) | Cleanup-pass invariants A1–A3 and their status | Accepted |
| [0012](0012-hisn-0.4.0-session-model.md) | hisn 0.4.0 session model | Accepted (minimum version superseded by 0014) |
| [0013](0013-ilm-emitter-removed.md) | ilm emitter removed | Accepted |
| [0014](0014-hisn-minimum-0.4.1.md) | hisn minimum raised to 0.4.1 | Accepted |
