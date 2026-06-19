# wakīl (wakil)

A standalone, long-running terminal agent. **One continuous conversation** — no
per-session/per-directory ceremony. You give it tasks ad hoc; it stays open,
keeps context, and executes locally behind a **per-command confirmation gate**.

wakil is a thin HTTP **client** of a remote *ilm* proxy (an OpenAI-compatible
Chat Completions API). The proxy is the "brain": it adds memory, learned
knowledge, and grounding server-side. wakil owns the UI, the conversation, and
local execution.

## Build

```sh
go build -o wakil ./cmd/wakil
```

Single static binary, no runtime deps.

In the default `docker` exec mode the agent runs commands inside a sandbox image
(`wakil-dev` by default). Build it once from the included `Dockerfile` — it
bundles Go, Node, Rust, and Python toolchains:

```sh
docker build -t wakil-dev .
```

(Or skip the image entirely and run on the host with `--exec direct`.)

## Run

```sh
export ILM_BASE_URL='http://proxy-host:11400'   # your ilm proxy
./wakil [workspace-path]
```

By default, tool calls run inside **one persistent Docker container** for the
process lifetime. The current directory (or the `workspace-path` positional arg)
is bind-mounted into the container at `/mnt/<dirname>` and the host Docker socket
is passed through so the agent can start containers on the host. Use
`--exec direct` to run on the host instead. **Every write/execute command
requires a y/n confirmation** (toggle off with `/auto`).

> **Security note:** wakil executes model-proposed shell commands, and in the
> default docker mode it bind-mounts the **host Docker socket** into the sandbox
> (`--docker-sock=true`) — effectively host-root access. Run it only against a
> proxy/model you trust, keep the confirmation gate on for untrusted tasks, and
> pass `--docker-sock=false` (or use `--exec direct` in a disposable VM) when you
> don't need host-Docker control.

### Config (precedence: defaults < config file < env < flags)

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `--base-url` | `ILM_BASE_URL` | — (required) | proxy base URL |
| `--api-key` | `ILM_API_KEY` | — | sent as `Authorization: Bearer <key>` |
| `--model` | `ILM_MODEL` | `ilm` | model name |
| `--exec` | `ILM_EXEC_MODE` | `docker` | `docker` \| `direct` |
| `--image` | `ILM_CONTAINER_IMAGE` | `wakil-dev` | sandbox container image (build via the included `Dockerfile`) |
| `--workdir` | `ILM_WORKDIR` | `/mnt/<dirname>` | working dir inside the container |
| `--host-workdir` | `ILM_HOST_WORKDIR` | cwd | host path bind-mounted into the container |
| `--docker-sock` | `ILM_DOCKER_SOCKET` | `true` | pass host Docker socket into the sandbox |
| `--resume` | — | — | resume the most recent session |
| `--resume-id` | — | — | resume a session by chat_id (or unique prefix) |
| `--auto` | — | — | auto-approve all tool calls without prompting |
| `--searxng-url` | `SEARXNG_URL` | — | enable SearXNG native search tool |
| `--config` | `WAKIL_CONFIG` | `~/.config/wakil/config.json` | JSON config file |

### Commands (in the TUI)

```
/new, /reset         fresh conversation (new chat_id, clears viewport)
/plan <task>         start a gather→plan→review→implement workflow for <task>
/plan --oracle=MODE  set per-run review schedule (every-step|on-deviation|phases-only)
/plan status         show current workflow phase and step
/plan approve        approve the plan; force-skip review (logged); advance past pauses
/plan review         retry the counsel plan review (when review is pending/unavailable)
/plan verify         re-run the final review (in verify state after gaps flagged)
/plan abort          cancel the active workflow
/compact             summarize older turns now (frees context)
/learn               ask the proxy to synthesise a fact to remember for next time
/auto                toggle auto-approve — all tool calls run without prompting (shown as AUTO in status bar)
/rawtools            toggle full tool output in context (default: capped at 8k chars)
/cwd                 show executor working directory
/mode                show execution backend
/history             transcript size
/sessions            list saved sessions (★ = current)
/mcp                 list connected MCP servers and their tools
/mcp reconnect NAME  reconnect a named MCP server
/help                full command list
/quit, /exit         leave (tears down the container)
```

Anything else is sent to the agent as a task. Type `@` to attach a file or
folder for context (a picker appears).

Sessions are saved automatically. Resume the most recent with `wakil --resume`,
or a specific one with `wakil --resume-id <prefix>`.

## Tools

| Tool | Gated | Description |
|---|---|---|
| `run_shell` | yes | Run a shell command; cwd persists across calls |
| `read_file` | no | Read a file with line numbers; supports offset/limit |
| `write_file` | yes | Write/overwrite a file |
| `edit_file` | yes | Replace an exact substring in a file (shows diff preview) |
| `list_dir` | no | List directory entries |
| `search_files` | no | Grep file contents for a pattern |
| `find_files` | no | Find files by name glob recursively |
| `open_url` | yes | Open a URL in the host browser (always runs on the host, not the sandbox) |
| `dispatch_subagent` | no | Spawn a read-only discovery subagent for a bounded task |

MCP tools (stdio or HTTP) are appended automatically when `mcp_servers` is
configured. The host Docker socket passthrough (`--docker-sock`) lets the agent
run `docker`/`docker compose` commands that affect the host daemon.

## Optional features

These are off by default and enabled via the JSON config file (or the matching
flags/env vars above):

- **`/plan` workflow** — a structured gather→plan→review→implement loop with an
  optional AI counsel checkpoint between phases.
- **Counsel (mashūra)** — `mashura__review` / `__debug` / `__decide` / `__check`
  tools that ask one or more external models for a second opinion. Enable with
  `oracle_enabled: true`. Supports three execution modes via named **panels**
  in `mashura_panels`:

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
- **Web search** — set `searxng_url` (or `--searxng-url`) for the native
  `searxng_search` tool. For a Google-backed alternative, the bundled
  `cmd/google_search_mcp` is a small MCP server (Google Custom Search + a URL
  reader) built with the same Go MCP SDK as wakil itself. Build it with
  `go build -o google_search_mcp ./cmd/google_search_mcp` and point a
  `mcp_servers` entry at the binary. It needs `GOOGLE_API_KEY` and `GOOGLE_CX`
  in its environment.
- **Cost sidebar** — per-source token/cost accounting; rates are configured under
  `costs` and default to unpriced ("—") rather than a misleading `$0.00`.
- **Backend-truth context sizing** — at startup wakil fetches the backend's real
  per-slot context window (`n_ctx`) through the proxy and sizes the context meter,
  pressure warnings, and compaction against it (falling back to a configured value
  with a loud warning if the backend is unreachable).

## How state works

The proxy routes by **message content**, and statefulness differs by path:

- **Task path** (normal requests with `tools`): standard OpenAI passthrough to a
  llama.cpp Qwen backend. It **honors the client `messages` array**, so the
  standard agent loop works (assistant `tool_calls` → execute → `role:"tool"`
  result → resend → final answer) and multi-turn continuity holds. wakil keeps
  a **bounded client-side transcript** and compacts older turns into a running
  summary (last *N* turns verbatim + summary) so context stays bounded.
- **Memory/meta path** (`### learn this`, `remember`, `what have you learned`,
  `forget …`): short-circuits server-side and comes back as **plain assistant
  text** (acks/lists), regardless of `tools`. The proxy **ignores resent history
  for recall** — memory is server-side, keyed off the conversation via
  `metadata.chat_id`.

Note: the proxy's memory **recall is eventually-consistent** (a fact may not be
recallable immediately after `### learn this`). That's a proxy characteristic,
not a wakil bug.

## Tests

```sh
go test ./...
```

Unit tests cover: streamed tool_call assembly from incremental arg fragments,
plain-text (no-tool_calls) branch, the full agent loop, the confirm gate
(accept/decline), executor read/write + cwd tracking, transcript compaction,
and config resolution.
