# Features

wakil's optional features are off by default. Flip them on via the JSON config
file or the matching flags/env vars (see [Configuration](configuration.md)).

## Feature index

| Feature | Config key | Documentation |
|---|---|---|
| LSP code intelligence | `lsp_enabled` | [↓](#lsp-code-intelligence) |
| Headless browser | `browser_enabled` | [↓](#headless-browser) |
| Counsel (Mashūra) | `oracle_enabled` | [↓](#counsel-mashūra) |
| Web search | `searxng_url` / `google_api_key` | [↓](#web-search) |
| Cost sidebar | `costs` | [↓](#cost-sidebar) |
| Memory and staging | built-in | [↓](#memory-and-staging) |
| `/plan` workflow | built-in | [workflows.md](workflows.md) |
| Trace capture | `trace_sessions` | [↓](#trace-capture) |
| Checkpoint/Rewind | built-in | [↓](#checkpointrewind) |
| AGENTS.md ingestion | built-in | [↓](#agentsmd-ingestion) |
| Lifecycle hooks | `hooks` | [↓](#lifecycle-hooks) |
| Code review | built-in | [↓](#code-review) |
| Repo map | built-in | [↓](#repo-map) |
| Worktree isolation | built-in | [↓](#worktree-isolation) |
| Correction capture | built-in | [↓](#correction-capture) |

## LSP code intelligence

`lsp_enabled: true` turns on `lsp_definition`, `lsp_references`, `lsp_hover`,
`lsp_symbols` — lookups that use a configured language server rather than text
search.

`lsp_definition` / `lsp_references` / `lsp_hover` detect language from the
file extension and route to whichever server is configured for it under
`lsp_servers` — nothing Go-specific in the config shape itself. `lsp_symbols`
is workspace-wide with no file to key off, so it defaults to the `go` entry.
The sandbox `Dockerfile` currently ships one server — `gopls`, pinned
to v0.22.0 — so Go is the only language covered by the test setup. Wiring in
`rust-analyzer`, `pyright`, or anything else under `lsp_servers` should route
the same way; that path is not part of the test setup.

```json
{
  "lsp_enabled": true,
  "lsp_servers": {
    "go": {"command": "gopls", "args": ["serve"]}
  }
}
```

Calls are line-anchored: `(path, line, symbol)`. The line number is exactly
what `read_file` already prints, so there's no extra lookup round-trip.
Unsupported operations return an explicit failure message, never a silent
empty result.

## Headless browser

`browser_enabled: true` turns on `browser_navigate`, `browser_screenshot`,
`browser_viewport`, `browser_click`, `browser_eval`, `browser_text`,
`browser_html`, `browser_reduced_motion` — a headless Chromium instance
(via [chromedp](https://github.com/chromedp/chromedp)) for visual verification,
DOM inspection, interaction testing, and responsive layout checks.

The browser runs inside the sandbox container — `chromium` is installed in the
`Dockerfile` image. It uses `--no-sandbox` (required inside the container's
capability-dropped environment) and `--disable-gpu`. All navigation targets
localhost or `file://` URLs; the sandbox's network namespace controls egress.

```json
{
  "browser_enabled": true
}
```

Common workflows:

- **Visual verification:** `browser_navigate` → `browser_screenshot` to capture
  a rendered page.
- **Responsive checks:** `browser_viewport` (e.g. 375×812 for mobile) →
  `browser_screenshot` → `browser_eval` to inspect computed styles.
- **Interaction testing:** `browser_navigate` → `browser_click` →
  `browser_text` to verify state changes.
- **`prefers-reduced-motion`:** `browser_reduced_motion` (emulate=true) →
  `browser_eval` to verify transitions are actually disabled at runtime, not
  just branched-on in code.

No confirmation needed — the browser runs inside the sandbox and cannot write
to the filesystem or execute arbitrary commands.

## Counsel (Mashūra)

`mashura__review` / `__debug` / `__decide` / `__check` — second opinions from
external models, on demand. Enable with `oracle_enabled: true`. Execution
mode is set per named **panel** in `mashura_panels`:

| Mode | Behaviour |
|---|---|
| `panel` | Query all models in parallel, return all answers in labeled sections |
| `fallback` | Try in order, stop on first success |
| `fusion` | Single [OpenRouter Fusion](https://openrouter.ai/docs/guides/features/plugins/fusion) call — models run in parallel internally, a judge synthesizes the result |
| `debate` | Two-round critique-of-critique: round 1 collects independent answers, round 2 has each model critique and refine based on all round-1 answers |

Model strings are provider-prefixed: `anthropic:claude-opus-4-8`,
`openrouter:google/gemini-2.5-pro`. Fusion mode uses OpenRouter's `~model`
syntax (`~anthropic/claude-opus-latest`).

Keys are read at call time, never stored: `ANTHROPIC_API_KEY` (or override via
`oracle_api_key_env`) for Anthropic, `OPENROUTER_API_KEY` for OpenRouter and
Fusion. `mashura_tool_panels` maps individual tools to panels.

wakil reads evidence files from disk on the model's behalf — the model
supplies **paths**, never content. Directory paths expand via `git ls-files`;
`path_ranges` scopes to specific line spans.

## Web search

Two native options, both built directly into wakil — no external binaries, no
MCP config.

- **SearXNG** — set `searxng_url` *(or `--searxng-url`)* for `searxng_search`
  + `searxng_url_read`.
- **Google** — set `google_api_key` and `google_cx` *(or `GOOGLE_API_KEY` /
  `GOOGLE_CX`)* for `google_search` + `google_fetch_url`.

## Cost sidebar

Per-source token and cost accounting. Rates live under `costs`; unpriced
sources are shown as `—` rather than `$0.00`.

## Memory and staging

Wakil has a two-tier memory architecture. Both are built-in — no external
services required.

| Tier | Store | Lifetime | Location | Gating |
|---|---|---|---|---|
| **T1 staging** | [kvr](https://github.com/treeol/kvrust) (Rust KV) | Ephemeral *(snapshot survives restarts)* | In-sandbox | Ungated — any agent |
| **T2 mid** | SQLite (pure Go) | 1h–7d TTL, auto-expires | Host-side | Direct active writes |
| **T2 durable** | SQLite (pure Go) | Permanent | Host-side | PROPOSED on write; main agent promotes |

**Staging (T1)** is a fast in-sandbox KV store for scratch space and
subagent handoffs. Keys are auto-prefixed with the writer's agent identity
(`main/` or `sub-<id>/`); cross-prefix reads are allowed so a parent can
read a child's findings. Staging survives sandbox restarts via periodic
snapshots. Ungated — staging writes touch no workspace state.

**Durable memory (T2)** is a host-side SQLite store that persists across
sessions **within a workspace**. Each workspace has its own isolated memory
DB at `<wakil-data>/memory/<workspace-key>/memory.db` — entries stored in
one workspace are **not** visible in another because anchors are
workspace-relative. Mid-tier entries auto-expire (1h–7d TTL); durable entries
are permanent and go through a propose→promote review flow. Subagents can
propose durable entries but only the main agent can promote them. Every
entry carries provenance (writer, taint signal, anchor staleness). A
memory digest is injected into the system prompt at session start.

**The bridge:** `memory_promote_from_staging` reads an untrusted staging
value and writes it to durable memory as a PROPOSED entry — the main
agent reviews and promotes it. The staging key's prefix is preserved as
provenance.

Full design docs: [`staging.md`](staging.md) · [`memory.md`](memory.md).

## Trace capture

Set `trace_sessions: true` (or pass `--trace`) to enable JSONL trace capture
for the session. Traces are written to `~/.local/share/wakil/traces` (or
`trace_dir` if set). Each trace file records the full conversation flow —
useful for debugging, auditing, and reproducing issues.

## Checkpoint/Rewind

Every turn starts a checkpoint that captures the pre-mutation state of files the agent touches. `/rewind` (or `/rewind <N>`) restores files to their pre-edit state and truncates the conversation — undo the last turn or the last N turns. Checkpoints are in-memory and per-session (cleared on compaction, session rotation, and resume). Shell command side effects are not reliably reverted; the rewind summary warns which turns had shell commands.

## AGENTS.md ingestion

Wakil looks for `AGENTS.md` in the workspace root and ancestor directories at session start. The content is injected into the system prompt as advisory project instructions — after Wakil's own operating instructions and before the runtime context (date, cwd, tools). This is untrusted repo content; it sets the taint flag for provenance tracking.

The `/init` command detects project conventions (build tool, test command, linter, language, framework) from manifest files (go.mod, package.json, Makefile, Cargo.toml, etc.) and creates an `AGENTS.md` file if one doesn't exist.

## Lifecycle hooks

Configure shell hooks that fire at specific lifecycle events. Hooks run on the host (not the sandbox), inherit the host environment, and receive `WAKIL_TOOL_NAME`, `WAKIL_FILE_PATH`, and `WAKIL_CWD` env vars.

| Timing | Config key | Behavior |
|---|---|---|
| `pre_tool` | `hooks.pre_tool` | Fires before a tool call. Non-zero exit blocks the tool. |
| `post_tool` | `hooks.post_tool` | Fires after a successful tool call. Cannot block. Output injected into the tool result. |
| `session_start` | `hooks.session_start` | Fires once at the first turn of a session. |
| `session_end` | `hooks.session_end` | Fires when the session ends (/new, /resume, /handoff). |
| `on_stop` | `hooks.on_stop` | Fires at process exit. |

Each hook entry has a `tool` pattern (empty = all tools, pipe-separated for alternatives like `write_file|edit_file`) and a `command` (shell string, runs via `sh -c`).

```json
{
  "hooks": {
    "post_tool": [
      {"tool": "write_file|edit_file", "command": "gofmt -w $WAKIL_FILE_PATH"}
    ]
  }
}
```

## Code review

`/review [ref]` runs a read-only subagent review of the current diff (unstaged changes by default, or against a git ref like `HEAD~1`). The reviewer checks for correctness, tests, security, and style using a fixed rubric. The review is strictly read-only — zero file mutations. The report is displayed in the TUI.

## Repo map

A lightweight file-tree outline is built at session start and injected into the system prompt as a one-line summary. The full outline is spilled to the tool cache so the agent can `read_file` it for the complete project layout. Rebuild on demand with `/repomap`.

## Worktree isolation

Edit-tier subagents in git repos (direct mode) run in isolated `git worktree add --detach` working directories. After a child finishes, its changes are diffed and applied as a patch to the parent workspace (serialized by a patch-apply lock). Disjoint edits from parallel children apply cleanly; conflicting patches are detected and the second child's changes are not applied (with a warning). Non-git directories and Docker mode fall back to the serialized writer-lock behavior.

## Correction capture

Wakil automatically detects when you correct its work and proposes storing a memory entry:

- **After `/rewind`**: if you undo the agent's work and then send a new message, Wakil proposes capturing your correction with the reverted file paths as anchors.
- **Explicit patterns**: messages like "no, use const not var", "stop doing X", "I wanted Y, not Z" trigger detection.

You must explicitly confirm before anything is stored — the `correction_capture` gate is carved out from `/auto` mode so it always prompts. Approved corrections are stored as PROPOSED memory entries (kind="correction") that require `memory_promote` before becoming active. Once active, they're automatically retrieved in future sessions working on similar tasks.
