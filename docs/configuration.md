# Configuration

Precedence: **defaults < config file < env < flags**. Config file is JSON at
`~/.config/wakil/config.json`, overridable via `WAKIL_CONFIG` / `--config`. On
first run, wakil auto-creates this file with a minimal template if it doesn't
exist — edit it to set your endpoint. Env vars use `WAKIL_*` (preferred) or
`ILM_*` (legacy aliases, same precedence).

## Flags and environment variables

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
| `--auto` | — | — | auto-approve tool calls without prompting *(destructive commands and counsel calls still gate)* |
| `--searxng-url` | `SEARXNG_URL` | — | enable the SearXNG native search tool |
| `--google-cx` | `GOOGLE_CX` | — | Google Programmable Search Engine ID *(pair with `GOOGLE_API_KEY`)* |
| `--mention-base` | — | current directory | base directory for `@` file mentions |
| `--host` | `ILM_HOST` / `WAKIL_HOST` | — | ilm proxy host *(alternative to `--base-url`)* |
| `--port` | `ILM_PORT` / `WAKIL_PORT` | `11400` | ilm proxy port *(used with `--host`)* |
| `--trace` | `WAKIL_TRACE_SESSIONS` | `false` | enable JSONL trace capture for this session |
| `--trace-dir` | `WAKIL_TRACE_DIR` | `~/.local/share/wakil/traces` | directory for trace files |
| `--list-sessions` | — | — | list saved sessions for this workspace and exit |
| `--all` | — | — | with `--resume`/`--list-sessions`: search all workspaces |
| `--ssh-signing` | — | `off` | SSH commit signing in the sandbox: `off`\|`auto`\|`<path>` |
| `--config` | `WAKIL_CONFIG` | `~/.config/wakil/config.json` | JSON config file path |

`lsp_enabled` is config-file only, no flag — see [LSP code intelligence](features.md#lsp-code-intelligence).

## Endpoints

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
authoritative. For `openai`-kind endpoints, `app_title` sets the OpenRouter
`X-Title` attribution header; when unset, it defaults to `wakil` for
openrouter.ai hosts and is omitted for any other host. Set to `""` to opt out.
The `HTTP-Referer` and `X-OpenRouter-Categories` headers are always the
hardcoded project defaults (`https://github.com/treeol/wakil` and `cli-agent`)
for openrouter.ai hosts and are **not** user-configurable — wakil always
identifies itself.

**Backward compatibility:** configs without an `endpoints` block keep working
unchanged — the top-level `base_url` (or `host`+`port`) synthesizes a single
`ilm-proxy` endpoint with model `ilm`, byte-identical request shape to before.

At runtime, `/backend <name>` switches endpoints (on `openai`-kind
endpoints), and `/model <name>` switches models — both re-resolve context
limits. Note the key caveat: `auth_header` values live in plaintext in
`config.json`; `chmod 600` it.

See [`config.example.json`](../config.example.json) for a fully commented
reference covering all options. For endpoint behavior, switching, and
state management, see [Endpoints and state](endpoints.md).

## Config-only fields

These have no flag or env var — set them in the JSON config file. The
[`config.example.json`](../config.example.json) in this repo is a fully commented
reference covering every section below.

| Field | Default | Meaning |
|---|---|---|
| `max_parallel_subagents` | `2` | Max concurrent `dispatch_subagent` workers per turn |
| `subagent_endpoint` | `""` (inherit) | Named endpoint for subagents (`""`/`"inherit"` = follow parent) |
| `subagent_backend` | `"inherit"` | Backend for subagents (`"inherit"`/`"default"`/`"<name>"`) |
| `costs` | — | Per-source pricing block (inference, mashura, search, external backends) |
| `mashura_panels` | — | Named counsel model panels |
| `mashura_tool_panels` | — | Maps each counsel tool to a named panel |
| `mashura_mode` | — | Default oracle consult schedule: `every-step` \| `on-deviation` \| `phases-only` *(alias: `wf_oracle_mode`)* |
| `oracle_enabled` | `false` | Gate for `mashura__*` counsel tools |
| `oracle_model` | `"claude-sonnet-4-6"` | Model ID for counsel calls |
| `oracle_api_key_env` | `"ANTHROPIC_API_KEY"` | Env var read at call time for the API key |
| `lsp_enabled` | `false` | Gate for `lsp_*` code-intelligence tools |
| `lsp_servers` | — | Maps language → server command |
| `browser_enabled` | `false` | Gate for `browser_*` headless-browser tools (chromedp + Chromium) |
| `mcp_servers` | — | MCP tool servers (stdio or HTTP) |
| `backend` | `""` | Default `X-Ilm-Backend` (ilm-proxy only) |
| `external_backends` | — | Backend names known to route to external providers |
| `aux_model` | `""` | Pins `X-Ilm-Aux-Model` (empty = follow main) |
| `kvr_disabled` | `false` | Disable the staging KV store *(auto-disabled in direct mode)* |
| `kvr_max_entries` | `100000` | Max entries in the staging store |
| `kvr_snapshot_interval_secs` | `300` | Staging snapshot frequency *(survives sandbox restarts)* |
| `docker_caps` | `[]` | Linux capabilities to re-add after `--cap-drop=ALL` *(e.g. `["CHOWN"]` if go build fails)* |
| `docker_memory` | `"4g"` | Container memory limit; must be ≥ `docker_tmpfs_size` *(tmpfs counts against the cgroup)* |
| `docker_pids_limit` | `512` | Max processes in the sandbox container *(0 = no limit)* |
| `docker_tmpfs_size` | `""` (→ 4g) | Size of the sandbox `/tmp` tmpfs *(empty uses built-in default)* |
| `docker_io_uring` | `false` | Enable io_uring via custom seccomp profile *(increases kernel attack surface — see [SECURITY.md](../SECURITY.md#seccomp))* |
| `agent_prompt_path` | `agent.txt` next to config | System prompt file path |
| `backend_max_retries` | `3` | Max retries for transient backend failures (unattended) |
| `compact_at_frac` | `0.75` | Compact at 75% of effective context |
| `keep_bytes_frac` | `0.60` | Keep 60% of effective context verbatim after compaction |
| `hard_max_frac` | `0.95` | Hard ceiling at 95% of effective context |
| `context_capacity_frac` | `0.80` | Use 80% of proxy's usable_ctx as working budget |
| `effective_ctx_max_chars` | `0` (disabled) | Absolute cap (chars) on effective context for large models. Apply to keep context within a working budget (e.g. `200000`); models with very large advertised context may degrade in practice below their nominal limit. Applied before fractions. Override at runtime with `/maxctx` |

## Agent prompt

The system prompt is loaded once at startup. Precedence:

1. **`agent_prompt_path`** in config (or `agent.txt` next to the config file by
   default) — if the file is readable, it is used.
2. **Embedded prompt** — the full `prompts/agent.txt` is baked into the binary
   via `go:embed`. If no file is found or the file is unreadable, the embedded
   full prompt is used automatically. No symlink or copy is needed for the
   default experience.

To customize the prompt, copy or symlink the source file into your config
directory:

```sh
ln -sf "$(pwd)/prompts/agent.txt" ~/.config/wakil/agent.txt
```

## Execution modes

Tool calls run inside one persistent Docker container for the process
lifetime by default. The workspace directory — positional arg, or cwd if
omitted — bind-mounts into the container at `/mnt/<dirname>`. `--exec direct`
runs on the host instead, no container.

## Sessions

Saved automatically, no flag required. `wakil --resume` picks up the most
recent one; `wakil --resume-id <prefix>` targets a specific `chat_id`.

`/handoff` summarizes the current session, stores the summary in durable
memory, and starts a fresh session with a continuation prompt — so you can
rotate sessions without losing context. The old session remains on disk;
`/resume <id>` returns to it.
