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
| `--exec` *(deprecated)* | `ILM_EXEC_MODE` / `WAKIL_EXEC_MODE` | — | deprecated alias of `--sandbox` (`docker` \| `direct`); prints a one-line warning. Config key `exec_mode` likewise maps onto `sandbox.backend`; an explicit contradiction (e.g. `exec_mode: direct` + `sandbox.backend: hisn`) is an error |
| `--sandbox` | `WAKIL_SANDBOX_BACKEND` | `docker` | sandbox backend: `docker` \| `hisn` \| `direct` (no sandbox — commands run on the host). Unknown values are an error (no silent fallback). See [Sandbox backends](#sandbox-backends) |
| `--sandbox-image` *(alias `--image`)* | `ILM_CONTAINER_IMAGE` / `WAKIL_IMAGE` | image pinned into the release binary *(`ghcr.io/treeol/wakil-sandbox@sha256:…`; see `wakil version`)*; dev builds fall back to `wakil-dev` with a warning | sandbox image. Resolution order: flag → env → config `sandbox.image` → legacy `image` → pinned digest → dev fallback. Release binaries pull the pinned image on first run. Build your own with `docker build -t wakil-dev .` |
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
| `--print-workspace-key` | — | — | print the workspace path, memory-store key, workspace key, sessionhost id/key and what `wakil plan gc` would resolve from this directory, each with its source (cwd / `WAKIL_WORKSPACE_PATH` / config / `WAKIL_WORKSPACE_KEY`), then exit. Read-only; creates no files |
| `--all` | — | — | with `--resume`/`--list-sessions`: search all workspaces |
| `--ssh-signing` | — | `off` | SSH commit signing in the sandbox: `off`\|`auto`\|`<path>`. Under `sandbox.hisn`: `auto` resolves the key **per repo, on the host, at plan time** — the repo's effective `commit.gpgsign`/`user.signingkey` decide (repo-local config wins, exactly as a host commit would); `commit.gpgsign` not true → no signing injected; `true` with an unset or unloadable key fails the plan. `sandbox.hisn.ssh_keys` gates only agent *forwarding*, not signing |
| `--config` | `WAKIL_CONFIG` | `~/.config/wakil/config.json` | JSON config file path |
| `--context-mode` | `WAKIL_CONTEXT_MODE` | `long-session` | context mode: `standard` (full history sent) \| `long-session` (collapse finished requests to question + answer + note); persisted per repo, beats the config file |

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
the global `api_key`) and optional `temperature` / `top_p` / `max_tokens` /
`tool_choice` — omitted from the request body entirely when unset, so server
defaults stay authoritative. `tool_choice` is the OpenAI Chat Completions
field sent verbatim (only on requests that carry tools); set it to `"auto"`
for endpoints whose tool_choice default is `"none"` — e.g. IO Intelligence
by io.net — so tool calls fire. The documented values are the OpenAI string
forms (`"auto"`, `"none"`, `"required"`); the field is not
validated — an empty string is sent as-is, like any other value. It is
honoured by any endpoint kind that sends the shared Chat-Completions wire
body. For `openai`-kind endpoints, `app_title`
sets the OpenRouter `X-Title` attribution header; when unset, it defaults to
`wakil` for openrouter.ai hosts and is omitted for any other host. Set to `""` to opt out.
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
| `mashura_models` | — | Per-model reviewer limits, keyed by `"provider:model"` or the bare model id: `context_tokens` (context window; default from the OpenRouter registry / built-in table, 128000 if unknown), `review_max_tokens` (answer budget; default `mashura_max_tokens`, never below 4096 for a review, and reduced to what fits: `min(configured, context − input − 512)`), and `reasoning` (per-model reasoning/thinking spec: `{"effort": "low\|medium\|high\|none", "max_tokens": N}` — when set, `max_tokens` on the wire is additive: reasoning budget + answer budget; the answer budget is never reduced). Mashūra never truncates what it sends: a document that fits the context window (90% of it, estimated at 3 bytes/token) is sent whole; a larger one is split at markdown headings into labelled "part i of n" calls with ~200 tokens of overlap and the findings merged; one that still cannot fit (a single source file or section larger than a part) fails with an error and nothing is sent. A reply cut at max_tokens is retried once chunked, then marked **VOID**; each reply must end with `LAST SECTION READ: <heading>` matching the part's last heading, else it is VOID. A VOID reply is shown as such and never counts as a review round or as "no findings" |
| `mashura_roles` | — | Per-role overrides for Mashūra counsel reasoning, keyed by role name (`"reviewer"` or `"check"`). When a role's `reasoning` is set, it applies to all calls dispatched under that role. Per-model `reasoning` (in `mashura_models` above) takes precedence for the reviewer role. For the check role, the role-level setting is the only source. Example: `{"reviewer": {"reasoning": {"effort": "medium", "max_tokens": 4096}}, "check": {"reasoning": {"effort": "low"}}}` |
| `oracle_timeout_seconds` | `0` (→ 300) | Total cap for a single Mashūra call (idle timeout: 120s). `0` = use built-in default (30min). |
| `mashura_tool_panels` | — | Maps each counsel tool to a named panel |
| `mashura_mode` | — | Default oracle consult schedule: `every-step` \| `on-deviation` \| `phases-only` *(alias: `wf_oracle_mode`)* |
| `skill_creation` | — | Automatic skill creation (`/skill-creation`); object with the keys below |
| `skill_creation.mode` | `"off"` | Default mode: `off`, `on` (a status notice suggests a skill; never added to tool results), `auto` (builds drafts), `auto-mashura` (Mashūra review, then activates new skills; setting it here is the standing opt-in to send sanitized drafts to the Mashūra panel). Numeric `skill_creation.*` limits must be >= 0 (0 = default) and `allowed_files` globs must be valid, or the config is rejected at load. A per-repo `/skill-creation` setting and a per-session override take precedence | *(0.4.4: default changed from `on`; automatic skill creation is deprecated and will be removed in a future release)*
| `skill_creation.max_auto_per_day` | `3` | Cap on automatically triggered runs per UTC day |
| `skill_creation.max_drafts` | `10` | Pending drafts allowed; further runs stop until drafts are reviewed |
| `skill_creation.draft_ttl_days` | `30` | Pending drafts older than this are rejected |
| `skill_creation.review_timeout_seconds` | `180` | Wall-clock bound for one whole pipeline run |
| `skill_creation.max_review_bytes` | `122880` | Largest draft that may be built / sent to Mashūra |
| `skill_creation.max_capture_files` | `12` | Files one automatic capture may include |
| `skill_creation.panel` | — | Mashūra panel used for skill reviews (default panel when empty) |
| `skill_creation.allowed_files` | `*.yml *.yaml *.sh *.sql *.conf *.md Dockerfile Dockerfile.*` | Base-name globs of capturable files. `.env*`, `*.pem`, `*.key`, `*secret*`, `*credential*`, `*token*`, `*id_rsa*` are never captured |
| `oracle_enabled` | `false` | Gate for `mashura__*` counsel tools |
| `oracle_model` | `"claude-sonnet-4-6"` | Model ID for counsel calls |
| `oracle_api_key_env` | `"ANTHROPIC_API_KEY"` | Env var read at call time for the API key |
| `mashura_fallback_model` | `""` | Explicitly prefixed fallback model (e.g. `"openrouter:anthropic/claude-sonnet-4"`) used when no `default` panel is configured. If empty, auto-detects based on which API key is set. Must include provider prefix. |
| `lsp_enabled` | `false` | Gate for `lsp_*` code-intelligence tools |
| `lsp_servers` | — | Maps language → server command |
| `style_guides_enabled` | `false` | Enable built-in and user-configured language style guides |
| `style_guide_paths` | `{}` | Map language ID → replacement or custom host-side guide file; paths relative to the config file, `~/` expanded |
| `browser_enabled` | `false` | Gate for `browser_*` headless-browser tools (chromedp + Chromium). Forced off (with a startup line) under `sandbox.backend: hisn` |
| `mcp_servers` | — | MCP tool servers (stdio or HTTP) |
| `backend` | `""` | Default `X-Ilm-Backend` (ilm-proxy only) |
| `external_backends` | — | Backend names known to route to external providers |
| `aux_model` | `""` | Pins `X-Ilm-Aux-Model` (empty = follow main) |
| `kvr_disabled` | `false` | Disable the staging KV store *(auto-disabled in direct mode)* |
| `kvr_max_entries` | `100000` | Max entries in the staging store |
| `kvr_sweep_interval_secs` | `30` | Staging TTL sweep frequency *(expired entries are already hidden on read; the sweep only reclaims memory)* |
| `kvr_snapshot_interval_secs` | `300` | Staging snapshot frequency *(survives sandbox restarts)* |
| `docker_caps` | `[]` | Linux capabilities to re-add after `--cap-drop=ALL` *(e.g. `["CHOWN"]` if go build fails)* |
| `docker_memory` | `"4g"` | Container memory limit; must be ≥ `docker_tmpfs_size` *(tmpfs counts against the cgroup)* |
| `docker_pids_limit` | `512` | Max processes in the sandbox container *(0 = no limit)* |
| `docker_tmpfs_size` | `""` (→ 4g) | Size of the sandbox `/tmp` tmpfs *(empty uses built-in default)* |
| `docker_io_uring` | `false` | Enable io_uring via custom seccomp profile *(increases kernel attack surface — see [SECURITY.md](../SECURITY.md#seccomp))* |
| `sandbox.docker.allow_userns` *(legacy alias `docker_allow_userns`)* | `false` | Let unprivileged user namespaces work inside the sandbox container (hisn development): Wakil's seccomp profile variant that allows `unshare`, `clone`/`clone3` with `CLONE_NEW*`, `setns`, `mount`, `umount2`, `pivot_root`, `open_tree`, `move_mount`, `mount_setattr`, `fsopen`/`fsconfig`/`fsmount`/`fspick` for the unprivileged uid (everything else unchanged — never `seccomp=unconfined`), plus `--security-opt apparmor=unconfined` and `--security-opt systempaths=unconfined` (unmasked `/proc`, needed to mount procfs in the userns). `no-new-privileges` and `--cap-drop=ALL` stay. *Weakens the container's isolation; startup prints a one-line warning when set* |
| `agent_prompt_path` | `agent.txt` next to config | System prompt file path |
| `backend_max_retries` | `3` | Max retries for transient backend failures (unattended) |
| `compact_at_frac` | `0.75` | Compact at 75% of effective context |
| `keep_bytes_frac` | `0.60` | Keep 60% of effective context verbatim after compaction |
| `hard_max_frac` | `0.95` | Hard ceiling at 95% of effective context |
| `context_capacity_frac` | `0.80` | Use 80% of proxy's usable_ctx as working budget |
| `effective_ctx_max_chars` | `0` (disabled) | Absolute cap (chars) on effective context for large models. Apply to keep context within a working budget (e.g. `200000`); models with very large advertised context may degrade in practice below their nominal limit. Applied before fractions. Override at runtime with `/maxctx` |
| `reasoning_budget_tokens` | `4096` | Tokens reserved for extended thinking |
| `answer_margin_tokens` | `4096` | Tokens reserved for the final answer |
| `context_tokens_fallback` | `131072` | `n_ctx` assumed when the backend context fetch fails |
| `keep_bytes` | `120000` | Max bytes of verbatim turns kept after compaction (absolute fallback when `n_ctx` unknown) |
| `summary_bytes` | `20000` | Cap on the running summary; re-summarize if exceeded; `0` = unlimited |
| `hard_max_bytes` | `160000` | Unconditional context ceiling (absolute fallback); `0` = disabled |
| `compact_at` | `145000` | Trigger compaction at this size (absolute fallback); `0` → use `max_chars` |
| `turn_tool_budget` | `40000` | Per-turn cumulative tool output budget |
| `tool_result_cap` | `8000` | Max chars kept in context per tool result; `0` = unlimited |
| `tool_result_ttl` | `3` | Evict large tool results after N completed turns; `-1` = never |
| `tool_cache_retention_days` | `30` | Once per process at startup (TUI and daemon), delete per-chat tool-cache dirs (spilled results, collapse archives, subagent summaries) whose newest file is older than N days; `0` = never. A session resumed later than that keeps its transcript, but its `full content at:` pointers will be gone (the chat resumed at launch via `--resume` is exempt) |
| `context_mode` | `"long-session"` | Context mode: `long-session` (default; after a request finishes, its working set is archived to disk and no longer sent; later requests see only the user message + final answer + a one-line note) \| `standard` (full history sent). Persisted per repo (beats the config file); flag `--context-mode`, env `WAKIL_CONTEXT_MODE`; `long` is an alias for `long-session`. Subagents always run in `standard` |
| `context_collapse_min_bytes` | `2000` | Min working-set size for a finished request to be collapsed in `long-session` mode; smaller turns aren't worth a note |
| `context_tail_turns` | `20` | In `long-session` mode, requests older than the N most recent degrade to a single pointer line (archive path + memory key); `0` = never degrade (sent size grows with session length) |
| `context_tail_answer_bytes` | `0` | Optional cap (default off: prior assistant reports are kept whole) on the final answer (including truncation marker) retained for tail turns in `long-session` mode; a longer answer is truncated rune-safe (the archive path in the collapse note below it is the recovery reference and is not repeated). Values below ~5 bytes are treated as disabled. The question (U), the collapse note, and degraded pointer turns are unaffected; `/keep`-exempt turns bypass collapse entirely. `0` (default) = keep full answers. This bounds the answer component of the tail, not the total context: U, notes, and uncollapsed turns are not capped by this key |
| `prompt_mode` | `"full"` | Prompt mode: `full` (complete system prompt always loaded; default) \| `core` (lean preamble with identity + safety gates only; the workflow extension is lazy-loaded as a second system message when the classifier detects a task). Persisted per repo; flag `--prompt-mode`, env `WAKIL_PROMPT_MODE`. A custom `agent_prompt_path` bypasses the split (the full file is used) |
| `max_tool_iterations` | `0` | Hard cap on tool round-trips per turn; `0` = unlimited |
| `browser_path` | `""` | Override Chromium/Chrome binary path *(empty = search PATH)* |
| `browser_idle_timeout_seconds` | `0` (→ 600) | Stop the headless browser after this many seconds without a `browser_*` call; the next call starts a fresh one. The browser is also stopped on `/new`, chat end and exit. It is started lazily on the first call, never with the container |
| `openrouter_api_key_env` | `"OPENROUTER_API_KEY"` | Env var for the OpenRouter API key *(used by mashura panel calls routed through OpenRouter)* |
| `subagent_max_tool_iterations` | `0` (→ 40) | Cap on tool round-trips per subagent dispatch; `0` = use built-in default |
| `subagent_turn_tool_budget` | `0` (→ 120000) | Per-turn cumulative tool output budget for subagents; `0` = use built-in default *(an operator cap on the class-based work budget: lowers it, never raises it)* |
| `subagent_tool_result_cap` | `0` (→ 12000) | Per-result char cap for subagents; `0` = use built-in default |
| `subagents.budget.read` | `0` (→ 40) | Budget % for `read` class subagents; `0` = use built-in default |
| `subagents.budget.search` | `0` (→ 25) | Budget % for `search` class subagents; `0` = use built-in default |
| `subagents.budget.edit` | `0` (→ 60) | Budget % for `edit` class subagents; `0` = use built-in default |
| `subagents.budget.verify` | `0` (→ 40) | Budget % for `verify` class subagents; `0` = use built-in default |
| `subagents.budget.reserve` | `0` (→ 8) | Reserve % carved out of every class budget (work budget = hardMax × (class − reserve) / 100); every effective class % must be greater than it; `0` = use built-in default |
| `stream_idle_timeout_seconds` | `0` (→ 120) | Max seconds between SSE bytes from the backend before the stream is declared stalled and retried; `0` = use built-in default |
| `bg_shell_timeout_seconds` | `0` (→ 3600) | Watchdog timeout for `notify_on_exit` background shells (async ops); `0` = use built-in default |
| `hooks` | — | Lifecycle hooks block (pre_tool, post_tool, session_start, session_end, on_stop) — see [features](features.md#lifecycle-hooks) |
| `verify` | — | Commands for `/verify` workflow verification (auto-detected from manifests if unset) |
| `output_mode` | `"debug"` | TUI verbosity: `debug` shows all conversation items; `simple` hides transient diagnostics and the live reasoning body *(env `WAKIL_OUTPUT_MODE`, flag `--output-mode`)* |
| `clipboard_mode` | `"auto"` | TUI copy transport: `auto` tries native clipboard tools (`wl-copy`/`xclip`/`xsel`/`pbcopy`/`clip.exe`) then OSC 52; `native` never emits OSC 52; `osc52` never runs a local tool *(env `WAKIL_CLIPBOARD_MODE`)*. Note: OSC 52 delivery depends on the terminal/tmux (`set -g set-clipboard on`) and cannot be verified by the app — the status hint says "unverified" |
| `ilm_stack` | — | ILM shadow-mode emitter block (`endpoint`, `token`, `mode`, `queue_path`, `batch_ms`, `max_output_bytes`). `mode=off` (default): zero behaviour change. `mode=shadow`: emit session/turn/tool events to an ILM-stack endpoint via a local durable queue + background sender. Env vars: `ILM_STACK_URL`, `ILM_STACK_TOKEN`, `ILM_MODE`, `ILM_QUEUE_PATH`, `ILM_BATCH_SIZE`, `ILM_OUTPUT_LIMIT`. |

The absolute byte fields (`keep_bytes`, `summary_bytes`, `hard_max_bytes`, `compact_at`,
`turn_tool_budget`, `tool_result_cap`, `tool_result_ttl`, `max_tool_iterations`,
`context_collapse_min_bytes`) are
fallbacks used when the backend's context window is unknown. When `n_ctx` is known, the
fraction-based fields above override them automatically.

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

## Language style guides

Set `style_guides_enabled` to `true` to opt in. Wakil includes concise guides for
Go, Python, TypeScript, and JavaScript. These advise on coding style only; they
do not run formatters, install tools, change dependencies, or override safety
rules, your current request, repository `AGENTS.md` instructions, or project
formatting/lint configuration.

To replace a built-in guide or add another language, configure a host-side file:

```json
{
  "style_guides_enabled": true,
  "style_guide_paths": {
    "go": "styles/go.md",
    "python": "~/Documents/python-style.md",
    "rs": "styles/rust.md"
  }
}
```

A configured path **replaces**, rather than supplements, the embedded guide for
that language. Relative paths resolve beside the selected config file (usually
`~/.config/wakil/config.json`), not the workspace; `~/` expands to your home
directory. Files are read once at startup and reloaded when Wakil is restarted.
They must be regular, nonempty UTF-8 files that fit the rendered-message limit;
an unreadable, invalid, or oversized configured file causes a startup error.
Symlinks to regular files are followed. Paths are host paths, including in
Docker mode.

Common language keys are canonicalized: `rs` and `rust` both identify a Rust
guide, recognize the names `rust` and `rs`, and match `.rs` files. Do not
configure both keys; that is a duplicate-language startup error. Other custom
keys retain their language-name and filename-extension behavior. Built-in
aliases cover `golang`, `py`/`python`, `js`/`javascript`, and
TypeScript/JavaScript filename variants including `.tsx` and `.jsx`.

`/styles` deterministically lists every successfully loaded guide, its source
(`embedded` or `user`), and whether it is active, available, disabled, or
selected but omitted by a limit. It reads startup state; it does not search
skills, invoke a model, or reload files. `/styles enable <language>` explicitly
selects a guide for the session, `/styles disable <language>` suppresses its
sticky and path activation, and `/styles enable|disable all` lifts or applies
the session-wide opt-out. `all` resumes automatic selection rather than
selecting every guide at once. These controls never change configuration.

Explicitly named languages in coding requests stay selected for the session.
File paths used by successful `read_file`, `read_file_full`, `write_file`, and
`edit_file` calls select a guide only for that request; the selection affects
the next model call, not an edit already generated. At most two guides and 4096
rendered bytes are active. When two explicit guides fill the selection, later
languages are omitted with a notice rather than replacing an explicit choice.

Guides are rebuilt as a transient system message on each model request. They
are **not** saved in conversation history, collapse archives, or summaries, so
compaction and long-session collapse cannot discard the active selection. New
or loaded sessions start with no selection; subsequent requests activate guides
again. Subagents inherit the active guides and may add their own, without
modifying the parent session. `Disable the Python guide` (or another configured
language) suppresses that language's sticky and path activation; `Enable the
Python guide` restores it. `Disable style guides` suppresses all activation for
the current session; `Enable style guides` allows subsequent activation again.

Style bytes count toward context limits; Wakil returns an explicit error rather
than delivering an over-limit request.

## Reasoning defaults

Wakil applies built-in default reasoning effort levels when no explicit
setting is configured (not via `mashura_roles`/`mashura_models`, not via
`/thinking`, not via `--think`):

| Role | Default effort | Default max_tokens |
|---|---|---|
| Main model | `medium` | (none — effort only) |
| Mashūra reviewer | `medium` | `8192` |
| Mashūra check | `none` (disabled) | — |
| Subagent | `low` | (effort only) |

These defaults are behavior changes for all users: the main model now sends
`reasoning: {effort: "medium"}` on every request to a reasoning-capable
endpoint (OpenRouter, Anthropic thinking models). Non-reasoning models ignore
the parameter. Use `/thinking off` or `--think off` to disable reasoning
explicitly. Use `mashura_roles` to override the Mashūra defaults.

A reviewer never gets more than 16384 reasoning tokens (the `max` effort
level maps to 16384 in the `EffortToBudget` table).

## Execution modes

Tool calls run inside one persistent Docker container for the process
lifetime by default. The workspace directory — positional arg, or cwd if
omitted — bind-mounts into the container at `/mnt/<dirname>`. `--exec direct`
runs on the host instead, no container.

## Sandbox backends

`sandbox.backend` selects where tool calls run: two sandbox backends exist, plus
`direct` (no sandbox, explicit opt-out). `docker`
(default) is the production sandbox and is unchanged. `hisn`
([github.com/treeol/hisn](https://github.com/treeol/hisn) >= 0.2.1) is a
rootless, daemonless alternative. Design, path model, lifecycle and known
limits: [sandbox-backends.md](sandbox-backends.md).

| Key | Default | Meaning |
|---|---|---|
| `sandbox.backend` | *unset* → **hisn when usable, else docker** | `docker` \| `hisn` \| `direct`. Unset means: hisn if no backend is chosen anywhere (file, env, flag, `exec_mode`), `hisn check` passes and hisn >= 0.3.1 is in `PATH`; otherwise docker, with the reason in the startup banner. Setting it pins the choice. Flag `--sandbox`, env `WAKIL_SANDBOX_BACKEND`. Replaces the deprecated top-level `exec_mode` |
| `sandbox.hisn.binary` | `hisn` | hisn executable (name on `PATH` or absolute path) |
| `sandbox.hisn.home_root` | `<state dir>/hisn-home/<workspace-hash>` | persistent host directory used as the sandbox `$HOME` *(absolute path)* |
| `sandbox.hisn.home` | `persist` | `persist`: the home directory is kept across chats (`[home]`, hisn >= 0.3.3; older hisn keeps the 0.4.x mount). `tmpfs`: a fresh tmpfs `$HOME` per session (needs hisn >= 0.3.3; a hard error on older hisn). Project override: `.wakil/sandbox.json` `"hisn": {"home": "tmpfs"}` |
| `sandbox.hisn.rootfs` | *unset* → host toolchain | session root filesystem (hisn >= 0.3.3): a rootfs store name such as `wakil-dev` (import with `hisn rootfs import wakil-dev --from-image wakil-dev`) or an absolute directory. Project override: `.wakil/sandbox.json` `"hisn": {"rootfs": "<name>"}` (name only). Cannot be combined with `hisn.profile` |
| `sandbox.hisn.grantable_rw` | `[]` | absolute host prefixes under which read-write grants outside the workspace are allowed *(fixed at session start; cannot be `/`)* |
| `sandbox.hisn.env_pass` | `GOFLAGS GOCACHE GOMODCACHE CARGO_HOME RUSTFLAGS npm_config_cache PIP_CACHE_DIR CI` | environment variable names copied into the sandbox; nothing else of the host environment enters. Names that alter host process startup (`LD_*`, `PATH`, `HOME`, `GODEBUG`, …) are rejected |
| `sandbox.hisn.tmp_size_mb` | `1024` | Size (MiB, 16..=65536) of the session-private `/tmp` tmpfs *(hisn 0.2.1 `session.tmp_size`)* |
| `sandbox.hisn.shm_size_mb` | `64` | Size (MiB, 1..=65536) of `/dev/shm` *(Wakil always sets `session.shm = true`)* |
| `sandbox.hisn.kvr_binary` | `kvr-server` from `PATH` | kvr-server executable run INSIDE the sandbox as the staging service *(hisn >= 0.3.1; must live under a directory the sandbox sees: `/usr`, `/opt`, `/nix` or a project `mounts_ro`; otherwise staging stays off and the banner says why — there is no host-side kvr)* |
| `sandbox.hisn.exec_via_cli` | `false` | Run tool-call execs through the `hisn exec` CLI (one client process per call) instead of the control-socket client. Fallback, one release |

With an explicit `hisn`: startup requires `hisn --version` >= 0.2.1 (>= 0.3.1 for profiles, services and risk labels) and `hisn check` exit
0, otherwise Wakil refuses to start (no fallback). Per-project additions (services, forwards, mounts, `risk_ack`, named profile) live in `.wakil/sandbox.json`, which only takes effect after `wakil sandbox trust`; see [sandbox-backends.md](sandbox-backends.md). The workspace is mounted at
its identical host path (`--workdir` is rejected); `kvr` staging runs as an in-sandbox service when a `kvr-server` binary is available (see `sandbox.hisn.kvr_binary`);
`browser_enabled` is forced off with one startup line (so one config serves
both backends), and the deprecated `exec_mode: direct` conflicts with it. The
startup line `sandbox: backend=<b> · browser tools: on|off` states the active
backend and whether browser tools are available. One hisn session per chat,
stopped on `/new`, `/resume`, handoff, `/rewind` and exit.

## Sessions

Saved automatically, no flag required. `wakil --resume` picks up the most
recent one; `wakil --resume-id <prefix>` targets a specific `chat_id`.

`/handoff` summarizes the current session, stores the summary in durable
memory, and starts a fresh session with a continuation prompt — so you can
rotate sessions without losing context. The old session remains on disk;
`/resume <id>` returns to it.
