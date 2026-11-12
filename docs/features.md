# Features

wakil's optional features are off by default. Flip them on via the JSON config
file or the matching flags/env vars (see [Configuration](configuration.md)).

## Feature index

| Feature | Config key | Documentation |
|---|---|---|
| LSP code intelligence | `lsp_enabled` | [↓](#lsp-code-intelligence) |
| Headless browser | `browser_enabled` | [↓](#headless-browser) |
| Counsel (Mashūra) | `oracle_enabled` | [↓](#counsel-mashūra) |
| Data policy (`/zdr`, `/region`) | `zdr`, `openrouter_region` | [↓](#data-policy) |
| Web search | `searxng_url` / `google_api_key` | [↓](#web-search) |
| Cost sidebar | `costs` | [↓](#cost-sidebar) |
| Memory and staging | built-in | [↓](#memory-and-staging) |
| `/plan` workflow | built-in | [tui.md](tui.md#plan-workflow) |
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
capability-dropped environment), `--disable-gpu`, `--disable-crash-reporter` and
`--disable-breakpad`. All navigation targets localhost or `file://` URLs; the
sandbox's network namespace controls egress.

**Lifecycle (0.4.4).** Nothing is started with the container. Chromium launches
on the first `browser_*` call, as a direct child of a `docker exec` session
(its own process group), with a per-launch profile directory
`/tmp/wakil-chrome-<random>` (also its `HOME`, so crashpad's files live under
it). It stops — process group TERMed, KILLed after 3 s, stragglers swept, profile
directory removed — after `browser_idle_timeout_seconds` (default 600) without a
call, on `/new` and chat end, and when Wakil exits; the next call starts a fresh
browser. The DevTools endpoint is Chromium-chosen and loopback-only inside the
container; Wakil reaches it through a `127.0.0.1` listener on the host that pipes
each connection through `docker exec -i … python3` — no port is published and
nothing listens on a non-loopback address. Under the `hisn` backend Chromium runs
the same way inside the hisn session, started through the backend's CLI
transport; the DevTools endpoint is again Chromium-chosen and loopback-only in
the session, and Wakil reaches it through a `127.0.0.1` listener on the host that
pipes each connection through `python3` in the session. Chromium (`chromium`, or
`browser_path`) and `python3` must be present in the session.

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
`path_ranges` scopes to specific line spans; `commit_range` (e.g. `a..b` or
`HEAD~3`) attaches the `git diff` of a revision range as one labelled block, so
a change set is reviewed as a whole rather than a HEAD snapshot.

A document is sent whole when it fits the reviewer's context, and as clearly
labelled parts (with a small overlap) when it does not — never silently cut. An
oversize **source file** is split into labelled line-range chunks and a final
synthesis call merges the per-chunk reviews. A reply that hit `max_tokens`, came
back empty, or mis-cites the last section it claims to have read is **VOID**: it
is retried once automatically, and if it is still VOID the round is reported as
**not obtained** — never merged, never counted as a review round, never "no
findings".

## Data policy

Two switches control what a request to a model host may carry: **`/zdr`**
(zero-data-retention) and **`/region`** (OpenRouter region pinning), set per
repo with `/zdr on|off|auto` / `/zdr reset` and `/region eu|us|off` /
`/region reset` (bare `/zdr` shows the effective value, where it comes from,
and the last request's decision; `/mode status` shows both lines). The
defaults are `off` for both, and **with both off the bytes on the wire are
identical to the unguarded path** — nothing changes for existing users.

- **`zdr`** (`off` default, `auto`, `on`): on OpenRouter hosts, `on` and
  `auto` add `"provider": {"zdr": true, "data_collection": "deny"}` (merged
  with any existing provider object, never replacing it) to every model
  request — main loop, subagents, Mashūra and aux calls all enforce it. On
  any other non-local host, `zdr on` refuses before sending (the error names
  the host and says Wakil cannot enforce the policy there — no silent
  fallback); `zdr auto` proceeds unenforced there (`not enforced (<host>)`).
  `auto` tolerates a provider without a ZDR endpoint: a 404 before any
  stream data retries **once** without the two fields, the model is
  remembered as no-ZDR for the session (later calls skip the first
  attempt), and one transcript notice fires (`zdr auto: no ZDR endpoint for
  <model>, sent without ZDR.`). Under `on` there is no silent fallback:
  if the request cannot be served under ZDR, the user sees the error and
  nothing is retried without ZDR. `/zdr off` reminds: *ZDR may still be
  enforced by your OpenRouter account settings.*
- **`openrouter_region`** (`off` default, `eu`, `us`): when the configured
  host is exactly `openrouter.ai`, `eu` sends to `eu.openrouter.ai` and
  `us` to `us.openrouter.ai` (same path, same key). Regional hosts in the
  config are used verbatim, never rewritten. Errors from the regional host
  are shown verbatim and there is **no fallback** to the global host.
  *Needs an OpenRouter Business or Enterprise plan.* `/region eu|us` says so
  on its output line.
- **Local hosts** (`localhost`, loopback, RFC 1918, link-local and ULA IP
  literals, plus any names in `trusted_local_hosts`) are always allowed,
  with no policy fields added (`n/a (local)`).
- **Per-repo overrides** (`/zdr`, `/region`) are stored on your side, keyed
  by the workspace — never in a file inside the repository, so a cloned
  repo cannot change them. User config provides the fallback through the
  `zdr` and `openrouter_region` config keys; a value in a committed project
  config is ignored.

All model traffic goes through one transport (`internal/proxy`); no other
package opens a connection to a model host. The **direct Anthropic path has
been removed**: counsel, the main loop and the init test call all send
through that transport, and a configured Anthropic key is ignored (Mashūra
shows one notice per session: *direct Anthropic access was removed; the key
is ignored, Mashūra uses the configured backend*). Configured
`anthropic:<model>` panel members are served through OpenRouter as
`anthropic/<model>`.

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

### Phase timeline

`WAKIL_TRACE=1` (or `--trace`) also writes how long each phase of start, `/new`,
`/resume`, `/handoff` and exit took, in milliseconds, to
`timeline-<UTC stamp>-<pid>.json` in the same directory. The file name is
printed after the terminal is restored — never during the alternate screen. The
standing `trace_sessions` setting does not turn it on. Measuring a repository
end to end: `scripts/perf/run.sh` (see [PERF-0.6.md](PERF-0.6.md)).

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

## Subagent dispatch discipline

`dispatch_subagent` / `dispatch_subagents` take a required **`class`** (`read`, `search`, `edit`, `verify`) that sizes the child's per-turn tool-output budget (`subagents.budget.*`, see [configuration](configuration.md); an explicit `subagent_turn_tool_budget` caps it), and an optional **`files`** list.

Every dispatch returns a harness-built result: `outcome` (`complete` / `partial` / `rejected` / `refused` / `failed`), `files_covered`, `files_not_read`, `files_extra`, `files_listed`, `remaining_scope`, `budget`. The file ledger is recorded from the child's actual read calls, never from what it says it did.

- **Budget exhaustion → `partial`.** The child's tools-off final answer is used; if unparseable it gets one reserve turn; if that fails too, `findings` is empty and `remaining_scope` holds the raw tail (≤ 2 KB).
- **Off-task.** An explicit `files` list with zero listed files read is `rejected` (findings spilled, not returned) and re-run once with the ledger appended; a second rejection prints `subagent <id> off-task twice: read 0/<N> assigned files — assign differently or take over?`.
- **Partial follow-up.** Only a strictly smaller file set with different task text is accepted, once; the same scope is refused without a model call. A second partial, or a partial with nothing left to split, prints `subagent <id> partial: <read>/<listed> files, <K> findings — split or take over?`.
- **Stop rule.** After either notice the task is stopped: further dispatch for it is refused by the harness until you send your next message. The agent does not take the work over on its own.

## Plan quarantine and `wakil plan gc`

In a tainted session, `memory_put plan/current` is redirected to `plan/tainted/<chatID>/current` (quarantined, never handed off; `/plan status` says so). `wakil plan gc` lists quarantined plan entries whose session file is gone (dry run); `wakil plan gc --apply` rejects them. It never touches an active `plan/current`.

**Which store?** A chat keys its per-project stores by `config.WorkspacePath()` — the launch directory (or `host_workdir` / the workspace argument), **unless `WAKIL_WORKSPACE_PATH` is set, which replaces it**. `wakil plan gc` always uses the current directory. `wakil --print-workspace-key` prints both with the source of each, and flags a divergence; when `WAKIL_WORKSPACE_PATH` is set, Wakil prints one startup line naming the path and the resulting keys, and `/sandbox` repeats it.

## Correction capture

Wakil automatically detects when you correct its work and proposes storing a memory entry:

- **After `/rewind`**: if you undo the agent's work and then send a new message, Wakil proposes capturing your correction with the reverted file paths as anchors.
- **Explicit patterns**: messages like "no, use const not var", "stop doing X", "I wanted Y, not Z" trigger detection.

You must explicitly confirm before anything is stored — the `correction_capture` gate is carved out from `/auto` mode so it always prompts. Approved corrections are stored as PROPOSED memory entries (kind="correction") that require `memory_promote` before becoming active. Once active, they're automatically retrieved in future sessions working on similar tasks.
