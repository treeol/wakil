# wakīl

[![CI](https://github.com/treeol/wakil/actions/workflows/ci.yml/badge.svg)](https://github.com/treeol/wakil/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/Go-1.25%2B-00ADD8?logo=go&logoColor=white)](https://go.dev)

A terminal-native coding agent. Go binary, thin HTTP client, zero framework
overhead — talks to any OpenAI-compatible Chat Completions endpoint directly
(llama.cpp server, OpenRouter, vLLM…), or through a remote *ilm* proxy that
adds memory and grounding. Endpoints are named in config and switchable
mid-session. wakil owns the TUI, tool execution, and session persistence.

```
you ── wakil (TUI · tool exec · sessions) ──┬── OpenAI-compatible endpoint ── model
                        ↑                   └── ilm proxy (memory · grounding) ── model
                per-command confirm gate
```

Every write/execute call is gated behind explicit `y/n` confirmation before it
touches your filesystem, shell, or Docker daemon. Every tool result comes from
real local execution — nothing the model reports is fabricated. Code
navigation is backed by an actual language server (`lsp_definition` /
`lsp_references` / `lsp_hover` / `lsp_symbols` via gopls), not grep-and-guess.

## Contents

- [Requirements](#requirements) · [Status](#status) · [Quickstart](#quickstart)
- [Security and the confirmation gate](#security-and-the-confirmation-gate)
- [Configuration](#configuration) · [The TUI](#the-tui) · [Tools](#tools)
- [Optional features](#optional-features) · [Memory and staging](#memory-and-staging) · [How state works](#how-state-works)
- [Testing](#testing) · [Project layout](#project-layout) · [Contributing](#contributing) · [License](#license)

## Requirements

| | |
|---|---|
| **Go 1.25+** | to build from source *(see `go.mod`)* |
| **Docker** | for the default `docker` exec mode *(skip with `--exec direct`)* |
| **An OpenAI-compatible endpoint** | a llama.cpp server, OpenRouter, or an ilm proxy — wakil is a client, not a standalone brain |

## Status

Early-stage. Config keys, session format, and the tool set will move between
commits. The confirmation gate is on by default for one reason: this agent
runs shell and Docker commands against your machine. Leave it on for anything
you haven't fully audited.

## Quickstart

```sh
# 1. Build — single static binary, no runtime deps
go build -o wakil ./cmd/wakil

# 2. Build the sandbox image for the default docker exec mode
#    (Go, Node, Rust, Python toolchains + gopls, baked in)
docker build -t wakil-dev .

# 3. Point it at an endpoint and go — workspace arg is optional
export ILM_BASE_URL='http://proxy-host:11400'   # ilm proxy (legacy shape)
./wakil ~/projects/myapp        # explicit path
# no argument → auto-mounts the current directory
cd ~/projects/myapp && ./wakil
```

For direct mode against a plain OpenAI-compatible server, declare named
endpoints in the config file instead (see
[Endpoints](#endpoints)) — `config.example.json` in this repo is a working
starting point.

Default `docker` mode: one persistent container for the process lifetime,
every tool call executes inside it. Skip step 2 and pass `--exec direct` to
run bare-metal on the host instead.

## Security and the confirmation gate

**Gated** — `run_shell`, `write_file`, `edit_file`, `delete_file`, `move_file`,
`run_background`, `kill_process`, `open_url`. Every call prompts `y/n` before
it runs, no exceptions carved out.

**Ungated** — `read_file`, `read_file_full`, `list_dir`, `search_files`,
`find_files`, `dispatch_subagent`, `dispatch_subagents`, `read_process_log`,
and the `lsp_*` code-intelligence tools. All structured, argument-constrained
calls: they read file contents, listings, and symbol data, but none of them
can execute arbitrary commands.

Subagents have two capability tiers: **discovery** (default, read-only) and
**edit** (can `edit_file` / `write_file` / `delete_file` / `move_file`,
gated on `/auto` consent, serialized by a writer lock — at most one edit
child at a time). `dispatch_subagent` / `dispatch_subagents` are ungated
because they spawn bounded workers with their own tool restrictions; the
edit tier inherits session-level consent and never prompts interactively.

`run_shell` is gated even for pure reads — `cat ~/.ssh/id_rsa` or `env` hits
the same `y/n` wall as anything else. `a` at a prompt auto-approves read-only
tools for the rest of the session; gated tools keep prompting unless you flip
full auto-approve with `/auto` (status bar shows `AUTO`). Destructive commands
and counsel calls gate even in auto mode — no override.

Default `docker` mode does **not** bind-mount the host Docker socket —
`--docker-sock` defaults to `false`. Pass `--docker-sock=true` to give the
agent `docker` / `docker compose` access to your real daemon. That's
host-root, functionally — powerful, and exactly as dangerous as it sounds.
Run untrusted tasks with the gate on, against an endpoint and model you
actually trust, and reach for a disposable VM when you don't need
host-Docker control.

**Memory as an injection channel.** `memory_put` is ungated — any tool
result can cause the model to write an entry. Mid-tier entries (TTL 1h–7d)
are directly active without review, and the memory digest is injected into
the system prompt at session start. A poisoned tool result (malicious repo
content, web page) could get the model to write an instruction-shaped
entry that rides into future sessions' prompts. Durable entries go through
propose→promote review, but mid-tier bypasses that gate. The taint signal
marks entries from sessions that touched external content (`tainted`), but
it is informational — nothing currently refuses tainted mid-tier writes.
Treat this with the same caution as the Docker socket: run untrusted tasks
with the gate on, and audit memory entries (`memory_list`) if you've been
operating on untrusted content.

## Configuration

Precedence: **defaults < config file < env < flags**. Config file is JSON at
`~/.config/wakil/config.json`, overridable via `WAKIL_CONFIG` / `--config`.
Env vars use `WAKIL_*` (preferred) or `ILM_*` (legacy aliases, same
precedence).

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `--base-url` | `ILM_BASE_URL` / `WAKIL_BASE_URL` | — *(required unless `endpoints` is set)* | endpoint base URL; overrides the selected endpoint's `base_url` |
| `--api-key` | `ILM_API_KEY` / `WAKIL_API_KEY` | — | sent as `Authorization: Bearer <key>` *(endpoint-level `auth_header` wins)* |
| `--model` | `ILM_MODEL` / `WAKIL_MODEL` | `ilm` | model name; overrides the selected endpoint's `model` |
| `--exec` | `ILM_EXEC_MODE` / `WAKIL_EXEC_MODE` | `docker` | `docker` \| `direct` |
| `--image` | `ILM_CONTAINER_IMAGE` / `WAKIL_IMAGE` | `wakil-dev` | sandbox image *(build from `Dockerfile`)* |
| `--workdir` | `ILM_WORKDIR` / `WAKIL_WORKDIR` | `/mnt/<dirname>` | working dir inside the container |
| `--host-workdir` | `ILM_HOST_WORKDIR` / `WAKIL_HOST_WORKDIR` | cwd *(auto-detected)* | host path bind-mounted into the container |
| `--docker-sock` | `ILM_DOCKER_SOCKET` / `WAKIL_DOCKER_SOCKET` | `false` | pass host Docker socket into the sandbox |
| `--resume` | — | — | resume the most recent session |
| `--resume-id` | — | — | resume a session by chat_id *(or unique prefix)* |
| `--auto` | — | — | auto-approve all tool calls without prompting |
| `--searxng-url` | `SEARXNG_URL` | — | enable the SearXNG native search tool |
| `--google-cx` | `GOOGLE_CX` | — | Google Programmable Search Engine ID *(pair with `GOOGLE_API_KEY`)* |
| `--config` | `WAKIL_CONFIG` | `~/.config/wakil/config.json` | JSON config file path |

`lsp_enabled` is config-file only, no flag — see
[LSP code intelligence](#lsp-code-intelligence).

### Endpoints

The `endpoints` block names each server wakil can talk to;
`default_endpoint` selects the active one at startup. Two kinds:

- `openai` — any plain OpenAI-compatible Chat Completions server
  (llama.cpp server, OpenRouter, vLLM…). `model` is **required** and is the
  literal string sent in requests. No ilm-specific headers or body fields are
  sent.
- `ilm-proxy` — the ilm proxy with memory/grounding. `model` defaults to the
  proxy alias `ilm`; backend prefix-routing and `X-Ilm-*` headers apply.

```json
{
  "endpoints": {
    "llama": {
      "kind": "openai",
      "base_url": "http://llama-host:8080",
      "model": "qwen3.6-35b"
    },
    "or": {
      "kind": "openai",
      "base_url": "https://openrouter.ai/api",
      "model": "anthropic/claude-sonnet-4-6",
      "auth_header": "Bearer sk-or-..."
    },
    "ilm": {
      "kind": "ilm-proxy",
      "base_url": "http://proxy-host:11400"
    }
  },
  "default_endpoint": "llama"
}
```

Per-endpoint options: `auth_header` (verbatim `Authorization` value, beats
the global `api_key`) and optional `temperature` / `top_p` / `max_tokens` —
omitted from the request body entirely when unset, so server defaults stay
authoritative.

**Backward compatibility:** configs without an `endpoints` block keep working
unchanged — the top-level `base_url` (or `host`+`port`) synthesizes a single
`ilm-proxy` endpoint with model `ilm`, byte-identical request shape to before.

At runtime, `/backend <name>` switches endpoints (on `openai`-kind
endpoints), and `/model <name>` switches models — both re-resolve context
limits. Note the key caveat: `auth_header` values live in plaintext in
`config.json`; `chmod 600` it.

### Config-only fields

These have no flag or env var — set them in the JSON config file. The
[`config.example.json`](config.example.json) in this repo is a fully commented
reference covering every section below.

| Field | Default | Meaning |
|---|---|---|
| `max_parallel_subagents` | `2` | Max concurrent `dispatch_subagent` workers per turn |
| `subagent_endpoint` | `""` (inherit) | Named endpoint for subagents (`""`/`"inherit"` = follow parent) |
| `subagent_backend` | `"inherit"` | Backend for subagents (`"inherit"`/`"default"`/`"<name>"`) |
| `costs` | — | Per-source pricing block (inference, mashura, search, external backends) |
| `mashura_panels` | — | Named counsel model panels |
| `mashura_tool_panels` | — | Maps each counsel tool to a named panel |
| `oracle_enabled` | `false` | Gate for `mashura__*` counsel tools |
| `oracle_model` | `"claude-sonnet-4-6"` | Model ID for counsel calls |
| `oracle_api_key_env` | `"ANTHROPIC_API_KEY"` | Env var read at call time for the API key |
| `lsp_enabled` | `false` | Gate for `lsp_*` code-intelligence tools |
| `lsp_servers` | — | Maps language → server command |
| `mcp_servers` | — | MCP tool servers (stdio or HTTP) |
| `backend` | `""` | Default `X-Ilm-Backend` (ilm-proxy only) |
| `external_backends` | — | Backend names known to route to external providers |
| `aux_model` | `""` | Pins `X-Ilm-Aux-Model` (empty = follow main) |
| `trace_sessions` | `false` | Trace every TUI session as JSONL |
| `trace_dir` | `~/.local/share/wakil/traces` | Directory for trace files |
| `kvr_disabled` | `false` | Disable the staging KV store *(auto-disabled in direct mode)* |
| `kvr_max_entries` | `100000` | Max entries in the staging store |
| `kvr_snapshot_interval_secs` | `300` | Staging snapshot frequency *(survives sandbox restarts)* |
| `agent_prompt_path` | `agent.txt` next to config | System prompt file path |
| `backend_max_retries` | `3` | Max retries for transient backend failures (unattended) |
| `compact_at_frac` | `0.75` | Compact at 75% of effective context |
| `keep_bytes_frac` | `0.60` | Keep 60% of effective context verbatim after compaction |
| `hard_max_frac` | `0.95` | Hard ceiling at 95% of effective context |
| `context_capacity_frac` | `0.80` | Use 80% of proxy's usable_ctx as working budget |

### Agent prompt

The system prompt is loaded once at startup from `agent.txt` next to the
config file (override with `agent_prompt_path`). The source of truth is
tracked in this repo at [`prompts/agent.txt`](prompts/agent.txt) — copy or
symlink it into your config directory:

```sh
ln -sf "$(pwd)/prompts/agent.txt" ~/.config/wakil/agent.txt
```

### Execution modes

Tool calls run inside one persistent Docker container for the process
lifetime by default. The workspace directory — positional arg, or cwd if
omitted — bind-mounts into the container at `/mnt/<dirname>`. `--exec direct`
runs on the host instead, no container.

### Sessions

Saved automatically, no flag required. `wakil --resume` picks up the most
recent one; `wakil --resume-id <prefix>` targets a specific `chat_id`.

## The TUI

Anything typed that isn't a slash command goes to the agent as a task. `@`
opens a picker to attach a file or folder for context.

### Commands

**Session**

```
/new, /reset         fresh conversation (new chat_id, clears viewport)
/compact             summarize older turns now (frees context)
/sessions            list saved sessions (★ = current)
/history             transcript size
/quit, /exit         leave (tears down the container)
```

**Workflow**

```
/plan <task>         start a gather→plan→review→implement workflow for <task>
/plan --oracle=MODE  set per-run review schedule (every-step|on-deviation|phases-only)
/plan status         show current workflow phase and step
/plan approve        approve the plan; force-skip review (logged); advance past pauses
/plan review         retry the counsel plan review (when review is pending/unavailable)
/plan verify         re-run the final review (in verify state after gaps flagged)
/plan abort          cancel the active workflow
```

**Executor and tools**

```
/cwd                 show executor working directory
/mode                show execution backend
/mcp                 list connected MCP servers and their tools
/mcp reconnect NAME  reconnect a named MCP server
```

**Endpoint and model**

```
/backend             ilm-proxy: show backend selection · openai: list configured endpoints
/backend <name>      ilm-proxy: set proxy backend · openai: switch to named endpoint
/model <name>        switch model (re-resolves context limits); tab-completes from the server's model list
```

**Meta**

```
/learn               ask the proxy to synthesise a fact to remember (ilm-proxy endpoints only —
                     refuses client-side on openai endpoints instead of faking success)
/auto                toggle auto-approve (shown as AUTO in status bar)
/rawtools            toggle full tool output in context (default: capped at 8k chars)
/help                full command list
```

### Keybindings

| Key | Action |
|---|---|
| `Enter` | Send input *(Shift+Enter for newline)* |
| `↑` / `↓` | Browse command history *(previous / next)* |
| `Ctrl+R` | Reverse incremental search through command history |
| `Ctrl+E` | Expand/collapse live reasoning while the model is thinking |
| `Ctrl+C` | Cancel in-flight turn *(press twice to force-quit)* |
| `Esc` | Cancel in-flight turn |
| `Ctrl+D` | Quit *(when idle)* |
| `y` / `n` | Approve / decline a pending tool call |
| `a` | Allow all read-only calls for this session |
| `@` | Attach a file or folder |

## Tools

| Tool | Gated | Description |
|---|---|---|
| `run_shell` | yes | Run a shell command; cwd persists across calls |
| `read_file` | no | Read a file with line numbers; supports offset/limit |
| `read_file_full` | no | Read an entire file (up to ~256 KB) in one call |
| `write_file` | yes | Write/overwrite a file |
| `edit_file` | yes | Replace an exact substring in a file *(shows diff preview)* |
| `list_dir` | no | List directory entries |
| `search_files` | no | Grep file contents for a pattern |
| `find_files` | no | Find files by name glob recursively |
| `open_url` | yes | Open a URL in the host browser *(always runs on the host, not the sandbox)* |
| `dispatch_subagent` | no | Spawn a subagent for a bounded task — discovery (read-only) or edit tier *(contiguous same-turn calls run in parallel)* |
| `dispatch_subagents` | no | Spawn several subagents concurrently, one per task *(bounded by `max_parallel_subagents`, default 2)* |
| `read_process_log` | no | Read the tail of a background process's log |
| `staging_put` | no | Store a value in the ephemeral in-sandbox KV store *(key auto-prefixed with agent identity)* |
| `staging_get` / `staging_get_many` | no | Retrieve values by key *(cross-prefix reads allowed — enables subagent handoffs)* |
| `staging_list` | no | List staging keys, optionally filtered by prefix |
| `staging_delete` | no | Delete a key under your prefix |
| `memory_put` | no | Write to durable memory: TTL present → mid-tier active; absent → durable proposed |
| `memory_get` | no | Retrieve the active entry for a key *(with provenance + staleness flags)* |
| `memory_search` | no | FTS5 full-text search over memory entries |
| `memory_list` | no | List entries by prefix, tier, or status |
| `memory_promote` | no | Promote a proposed durable entry to active *(main agent only)* |
| `memory_reject` | no | Reject a proposed durable entry *(main agent only)* |
| `memory_forget` | no | Supersede an active entry with a tombstone *(main agent only)* |
| `memory_promote_from_staging` | no | Bridge a staging value into durable memory as proposed *(main agent only)* |
| `lsp_definition` / `lsp_references` / `lsp_hover` / `lsp_symbols` | no | Language-server-backed code intelligence *(off by default — see below)* |

MCP tools *(stdio or HTTP)* append automatically when `mcp_servers` is
configured. The host Docker socket passthrough (`--docker-sock`) is what lets
`docker` / `docker compose` calls reach the host daemon.

### Subagent tabs

When `dispatch_subagent` or `dispatch_subagents` runs, a tab opens in the
bottom tab bar for each child. The sidebar shows the child's endpoint, model,
chat ID, and (when finished) cost, files changed, and context size. Tabs are
routed by `chat_id`, so concurrent subagents stream to their own panes
without cross-contamination.

Tab dot states:

| State | Dot | Meaning |
|---|---|---|
| Queued | `●` dim gray | Dispatched, waiting for a parallelism slot |
| Running | `●` pulsing yellow | Worker acquired a slot, request in flight |
| Finished | `✓` dim green | Child returned; display-only — authoritative `done` pending |
| Done | `✓` solid green | Authoritative completion (cost folded, grounding attached) |

A child that finishes while siblings are still running shows the dim green
`✓` immediately — it doesn't wait for the slowest sibling. The sidebar
displays a timestamped "✓ finished" status with cost and a one-line summary
preview. When the authoritative `SubagentDoneMsg` arrives (after the cost
fold in Phase C), the tab enriches to solid green with no visual regression.

Click a finished or done tab's `×` to close it; running tabs show `·`
instead. Tabs are pruned (oldest finished first) past `maxSubTabs` (12);
running and focused tabs are never pruned.

## Optional features

Off by default. Flip on via the JSON config file or the matching flags/env
vars above.

### LSP code intelligence

`lsp_enabled: true` turns on `lsp_definition`, `lsp_references`, `lsp_hover`,
`lsp_symbols` — real language-server lookups, semantically scoped, instead of
grepping for identifier text across unrelated code.

`lsp_definition` / `lsp_references` / `lsp_hover` detect language from the
file extension and route to whichever server is configured for it under
`lsp_servers` — nothing Go-specific in the config shape itself. `lsp_symbols`
is workspace-wide with no file to key off, so it defaults to the `go` entry.
The sandbox `Dockerfile` currently ships exactly one server — `gopls`, pinned
to v0.22.0 — so Go is the only language proven end-to-end today. Wiring in
`rust-analyzer`, `pyright`, or anything else under `lsp_servers` should route
the same way; that path just hasn't been exercised yet.

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

### `/plan` workflow

Gather → plan → review → implement, with an optional AI counsel checkpoint
between phases. Commands under [Workflow](#commands) above.

### Counsel — mashūra

`mashura__review` / `__debug` / `__decide` / `__check` — second opinions from
external models, on demand. Enable with `oracle_enabled: true`. Execution
mode is set per named **panel** in `mashura_panels`:

| Mode | Behaviour |
|---|---|
| `panel` | Query all models sequentially, return all answers in labeled sections |
| `fallback` | Try in order, stop on first success |
| `fusion` | Single [OpenRouter Fusion](https://openrouter.ai/docs/guides/features/plugins/fusion) call — models run in parallel internally, a judge synthesizes the result |

Model strings are provider-prefixed: `anthropic:claude-opus-4-8`,
`openrouter:google/gemini-2.5-pro`. Fusion mode uses OpenRouter's `~model`
syntax (`~anthropic/claude-opus-latest`).

Keys are read at call time, never stored: `ANTHROPIC_API_KEY` (or override via
`oracle_api_key_env`) for Anthropic, `OPENROUTER_API_KEY` for OpenRouter and
Fusion. `mashura_tool_panels` maps individual tools to panels.

wakil reads evidence files from disk on the model's behalf — the model
supplies **paths**, never content. Directory paths expand via `git ls-files`;
`path_ranges` scopes to specific line spans.

### Web search

Two native options, both built directly into wakil — no external binaries, no
MCP config.

- **SearXNG** — set `searxng_url` *(or `--searxng-url`)* for `searxng_search`
  + `searxng_url_read`.
- **Google** — set `google_api_key` and `google_cx` *(or `GOOGLE_API_KEY` /
  `GOOGLE_CX`)* for `google_search` + `google_fetch_url`.

### Cost sidebar

Per-source token and cost accounting. Rates live under `costs`; unpriced
sources show `—`, not a misleading `$0.00`.

### Memory and staging

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
sessions. Mid-tier entries auto-expire (1h–7d TTL); durable entries are
permanent and go through a propose→promote review flow. Subagents can
propose durable entries but only the main agent can promote them. Every
entry carries provenance (writer, taint signal, anchor staleness). A
memory digest is injected into the system prompt at session start.

**The bridge:** `memory_promote_from_staging` reads an untrusted staging
value and writes it to durable memory as a PROPOSED entry — the main
agent reviews and promotes it. The staging key's prefix is preserved as
provenance.

Full design docs: [`docs/staging.md`](docs/staging.md) ·
[`docs/memory.md`](docs/memory.md).

### Backend-truth context sizing

At startup (and on every `/backend` / `/model` switch) wakil resolves the
real per-slot context window (`n_ctx`) and sizes the context meter, pressure
warnings, and compaction against it — with a loud fallback warning when
nothing answers. Resolution depends on endpoint kind:

- `ilm-proxy` — `/v1/ilm/limits` (includes the proxy's pre-computed
  `usable_ctx`), then `/props`.
- `openai` — `/props` for llama.cpp servers; for `openrouter.ai` the
  configured model is resolved against OpenRouter's public model registry.

## How state works

**On `openai` endpoints** state is simple: the standard agent loop runs
against a stateless server — assistant `tool_calls` → execute → `role:"tool"`
result → resend → final answer. wakil keeps a **bounded client-side
transcript**, compacting older turns into a running summary *(last N turns
verbatim + summary)*. There is no server-side memory; `/learn` refuses
client-side rather than letting a bare model fake a memory ack.

Client-side **durable memory** (T2) works on all endpoints — the SQLite
store is host-side and endpoint-independent. `memory_put`,
`memory_search`, and the propose→promote flow operate regardless of
whether the backend is an ilm proxy or a plain OpenAI server. See
[Memory and staging](#memory-and-staging) above.

**On `ilm-proxy` endpoints** the proxy additionally routes by **message
content**; statefulness differs by path.

**Task path** *(normal requests with `tools`)* — standard OpenAI passthrough
to a llama.cpp Qwen backend. Same clean agent loop and bounded transcript as
above.

**Memory / meta path** *(`### learn this`, `remember`, `what have you
learned`, `forget …`)* — short-circuits server-side, returns plain assistant
text *(acks / lists)* regardless of `tools`. Resent history is ignored for
recall; memory lives server-side, keyed by `metadata.chat_id`.

> Memory recall is **eventually consistent** — a fact may not be recallable
> immediately after `### learn this`. Proxy characteristic, not a wakil bug.

## Testing

```sh
go test ./...
```

Coverage in `cmd/wakil/*_test.go` and `internal/`: streamed tool_call assembly
from incremental arg fragments, the plain-text *(no-tool_calls)* branch, the
full agent loop, the confirm gate *(accept/decline)*, executor read/write +
cwd tracking, transcript compaction, config resolution, and the LSP
protocol/serialization layer. Endpoint decoupling is golden-tested: request
shape per kind *(no `metadata` / `X-Ilm-*` on `openai`; byte-identical proxy
shape preserved)*, endpoint config validation and legacy synthesis,
kind-aware limits resolution *(props / OpenRouter registry / proxy route,
with request-log assertions that the wrong routes are never called)*,
`/learn` gating *(zero requests on `openai`)*, `/model` and `/backend`
switch semantics with subagent inheritance, and retry classification
*(429/408/529 retryable, other 4xx fatal)*.

## Project layout

```
cmd/wakil/         main package — entry point, CLI, TUI wiring, client tests
internal/
  agent/           the agent loop and tool-call assembly
  config/          flag/env/file config resolution
  counsel/         mashūra — external-model counsel (review/debug/decide/check)
  exec/            executor backends (docker, direct) + cwd tracking
  lsp/             language-server client — manager, JSON-RPC transport, tools
  memory/          durable memory store — SQLite, two tiers, FTS5, provenance
  orregistry/      OpenRouter model registry fetch + cache (context lengths)
  proxy/           chat endpoint HTTP client (openai + ilm-proxy kinds)
  staging/         kvr client — in-sandbox ephemeral KV store (UDS wire protocol)
  tools/           the tool set (run_shell, read_file, edit_file, …)
  trace/           execution tracing
  tui/             terminal UI
  workflow/        /plan gather→plan→review→implement state machine
Dockerfile         sandbox image — Go, Node, Rust, Python toolchains, gopls
```

## Contributing

```sh
go build -o wakil ./cmd/wakil
go test ./...
```

Both green before you send a patch. Keep the confirmation gate honest — no
ungated write/execute paths, ever.

## License

Apache License 2.0 — see [LICENSE](LICENSE).

Support development at <https://rete-it.ch/donation.html>.
