# wakīl

A standalone, long-running terminal agent — one continuous conversation, no
per-session or per-directory ceremony. You give it tasks ad hoc; it stays open,
keeps context, and executes locally behind a **per-command confirmation gate**.

wakil is a thin HTTP **client** of a remote *ilm* proxy (an OpenAI-compatible
Chat Completions API). The proxy is the brain — it adds memory, learned
knowledge, and grounding server-side. wakil owns the UI, the conversation, and
local execution.

```
you ── wakil (TUI, local exec) ── ilm proxy (memory + grounding) ── model
              ↑ confirm gate                ↑ metadata.chat_id keys state
```

## Why

- **One conversation, kept open.** No `--session` flags, no per-project state
  to manage. Resume the last session or a specific one; context persists.
- **Local execution, gated.** Every write or execute command asks `y/n` before
  it runs. Toggle auto-approve per session when you trust the task.
- **Thin client of a proxy you control.** The proxy holds memory and grounding;
  wakil never stores API keys for counsel models, never invents tool output.
- **Backend-truth sized.** Context meter, pressure warnings, and compaction are
  driven by the backend's real `n_ctx`, not a guessed constant.

## Quickstart

```sh
# 1. Build the binary (single static binary, no runtime deps)
go build -o wakil ./cmd/wakil

# 2. Build the sandbox image for the default docker exec mode
#    (bundles Go, Node, Rust, Python toolchains)
docker build -t wakil-dev .

# 3. Point it at your ilm proxy and run — the workspace arg is optional
export ILM_BASE_URL='http://proxy-host:11400'
./wakil ~/projects/myapp        # explicit path
# or, with no argument it auto-mounts the current directory:
cd ~/projects/myapp && ./wakil
```

Prefer to run on the host without Docker? Skip step 2 and add `--exec direct`.

## Security and the confirmation gate

wakil executes model-proposed shell commands. Every write or execute tool call
prompts for `y/n` approval; read-only calls are ungated. Toggle auto-approve
with `/auto` in the TUI or `--auto` on launch — shown as `AUTO` in the status
bar.

In the default `docker` mode the sandbox bind-mounts the **host Docker socket**
(`--docker-sock=true`, the default), which is effectively host-root access. It
lets the agent run `docker`/`docker compose` against the host daemon — useful,
and dangerous. Keep the gate on for untrusted tasks, run only against a proxy
and model you trust, and pass `--docker-sock=false` (or use `--exec direct` in
a disposable VM) when you don't need host-Docker control.

## Configuration

Precedence: **defaults < config file < env < flags**. The config file is JSON
at `~/.config/wakil/config.json` by default, overridable via `WAKIL_CONFIG` /
`--config`.

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `--base-url` | `ILM_BASE_URL` | — *(required* | proxy base URL |
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

### Execution modes

By default, tool calls run inside **one persistent Docker container** for the
process lifetime. The workspace directory (positional arg, or cwd if omitted)
is bind-mounted into the container at `/mnt/<dirname>`. Use `--exec direct` to
run on the host instead.

### Sessions

Sessions are saved automatically. Resume the most recent with `wakil --resume`,
or a specific one with `wakil --resume-id <prefix>`.

## The TUI

Anything you type that isn't a slash command is sent to the agent as a task.
Type `@` to attach a file or folder for context — a picker appears.

### Commands

**Session**

```
/new, /reset         fresh conversation (new chat_id, clears viewport)
/compact             summarize older turns now (frees context)
/sessions           list saved sessions (★ = current)
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
| `write_file` | yes | Write/overwrite a file |
| `edit_file` | yes | Replace an exact substring in a file *(shows diff preview)* |
| `list_dir` | no | List directory entries |
| `search_files` | no | Grep file contents for a pattern |
| `find_files` | no | Find files by name glob recursively |
| `open_url` | yes | Open a URL in the host browser *(always runs on the host, not the sandbox)* |
| `dispatch_subagent` | no | Spawn a read-only discovery subagent for a bounded task |

MCP tools *(stdio or HTTP)* are appended automatically when `mcp_servers` is
configured. The host Docker socket passthrough (`--docker-sock`) lets the agent
run `docker` / `docker compose` commands that affect the host daemon.

## Optional features

Off by default; enable via the JSON config file or the matching flags and env
vars above.

### `/plan` workflow

A structured gather → plan → review → implement loop with an optional AI
counsel checkpoint between phases. See the workflow commands above.

### Counsel — mashūra

`mashura__review` / `__debug` / `__decide` / `__check` ask one or more external
models for a second opinion. Enable with `oracle_enabled: true`. Execution
modes are selected via named **panels** in `mashura_panels`:

| Mode | Behaviour |
|---|---|
| `panel` | Query all models sequentially, return all answers in labeled sections |
| `fallback` | Try in order, stop on first success |
| `fusion` | Single [OpenRouter Fusion](https://openrouter.ai/docs/guides/features/plugins/fusion) call — models run in parallel internally, a judge synthesizes the result |

Model strings are prefixed by provider: `anthropic:claude-opus-4-8` or
`openrouter:google/gemini-2.5-pro`. Fusion mode uses OpenRouter's `~model`
syntax (`~anthropic/claude-opus-latest`).

Keys are read at call time and never stored: `ANTHROPIC_API_KEY` (configurable
via `oracle_api_key_env`) for Anthropic models; `OPENROUTER_API_KEY` for
OpenRouter and Fusion. Map tools to panels with `mashura_tool_panels`.

Wakil reads evidence files from disk — the model supplies **paths**, not
content. Directory paths expand via `git ls-files`; line ranges are supported
via `path_ranges`.

### Web search

Two native options, both built directly into wakil — no external binaries or
MCP config needed.

- **SearXNG** — set `searxng_url` *(or `--searxng-url`)* for the native
  `searxng_search` + `searxng_url_read` tools.
- **Google** — set `google_api_key` and `google_cx` in config *(or
  `GOOGLE_API_KEY` / `GOOGLE_CX` env vars)* for the native `google_search` +
  `google_fetch_url` tools.

### Cost sidebar

Per-source token and cost accounting. Rates are configured under `costs` and
default to unpriced *("—")* rather than a misleading `$0.00`.

### Backend-truth context sizing

At startup wakil fetches the backend's real per-slot context window (`n_ctx`)
through the proxy and sizes the context meter, pressure warnings, and
compaction against it — falling back to a configured value with a loud warning
if the backend is unreachable.

## How state works

The proxy routes by **message content**, and statefulness differs by path.

**Task path** *(normal requests with `tools`)* — standard OpenAI passthrough to
a llama.cpp Qwen backend. It honors the client `messages` array, so the
standard agent loop works: assistant `tool_calls` → execute → `role:"tool"`
result → resend → final answer. Multi-turn continuity holds. wakil keeps a
**bounded client-side transcript** and compacts older turns into a running
summary *(last N turns verbatim + summary)* so context stays bounded.

**Memory / meta path** *(`### learn this`, `remember`, `what have you learned`,
`forget …`)* — short-circuits server-side and comes back as plain assistant
text *(acks / lists)*, regardless of `tools`. The proxy ignores resent history
for recall; memory is server-side, keyed off the conversation via
`metadata.chat_id`.

> The proxy's memory recall is **eventually consistent** — a fact may not be
> recallable immediately after `### learn this`. That's a proxy
> characteristic, not a wakil bug.

## Testing

```sh
go test ./...
```

Unit tests in `cmd/wakil/*_test.go` and `internal/` cover: streamed tool_call
assembly from incremental arg fragments, the plain-text *(no-tool_calls)*
branch, the full agent loop, the confirm gate *(accept/decline)*, executor
read/write + cwd tracking, transcript compaction, and config resolution.

## Project layout

```
cmd/wakil/         main package — entry point, CLI, TUI wiring, client tests
internal/
  agent/           the agent loop and tool-call assembly
  config/          flag/env/file config resolution
  counsel/         mashūra — external-model counsel (review/debug/decide/check)
  exec/            executor backends (docker, direct) + cwd tracking
  proxy/           ilm proxy HTTP client
  tools/           the tool set (run_shell, read_file, edit_file, …)
  trace/           execution tracing
  tui/             terminal UI
  workflow/        /plan gather→plan→review→implement state machine
tb_adapter/        adapter tooling (Python)
Dockerfile         sandbox image — Go, Node, Rust, Python toolchains
data/              runtime data
```

## Contributing

Build and test before sending a patch:

```sh
go build -o wakil ./cmd/wakil
go test ./...
```

Keep the confirmation gate honest — don't add ungated write/execute paths.

## License

Apache License 2.0 — see [LICENSE](LICENSE).

Support development at <https://rete-it.ch/donation.html>.
