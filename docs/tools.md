# Tools

## Tool reference

| Tool | Gated | Description |
|---|---|---|
| `run_shell` | yes | Run a shell command; cwd persists across calls |
| `read_file` | no | Read a file with line numbers; supports offset/limit |
| `read_file_full` | no | Read an entire file (up to ~256 KB) in one call |
| `write_file` | yes | Write/overwrite a file |
| `edit_file` | yes | Replace an exact substring in a file *(shows diff preview)* |
| `delete_file` | yes | Delete a file or empty directory |
| `move_file` | yes | Rename or move a file within the workspace |
| `list_dir` | no | List directory entries |
| `search_files` | no | Grep file contents for a pattern |
| `find_files` | no | Find files by name glob recursively |
| `open_url` | yes | Open a URL in the host browser *(always runs on the host, not the sandbox)* |
| `dispatch_subagent` | no | Spawn a subagent for a bounded task — discovery (read-only), edit (file mutation, needs `/auto`), or tools (MCP/LSP/web search, needs `/auto`) *(contiguous same-turn calls run in parallel)* |
| `dispatch_subagents` | no | Spawn several subagents concurrently, one per task *(bounded by `max_parallel_subagents`, default 2)* |
| `run_background` | yes | Start a command in the background, detached from the current shell |
| `kill_process` | yes | Stop a background process started with `run_background` |
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
| `list_skills` / `skill_search` / `skill_history` | no | Browse the global skill store (reference docs shared across all projects) |
| `load_skill` | no | Load a skill; for [template skills](#skill-templates) file bodies are replaced by a params/files summary unless `include_files=true` |
| `save_skill` / `update_skill` / `forget_skill` | yes | Write the global skill store *(main agent only; never auto-approved)*. `update_skill` keeps an existing template section when the new value has none; `drop_template=true` removes it |
| `apply_skill_template` | yes | Write a template skill's files into the workspace exactly as stored, filling `@@NAME@@` placeholders *(main agent only; never auto-approved)* |
| `capture_skill_template` | yes | Store workspace files verbatim as a skill's template files *(main agent only; never auto-approved)* |
| `check_skill_template` | no | Report whether files written by `apply_skill_template` are unchanged, modified, or missing, and whether their skill has a newer version |
| `lsp_definition` / `lsp_references` / `lsp_hover` / `lsp_symbols` | no | Language-server-backed code intelligence *(off by default — see [features](features.md#lsp-code-intelligence))* |
| `browser_navigate` / `browser_screenshot` / `browser_viewport` / `browser_click` / `browser_eval` / `browser_text` / `browser_html` / `browser_reduced_motion` | no | Headless-browser tools — visual verification, DOM inspection, interaction testing *(off by default — see [features](features.md#headless-browser))* |

MCP tools *(stdio or HTTP)* append automatically when `mcp_servers` is
configured. The host Docker socket passthrough (`--docker-sock`) is what lets
`docker` / `docker compose` calls reach the host daemon.

## Gating

**Gated** — `run_shell`, `write_file`, `edit_file`, `delete_file`, `move_file`,
`run_background`, `kill_process`, `open_url`. By default each call prompts `y/n`
before it runs; `/auto` changes this (see below). The skill write tools
(`save_skill`, `update_skill`, `forget_skill`, `capture_skill_template`) and
`apply_skill_template` always prompt, even in `/auto`.

**Ungated** — `read_file`, `read_file_full`, `list_dir`, `search_files`,
`find_files`, `dispatch_subagent`, `dispatch_subagents`, `read_process_log`,
and the `lsp_*` code-intelligence tools. All structured, argument-constrained
calls: they read file contents, listings, and symbol data, but none of them
can execute arbitrary commands.

`run_shell` is gated even for pure reads — `cat ~/.ssh/id_rsa` or `env` is
gated the same as any other call. `a` at a prompt auto-approves read-only
tools for the rest of the session; gated tools keep prompting unless you flip
full auto-approve with `/auto` (status bar shows `AUTO`). Destructive commands
and counsel calls gate even in auto mode — no override.

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

See [SECURITY.md](../SECURITY.md) for the full threat model and sandbox
classification.

## Skill templates

A skill can carry template files alongside its prose: setups you repeat
across projects (a deploy script plus stack file, a database bootstrap).
The files live in the skill value, so they get the skill store's version
history, secret screening, and confirm gate.

- **Capture.** `capture_skill_template` reads files from the workspace (text
  only, ≤64 KiB each, ≤50 files). Each literal `value` you pass as a param
  (≥3 bytes) becomes an `@@NAME@@` placeholder; the value itself is not
  stored. A param with a `default` is optional, one without is required,
  and `pattern` is a full-match regexp. Capturing into an existing skill
  keeps its prose and replaces only the files.
- **Apply.** `apply_skill_template(key, params, target_dir, overwrite)` renders
  the files host-side and writes exactly those bytes and file modes, so the
  model never retypes the bundle. Placeholders are substituted in a single
  pass with no logic, so `{{…}}` text (docker `--format`, Helm) is left
  untouched. Files that differ in content or mode are only replaced with
  `overwrite=true`, and the confirm then shows a diff. Nothing is written if
  a target or the skill changes while the confirm is open. Verify steps
  stored in the skill are listed, never run. Content changes go into the
  `/rewind` checkpoint. `/rewind` does not restore file modes.
- **Provenance.** Each written file is recorded in `.wakil/skill-templates.json`
  (skill key, skill entry ID, params, sha256, mode); `check_skill_template`
  reads it. A corrupt provenance file blocks `apply_skill_template` instead of
  being replaced. Paths under `.wakil/` can't be template outputs.
- **No secrets.** Template params are not a secret channel. Their values pass
  through tool arguments, the transcript, and the provenance file. Templates
  should reference `.env` files or docker secrets instead.

Storage format (the last part of the skill value, written by
`capture_skill_template`; it can be edited via `update_skill`):

```
=== wakil-template v1 ===
{"params":[{"name":"STACK_NAME","pattern":"[a-z0-9-]+"}],"files":[{"path":"deploy.sh","mode":"0755"}],"verify":["bash -n deploy.sh"]}
=== file: deploy.sh ===
<verbatim body>
=== end: deploy.sh ===
```

## Subagents

Subagents have three capability tiers:

- **discovery** (default, read-only) — read files, search, list directories.
  Cannot modify files.
- **edit** — can `edit_file` / `write_file` / `delete_file` / `move_file`,
  gated on `/auto` consent. In git repos (direct mode), edit children run in
  isolated git worktrees — parallel edits without writer-lock serialization.
  Non-git directories and Docker mode fall back to serialized execution (at
  most one edit child at a time).
- **tools** — adds MCP tools from a configured allowlist, LSP, and web search
  to the discovery set. Also gated on `/auto`; mutating MCP calls are
  serialized per-server.

`dispatch_subagent` / `dispatch_subagents` are ungated because they spawn
bounded workers with their own tool restrictions; the edit and tools tiers
inherit session-level consent and never prompt interactively.

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
