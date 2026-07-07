# wakīl

A terminal-native coding agent. Go binary, thin HTTP client, zero framework
overhead — talks to any OpenAI-compatible Chat Completions endpoint through a
remote *ilm* proxy that owns memory, grounding, and the model. wakil owns the
TUI, tool execution, and session persistence.

```
you ── wakil (TUI · tool exec · sessions) ── ilm proxy (memory · grounding) ── model
                        ↑
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
- [Optional features](#optional-features) · [How state works](#how-state-works)
- [Testing](#testing) · [Project layout](#project-layout) · [Contributing](#contributing) · [License](#license)

## Requirements

| | |
|---|---|
| **Go 1.25+** | to build from source *(see `go.mod`)* |
| **Docker** | for the default `docker` exec mode *(skip with `--exec direct`)* |
| **An ilm proxy** | reachable at a URL you control — wakil is a client, not a standalone brain |

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

# 3. Point it at your ilm proxy and go — workspace arg is optional
export ILM_BASE_URL='http://proxy-host:11400'
./wakil ~/projects/myapp        # explicit path
# no argument → auto-mounts the current directory
cd ~/projects/myapp && ./wakil
```

Default `docker` mode: one persistent container for the process lifetime,
every tool call executes inside it. Skip step 2 and pass `--exec direct` to
run bare-metal on the host instead.

## Security and the confirmation gate

**Gated** — `run_shell`, `write_file`, `edit_file`, `delete_file`, `move_file`,
`run_background`, `kill_process`, `open_url`. Every call prompts `y/n` before
it runs, no exceptions carved out.

**Ungated** — `read_file`, `read_file_full`, `list_dir`, `search_files`,
`find_files`, `dispatch_subagent`, `read_process_log`, and the `lsp_*`
code-intelligence tools. All structured, argument-constrained calls: they read
file contents, listings, and symbol data, but none of them can execute
arbitrary commands.

`run_shell` is gated even for pure reads — `cat ~/.ssh/id_rsa` or `env` hits
the same `y/n` wall as anything else. `a` at a prompt auto-approves read-only
tools for the rest of the session; gated tools keep prompting unless you flip
full auto-approve with `/auto` (status bar shows `AUTO`). Destructive commands
and counsel calls gate even in auto mode — no override.

Default `docker` mode bind-mounts the **host Docker socket**
(`--docker-sock=true`) — that's host-root, functionally. It buys the agent the
ability to run `docker` / `docker compose` against your real daemon. Powerful,
and exactly as dangerous as it sounds. Run untrusted tasks with the gate on,
against a proxy and model you actually trust, and reach for
`--docker-sock=false` or `--exec direct` in a disposable VM when you don't
need host-Docker control.

## Configuration

Precedence: **defaults < config file < env < flags**. Config file is JSON at
`~/.config/wakil/config.json`, overridable via `WAKIL_CONFIG` / `--config`.

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `--base-url` | `ILM_BASE_URL` | — *(required)* | proxy base URL |
| `--api-key` | `ILM_API_KEY` | — | sent as `Authorization: Bearer <key>` |
| `--model` | `ILM_MODEL` | `ilm` | model name |
| `--exec` | `ILM_EXEC_MODE` | `docker` | `docker` \| `direct` |
| `--image` | `ILM_CONTAINER_IMAGE` | `wakil-dev` | sandbox image *(build from `Dockerfile`)* |
| `--workdir` | `ILM_WORKDIR` | `/mnt/<dirname>` | working dir inside the container |
| `--host-workdir` | `ILM_HOST_WORKDIR` | cwd *(auto-detected)* | host path bind-mounted into the container |
| `--docker-sock` | `ILM_DOCKER_SOCKET` | `true` | pass host Docker socket into the sandbox |
| `--resume` | — | — | resume the most recent session |
| `--resume-id` | — | — | resume a session by chat_id *(or unique prefix)* |
| `--auto` | — | — | auto-approve all tool calls without prompting |
| `--searxng-url` | `SEARXNG_URL` | — | enable the SearXNG native search tool |
| `--google-cx` | `GOOGLE_CX` | — | Google Programmable Search Engine ID *(pair with `GOOGLE_API_KEY`)* |
| `--config` | `WAKIL_CONFIG` | `~/.config/wakil/config.json` | JSON config file path |

`lsp_enabled` is config-file only, no flag — see
[LSP code intelligence](#lsp-code-intelligence).

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

**Meta**

```
/learn               ask the proxy to synthesise a fact to remember for next time
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
| `dispatch_subagent` | no | Spawn a read-only discovery subagent for a bounded task *(contiguous same-turn calls run in parallel)* |
| `dispatch_subagents` | no | Spawn several discovery subagents concurrently, one per task *(bounded by `max_parallel_subagents`, default 2)* |
| `read_process_log` | no | Read the tail of a background process's log |
| `lsp_definition` / `lsp_references` / `lsp_hover` / `lsp_symbols` | no | Language-server-backed code intelligence *(off by default — see below)* |

MCP tools *(stdio or HTTP)* append automatically when `mcp_servers` is
configured. The host Docker socket passthrough (`--docker-sock`) is what lets
`docker` / `docker compose` calls reach the host daemon.

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

### Backend-truth context sizing

At startup wakil pulls the backend's real per-slot context window (`n_ctx`)
through the proxy and sizes the context meter, pressure warnings, and
compaction against it — with a loud fallback warning if the backend is
unreachable.

## How state works

The proxy routes by **message content**; statefulness differs by path.

**Task path** *(normal requests with `tools`)* — standard OpenAI passthrough
to a llama.cpp Qwen backend. The client `messages` array is honored, so the
standard agent loop runs clean: assistant `tool_calls` → execute →
`role:"tool"` result → resend → final answer. Multi-turn continuity holds.
wakil keeps a **bounded client-side transcript**, compacting older turns into
a running summary *(last N turns verbatim + summary)* to keep context bounded.

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
protocol/serialization layer.

## Project layout

```
cmd/wakil/         main package — entry point, CLI, TUI wiring, client tests
internal/
  agent/           the agent loop and tool-call assembly
  config/          flag/env/file config resolution
  counsel/         mashūra — external-model counsel (review/debug/decide/check)
  exec/            executor backends (docker, direct) + cwd tracking
  lsp/             language-server client — manager, JSON-RPC transport, tools
  proxy/           ilm proxy HTTP client
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
