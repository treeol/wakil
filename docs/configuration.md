# Configuration

Precedence: **defaults < config file < env < flags**. Config file is JSON at
`~/.config/wakil/config.json`, overridable via `WAKIL_CONFIG` / `--config`.
**Run `wakil init` to create it** (see [Setup: `wakil init`](#setup-wakil-init));
since 0.6.3 wakil no longer writes a stub file on first start. Env vars use
`WAKIL_*` (preferred) or `ILM_*` (legacy aliases, same precedence).

## Setup: `wakil init`

`wakil init` creates the config without hand-editing JSON. On a terminal it asks;
otherwise it takes flags. Steps: provider (`openrouter` or `openai-compatible`,
e.g. Ollama at `http://localhost:11434`), credentials, model (listed from the
endpoint's model list, at most 15, tool-capable first, with context length and
price per 1M tokens when known), one **test call** with a trivial tool (success
means a tool call came back), approval mode, and the sandbox probe.

- **The key is never a flag.** It enters only as hidden terminal input (init
  refuses to read it if echo cannot be turned off) or through an environment
  variable named with `--key-env NAME`, in which case the file stores only the
  name (`api_key_env`).
- Failures of the test call are one-liners: `401/403` key rejected; `404` with
  OpenRouter's `free-model-training-violation` means your OpenRouter privacy
  settings exclude the model's only provider (allow it at
  <https://openrouter.ai/settings/privacy> or pick another model); other `404`
  model not found; `429` rate limited (free models: 50 requests/day without
  credits); a reply without a tool call means tool calling may be unsupported.
  Interactively you may pick another model, save anyway, or abort.
- An existing config is **never touched** unless `--force`; then it is first
  copied to `config.json.bak-<UTC timestamp>` (mode 0600).
- The file holds only what you chose (`endpoints.<name>`, `default_endpoint`,
  `approval`, and `sandbox.backend` only when you did not take the automatic
  choice), mode 0600 in a 0700 directory. Other config keys keep their defaults.

Flags for scripts: `--provider openrouter|openai-compatible`, `--base-url URL`,
`--model ID`, `--key-env NAME`, `--approval ask|auto`,
`--sandbox hisn|docker|direct`, `--skip-test`, `--force`, `--yes` (accept
defaults; also save when the test call fails, with a warning). Example:

```sh
OPENROUTER_API_KEY=... wakil init --provider openrouter --key-env OPENROUTER_API_KEY --yes
```

Without any config, `wakil` on a terminal asks "No config found. Run setup now?
[Y/n]"; without a terminal it prints `no config: run `wakil init``.

### `wakil config show`

Prints the effective configuration with each value's source (`default`, `file`,
`env`, `flag`). Secrets (`api_key`, `auth_header`) are never shown, not even in
part: the row reads `set (<N> chars)` or `unset`. `--json` gives machine output. Flags that wakil itself takes
(`--config`, `--model`, `--sandbox`, ...) may follow, so you can see what they
change. Nothing is written.

## Flags and environment variables

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `--base-url` | `ILM_BASE_URL` / `WAKIL_BASE_URL` | — *(required unless `endpoints` is set)* | endpoint base URL; overrides the selected endpoint's `base_url` |
| `--api-key` | `ILM_API_KEY` / `WAKIL_API_KEY` | — | sent as `Authorization: Bearer <key>` *(endpoint-level `auth_header` wins)* |
| `--model` | `ILM_MODEL` / `WAKIL_MODEL` | `ilm` | model name; overrides the selected endpoint's `model` |
| `--exec` *(deprecated)* | `ILM_EXEC_MODE` / `WAKIL_EXEC_MODE` | — | deprecated alias of `--sandbox` (`docker` \| `direct`); prints a one-line warning. Config key `exec_mode` likewise maps onto `sandbox.backend`; an explicit contradiction (e.g. `exec_mode: direct` + `sandbox.backend: hisn`) is an error |
| `--sandbox` | `WAKIL_SANDBOX_BACKEND` | *unset* → hisn when usable, else `docker` | sandbox backend: `docker` \| `hisn` \| `direct` (no sandbox — commands run on the host). Unknown values are an error (no silent fallback). See [Sandbox backends](#sandbox-backends) |
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
| `--trace` | `WAKIL_TRACE_SESSIONS` | `false` | enable JSONL trace capture for this session; also writes the phase timeline (below) |
| — | `WAKIL_TRACE` | `false` | `1`/`true`/`yes`/`on`: write the phase timeline for this run — milliseconds per phase of start, `/new`, `/resume`, `/handoff` and exit — to `timeline-<UTC stamp>-<pid>.json` in the trace directory (`--trace-dir`, `WAKIL_TRACE_DIR`, default `~/.local/share/wakil/traces`). Read before the config is loaded, so config load is timed. `trace_sessions` does **not** enable it. Off, it costs one nil check per phase. See [PERF-0.6.md](PERF-0.6.md) |
| `--trace-dir` | `WAKIL_TRACE_DIR` | `~/.local/share/wakil/traces` | directory for trace files |
| — | `WAKIL_DEBUG_REQUESTS` | *unset* | debugging aid: when set to a directory path, every model request body (messages and tool definitions, exactly as sent) is written there, one JSON file per request named `<session id>-NNNNNN-<timestamp>.json` (0600 files in a 0700 directory). **Warning: the files contain the full session content.** The dump never carries an API key or any request header — only the body. If writing fails, one warning is logged and the request continues. Unset means no dump and no cost. The same variable also enables a **panel capture** for Mashūra/counsel calls: one 0600 file per call named `panel-<label>-NNNNNN-<timestamp>.json` holds the request body as sent, the served model, the HTTP status, every raw usage object in order, and the last three raw stream lines before `[DONE]`. Headers and the API key are never written. |
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

Credentials per endpoint, in order of precedence: `auth_header` (the verbatim
`Authorization` value, e.g. `"Bearer sk-or-..."`; an explicit `auth_header`
always wins), then `api_key_env` (the **name** of an environment variable,
read at request time and sent as `Bearer <value>`; the key never has to be in
the file; the variable must be set whenever you run wakil), then the legacy
top-level `api_key`.

`approval` (top level, `"ask"` default or `"auto"`) is the standing approval
mode: `"auto"` is the same as `--auto` for the interactive TUI. **`wakil run`
(headless) ignores it**: unattended runs never widen their approvals from a file
written for interactive use. `wakil init` writes it.

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
      "api_key_env": "OPENROUTER_API_KEY"
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
hardcoded project defaults (`https://wakiil.pro` and `cli-agent`)
for openrouter.ai hosts and are **not** user-configurable — wakil always
identifies itself. OpenRouter requests also carry two body fields — `session_id`
(the session's chat id; each subagent's own) for session stickiness, and
`plugins: [{"id":"context-compression","enabled":false}]` so OpenRouter never
silently drops middle messages. Both are OpenRouter-host only; every other host
sends byte-identical requests. Costs: when the provider reports `usage.cost`,
that is the billed amount for the request; the local price table stays the
fallback. `/mode status` shows the session totals (billed, provider-reported vs
estimated, cache-read share, and the main / subagents / Mashūra / aux split).
**Calls that fail mid-stream are recorded from the published usage of that
request** (exact usage when the provider sent it, otherwise the provisional
input estimate marked estimated, unpriced when no price is known). Nothing
is recorded for user cancels, policy refusals before sending, or HTTP error
statuses before stream data.

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
| `mashura_models` | — | Per-model reviewer limits, keyed by `"provider:model"` or the bare model id: `context_tokens` (context window; default from the OpenRouter registry / built-in table, 128000 if unknown), `review_max_tokens` (answer budget; default `mashura_max_tokens`, never below 4096 for a review, and reduced to what fits: `min(configured, context − input − 512)`), and `reasoning` (per-model reasoning/thinking spec: `{"effort": "low\|medium\|high\|none", "max_tokens": N}` — when set, `max_tokens` on the wire is additive: reasoning budget + answer budget; the answer budget is never reduced). Mashūra never truncates what it sends: a document that fits the context window (90% of it, estimated at 3 bytes/token) is sent whole; a larger one is split at markdown headings into labelled "part i of n" calls with ~200 tokens of overlap and the findings merged; one that still cannot fit (a single source file or section larger than a part) fails with an error and nothing is sent. A reply missing/wrong `LAST SECTION READ: <heading>` citation (matching the part's last heading) but with non-empty review content is accepted and flagged coverage not confirmed (an obtained review, not VOID); VOID remains only for empty, citation-only or cut-at-limit replies (cut off with reasoning ≥80% of total → reason `reasoning used the budget`). VOID replies are retried by cause at most once and at most 2 attempts per member per part: limit → doubled total answer budget (bounded by model limits/context) + reasoning one step down; empty → reasoning one step down; citation/other → no retry; 429/5xx keep a single retry; never an identical request again. A VOID reply is shown as such (uniform `VOID <model> cause=<c> finish=<f> reasoning=<n> answer=<n> attempts=<n> cost=<usd|unpriced>` line, one per-operation cost line, and a `panel-void-<label>-*.json` debug capture with cause/finish/tokens/attempts/cost/text length/coverage flag/budget stop, no headers/keys) and never counts as a review round or as "no findings" |
| `mashura_roles` | — | Per-role overrides for Mashūra counsel reasoning, keyed by role name (`"reviewer"` or `"check"`). When a role's `reasoning` is set, it applies to all calls dispatched under that role. For both roles, the model resolved for the call contributes its per-model `reasoning` (in `mashura_models` above) as the base, and the role setting overrides the effort (and, when set, the max_tokens); the role-level setting is used when the model sets none. Built-in defaults when unset: reviewer = `medium` (max_tokens 8192), check = `low` (no max_tokens). Answer floor is 4096 for all counsel tools including check. Example: `{"reviewer": {"reasoning": {"effort": "medium", "max_tokens": 4096}}, "check": {"reasoning": {"effort": "low"}}}` |
| `mashura_review_budget_usd` | `0` (off) | Cap in USD on one panel operation (all members, rounds, retries, parts). Checked before each retry, debate round 2 and next fallback member; on stop the remaining work is NOT COMPLETED with reason "review budget reached" and obtained reviews are kept. The session `--budget` is respected the same way before retries and debate round 2. |
| `oracle_timeout_seconds` | `300` | Total cap in seconds for a single Mashūra call (a stream that delivers no bytes for 120 s (`stream_idle_timeout_seconds`) is also treated as stalled). The built-in config default is 300. An explicit `0` selects idle+total: the worker may run up to 30 minutes, but the watchdog cuts a stream silent for the idle bound — a slow, steadily streaming reply is not cut at 300 s. Alias `mashura_timeout_seconds`; `/mashura timeout <seconds>` sets it for the repo |
| `mashura_tool_panels` | — | Maps each counsel tool to a named panel |
| `mashura_mode` | — | Default oracle consult schedule: `every-step` \| `on-deviation` \| `phases-only` *(alias: `wf_oracle_mode`; `phases-only` is an alias of `on-deviation`)* |
| `skill_creation` | — | Automatic skill creation (`/skill-creation`); object with the keys below |
| `skill_creation.mode` | `"off"` | Default mode: `off`, `on` (a status notice suggests a skill; never added to tool results), `auto` (builds drafts), `auto-mashura` (Mashūra review, then activates new skills; setting it here is the standing opt-in to send sanitized drafts to the Mashūra panel). Numeric `skill_creation.*` limits must be >= 0 (0 = default) and `allowed_files` globs must be valid, or the config is rejected at load. A per-repo `/skill-creation` setting and a per-session override take precedence | *(0.4.4: default changed from `on`; off means the user opts in)*
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
| `oracle_api_key_env` | `"ANTHROPIC_API_KEY"` | Legacy env-var name (the direct Anthropic path was removed, so Mashūra calls always route through OpenRouter). After the level change it no longer gates the tools: the tools are offered when the effective level is not `off` and the `openrouter_api_key_env` key is present. Kept in old configs for reading; a session that derives from old keys prints a one-time notice and lists it among the ignored keys when the new block is present. |
| `zdr` | `"off"` | Zero-data-retention switch for OpenRouter traffic: `off` \| `auto` \| `on`. **Off by default, and with both switches off the bytes on the wire are identical to the unguarded path.** `on` adds `"provider": {"zdr": true, "data_collection": "deny"}` (merged with any existing provider object) to every OpenRouter request and refuses non-OpenRouter, non-local hosts before sending. `auto` sends the same fields and, on an HTTP 404 before any stream data, retries once without them; a model that falls back is remembered for the rest of the session (later calls skip the first attempt) and shows one notice (`zdr auto: no ZDR endpoint for <model>, sent without ZDR.`). A per-repo `/zdr on\|off\|auto` override wins over this key. |
| `openrouter_region` | `"off"` | Region pin for OpenRouter traffic: `off` \| `eu` \| `us`. `eu` sends to `eu.openrouter.ai` and `us` to `us.openrouter.ai` (same path, same key) **only when the configured host is exactly `openrouter.ai`**; a regional host in the config is used verbatim. Errors from the regional host are shown verbatim — there is **no fallback** to the global host. Needs an OpenRouter Business or Enterprise plan. A per-repo `/region eu\|us\|off` override wins over this key. |
| `trusted_local_hosts` | `[]` | Host names (non-IP) treated as local for the data policy, in addition to `localhost` and loopback/RFC 1918/link-local/ULA IP literals. No DNS lookup is made for classification. Local hosts are always allowed (`n/a (local)`), with no policy fields added. |
| `correction_capture` | `false` | Gates the correction-capture loop: when `true`, user corrections (explicit negation patterns, or `/rewind` followed by a related message) are detected and proposed for durable-memory storage behind a confirmation prompt; when `false` (default) no detection runs, no prompt appears and auto mode is never suspended by a correction. |
| `mashura_fallback_model` | `""` | Explicitly prefixed fallback model (e.g. `"openrouter:anthropic/claude-sonnet-4"`) used when no `default` panel is configured. If empty, the fallback is `"openrouter:"` + the OpenRouter default model when the OpenRouter key is set. Must include provider prefix. |
| `lsp_enabled` | `false` | Gate for `lsp_*` code-intelligence tools |
| `lsp_servers` | — | Maps language → server command |
| `style_guides_enabled` | `false` | Enable built-in and user-configured language style guides |
| `style_guide_paths` | `{}` | Map language ID → replacement or custom host-side guide file; paths relative to the config file, `~/` expanded |
| `browser_enabled` | `false` | Gate for `browser_*` headless-browser tools (chromedp + Chromium). Not forced off under `sandbox.backend: hisn`: Chromium runs inside the hisn session (needs Chromium and `python3` in it) |
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
| `tool_cache_retention_days` | `30` | Once per process at startup, delete per-chat tool-cache dirs (spilled results, collapse archives, subagent summaries) whose newest file is older than N days; `0` = never. A session resumed later than that keeps its transcript, but its `full content at:` pointers will be gone (the chat resumed at launch via `--resume` is exempt) |
| `context_mode` | `"long-session"` | Context mode: `long-session` (default; after a request finishes, its working set is archived to disk and no longer sent; later requests see only the user message + final answer + a one-line note) \| `standard` (full history sent). Persisted per repo (beats the config file); flag `--context-mode`, env `WAKIL_CONTEXT_MODE`; `long` is an alias for `long-session`. Subagents always run in `standard` |
| `context_collapse_min_bytes` | `2000` | Min working-set size for a finished request to be collapsed in `long-session` mode; smaller turns aren't worth a note |
| `context_tail_turns` | `20` | In `long-session` mode, requests older than the N most recent degrade to a single pointer line (archive path + memory key); `0` = never degrade (sent size grows with session length) |
| `context_tail_answer_bytes` | `0` | Optional cap (default off: prior assistant reports are kept whole) on the final answer (including truncation marker) retained for tail turns in `long-session` mode; a longer answer is truncated rune-safe (the archive path in the collapse note below it is the recovery reference and is not repeated). Values below ~5 bytes are treated as disabled. The question (U), the collapse note, and degraded pointer turns are unaffected; `/keep`-exempt turns bypass collapse entirely. `0` (default) = keep full answers. This bounds the answer component of the tail, not the total context: U, notes, and uncollapsed turns are not capped by this key |
| `context_prune` | `"off"` | **Experimental, off by default.** Dynamic context pruning by per-round value (behind this switch). `off` (default): behaviour and wire bytes are identical to not having the feature (`context_mode` still governs collapse). `on`: the model rates the previous tool round with `[[keep]]` / `[[drop]]` / `[[stub: <one line>]]`; tags are stripped before storing. When the projected request exceeds `context_prune_at` × the hard max, lowest-value rounds (drop before stub, oldest first) have their tool-result contents replaced by a one-line stub in the projection; the original round is archived first. Never assistant messages or tool_calls; write-class rounds are never dropped, only stubbed; `/keep` turns and the most recent `context_prune_protect` rounds are never pruned (except `context_mark` marks in `immediate` mode). At the emergency level (≥ 90% of the hard max) the oldest unrated rounds are stubbed without a model rating; a superseded round (re-read, edited, written or repeated later) is auto-stubbed when the trigger runs. In our own tests pruning gave no reliable saving on large-context models; this release adds safeguards (reread lock, `drop_reason` for large drops, marks take effect after three further tool steps, rejected thin notes) that make it safe to turn on |
| `context_prune_at` | `0.70` | Fraction of the effective hard max that triggers pruning. The effective hard max is the fraction-based ceiling when the backend's context limit is known (`0.95 × usable tokens × capacity × 4 chars/token`), otherwise the absolute `hard_max_bytes` fallback. Must be in `(0, 1)`. Default `0.70` |
| `context_prune_target` | `0.55` | Fraction of the hard max that pruning aims to reach (stops when the projected request is ≤ this). Must satisfy `0 < context_prune_target < context_prune_at < 1`. Default `0.55` |
| `context_prune_protect` | `3` | The most recent tool rounds that are never pruned, regardless of rating (except `context_mark` marks in `immediate` mode — the model's explicit decision wins). Must be ≥ 1. Default `3` |
| `context_prune_marks` | `"immediate"` | When marked rounds apply: `immediate` (default) prunes rounds marked drop/stub via `context_mark` regardless of the trigger, at the first request build after three further real tool steps (a tool call in the same message as `context_mark` counts as the first; a mark made right before the final answer prunes nothing while that answer is written and stays pending for the next request; last-N protection does not apply to marked rounds; `/keep`, pinned, collapsed, archive-failure, and never-inflate protections still apply). `on-trigger` keeps trigger-based behaviour (marked rounds prune only at the trigger). Ratings from `prev`/`[[...]]`/the rating pass always keep trigger-based behaviour. Also: a large drop (4 KB or more) needs a `drop_reason` (≥15 chars) in `context_mark`, and a file marked then re-read is locked (later drop/stub refused, `keep` still works) |
| `context_prune_at_tokens` | `0` | Absolute trigger in tokens. Converted with the codebase token estimate (4 chars/token) to chars. When both token keys are set, effective trigger = `min(context_prune_at_tokens × 4, context_prune_at × hard_max)` — a backend whose effective hard max is large can trigger at the absolute tokens even when the fraction never fires there; a backend with a small effective hard max takes the min. Default `0` (not set: fractions only) |
| `context_prune_target_tokens` | `0` | Absolute target in tokens (same conversion). When both token keys are set, must satisfy `0 < context_prune_target_tokens < context_prune_at_tokens` and both keys must be set together. Default `0` (not set) |
| `context_prune_rater` | `"off"` | Model for the one-shot rating pass that rates unrated rounds when pruning is triggered. `"main"` uses the session's current endpoint/model. Any other value is an endpoint name from the endpoints block (e.g. a cheap fast model). The pass is a separate request (cost counted normally), never enters `a.Conv`, and adds one status line per pass (`· prune rating pass: N rounds (drop X · stub Y · keep Z) · 3.1s · <model>`). `/mode status` shows `passes N`; `/prune status` shows the rater model. Default `"off"` (no pass); `"main"` or an endpoint name opts in |
| `context_prune_rater_max_rounds` | `40` | How many unrated rounds one rating pass rates at once (oldest first). Default `40`. Must be `> 0` |
| `context_prune_rater_timeout` | `60` | Seconds to bound one rating pass. Default `60`. Must be `> 0` |
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
| Mashūra check | `low` | — |
| Subagent | `low` | (effort only) |

These defaults are behavior changes for all users: the main model now sends
`reasoning: {effort: "medium"}` on every request to a reasoning-capable
endpoint (OpenRouter, Anthropic thinking models). Non-reasoning models ignore
the parameter. Use `/thinking off` or `--think off` to disable reasoning
explicitly. Use `mashura_roles` to override the Mashūra defaults.

A reviewer never gets more than 16384 reasoning tokens (the `max` effort
level maps to 16384 in the `EffortToBudget` table).

## Mashūra levels

Mashūra (the `mashura__review`, `mashura__debug`, `mashura__decide`, and
`mashura__check` tools) is configured in one block:

```json
"mashura": {
  "level": "medium",
  "members": ["openrouter:anthropic/claude-sonnet-4"],
  "overrides": { "check": { "level": "low" } }
}
```

- `level` is one of `off`, `low`, `medium`, `high`.
- `members` is the panel: every member runs as a panel at every level
  (all members of a call run as a panel, as before).
- `overrides` is optional. Each tool (`review`, `debug`, `decide`, `check`)
  may set its own `members` and/or `level`. Unknown tool names and levels are
  rejected at load. With the block present, per-model and per-role reasoning
  (`mashura_models.*.reasoning`, `mashura_roles`) no longer apply; `mashura_models`
  context/budget limits still do.

What a level sets (for every tool unless an override says otherwise):

| Level | Tools | Reasoning | Debate | Auto-counsel |
|---|---|---|---|---|
| `off` | none | — | — | — |
| `low` | offered | off (`none` effort) | no | suggest only |
| `medium` | offered | `medium` | no | on (ceiling) |
| `high` | offered | `max` (highest accepted, clamped exactly as before) | yes | on (ceiling) |

Debate is available at `high` only. With the block present, debate is chosen by the
level (the panel's legacy `mode` is ignored). `/counsel` still chooses within
the ceiling: `off` → silent, `low` → suggest, `medium`/`high` → auto allowed;
your `/counsel` mode (default `suggest`) picks within it, so `medium` changes no
behavior by default.

`check` is deliberately calmer: it stays at `low` effort under `low` and
`medium` and follows the level only at `high`. The `/mashura` status table shows
this explicitly.

Precedence, highest first: level `off` (from any source) → per-tool override
→ per-repo level set with `/mashura` → config level.

Per-repo level: `/mashura off|low|medium|high` sets it, `/mashura reset`
removes it. It is stored in RepoState (the per-user store keyed by workspace),
never in the repository. `/mashura` (bare) shows the effective level and where
it comes from, the members, the overrides, the effective setting per tool, and
the equivalent new block as JSON for copying. `/mode status` shows the level
and member count. Changing the tool set mid-session adds the line "Changing
this mid-session resets the prompt cache."

Old keys: a config with only the old keys keeps working. At load (the file is
never rewritten), members come from the panel the review tool points to (or the
default panel); a tool pointing elsewhere gets an override; a role with an
explicit reasoning effort becomes an override level (`none`/`low` → `low`,
`medium` → `medium`, `high` and above → `high`); `oracle_enabled: false` →
`off`, otherwise `medium`; a legacy panel in `fusion` mode is treated as a normal
panel. One notice per session says the settings were derived and that `/mashura`
prints the new block. If both the new block and old keys are present, the new
block wins and one notice lists the ignored old keys.

The legacy `fusion` panel mode, the `CallOracle`/`CallOracleURL` helpers, and
the `oracle__ask` tool alias were removed. `oracle_api_key_env` no longer gates
the tools; the gate is the unified predicate: effective level not `off` AND
effective members non-empty AND the OpenRouter key (named by
`openrouter_api_key_env`) is present.

## Execution modes

Tool calls run inside one persistent Docker container for the process
lifetime by default. The workspace directory — positional arg, or cwd if
omitted — bind-mounts into the container at `/mnt/<dirname>`. `--sandbox direct`
runs on the host instead, no container.

## Sandbox backends

`sandbox.backend` selects where tool calls run: two sandbox backends exist, plus
`direct` (no sandbox, explicit opt-out). `docker`
is the fallback when hisn is not usable, and the production sandbox otherwise. `hisn`
([github.com/treeol/hisn](https://github.com/treeol/hisn) >= 0.4.2) is a
rootless, daemonless alternative. Design, path model, lifecycle and known
limits: [sandbox-backends.md](sandbox-backends.md).

| Key | Default | Meaning |
|---|---|---|
| `sandbox.backend` | *unset* → **hisn when usable, else docker** | `docker` \| `hisn` \| `direct`. Unset means: hisn if no backend is chosen anywhere (file, env, flag, `exec_mode`), `hisn check` passes and hisn >= 0.4.2 is in `PATH`; otherwise docker, with the reason in the startup banner. Setting it pins the choice. Flag `--sandbox`, env `WAKIL_SANDBOX_BACKEND`. Replaces the deprecated top-level `exec_mode` |
| `sandbox.hisn.binary` | `hisn` | hisn executable (name on `PATH` or absolute path) |
| `sandbox.hisn.home_root` | `<state dir>/hisn-home/<workspace-hash>` | persistent host directory used as the sandbox `$HOME` *(absolute path)* |
| `sandbox.hisn.home` | `persist` | `persist`: the home directory is kept across chats (`[home]`). `tmpfs`: a fresh tmpfs `$HOME` per session. Project override: `.wakil/sandbox.json` `"hisn": {"home": "tmpfs"}` |
| `sandbox.hisn.rootfs` | *unset* → host toolchain | session root filesystem: a rootfs store name such as `wakil-dev` (import with `hisn rootfs import wakil-dev --from-image wakil-dev`) or an absolute directory. Project override: `.wakil/sandbox.json` `"hisn": {"rootfs": "<name>"}` (name only). Cannot be combined with `hisn.profile` |
| `sandbox.hisn.grantable_rw` | `[]` | absolute host prefixes under which read-write grants outside the workspace are allowed *(fixed at session start; cannot be `/`)* |
| `sandbox.hisn.env_pass` | `GOFLAGS GOCACHE GOMODCACHE CARGO_HOME RUSTFLAGS npm_config_cache PIP_CACHE_DIR CI` | environment variable names copied into the sandbox; nothing else of the host environment enters. Names that alter host process startup (`LD_*`, `PATH`, `HOME`, `GODEBUG`, …) are rejected |
| `sandbox.hisn.ssh_agent` | `false` | Forward the host ssh-agent socket into the session. Required for `ssh_signing` and for ssh git transports. The socket path is reserved: it cannot be added to `env_pass` |
| `sandbox.hisn.ssh_signing` | `off` | Signing mode for the hisn session (`off`\|`auto`\|`<path>`). Requires `sandbox.hisn.ssh_agent = true` unless `off`; the private key never enters the sandbox. See `--ssh-signing` above |
| `sandbox.hisn.ssh_keys` | `[]` | Extra public-key fingerprints (`SHA256:…`) the agent may use inside the session: the agent-forwarding allowlist only. Since 0.5.1 it does not gate signing (the signing key is resolved per repo on the host at plan time and its fingerprint is added to the session allowlist). Global only; an entry that is not a `SHA256:` fingerprint is a config error |
| `sandbox.hisn.git_identity_name` / `sandbox.hisn.git_identity_email` | *unset* → the host repo's `user.name` / `user.email` | Commit identity written into the session gitconfig. Global only |
| `sandbox.hisn.tmp_size_mb` | `1024` | Size (MiB, 16..=65536) of the session-private `/tmp` tmpfs *(`session.tmp_size`)* |
| `sandbox.hisn.shm_size_mb` | `64` | Size (MiB, 1..=65536) of `/dev/shm` *(Wakil always sets `session.shm = true`)* |
| `sandbox.hisn.kvr_binary` | `kvr-server` from `PATH` | kvr-server executable run INSIDE the sandbox as the staging service *(must live under a directory the sandbox sees: `/usr`, `/opt`, `/nix` or a project `mounts_ro`; otherwise staging stays off and the banner says why — there is no host-side kvr)* |
| `sandbox.hisn.exec_via_cli` | `false` | Run tool-call execs through the `hisn exec` CLI (one client process per call) instead of the control-socket client. Fallback, one release |

With an explicit `hisn`: startup requires `hisn --version` >= 0.4.2 and `hisn check` exit
0, otherwise Wakil refuses to start (no fallback). How a session is started, tracked and stopped (`run --detach`, `supervisor.pid`, `hisn stop`) is described once, in [sandbox-backends.md](sandbox-backends.md#supervisor-and-the-detach-contract). Per-project additions (services, forwards, mounts, `risk_ack`, named profile) live in `.wakil/sandbox.json`, which only takes effect after `wakil sandbox trust`; see [sandbox-backends.md](sandbox-backends.md). The workspace is mounted at
its identical host path (`--workdir` is rejected); `kvr` staging runs as an in-sandbox service when a `kvr-server` binary is available (see `sandbox.hisn.kvr_binary`);
`browser_enabled` is not forced off and Chromium runs inside the hisn session (needs Chromium and `python3` in it), and the deprecated `exec_mode: direct` conflicts with it. The
startup line `sandbox: backend=<b> · browser tools: on|off` states the active
backend and whether browser tools are available. One hisn session per chat,
stopped on `/new`, `/resume`, handoff, `/rewind` and exit.

## Subcommands and run-time flags

| Command | Meaning |
|---|---|
| `wakil run [flags] "<task>"` | headless, non-interactive run; exits with a code. Flags and exit codes: [headless.md](headless.md) |
| `wakil sandbox trust [dir]` | approve the project's `.wakil/sandbox.json` (interactive: reads `/dev/tty`; no flag skips the question). Until it is approved the file does not take effect, see [sandbox-backends.md](sandbox-backends.md#per-project-overrides-wakilsandboxjson) |
| `wakil state migrate --from-legacy [--apply]` | copy attributable legacy shared-store memory entries into their project's store. Dry run unless `--apply`; never deletes and never writes the legacy store |
| `wakil plan gc [--apply]` | list, and with `--apply` reject, dead quarantined `plan/current` entries (see [features.md](features.md#plan-quarantine-and-wakil-plan-gc)) |
| `wakil init [flags]` | create the config (interactive on a terminal, flags otherwise); see [Setup: `wakil init`](#setup-wakil-init) |
| `wakil config show [--json]` | the effective config with each value's source (default / file / env / flag); secrets redacted; writes nothing |
| `wakil doctor` | check the container runtime, config and image |
| `wakil version` | print version and the pinned sandbox image |

`--attach-image <path>` (also a flag of the interactive command) attaches an image
file (png, jpeg, gif, webp; comma-separate several) to the first message.

## Sessions

Saved automatically, no flag required. `wakil --resume` picks up the most
recent one; `wakil --resume-id <prefix>` targets a specific `chat_id`.

`/handoff` summarizes the current session, stores the summary in durable
memory, and starts a fresh session with a continuation prompt — so you can
rotate sessions without losing context. The old session remains on disk;
`/resume <id>` returns to it.
