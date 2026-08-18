# wakīl

[![CI](https://github.com/treeol/wakil/actions/workflows/ci.yml/badge.svg)](https://github.com/treeol/wakil/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/Go-1.26%2B-00ADD8?logo=go&logoColor=white)](https://go.dev)

A terminal-native coding agent. A Go binary that sends HTTP requests to any
OpenAI-compatible Chat Completions endpoint (llama.cpp server, OpenRouter,
vLLM…) or to an optional *ilm* proxy that adds server-side memory recall and
routing. wakil provides the TUI, tool execution, session persistence, and
built-in durable memory — no external services required.

```
you → wakil → configured endpoint
                ├─ OpenAI-compatible model server (llama.cpp, OpenRouter, vLLM, Ollama)
                └─ optional ilm-proxy → routed backend/model
                                     (server-side recall, /learn, backend routing)
```

## Why wakil?

- **Docker sandbox isolation** — convenience-grade isolation with read-only
  rootfs, dropped capabilities, and resource limits. The host Docker socket
  is opt-in and off by default. (See [SECURITY.md](SECURITY.md) for the full
  threat model.)
- **Per-command confirmation gate** — workspace file mutations and command
  execution require confirmation by default; destructive operations and
  counsel calls remain gated even in auto mode. Structured read tools are
  ungated. (See [SECURITY.md](SECURITY.md).)
- **Model-agnostic** — works with OpenAI-compatible Chat Completions
  endpoints tested with llama.cpp, OpenRouter, and vLLM. Ollama is also
  expected to work via its OpenAI-compatible API. No vendor lock-in;
  bring your own local or hosted model. The optional ilm proxy adds
  server-side recall and routing but is not required.
- **Cross-session, per-workspace memory** — built-in SQLite store with
  propose→promote review, provenance tracking, and per-workspace isolation.
  No external database required. (See [docs/memory.md](docs/memory.md).)
- **In-sandbox staging (kvr)** — fast ephemeral KV store backed by a Rust
  UDS server for scratch space and subagent handoffs. Snapshots survive
  sandbox restarts. No external service. (See [docs/staging.md](docs/staging.md).)
- **Subagent delegation** — three capability tiers (discovery / edit / tools)
  with parallel dispatch and a writer lock for edit-tier serialization.
  (See [docs/tools.md](docs/tools.md#subagents).)
- **Multi-model counsel (Mashūra)** — on-demand second opinions from
  external models. Panel, fallback, fusion, and debate modes. Keys read at
  call time. (See [docs/features.md](docs/features.md#counsel-mashūra).)
- **LSP-backed code intelligence** — semantic symbol navigation (definition,
  references, hover, workspace symbols) via gopls, designed to support other
  language servers. (See [docs/features.md](docs/features.md#lsp-code-intelligence).)
- **Headless browser** — visual verification, DOM inspection, and
  interaction testing inside the sandbox. Responsive layout checks,
  `prefers-reduced-motion` emulation. (See [docs/features.md](docs/features.md#headless-browser).)
- **MCP tool integration** — connect stdio or HTTP MCP servers; tools
  appear automatically in the agent's toolset. (See [docs/configuration.md](docs/configuration.md#config-only-fields).)
- **Session persistence and `/handoff`** — sessions save automatically and
  can be resumed. `/handoff` summarizes the current session into durable
  memory and starts a fresh one — rotate sessions without losing context.
  (See [docs/configuration.md](docs/configuration.md#sessions).)
- **Context management** — backend-truth context sizing, compaction
  (`/compact`), and `/maxctx` for capping effective context on large models.
  (See [docs/endpoints.md](docs/endpoints.md#backend-truth-context-sizing).)
- **Per-source cost accounting** — token and cost tracking per inference,
  counsel, and search source. (See [docs/features.md](docs/features.md#cost-sidebar).)
- **SSH commit signing passthrough** — sign commits inside the sandbox
  using the host's SSH agent; the private key never enters the sandbox.
- **JSONL trace capture** — full session tracing for debugging, auditing,
  and reproduction. (See [docs/features.md](docs/features.md#trace-capture).)

## Quickstart

```sh
# 1. Build — single Go binary, no runtime deps
go build -o wakil ./cmd/wakil

# 2. Build the sandbox image (Go, Node, Rust, Python toolchains + gopls, baked in)
docker build -t wakil-dev .

# 3. Create a minimal config and edit it to add your endpoint
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

> **Why an explicit config block?** The legacy env-var-only path
> (`ILM_BASE_URL=... ./wakil`) synthesizes an `ilm-proxy` kind endpoint,
> sending proxy-shaped requests to non-proxy servers. Using an explicit
> `endpoints` block with `"kind": "openai"` is the correct way to target a
> plain OpenAI-compatible server. See [docs/endpoints.md](docs/endpoints.md).

For OpenRouter, use `https://openrouter.ai/api` as `base_url` and set
`auth_header` to `"Bearer sk-or-..."`. For Ollama, `http://localhost:11434`
with model `llama3`.

No Docker? Pass `--exec direct` to run on the host without a container — the
confirmation gate is still on. See [docs/configuration.md](docs/configuration.md).

See [`config.example.json`](config.example.json) for a fully commented
reference covering all options.

## Requirements

| | |
|---|---|
| **Go 1.26+** | to build from source *(see `go.mod`)* |
| **Docker** | for the default `docker` exec mode *(skip with `--exec direct`)* |
| **An OpenAI-compatible endpoint** | a llama.cpp server, OpenRouter, or an ilm proxy — wakil is a client, so an external inference endpoint is required |

## Status

Early-stage. Config keys, session format, and the tool set may change between
commits. The confirmation gate is on by default — the agent can run shell
commands and write files, either inside the Docker sandbox (default) or on
the host (direct mode). Keep it enabled for anything you have not fully
audited.

## Documentation

| Topic | Document |
|---|---|
| Configuration (flags, env, config-only fields, endpoints) | [docs/configuration.md](docs/configuration.md) |
| Endpoint kinds, switching, state management, `/learn` | [docs/endpoints.md](docs/endpoints.md) |
| Tools (tool reference, gating, subagents) | [docs/tools.md](docs/tools.md) |
| TUI (commands, keybindings, SSH clipboard) | [docs/tui.md](docs/tui.md) |
| `/plan` workflow (phases, oracle review) | [docs/workflows.md](docs/workflows.md) |
| Features (LSP, browser, counsel, search, memory, tracing) | [docs/features.md](docs/features.md) |
| Durable memory design | [docs/memory.md](docs/memory.md) |
| Staging store design | [docs/staging.md](docs/staging.md) |
| Remote provisioning | [docs/remote-provisioning.md](docs/remote-provisioning.md) |
| Security policy and threat model | [SECURITY.md](SECURITY.md) |
| Contributing and PR checklist | [CONTRIBUTING.md](CONTRIBUTING.md) |

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
Dockerfile         sandbox image — Go, Node, Rust, Python toolchains, gopls, docker CLI + compose, gh, golangci-lint
```

## Security

wakil executes shell commands and writes files. The confirmation gate is the
primary defense — every workspace mutation and command execution prompts `y/n`
before it runs. Docker mode provides convenience-grade isolation (read-only
rootfs, dropped capabilities, resource limits) but is **not** adversarial-grade.
Direct mode runs on the host with no container isolation.

> **Running untrusted tasks?** Keep the gate on, do not enable
> `docker_socket`, and audit memory entries (`memory_list`) after operating
> on untrusted content. See [SECURITY.md](SECURITY.md) for the full threat
> model, data-egress disclosure, and hardening checklist.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for build/test instructions and PR
checklist. For security concerns, see [SECURITY.md](SECURITY.md).

## License

Apache License 2.0 — see [LICENSE](LICENSE).
