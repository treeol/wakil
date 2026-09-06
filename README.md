# wakīl

[![CI](https://github.com/treeol/wakil/actions/workflows/ci.yml/badge.svg)](https://github.com/treeol/wakil/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/Go-1.26%2B-00ADD8?logo=go&logoColor=white)](https://go.dev)

A terminal-native coding agent. It runs in your terminal, talks to any
OpenAI-compatible Chat Completions endpoint, and stores sessions, memory, and
traces locally on disk. No hosted control plane required — you bring your own
model endpoint.

```
you → wakil → your model (llama.cpp, OpenRouter, vLLM, Ollama, …)
              ↳ optional ilm proxy → routed backend/model (server-side recall, /learn)
```

## Why wakil?

- **Sandboxed by default** — commands run inside a Docker container with a
  read-only rootfs and dropped capabilities. Commands and workspace mutations
  require confirmation by default; read-only tools may run without prompting.
  Docker mode provides convenience-grade isolation, not a security boundary
  for adversarial workloads. (See [SECURITY.md](SECURITY.md).)
- **Model-agnostic** — works with OpenAI-compatible Chat Completions endpoints;
  tested with llama.cpp, OpenRouter, and vLLM. Ollama is expected to work via
  its OpenAI-compatible API. An optional `ilm` proxy adds server-side recall
  and routing but is not required. (See [docs/endpoints.md](docs/endpoints.md).)
- **Remembers across sessions** — built-in per-workspace memory so it recalls
  decisions, architecture, and context from previous sessions. No external
  database required. (See [docs/memory.md](docs/memory.md).)
- **Not just chat** — reads and edits files, runs commands, navigates code
  with LSP, drives a headless browser for visual checks, and delegates to
  subagents for parallel work.

## Quickstart

```sh
# 1. Build — single Go executable
go build -o wakil ./cmd/wakil

# 2. Build the sandbox image (Go, Node, Rust, Python + gopls, baked in)
docker build -t wakil-dev .

# 3. Create a config and add your endpoint
mkdir -p ~/.config/wakil
cat > ~/.config/wakil/config.json << 'EOF'
{
  "endpoints": {
    "local": {
      "kind": "openai",
      "base_url": "http://localhost:8080",
      "model": "qwen3.6-35b"
    }
  },
  "default_endpoint": "local"
}
EOF

# 4. Run — workspace arg is optional, defaults to cwd
./wakil ~/projects/myapp
```

That's it. wakil starts, connects to your model, and you're coding.

**Common endpoints:**

| Provider | `base_url` | Notes |
|---|---|---|
| llama.cpp | `http://localhost:8080` | Local, free |
| OpenRouter | `https://openrouter.ai/api` | Set `auth_header: "Bearer sk-or-..."` |
| Ollama | `http://localhost:11434` | Use model `llama3` |
| vLLM | `http://localhost:8000` | OpenAI-compatible |

> **Tip:** Use an explicit `endpoints` block with `"kind": "openai"` for
> plain OpenAI-compatible servers. The legacy env-var path
> (`ILM_BASE_URL=... ./wakil`) sends proxy-shaped requests — see
> [docs/endpoints.md](docs/endpoints.md).

**No Docker?** Run directly on the host with `--exec direct` — the
confirmation gate is still on.

See [`config.example.json`](config.example.json) for all options, or
[docs/configuration.md](docs/configuration.md) for the full reference.

## Daemon mode

wakil can also run as a long-lived daemon — own session state, serve multiple
clients, and provide a web UI:

```sh
# Start the daemon
./wakil daemon

# Connect a TUI to it
./wakil --remote

# Or run it in Docker
docker compose up -d
```

The daemon serves a built-in web console for browser-based session management,
backend configuration, and live event viewing. Authentication uses session
cookies with SameSite=Strict and origin validation; API tokens and OIDC are
also supported.

See [docs/remote-provisioning.md](docs/remote-provisioning.md) for setup
details and [docs/archive/design/wakild-foundation.md](docs/archive/design/wakild-foundation.md)
for the architecture.

## What it can do

| Feature | What it means |
|---|---|
| **File tools** | Read, edit, search, and navigate files in your workspace |
| **Shell execution** | Run commands inside the sandbox, with `y/n` confirmation by default |
| **Code intelligence** | LSP-backed symbol navigation (go-to-definition, find references, hover) via gopls, designed to support other language servers |
| **Headless browser** | Screenshot pages, inspect DOM, test layouts, emulate reduced-motion — all inside the sandbox |
| **Subagents** | Delegate to parallel workers for discovery, editing, or tool-use tasks |
| **Durable memory** | Per-workspace memory with propose→promote review and provenance — it remembers what it learned |
| **Multi-model counsel** | Get a second opinion from external models on reviews, bugs, or decisions. Destructive operations and counsel remain gated even in auto mode. Keys read at call time, never stored. |
| **Session persistence** | Sessions save automatically; `/resume` to continue, `/handoff` to summarize and start fresh |
| **MCP tools** | Connect stdio or HTTP MCP servers; tools appear automatically |
| **Cost tracking** | Per-source token and cost accounting so you know what you're spending |
| **SSH commit signing** | Sign commits inside the sandbox using your host SSH agent — the key never enters the sandbox |
| **Tracing** | Full JSONL session traces for debugging and reproduction |
| **Context management** | Backend-truth context sizing, `/compact`, and `/maxctx` for capping context on large models |
| **In-sandbox staging** | Fast ephemeral KV store for scratch space and subagent handoffs — snapshots survive sandbox restarts |

## Requirements

| | |
|---|---|
| **Go 1.26+** | to build from source |
| **Docker** | for the default sandboxed exec mode (skip with `--exec direct`) |
| **A model endpoint** | any OpenAI-compatible Chat Completions server — wakil is a client, so you need an inference endpoint to talk to |

## Security

wakil executes shell commands and writes files. The confirmation gate is the
primary defense — every workspace mutation and command execution prompts
`y/n` before it runs. Read-only tools (file reads, search, LSP) may run
without prompting. Destructive operations and counsel calls remain gated even
in auto mode.

Docker mode adds a read-only rootfs, dropped capabilities, and resource
limits, but is **not** adversarial-grade. Direct mode runs on the host with no
container isolation.

Configured model endpoints, optional counsel models, HTTP MCP servers, and
search may send data externally — wakil has no required hosted control plane,
but networked tools you configure can egress. See [SECURITY.md](SECURITY.md)
for the full threat model, data-egress disclosure, and hardening checklist.

> **Running untrusted tasks?** Keep the gate on, do not enable
> `docker_socket`, and audit memory entries (`memory_list`) after operating
> on untrusted content.

## Documentation

| Topic | Document |
|---|---|
| Configuration (flags, env, config fields, endpoints) | [docs/configuration.md](docs/configuration.md) |
| Endpoint kinds, switching, state management | [docs/endpoints.md](docs/endpoints.md) |
| Tools (reference, gating, subagents) | [docs/tools.md](docs/tools.md) |
| TUI (commands, keybindings) | [docs/tui.md](docs/tui.md) |
| `/plan` workflow | [docs/workflows.md](docs/workflows.md) |
| Features (LSP, browser, counsel, search, memory, tracing) | [docs/features.md](docs/features.md) |
| Durable memory design | [docs/memory.md](docs/memory.md) |
| Staging store design | [docs/staging.md](docs/staging.md) |
| Daemon architecture | [docs/archive/design/wakild-foundation.md](docs/archive/design/wakild-foundation.md) |
| Remote provisioning | [docs/remote-provisioning.md](docs/remote-provisioning.md) |
| Security policy and threat model | [SECURITY.md](SECURITY.md) |
| Contributing and PR checklist | [CONTRIBUTING.md](CONTRIBUTING.md) |

## Status

Early-stage. Config keys, session format, and the tool set may change between
commits.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for build/test instructions and the PR
checklist. For security concerns, see [SECURITY.md](SECURITY.md).

## License

Apache License 2.0 — see [LICENSE](LICENSE).
