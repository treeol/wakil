# Documentation

One line per document. The project README is [../README.md](../README.md);
release history is in [../CHANGELOG.md](../CHANGELOG.md).

## Getting started

- [../README.md](../README.md) — install, quickstart, features.
- [tui.md](tui.md) — slash commands, keybindings, how skills reach the model, the `/plan` workflow.
- [headless.md](headless.md) — `wakil run`: flags, approval rules, exit codes, output events.
- [endpoints.md](endpoints.md) — endpoint kinds (`openai`, `ilm-proxy`) and runtime switching.

## Configuration

- [configuration.md](configuration.md) — every flag, environment variable and config key, plus subcommands.
- [../config.example.json](../config.example.json) — commented reference config.
- [features.md](features.md) — optional features: LSP, browser, counsel, search, trace, hooks, worktrees.
- [tools.md](tools.md) — tool reference, gating, skill templates, automatic skill creation, subagents.
- [memory.md](memory.md) — durable memory tiers, taint, retrieval.
- [staging.md](staging.md) — the ephemeral key-value staging store.
- [long-session-mode.md](long-session-mode.md) — context collapse and the `/mode` setting.

## Sandbox and hisn

- [sandbox-backends.md](sandbox-backends.md) — the hisn, docker and direct backends; hisn 0.4.1 minimum and the detach contract; profiles; `.wakil/sandbox.json`; known limits.
- [../SECURITY.md](../SECURITY.md) — threat model, sandbox classification, hardening checklist.

## Architecture and ADRs

- [adr/README.md](adr/README.md) — index of architecture decision records 0000 to 0014.
- [adr/0014-hisn-minimum-0.4.1.md](adr/0014-hisn-minimum-0.4.1.md) — the current hisn minimum and why.

## Development

- [../CONTRIBUTING.md](../CONTRIBUTING.md) — build, test, lint, coverage gate, package layout.
- [../AGENTS.md](../AGENTS.md) — build, test and CI gates for coding agents working in the repo.
- [follow-ups.md](follow-ups.md) — deferred work that is still open.

## Historical

These describe the state at the version named in their banner and are kept as records.

- [hisn-ctl.md](hisn-ctl.md) — hisn 0.2.1 control-socket client design note.
- [design-note-hisn-031.md](design-note-hisn-031.md) — hisn 0.3.1 integration design (shipped as Wakil 0.4.0).
- [truncation-audit.md](truncation-audit.md) — audit of content truncated before model calls.
- [reasoning-audit.md](reasoning-audit.md) — per-call-site reasoning and thinking budget audit.
- [continuous-mode.md](continuous-mode.md) — turn-outcome contract for a future continuous coordinator.
- [PERF-0.6.md](PERF-0.6.md) — start and exit timings for 0.6.0.
