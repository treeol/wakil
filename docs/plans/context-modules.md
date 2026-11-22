# Context modules — discovery and design

Branch: `work/context-modules` (from `master` @ `c89e5a8`, tag `v0.8.9`). Local only.

Scope for this session: **no product code changes.** The only new code is
`scripts/context_usage.sh` plus its test. Everything else is measurement and a
design document.

## 0. State check

```
$ git status --short
(no output)
$ git checkout master
Already on 'master'
$ git rev-parse HEAD
c89e5a82a02144e1318d6343ceccd7eac11342c4
$ git describe --tags
v0.8.9
$ git checkout -b work/context-modules
Switched to a new branch 'work/context-modules'
$ git stash list
stash@{0}: WIP on work/0.8.3: 1c1b300 fix: approvals carry over, resume paths agree, grant key works
stash@{1}: On master: phase-F: signing_guard checkGitConfigSub goto->joinedValueFlag (pre-existing, not Phase A)
```

Both stashes are left alone.

Tier (task-workflow): Tier 3 — cross-cutting design touching prompt assembly,
tool registration, proxy caching and subagent construction. Mashūra rounds are
capped at two by the task, which is the deliberate deviation from the skill's
"1 pre + 1 post" (the second round is a review of the finished design, not of a
diff).

## 1. The questions

1. What goes into a request today — every part in order, with sizes and
   estimated tokens, plus the function that assembles each part.
2. What already exists for loading on demand (core/extension split, `/prompt
   core`, skills, deferred tools, subagent prompt/tools).
3. Prompt caching per backend: cache breakpoints, and what changes between
   turns.
4. The memory block: where it is built, its size, whether it is part of the
   first user message, and what changes if it becomes a module.
5. Inventory: one table for prompt sections, one for tool definitions — bytes,
   estimated tokens, which feature needs it, proposed class.
6. `scripts/context_usage.sh` plus a synthetic-fixture test that proves a
   secret marker never reaches the output.
7. The design itself (module list, index format, load rule, cache rule, style
   file, subagent/headless/Mashūra delivery, missing-module fallback, stages).
8. How each stage is measured on the bench.

## 2. How each question will be answered

| # | Method | Files / functions / commands |
|---|---|---|
| 1 | Read the assembly path end to end; measure each literal and each injected section with `wc -c`; measure the **final serialised tool JSON** (not Go source) including pruning's schema mutation and agent-level tool defs. | `internal/agent/app.go` (`buildPreambleForNetwork` :1931, `ensurePreamble` :2156, `evictStaleToolResults` :2358, `buildSubagentTools` :2649), `internal/wiring/bootstrap.go` (`loadAgentPrompt` :649), `prompts/embed.go`, `prompts/*.txt`, `internal/agent/mcp_manager.go` (`BuildTools` :1018), `internal/tools/tools.go`, `internal/agent/toolcap.go`, `internal/agent/retrieval.go`, `internal/agent/turn_phases.go` (Stream call :324) |
| 2 | Read the split-prompt machinery, skill tools, subagent construction (verified already: subagents get their own short prompt + curated tools, not the parent's), and the tool-eviction/cap mechanics that any tool-delivered module depends on. | `internal/agent/classifier.go` (`maybeActivateExtension` :155, `classifyRequest` :23), `internal/agent/app_state.go` (`IsPromptCore` :225), `internal/tools/skills.go`, `internal/agent/subagent.go` (:431-490, :1470-1545), `internal/agent/app.go` (`buildSubagentTools` :2649, `evictStaleToolResults` :2358), `internal/agent/compact.go` |
| 3 | Read the proxy's cache-decoration path and the Anthropic vs OpenRouter request shapes (**cacheable prefix order tools→system→messages is recalled, not verified**); check counsel (Mashūra) caching separately. | `internal/proxy/client.go` (`Stream` :1152, `computeCacheBreakpoints` :308, `anthropicCacheHeuristic` :342), `internal/proxy/cache_control_test.go` (spec), `internal/proxy/cost.go`/`cost_cache.go`, `internal/counsel/oracle.go` (`UseCache` :167), `internal/agent/cache_stable.go` |
| 4 | Measure **both** memory artifacts separately (digest in `Conv[0]`; retrieval block in the user message); confirm where the retrieval block is appended in the turn path and whether it persists byte-identically. | `internal/agent/retrieval.go` (whole file), its caller in `internal/agent/turn_phases.go`/`send` path, `internal/agent/app.go:2038-2100`, `internal/agent/sessionhistory_bridge.go` (strip logic) |
| 5 | Aggregate the measurements from 1 and 3 into two tables with a proposed class per row. | derived |
| 6 | Write the script, write the fixture, run both. | `scripts/context_usage.sh`, `scripts/context_usage_test.sh` (or a Go test if the repo convention demands Go — decide from `scripts/` conventions) |
| 7 | Synthesize 1-6. | this document |
| 8 | Read the bench harness columns and the task set; state what each stage measures. | `scripts/bench.sh`, `bench/README.md`, `bench/tasks/*/task.txt`, `bench/results/*.csv` header |

Measurement conventions for the whole document:
- Every individually attributable byte count comes from `wc -c` on a concrete
  file, or from a command whose output is quoted.
- Token estimates are **estimates**: `bytes / 4`, marked as such. If any
  token counter exists in the tree it will be named and used instead; a
  preliminary search found none (`grep -rn "EstimateTokens\|TokenCount"` over
  `internal/` returns no non-test hit), so `bytes/4` is the working rule.
- Numbers that depend on a live session (memory digest, AGENTS.md size in this
  repo) are measured from the files that feed them, not from
  `~/.local/share/wakil`.
- **Saving arithmetic** (from C3) is always
  `removed prefix bytes − (index bytes + loader tool def bytes + module text
  re-sent per turn)`. A stage that reports only "removed bytes" is incomplete.
- Cache pricing rates come from what the proxy records
  (`internal/proxy/cost*.go`); anything recalled rather than read is labelled
  unverified.

## 3. What the document will contain

Sections 1-8 above, in the order the questions are asked, each appended as its
own signed commit. Plus:

- One "request composition today" table (question 1) that the final report
  reuses verbatim.
- Two inventory tables (question 5) with `class` and `reason` columns, every
  class explicitly marked as a proposal.
- The design section written as a standalone artifact (module list, index
  format, load rule, cache rule, style file, delivery rules, fallback, stages).
- "Open questions" and "Unverified" sections at the end.

## 4. Corrections folded in from the plan review (Mashūra round 1)

Three reviewers ran; all three flagged the same core gaps. Each was checked
against the code before being accepted. Only BLOCKING items are folded in here;
the rest are tracked in §6.

### C1 — Subagent baseline was wrong (verified, corrected)

The draft claimed "subagents inherit the parent's prompt and tools today". That
is false in both halves:

- Prompt: a subagent gets one of three short purpose-built constants —
  `subagentSystemPrompt` (`internal/agent/subagent.go:432`),
  `subagentEditSystemPrompt` (`:454`), `subagentToolsSystemPrompt` (`:484`) —
  selected at `internal/agent/subagent.go:1533-1541`. It never gets
  `AgentPrompt`.
- Preamble: `ensurePreamble` returns immediately when `InjectDate` is off, and
  its comment (`internal/agent/app.go:2154-2155`) says so explicitly:
  "No-op when InjectDate is off (subagents, tests) — those paths … continue to
  get no preamble at all."
- Tools: `buildSubagentTools` (`internal/agent/app.go:2649`) builds a curated
  set — `wtools.DiscoveryTools` + conditional search/LSP/browser plus
  allowlisted MCP servers — and its doc comment states it never includes
  `run_shell`, `dispatch_subagent`, `run_background`, `kill_process`,
  `open_url`, or `mashura__*`. Discovery and edit tiers get hardcoded lists
  (`subagentToolNames` returns nil for them, `:2681`).

Consequence for the design: subagents are already a separate, much smaller
context. A module system must define their module set explicitly rather than
"inherit", and question 2 measures their real cost.

### C2 — There are two memory artifacts, not one (verified, corrected)

| Artifact | Where | Built by | Size |
|---|---|---|---|
| Memory **digest** line | inside `Conv[0]`, the system prompt | `buildPreambleForNetwork` (`internal/agent/app.go:2038-2100`) | bounded: counts + ≤5 recent keys + ≤400 B namespace inventory |
| Memory/skill **retrieval block** | folded into the per-turn **user** message | `retrieveMemoryContext` (`internal/agent/retrieval.go:51`) | capped at 2048 B parent / 1024 B subagent (`:27-28`) |

The draft's risk note ("memory already lives in the user message") was only
true for the retrieval block. The digest is *inside* the cached prefix and is
deliberately day-stable and *not* live — its own comment (`app.go:2039-2043`)
says invalidating on every memory mutation "would destroy prompt-cache prefix
stability". Question 4 now measures both and treats them as separate decisions.

### C3 — "Cheaper" needs a cost model, not a byte count (accepted)

Removing bytes from a cached prefix does not save `bytes` worth of money.
Reviewers' recollection is that Anthropic-style cache reads bill at ~0.1× and
writes at ~1.25×; **this is recalled, not verified here** — the design will
quote the rates the proxy actually records (`internal/proxy/cost.go`,
`cost_cache.go`) and mark anything from memory as unverified.

Two consequences that are BLOCKING for the structure of the design:

1. Every stage must state its saving as
   `removed prefix bytes − (index bytes + loader tool definition bytes +
   module text re-sent on every subsequent turn)`, not as removed bytes.
2. A loaded module delivered as a **tool result is not free after load** — it
   is re-sent each turn until eviction. Verified mechanics:
   `ToolResultCap` default 8000 chars (`internal/config/config.go:1567`),
   `TurnToolBudget`, and `evictStaleToolResults`
   (`internal/agent/app.go:2358`) which stubs unpinned `role=="tool"` messages
   older than `ToolResultTTL`. Subagents set `ToolResultTTL = -1` (never
   evict, `internal/agent/subagent.go:38`) and `ToolResultCap = 12_000`
   (`:35`). So the load rule must specify pinning, cap exemption, and
   reload-after-eviction.

### C4 — Cache prefix order and the tool array (accepted as must-verify)

Reviewers (from recall, unverified) state the cacheable prefix order is
`tools → system → messages` on Anthropic-style APIs, so **mutating the tools
array mid-session invalidates the whole prefix**. The codebase already treats
this as a real constraint: `internal/agent/cache_stable.go:9-13` sorts tools
within a group "so that non-deterministic ordering … does not silently
invalidate the prompt-cache prefix", and `BuildTools`
(`internal/agent/mcp_manager.go:1018-1050`) appends conditional groups *after*
built-ins to preserve the built-in prefix.

Question 3 must therefore answer, from `internal/proxy/` and
`cache_control_test.go` rather than from recall: the wire order of tools vs
messages, how multiple `system` messages serialise per backend, and where the
breakpoints land. This decides between the two viable deferred-tool shapes —
mutating the tools array vs a stable dispatcher tool with schemas delivered
append-only.

Also BLOCKING: `maybeActivateExtension` (`internal/agent/classifier.go:155`)
already loads a prompt lazily by inserting a **second system message at
Conv[1]**. That precedent's cache behaviour must be measured, not assumed, and
its insertion point must not break `ensurePreamble`'s slot identification
(`app.go:2196-2210`, which identifies its own slot by position and the
"Current date:" line) or the handoff-seed logic.

### C5 — AGENTS.md as a module changes taint, not just cost (accepted)

Loading AGENTS.md currently sets `a.touchedExternal` and records a taint
reason at preamble-build time (`internal/agent/app.go:1976-1979`). Deferring it
moves the taint timing and lets the model skip project rules silently. The
design must preserve the taint invariant and state the skip risk.

### C6 — Measurement artifacts must be precise (accepted)

- Q1 must measure the **final serialised `[]proxy.Tool` JSON** actually handed
  to `proxy.Stream` (`internal/proxy/client.go:1152`), not `wc -c` on Go
  source, and must account for context-pruning's post-`BuildTools` schema
  mutation (`stripPrevFromToolCall`) and the extra agent-level tool
  definitions (`finalize_goal`, `context_mark`, `staging_*`, `memory_*`,
  `*_skill*`).
- Q8 must confirm whether `bench/results/*.csv` carries cache-read /
  cache-write columns. Its header today is
  `date,wakil_version,model,task,run,result,exit_reason,seconds,tokens_in,tokens_out,cost_usd,tool_calls,collapses,mashura_rounds`
  — it has **no** cached-token column, which is itself a finding for Q8.
- The headline numbers (39k input, ~15k system prompt, 13.7k tools) are the
  user's, marked as to-verify; they are not repeated as fact anywhere in this
  document until measured.
- Q6's script needs a defined input (trace JSONL — `internal/trace/trace.go`
  `Record`/`ToolTrace`) and a **numeric-output allowlist**, not just one
  secret marker: tool names + counts + session counts only, nothing else on
  stdout *or* stderr.

## 5. Open risks

- **Prompt caching is the whole game.** If a module is injected anywhere before
  the last stable byte of the request, every load invalidates the prefix and
  the "saving" turns negative. The cache rule has to be derived from what the
  proxy actually emits, not from what the API docs say (see C4).
- **Deferred tools and the tool-array cache contract** (C4). A model that
  needs a deferred tool and does not know it exists fails in a way that is hard
  to attribute; `executeToolCall` dispatches by name and falls through to
  `handleMCPTool` for unknown names, so an unloaded deferred tool must fail
  loudly with a "load it first" message rather than silently.
- **A tool-delivered module is re-sent every turn** until eviction (C3), so
  long sessions can invert the saving. The load rule must name the pinning and
  eviction behaviour.
- **AGENTS.md deferral moves taint timing** (C5).
- **Estimation error.** `bytes/4` under-counts markdown-heavy English and
  over-counts code. Every "saving" figure in the design is marked as an
  estimate until the bench confirms it.
- **The script is the only new artifact and must never print content** — a bug
  there leaks conversation data to a terminal or a log.
- **Bench cannot currently detect a cache regression** (no cached-token column
  in the CSV), which may force Q8 to propose a column addition as a
  precondition rather than a measurement.

## 6. Non-blocking findings (listed, not folded in)

From the same round, deferred to the design section as discussion points:

- Inventory tables should be multi-column (core/full/custom prompt, prune
  on/off, AGENTS.md present, each conditional tool group) rather than one
  static table — a single table will misstate the typical case.
- The bench needs explicit pass/fail thresholds, not just columns.
- Missing-module fallback needs its own bench task, not just a design rule.
- Tool schema JSON escaping overhead and per-message framing are real but
  small; measure once in Q1 rather than model twice.

---

# Question 1 — What goes into a request today

All measurements below were run in this session. Byte counts are `wc -c` or
exact `len()` over the bytes that reach the wire; token counts are
**estimates** at `bytes / 4` (no token counter exists in the tree —
`grep -rn "EstimateTokens|TokenCount" internal/` has no non-test hit).

## 1.1 The order of parts

A request has two top-level fields that matter: `messages` and `tools`
(`internal/proxy/client.go:1197-1253`, the `wireBody` struct). Inside
`messages`, the order is:

```
messages[0]  = system  ← the preamble, built by buildPreambleForNetwork
messages[1]  = user    ← the first user message, with the retrieval block
                          prepended to its content
 ...
```

When `/prompt core` is active and the classifier fires, a **second system
message is inserted at index 1** (`maybeActivateExtension`,
`internal/agent/classifier.go:155-208`), so the shape becomes
`[system, system, user, ...]`.

A style guide, when enabled, is inserted as **another system message directly
after `messages[0]`** at request-build time, not in `Conv`
(`requestMessages`, `internal/agent/styleguides.go:412-428`).

The `tools` array is a sibling field, not a message.

## 1.2 `messages[0]` — the preamble, part by part

Assembled by `(*App).buildPreambleForNetwork`, `internal/agent/app.go:1931-2105`,
called from `buildPreamble` (`:1927`) and installed by `ensurePreamble`
(`:2156`) once per turn (`internal/agent/app.go:1269`). Parts in emission
order:

| # | Part | Emitted when | Bytes | Est. tokens | Assembled by |
|---|---|---|---|---|---|
| 1 | Context-pruning section | `context_prune` on (default **off**) | 0 by default | 0 | `pruneSystemSection` (`app.go:1949`) |
| 2 | Agent prompt — `prompts/agent.txt` with its version line stripped | full mode (default), or a custom `agent_prompt_path` | 52 858 | 13 214 | `loadAgentPrompt` (`internal/wiring/bootstrap.go:649`), embedded via `prompts/embed.go:27` |
| 3 | AGENTS.md section | a readable AGENTS.md at/above cwd, bounded by the workspace root | 6 704 | 1 676 | `loadAgentsMD` (`internal/agent/agentsmd.go`), rendered at `:239-241` |
| 4 | Date line | `InjectDate` on (all non-subagent sessions) | 131 | 32 | `app.go:1932-1933` |
| 5 | Working-directory line | an executor or `work_dir` is set | 44 | 11 | `app.go:1997-2003` |
| 6 | Sandbox tool inventory | executor reports sandbox tools | 146 | 36 | `app.go:2004-2010` |
| 7 | LSP inventory line | `LSPEnabled` (default off) | 300 | 75 | `app.go:2011-2017` |
| 8 | Browser inventory line | `BrowserEnabled` (default off) | 331 | 82 | `app.go:2018-2025` |
| 9 | Repo-map summary line | executor present and `InjectDate` on | 171 | 42 | `injectRepoMap` (`app.go:2030-2037`) |
| 10 | Memory digest line | a memory store with ≥1 entry | 591 | 147 | `app.go:2044-2100` |
| 11 | Network mode line | always | 48 | 12 | `networkModeLine` (`app.go:2125`) |

Measured preamble total in this session (this repo, this environment,
`prompt_mode: full`, prune off, LSP/browser off, memory populated):

```
prompts/agent.txt                52858 B  13214 est_tok   (wc -c 52879, minus the stripped version line + trailing newline)
AGENTS.md rendered                6704 B   1676 est_tok   (6544 B file + header/preamble)
date line                          131 B     32 est_tok
cwd line                            44 B     11 est_tok
sandbox tools line                 146 B     36 est_tok
repo-map line                      171 B     42 est_tok
memory digest line                 591 B    147 est_tok
network line                        48 B     12 est_tok
--------------------------------------------------------------
TOTAL Conv[0]                    61333 B  15333 est_tok
```

Counting method for the literal lines (date/cwd/LSP/browser/network/sandbox):
each string was copied from source and measured with `len(s.encode())` — the
date line used this session's actual date, and the memory digest used this
session's actual digest as printed in the system preamble. They are quoted,
not estimated from the source text.

`wc -c` on the same file disagrees (52 879 vs 52 858) by exactly the
`# prompt-version: 1\n` line plus the trailing newline that
`loadAgentPrompt` strips (`internal/wiring/bootstrap.go:649`, `:657-670`).

## 1.3 `tools` — the serialised tool definitions

Measured by marshalling the real `[]proxy.Tool` that `BuildTools`
(`internal/agent/mcp_manager.go:1018`) returns, with `json.Marshal`, over
`config.DefaultConfig()`:

```
defaults (builtins only)         count=49 json_bytes= 39563 est_tokens= 9890
defaults + search+lsp+browser    count=63 json_bytes= 47972 est_tokens=11993
  delta over builtins: 8409 B (2102 est_tok)
discovery-subagent               count=34 json_bytes= 25227 est_tokens= 6306
```

The ten largest definitions in the default set:

| Tool | Bytes | Est. tokens |
|---|---|---|
| `capture_skill_template` | 3 746 | 936 |
| `dispatch_subagent` | 2 279 | 569 |
| `apply_skill_template` | 1 941 | 485 |
| `dispatch_subagents` | 1 921 | 480 |
| `collapse_context` | 1 762 | 440 |
| `memory_put` | 1 613 | 403 |
| `save_skill` | 1 203 | 300 |
| `read_file` | 1 069 | 267 |
| `edit_file` | 1 032 | 258 |
| `run_background` | 915 | 228 |

The measurement program lives in `tmp/measure/main.go` (gitignored, not
product code) and is reproduced in Appendix A.

**Caveat (from the plan review, C6):** these figures exclude the
context-pruning mutation of tool schemas (`stripPrevFromToolCall`) because
prune is off by default; with prune on the "prev" property is added to every
tool that takes it, which grows each definition. The design must re-measure
with prune on before quoting a saving.

## 1.4 The other request parts

| Part | Where | Bytes | Est. tokens | Notes |
|---|---|---|---|---|
| Retrieval (memory/skill) block | prepended to the **user** message content, `internal/agent/app.go:1266-1268` | ≤ 2 048 (parent), ≤ 1 024 (subagent) | ≤ 512 / ≤ 256 | `retrievalCap`/`retrievalCapSubagent`, `internal/agent/retrieval.go:27-28` |
| Workflow directive | prepended before the retrieval block, `app.go:1261-1265` | task-dependent | — | only when a workflow is active |
| Style guide message | extra system message at index 1, `styleguides.go:412-428` | 1 535 for the built-in Go guide (`google-style-guides/go.md` 1 421 B + header) | 383 | only when `style_guides_enabled` and a language is selected |
| Chat `metadata` | body field, `client.go:1254-1267` | a few dozen B | <20 | proxy shape only |

## 1.5 Headless (`wakil run`) vs TUI

Measured by reading the bootstrap path, not by running the binary:

- Both paths call the **same** `BuildApp` (`internal/wiring/bootstrap.go:247`),
  which builds the same `App`, the same prompt, the same `BuildTools` set.
- Headless differs in exactly three ways that affect request size:
  1. `IsHeadless: true` (`internal/wiring/headless.go:284`) — no TUI, and no
     human to re-send a prompt; it does not alter the preamble.
  2. `--no-memory` (bench uses it, `scripts/bench.sh:777`) strips the memory
     tools and leaves `MemoryStore` nil, so the **memory digest line and the
     memory tool definitions are absent**. On this repo that removes 591 B
     from the preamble plus the 10 memory tool definitions.
  3. No style guide is loaded unless configured.
- Therefore the bench's typical headless request is *smaller* than the TUI's,
  by the memory digest + memory tools. The bench CSV is written from the same
  `ResultJSON` either way (`internal/wiring/resultjson.go:80`).

Measured sizes of the memory tool group (from the per-tool table):
`memory_put` 1 613 + `memory_promote_from_staging` 812 + `memory_search` 671
+ `memory_list` 632 + `memory_promote` 541 + `memory_reject` 409 +
`memory_get` 398 + `memory_forget` 361 = **5 437 B ≈ 1 359 est. tokens**.

## 1.6 The claim to verify

The task states ~39k input tokens, ~15k system prompt, ~13.7k tool
definitions. Measured here:

| Claim | Measured | Verdict |
|---|---|---|
| ~15k system prompt | 15 333 est. tokens (Conv[0] only) | **supported** (estimate, bytes/4) |
| ~13.7k tool definitions | 9 890 est. tokens (default set), 11 993 with all optional groups | **not supported** — the claim is ~40 % high for the default set; it matches nothing measured |
| ~39k input tokens per request | 15 333 + 9 890 = **25 223 est. tokens** for preamble+tools, before any conversation | **not supported as a floor**; 39k would require ~14k tokens of conversation/skills/system-reminder already present |

The discrepancy is itself a finding: either the 39k figure came from a
session with a long conversation, extra MCP tools, or a style guide, or the
13.7k figure counted a different set. Answering it needs a real trace, which
this session may not read (`~/.local/share/wakil` is off limits) — so it stays
**unverified**, and Q8's bench column `tokens_in` is the way to settle it.

## 1.7 Functions that assemble each part (index)

| Part | Function | File:line |
|---|---|---|
| Agent prompt text | `loadAgentPrompt` | `internal/wiring/bootstrap.go:649` |
| Core/extension split | `buildPreambleForNetwork` + `maybeActivateExtension` | `internal/agent/app.go:1957-1964`, `classifier.go:155` |
| AGENTS.md | `loadAgentsMD` | `internal/agent/agentsmd.go` (render at `:239`) |
| Date / cwd / inventory | `buildPreambleForNetwork` | `internal/agent/app.go:1931-2037` |
| Repo map | `injectRepoMap` | `internal/agent/repomap.go` |
| Memory digest | `buildPreambleForNetwork` (inline) | `internal/agent/app.go:2044-2100` |
| Network line | `networkModeLine` | `internal/agent/app.go:2125` |
| Preamble installation | `ensurePreamble` | `internal/agent/app.go:2156` |
| Retrieval block | `retrieveMemoryContext` / `formatRetrievedContext` | `internal/agent/retrieval.go:51`, `:119` |
| Style guide message | `requestMessages` | `internal/agent/styleguides.go:412` |
| Tool definitions | `BuildTools` | `internal/agent/mcp_manager.go:1018` |
| Wire body | `Client.Stream` | `internal/proxy/client.go:1152` |

---

# Question 2 — What already exists for loading on demand

Wakil already has four on-demand mechanisms and one conditional-group
mechanism. Only one of them (skills) is a true "load text into context when
needed" path; the others are variants of "don't add it in the first place."

## 2.1 The core/extension prompt split (`/prompt core`)

**How it works.** Two embedded prompts replace the monolithic one:

| Prompt | File | Bytes | Est. tokens |
|---|---|---|---|
| full | `prompts/agent.txt` | 52 858 | 13 214 |
| core | `prompts/core.txt` | 18 572 | 4 643 |
| extension | `prompts/extension.txt` | 34 605 | 8 651 |

Core and extension together are 1.05× the full prompt (they overlap slightly —
`## Core loop` is 3 401 B in full and 3 612 B in core; `## Files` 1 340 vs 936).
So `core` mode saves 13 214 − 4 643 = **8 571 est. tokens** until the
extension fires, and after it fires costs 4 643 + 8 651 = 13 294 — marginally
*more* than full mode.

Section-by-section sizes (measured, `##`-heading spans):

| Section | full | core | extension |
|---|---|---|---|
| Global precedence ladder | 2 385 | 2 385 | — |
| Role | 830 | 830 | — |
| Core loop | 3 401 | 3 612 | — |
| Secrets | 3 911 | 3 911 | — |
| Shell | 2 486 | 2 486 | — |
| Files | 1 340 | 936 | — |
| External side-effect gating | 2 872 | 2 872 | — |
| Honesty Invariants | 860 | 860 | — |
| Recovery | 277 | 277 | — |
| Documentation upkeep | 1 353 | — | 1 353 |
| What "verified" means | 779 | — | 1 007 |
| Search & Research | 4 987 | — | 4 987 |
| File vs. Inline | 585 | — | 585 |
| Skills | 1 617 | — | 1 617 |
| Tool / Cost Discipline | 620 | — | 620 |
| Durable Memory | 8 402 | — | 8 402 |
| Truncated tool results | 2 772 | — | 2 772 |
| Async execution | 4 446 | — | 4 446 |
| Subagents | 7 073 | — | 7 073 |
| Interaction & Output | 1 220 | — | 1 220 |

**Where the code is.** `prompts/embed.go` (three `//go:embed`s),
`internal/wiring/bootstrap.go:348-351` (loads core+extension only when no
custom prompt file), `internal/agent/app.go:1957-1964` (picks which one the
preamble uses), `internal/agent/classifier.go:23` (`classifyRequest`) and
`:155` (`maybeActivateExtension`).

**Activation rule.** `classifyRequest` is a static Go classifier with no model
call: a greeting → false; any code/file reference, URL, or backticked snippet
→ true; any of ~40 task keywords → true; text ≥30 chars → true; otherwise
false. Its own comment says it is "intentionally over-broad: better to
activate too often than too rarely."

**Cost per request.** In core mode before activation: Conv[0] loses 8 571
est. tokens. After activation the extension is a **second system message at
Conv[1]**, pinned and `Turn==0`, so it is re-sent on every subsequent request
and survives compaction (`maybeActivateExtension`, `classifier.go:186-207`) —
i.e. it is a *one-way* load with a permanent cost, exactly the shape the
design will need for modules.

**Why this is the key precedent.** It is a working, shipped lazy-load that
already solves: where to insert (after Conv[0], before the user message),
how to survive compaction (`Pinned` + `Turn==0`), and how to avoid breaking
`ensurePreamble`'s slot logic. But it has **no unload path** and its cache
behaviour has never been measured.

## 2.2 Skills (`load_skill`, `list_skills`, `skill_search`, …)

**How it works.** A skill store holds named reference docs. Three access
tiers:

1. **Automatic retrieval** — `retrieveMemoryContext` searches memory *and*
   skills by FTS5 on the user's text and injects up to 2 skill entries, each
   truncated to 500 B, inside the 2 048 B retrieval block
   (`internal/agent/retrieval.go:43-46`, `:82-95`). This is automatic but
   *summary-only*: the model sees a key, a taint label and ≤500 B of prose.
2. **Explicit load** — `load_skill` returns the full skill body. It is
   **exempt from `ToolResultCap`** (see `capabilities`/`CapOrStub` exemption
   list together with `read_file_full`), so a large skill is not truncated.
3. **Template apply** — `apply_skill_template` writes files; the skill body
   never enters context as prose.

Template skills are deliberately prose-only on retrieval: "Template skills:
retrieval shows prose only — file bodies are reached through load_skill /
apply_skill_template, never the preamble" (`retrieval.go:224-226`).

**Cost per request.** Zero in the preambles. The retrieval block costs ≤512
est. tokens when it fires, in the user message. `load_skill` costs the tool
definition (899 B) plus whatever is loaded, re-sent per turn thereafter.

**This is the closest existing analogue to a context module** — with one
crucial difference: skills are global and project-agnostic by policy, while
context modules would be per-repo or per-feature prompt text.

## 2.3 Deferred / conditional tools

There is **no deferred-tool mechanism**. What exists is conditional
*registration*: `BuildTools` (`internal/agent/mcp_manager.go:1018-1050`)
appends groups in a fixed order and each optional group is present or absent
for the whole session:

| Group | Gate | Bytes (measured) |
|---|---|---|
| built-ins (49 tools) | always | 39 563 |
| `check_pending`, `wait_for_completion`, `collapse_context` | always | (inside built-ins) |
| search (`searxng_*`, 2 tools) | `searxng_url` set | 1 135 |
| MCP (`server__tool`, variable) | ≥1 connected server | unbounded (one per server tool) |
| Mashūra (`mashura__*`, 4 tools) | key present and level ≠ off | not measured here (no key in this environment) |
| LSP (4 tools) | `lsp_enabled` | 2 575 |
| browser (8 tools) | `browser_enabled` | 4 685 |

Group sizes by feature, default set (measured):

```
async             6 tools   4546 B  1136 est_tok
git               5 tools   2248 B   562 est_tok
memory            8 tools   5437 B  1359 est_tok
skills-read       4 tools   2060 B   515 est_tok
skills-template   3 tools   6394 B  1598 est_tok
skills-write      3 tools   2542 B   635 est_tok
staging           5 tools   2304 B   576 est_tok
subagent          2 tools   4200 B  1050 est_tok
```

**Cost per request.** Each registered group is re-sent on every request. There
is no way today to offer a tool only when a task needs it — the closest thing
is `/auto`-style gating of *execution*, not of definition.

`cache_stable.go:9-13` documents the constraint this creates: groups are
appended rather than interleaved "so that toggling a conditional group only
invalidates from that group onward."

## 2.4 Subagents — a *different* context, not a subset of the parent's

Corrected from the plan review. A subagent does **not** inherit the parent's
prompt or tools:

| Tier | Prompt constant | Bytes | Est. tokens | Tools | Tool bytes |
|---|---|---|---|---|---|
| discovery | `subagentSystemPrompt` (`subagent.go:432`) | 1 924 | 481 | `DiscoveryTools` (34) | 25 227 |
| edit | `subagentEditSystemPrompt` (`:454`) | 2 962 | 740 | `EditTools` (34) | 26 195 |
| tools | `subagentToolsSystemPrompt` (`:484`) | 2 353 | 588 | `buildSubagentTools` (discovery + search/LSP/browser/allowlisted MCP) | 25 227+ |

Selected at `internal/agent/subagent.go:1533-1541`. No preamble at all
(`ensurePreamble` no-ops when `InjectDate` is off, `app.go:2154-2158`), so a
subagent gets **no** AGENTS.md, no date line, no memory digest, no repo map.

Cache detail worth noting: `DiscoveryTools` is described as "the shared prefix
for cache stability" (`app.go:2642-2646`) — the edit and tools tiers extend the
same prefix so a parent that dispatches several children pays the cached
prefix once.

**Cost per request.** ~30k est. tokens per subagent invocation (prompt +
tools), versus ~25k for the parent's first request. A subagent is *not*
cheap; it is cheap **relative to the parent re-deriving the same information**.

## 2.5 Context collapse (the one existing "unload" mechanism)

`collapse_context` (`internal/agent/collapse.go`) and mid-turn collapse
replace finished requests with a summary + archive pointer in long-session
mode. This is the only mechanism that *removes* bytes from the request after
they were added, and it is the natural hook for unloading a module — but it
operates on whole turns, not on individual system messages, and it is
disabled in standard mode.

## 2.6 What is missing

| Capability | Status |
|---|---|
| Load prompt text by tool call | Exists once, statically (`maybeActivateExtension`), no unload |
| Load prompt text per repo | Missing (AGENTS.md is loaded unconditionally) |
| Offer a tool only when needed | Missing (only whole-session conditional groups) |
| Unload prompt text without a collapse | Missing |
| Per-module cache accounting | Missing (no instrumentation) |
| Module index for the model | Missing |
| User-editable writing style file | Partially: `agent_prompt_path` replaces *everything*; `style_guide_paths` replaces code style only. A writable "writing style" module does not exist. |

---

# Question 3 — Prompt caching

## 3.1 There is one wire shape, not three

Wakil has **one** outbound request shape for model calls: OpenAI Chat
Completions (`Client.Stream`, `internal/proxy/client.go:1152`, body struct at
`:1197-1253`). Endpoints have two `kind` values — `openai` (default) and
`ilm-proxy` (`internal/config/config.go:41`, `internal/proxy/client.go:472-473`)
— but both speak Chat Completions; `Kind` only changes headers, metadata, the
model field and default `max_tokens` (`client.go:1161`, `:1227`, `:1254-1268`).

**There is no direct Anthropic Messages API path.** `internal/counsel/hardening_test.go:385`
states it explicitly: "The direct Anthropic cache_control block shape is gone
with the direct path". So the three backends the question names reduce to:

| Backend in the question | What actually exists |
|---|---|
| OpenRouter | `Kind: "openai"` with an OpenRouter base URL. Cache breakpoints are emitted as **OpenAI-style `content` parts with a `cache_control` field**, which OpenRouter maps onto Anthropic/Gemini caching. |
| Anthropic direct | **Does not exist.** Anthropic is reached through OpenRouter as `anthropic/claude-*`. |
| Local proxy | `Kind: "ilm-proxy"` — same Chat Completions shape, plus `metadata.chat_id` and `X-Ilm-*` headers. |

## 3.2 Where the cache breakpoints are

`computeCacheBreakpoints` (`internal/proxy/client.go:308-325`):

```go
marked[0] = true                                  // first message, if it has content
marked[last non-null message] = true              // scanning backward
```

Exactly **two** breakpoints, always: the first message (the preamble at
`Conv[0]`) and the last message with non-null content (the moving tail). The
comment calls these "static" and "moving."

The breakpoints only exist when caching is on, decided by
`anthropicCacheHeuristic` (`client.go:342-349`):

- `cache_control` explicitly `true` in the endpoint config → on.
- explicitly `false` → off, overrides the heuristic.
- unset → on **iff** the model string contains `anthropic/claude` or starts with
  `claude-`.

So for every non-Anthropic model — a local llama.cpp/vLLM endpoint, or an
OpenRouter route to Gemini/DeepSeek — **Wakil emits no breakpoints at all** and
relies on automatic prefix caching. That matters for the design: a module
system's win on such models comes purely from a shorter prefix, not from
breakpoint placement.

Decoration happens per message in `marshalWireMessages`
(`internal/proxy/client.go:239-290`): a marked message with text content is
emitted as a one-element `content` array of
`{"type":"text","text":…,"cache_control":{"type":"ephemeral"}}`. Unmarked
messages stay plain strings, and the golden test
`TestCacheControlUnsetByteIdentical` (`internal/proxy/cache_control_test.go:21`)
proves requests are byte-identical to the pre-cache shape when caching is off.

Verified by test, not by reading only: `TestCacheControlMarksFirstAndLast`
(`cache_control_test.go:110`) asserts messages[0] and the last non-null message
get parts-shaped content and everything between stays plain.

## 3.3 Where tools sit relative to the cache prefix

**Answerable from the code:** `tools` is a **sibling top-level field** of
`messages` in the body (`client.go:1203` vs `:1202`), serialised as
`"tools":[…]` after `"messages":[…]` in the Go struct order — but JSON object
key order on the wire is irrelevant; what matters is the provider's cache
prefix construction, which is documented as tools-then-system-then-messages for
Anthropic. **This session cannot verify that from Wakil's code** — it is
provider-side behaviour. What Wakil's code *does* establish:

- Tools are re-sent on every request; there is no incremental tool protocol.
- `cache_stable.go:9-13` treats tool ordering as cache-critical and sorts
  within groups for exactly that reason.
- The breakpoints mark **messages only** — `computeCacheBreakpoints` takes
  `[]Message`, never touching `[]Tool`. So if a provider caches tools as part
  of the prefix, **Wakil places no breakpoint boundary after the tools block**;
  the first breakpoint is inside `messages[0]`.

This is an important open issue for the design, not a resolved fact. Marked
**unverified** (§ Unverified).

## 3.4 What changes between turns today

This is the decisive half of the question, and it is answerable from code.

| What | Changes when | Effect on the cached prefix |
|---|---|---|
| `Conv[0]` (preamble) | day rollover; `/prompt` switch; a session setting change (`invalidatePreambleLocked`, `app.go:2135`); network-mode change; memory-store statistics changing across a midnight boundary | **Invalidates everything after it** — the whole request. Deliberately limited to once per day (`ensurePreamble` doc comment, `app.go:2142-2155`). |
| `messages[1]` — the first user message, **including the retrieval block** | every turn, because it is the *first* user message of the session and the retrieval block is baked into it | Nothing after the fact: it is written once and stays. Byte-stable thereafter. |
| The **tail** message | every turn (new user message / tool result) | The moving breakpoint follows it; that is the intended design. |
| `tools` | only when a conditional group appears/disappears (config reload, MCP server reconnect changing `OpenTools`, `/backend` switch) | **Whole prefix**, if tools are inside it. |
| Style-guide system message at index 1 | `/styles enable|disable`, or per-turn path evidence changing the selection | **Everything after index 0** — this is a real, currently-shipped invalidation hazard: a turn that touches a `.py` file selects the Python guide and inserts a system message at index 1, busting the cache for the rest of the conversation. |
| Extension prompt at index 1 (core mode) | once per session, when the classifier first fires | One-time whole-prefix invalidation, then stable. |
| `metadata.chat_id` / `session_id` | never mid-session | none |

The retrieval block's placement is the single most important existing decision
for the design, and `internal/agent/retrieval.go:12-18` says so:

> The block is folded into the user message content (same pattern as workflow
> directives) — not injected as a separate system message — to preserve the
> prompt-cache prefix (Conv[0] byte stability).

That is the same constraint any module system faces, already solved once.

## 3.5 Mashūra (counsel) caching — separate path

`internal/counsel` builds its own request bodies and sends them through the
same transport (`internal/proxy/transport.go`). Its caching:
`PanelCallConfig.UseCache` is set **only for debate mode**
(`internal/counsel/oracle.go:167-170`), and the mechanism is OpenRouter
`session_id` affinity (`oracle.go:634-636`, `:1171`) plus the identical
briefing across rounds — not `cache_control` parts. So counsel's cost model is
unrelated to the main loop's, and a module system does not affect it except
that council briefings attach source files (already path-based).

## 3.6 What this means for where modules may be added

Derived strictly from 3.2-3.4:

1. **A module appended after the moving tail is free** with respect to the
   existing prefix (it is just more tail), but it is re-sent every subsequent
   turn — so it costs `module_bytes × remaining_turns` in cache-read terms,
   not zero.
2. **A module inserted at index 1 (a new system message) invalidates
   everything after `messages[0]` once.** With a cache-read discount around
   10×, one invalidation costs roughly the whole conversation's cached prefix
   once. For a long conversation that is far more than the module's own size —
   this is exactly why the plan review's cost-model objection (C3) is blocking.
3. **Removing a module** is the only operation that produces a pure saving, and
   it can only happen where the conversation is already being rewritten — i.e.
   at a collapse. Hence the design's "unload only at a collapse" rule is not a
   stylistic preference; it follows from the mechanics.

## 3.7 Measured cost of the things that would change

| Item | Bytes | Est. tokens |
|---|---|---|
| `prev` property added to every tool when prune is on | +6 073 | +1 518 |
| Tool set with all optional groups vs builtins only | +8 409 | +2 102 |
| Memory tool group (8 tools) | 5 437 | 1 359 |
| Skill-template tool group (3 tools) | 6 394 | 1 598 |
| Built-in Go style guide message | 1 535 | 383 |

These are the quantities a module system would move in or out of the prefix;
they are the inputs to the break-even arithmetic §7 must produce.

---

# Question 4 — The memory block(s)

## 4.1 The symptom and its cause

Every `/resume` row for sessions using memory starts with
`## Relevant context from memory (untrust…`. The chain:

1. `App.Send` prepends the retrieval block to the user text before storing it
   (`internal/agent/app.go:1266-1268`):
   ```go
   if memCtx := a.retrieveMemoryContext(ctx, stripRetrievalBlock(userText)); memCtx != "" {
       stored = memCtx + "\n" + stored
   }
   ```
2. `stored` becomes the `user` message content (`app.go:1283`), so the
   envelope is **persisted with the message**.
3. The resume picker renders the first user message verbatim:
   `SessionSummary.Turns()` (`internal/core/sessionclient/facade.go:102-117`)
   and `agent.SessionTurns` (`internal/agent/session.go:282-293`) both take
   `DerefStr(m.Content)` of the first `role=="user"` message — **unstripped**.
   `renderResumePicker` (`internal/tui/resume_picker.go:176-180`) truncates it
   to 40 chars, which is exactly enough to show the header and nothing else.

So it is a **display bug in the resume picker**: the strip function exists
(`stripRetrievalBlock`) and is used at index time, at handoff, and at
skill-creation, but not in the two `Turns()` implementations.

This is a finding to report, not to fix in this session (DO NOT ALTER: no
product code).

## 4.2 Two artifacts, not one (measured separately)

### (a) The memory digest line — inside `Conv[0]`

Built inline in `buildPreambleForNetwork` (`internal/agent/app.go:2044-2100`),
from `MemoryStore.Stats(ctx, 5)` (`internal/memory/store.go:1222`). Content:

- `Memory: N active durable entries`
- `, M mid-tier` when > 0
- `, P pending proposals` when > 0
- `Recent: ` up to 5 durable keys (`store.go:1241-1256`, `recentN=5`)
- `Namespaces: ` up to 15 prefixes with counts (`store.go:74` `prefixCap = 15`,
  `:1258-1269`), rendered with a 400-byte display budget (`app.go:2084`)
- `Use memory_search or memory_get to retrieve entries.`

Measured for this session's store, exactly as printed in the preamble above:
**591 B / 147 est. tokens.**

Properties:
- Lives in the **cached prefix**, at `messages[0]`.
- Deliberately **not live** — the comment at `app.go:2039-2043` says the
  digest is a session-start snapshot and invalidating on every memory write
  "would destroy prompt-cache prefix stability."
- Changes only at session start or day rollover.
- Contains **counts and key names**, no entry content. Key names are
  proprietary (they name the user's project history).

### (b) The retrieval block — prepended to the user message

Built by `retrieveMemoryContext` (`internal/agent/retrieval.go:51-102`),
formatted by `formatRetrievedContext` (`:119-170`).

- Cap: `retrievalCap = 2048` parent, `retrievalCapSubagent = 1024`
  (`retrieval.go:27-28`).
- Selection: FTS5 OR-query over the user text, up to 3 memory entries + 2
  skill entries (`:43-46`), active entries only, unconfirmed auto-skills
  dropped (`:88`).
- Envelope: header 88 B, end marker 26 B, separator 4 B, per-entry prefix
  `[key | taint] ` and a **500-byte per-entry value cap** (`:231`).
- Fully budgeted: the cap covers header, end marker, separators and the
  truncation notice (`:123-128`).
- Structural strip markers so retrieved content is never re-indexed
  (`retrievalBlockHeader`/`retrievalBlockEnd`, `:34`, `:40`; the strip is
  `stripRetrievalBlock`, `internal/agent/sessionhistory_bridge.go:181`).

Measured, this session's actual block as injected into this request:
**757 B / 189 est. tokens** (3 entries: a handoff, a ctx entry, a skill).

### Which one is "the memory block"?

Both are. They have opposite cache properties and must be decided separately:

| | Digest (a) | Retrieval (b) |
|---|---|---|
| Position | `messages[0]`, inside the cached prefix | first user message, **after** the breakpoint |
| Changes per turn | no | no (written once, frozen) |
| Changes per session | at start / day rollover | never after the first turn |
| Byte-stable across turns | yes | yes |
| Cost | 147 est. tokens, **always** | ≤512 est. tokens, **once** |
| Grows with memory size | no (bounded) | no (capped) |
| Leak surface | key names, namespace names | entry values (already the design intent) |

## 4.3 Is it part of the first user message?

The retrieval block: **yes**, by construction (`app.go:1266-1268` prepends
before `appendConv`), and it persists in `Conv` exactly as written — it is not
recomputed for later turns, and the first user message is never rewritten
(unless compaction/collapse rewrites the turn, at which point the block goes
with it).

The digest: **no** — it is inside `messages[0]`, the system preamble.

## 4.4 What changes if each becomes its own module

**If the digest becomes an on-demand module** (`memory_status`-style tool or a
`load_module("memory")`):
- Saves 147 est. tokens per request until loaded — trivially small.
- Costs one tool round-trip when the model actually needs memory counts.
- **Loses** the passive nudge: today the model learns *that memory exists and
  which namespaces are populated* without asking. Removing it will reduce
  memory use before the model knows to search. Net: likely a loss on
  quality, negligible on cost. Recommendation: keep as core, or shrink to one
  sentence ("Memory: N entries — use memory_search").

**If the retrieval block becomes a module:**
- It is already effectively a module — injected once, capped, at the first
  user message, after the cache breakpoint. Moving it "out" would mean
  deferring it to a tool call, which costs a round-trip and removes the
  zero-effort context the model currently gets.
- The real improvement available is **not** modularisation but fixing the
  resume-picker strip (§4.1) and possibly raising the 500-byte per-entry cap
  when a high-scoring entry is truncated.

**If the envelope moves into `Conv[0]`** (a tempting "simplification"): it
would invalidate the prefix every turn, because retrieval depends on the user's
text. This is strictly worse and the code comment at `retrieval.go:12-18`
already warns about it.

## 4.5 Findings to report

1. Resume-picker titles show the raw envelope — a display bug with a known
   fix (strip in `SessionTurns` / `SessionSummary.Turns`).
2. The digest is bounded and cheap; modularising it is not worth it.
3. The retrieval block is already correctly placed for caching; the only
   gains there are correctness (strip) and quality (per-entry cap), not bytes.

---

# Question 5 — Inventory

Every class below is a **proposal**, with its reason. "Core" = always in the
request; "workspace" = per repo, loaded once at session start; "on-demand" =
loaded when a task needs it; "remove" = the bytes are not worth their place.

## 5.1 Prompt sections

Sizes are the `##`-heading spans measured in §2.1 (`prompts/agent.txt`). All
sections currently ship in the full prompt (the default).

| Section | Bytes | Est. tokens | Needed by | Proposed class | Reason |
|---|---|---|---|---|---|
| Global precedence ladder | 2 385 | 596 | every turn — it is the conflict resolver | **core** | Cheap, and a wrong precedence decision is the most expensive failure mode. |
| Role | 830 | 207 | every turn | **core** | Identity; removing it changes nothing else. |
| Core loop | 3 401 | 850 | every turn | **core** | The procedure itself. |
| Secrets | 3 911 | 977 | every turn | **core** | Tier-1 safety. Never deferred. |
| Shell | 2 486 | 621 | any turn that calls `run_shell` | **core** (compact) | Nearly every session shells out; but ~40 % is approval-gate prose that could move to the tool description. Proposal: keep a 1 200 B version core, move the rest. |
| Files | 1 340 | 335 | any turn that reads/writes | **core** | Small. |
| External side-effect gating | 2 872 | 718 | any turn with a mutation | **core** | Tier-2 safety. Never deferred. |
| Honesty Invariants | 860 | 215 | every turn | **core** | Tier-4. |
| Recovery | 277 | 69 | rare | **core** | Negligible size. |
| Documentation upkeep | 1 353 | 338 | implement tasks only | **on-demand** (`implement`) | Only fires when code changes; 338 tokens saved on read/answer turns. |
| What "verified" means | 779 | 194 | turns that claim completion | **core** (compact) | It is the contract for every final report; but the per-output-class table could shrink to the three most-used rows. |
| Search & Research | 4 987 | 1 246 | turns that search | **on-demand** (`search`) | Large; only useful when the model searches or delegates. |
| File vs. Inline Decision | 585 | 146 | turns producing long content | **on-demand** (`write`) | Rare in coding sessions. |
| Skills | 1 617 | 404 | turns touching skills | **on-demand** (`skills`) | |
| Tool / Cost Discipline | 620 | 155 | rare | **remove** | Tier-5 guidance that fits in one sentence; cheap but redundant with the Role section. |
| Durable Memory | 8 402 | 2 100 | turns that read/write memory | **on-demand** (`memory`) | **The single biggest section.** Only needed when the model uses a memory tool — and `memory_search`'s own description can point at the module. |
| Truncated tool results & collapse | 2 772 | 693 | long sessions | **on-demand** (`long-session`) | Only meaningful when `context_mode: long-session` or results get spilled. |
| Async execution | 4 446 | 1 111 | turns using async tools | **on-demand** (`async`) | Large; the four async tool descriptions already carry most of the rule. |
| Subagents | 7 073 | 1 768 | turns that dispatch | **on-demand** (`subagents`) | **Second biggest.** The `dispatch_subagent` tool description duplicates the trigger rules. |
| Interaction & Output | 1 220 | 305 | every turn | **core** (compact) | Report structure matters every turn; could halve. |

Core total under this proposal: 2 385 + 830 + 3 401 + 3 911 + 1 200 + 1 340 +
2 872 + 860 + 277 + 400 + 620 ≈ **18 100 B ≈ 4 525 est. tokens**, versus
13 214 today for the whole prompt — a **~8 700 est.-token saving** on the
system prompt, at the cost of an index (below).

## 5.2 Tool definitions

Measured from the serialised `[]proxy.Tool` (§1.3). Only the default 49-tool
set is listed; conditional groups follow.

| Tool | Bytes | Est. tokens | Needed by | Proposed class | Reason |
|---|---|---|---|---|---|
| `capture_skill_template` | 3 746 | 936 | skill authoring | **on-demand** | Largest single definition; used rarely, and only after `skill-create` intent. |
| `dispatch_subagent` | 2 279 | 569 | delegation | **core** (shorten) | The description restates the whole Subagents prompt section; halve it. |
| `apply_skill_template` | 1 941 | 485 | skill authoring | **on-demand** | |
| `dispatch_subagents` | 1 921 | 480 | delegation | **core** (shorten) | Near-duplicate of the singular form's description. |
| `collapse_context` | 1 762 | 440 | long-session mode | **on-demand** (gated to `long-session`) | 440 tokens in a mode that may never be active. |
| `memory_put` | 1 613 | 403 | memory writes | **on-demand** (`memory` group) | |
| `save_skill` | 1 203 | 300 | skill authoring | **on-demand** | |
| `read_file` | 1 069 | 267 | every task | **core** | |
| `edit_file` | 1 032 | 258 | edit tasks | **core** | |
| `run_background` | 915 | 228 | server/dev tasks | **core** | Common enough; the async group is cohesive. |
| `load_skill` | 899 | 224 | skills | **core** (it is the loader for skill modules) | |
| `write_binary_file` | 896 | 224 | rare | **on-demand** (`edit-extras`) | |
| `staging_put` | 854 | 213 | subagent handoff | **on-demand** (`staging`) | |
| `update_skill` | 851 | 212 | skill authoring | **on-demand** | |
| `read_file_full` | 844 | 211 | every task | **core** | |
| `memory_promote_from_staging` | 812 | 203 | memory | **on-demand** (`memory`) | |
| `list_dir` | 805 | 201 | every task | **core** | |
| `search_files` | 791 | 197 | every task | **core** | |
| `run_shell` | 786 | 196 | every task | **core** | |
| `move_file` | 728 | 182 | refactors | **on-demand** (`edit-extras`) | |
| `check_skill_template` | 707 | 176 | skill authoring | **on-demand** | |
| `git_diff` | 688 | 172 | git work | **on-demand** (`git`) | 562 tokens for the whole git group, used only in git-aware sessions. |
| `memory_search` | 671 | 167 | memory | **on-demand** (`memory`) | But this is the *entry point* — must stay discoverable. |
| `check_pending` | 650 | 162 | async | **core** | Small, always relevant. |
| `memory_list` … `memory_forget` | 632+541+409+398+361 = 2 341 | 585 | memory | **on-demand** (`memory`) | |
| `delete_file`, `write_file` | 605+563 = 1 168 | 291 | edit tasks | **core** | |
| `find_files` | 599 | 149 | every task | **core** | |
| `math_eval` | 552 | 138 | rare | **on-demand** (`misc`) | |
| `open_url` | 512 | 128 | host interaction | **on-demand** (`misc`) | |
| `git_log`, `git_blame`, `git_show`, `git_status` | 510+406+340+304 = 1 560 | 390 | git work | **on-demand** (`git`) | |
| `skill_history`, `skill_search`, `list_skills` | 450+420+291 = 1 161 | 290 | skills | **on-demand** (`skills`) | Except `list_skills`/`skill_search` if skills stay core-discoverable. |
| `wait_for_completion` | 442 | 110 | async | **core** | |
| `read_process_log`, `kill_process` | 382+395 = 777 | 194 | async | **core** | |
| `staging_*` (get/get_many/list/delete) | 346+398+352+354 = 1 450 | 362 | subagent handoff | **on-demand** (`staging`) | |

Conditional groups (not in the default set):

| Group | Tools | Bytes | Est. tokens | Proposed class | Reason |
|---|---|---|---|---|---|
| Mashūra | 4 | not measured (no key) | — | **on-demand** | Already gated by config; could be gated by intent too. |
| LSP | 4 | 2 575 | 643 | **on-demand** | Only useful in languages with a server. |
| Browser | 8 | 4 685 | 1 171 | **on-demand** | Largest optional block. |
| Search (searxng) | 2 | 1 135 | 283 | **on-demand** | |
| MCP | variable | unbounded | — | **on-demand, essential** | MCP servers can contribute arbitrarily many tools; this is where the design pays off most. |

## 5.3 The size of the proposal

| Bucket | Bytes | Est. tokens |
|---|---|---|
| Core tools (proposed) | ≈ 16 500 | ≈ 4 100 |
| On-demand tools | ≈ 23 000 | ≈ 5 800 |
| **Total today** | **39 563** | **9 890** |

So the tool side has ~5 800 est. tokens of movable bytes — but the index and
the loader tool cost some of that back (§7).

## 5.4 Caveat

These are single-configuration measurements (default config, this repo).
Conditional on: `prompt_mode`, `context_prune`, `searxng_url`, `lsp_enabled`,
`browser_enabled`, MCP servers, `--no-memory`, `style_guides_enabled`. A
single static table understates MCP-heavy or Docker-heavy setups and
overstates the bench's `--no-memory` headless runs (which lack the 1 359
est.-token memory group and the 147-token digest).

---

# Question 6 — Usage measurement

## 6.1 What the script reads

Wakil's trace files are JSONL, one `Record` per line
(`internal/trace/trace.go:31-62`). The fields the script uses:

- `type` — `"store_header"` or `"turn"`; only `turn` records carry tool calls.
- `session_id` — the session a turn belongs to.
- `tool_calls[]` — a list of `ToolTrace` objects
  (`internal/trace/trace.go:66-71`) whose `name` is the tool name.

Everything else in a record (arguments, results, content, byte counts, model,
workspace) is **never named** by the reader, so it cannot be printed.

## 6.2 `scripts/context_usage.sh`

```
usage: context_usage.sh TRACE [TRACE ...]
  prints TOOL<TAB>CALLS<TAB>SESSIONS for every tool in the traces
```

- Output is exactly a header row plus one row per tool, tab separated,
  sorted by `CALLS` descending then `TOOL` ascending.
- `SESSIONS` counts distinct `session_id`s, so the same session appearing in
  two files counts once.
- A missing file, a directory, an unreadable file or a malformed trace is a
  **hard error** naming only the path — a silently skipped file would make a
  usage report quietly wrong.
- `--help` prints usage to stdout; no arguments prints it to stderr and exits 2.

## 6.3 Why it cannot leak

The jq program names three fields and emits one derived pair:

```jq
select(type == "object" and .type == "turn")
| ((.session_id // "") | tostring) as $sid
| (.tool_calls // []) | .[]
| select(type == "object")
| select(.name | type == "string")
| [(.name), $sid]
| @tsv
```

There is no `tostring` on a whole record, no `debug`, no `cat` of an input
file, and every error message is built from `$prog` and `$f` (the path the
user supplied), never from file content. The awk stage prints only
`name`, `calls`, `sessions`.

## 6.4 The test, and what it actually proves

`scripts/context_usage_test.sh` builds synthetic traces in which **every**
content-bearing field carries the marker
`SECRET-MARKER-4f1c2a-do-not-print`: `tool_arguments`, `tool_result`,
`message`, `memory_key`, `model`, `workspace`, and a malformed line containing
it. It then checks ten cases:

```
$ sh scripts/context_usage_test.sh
  PASS counts and layout for one file
  PASS counts accumulate across two files
  PASS a repeated session id counts once for SESSIONS
  PASS an unusual tool name is printed, not evaluated
  PASS --help prints usage
  PASS no arguments exits non-zero
  PASS a missing file exits non-zero
  PASS a directory argument exits non-zero
  PASS a malformed trace exits non-zero
  PASS an empty trace yields the header only
context_usage_test: all checks passed
```

The marker assertion runs on **stdout and stderr** for the normal run, the
two-file run, `--help`, the missing-file path and the malformed-trace path.

The test was itself checked for brain-deadness: printing every raw record to
stderr (a deliberate leak) makes it report

```
  FAIL one file (stderr): the secret marker leaked into the output
  FAIL two files (stderr): the secret marker leaked into the output
  FAIL malformed trace (stderr): the secret marker leaked into the output
```

so the assertion is not vacuous. (The leak was reverted before commit.)

Also verified: the script and the test both pass under `sh` (bash) and under
`dash`, and both are accepted by `sh -n`. **`shellcheck` is not installed in
this environment**, so the `shellcheck -s sh` gate CI runs has **not** been
exercised locally — marked in §Unverified.

## 6.5 The host command for the user

Trace files live under the user's data directory, which this session may not
read. The user runs:

```sh
# every trace in the default store, one report
sh scripts/context_usage.sh "$HOME"/.local/share/wakil/traces/*.jsonl

# a single session
sh scripts/context_usage.sh "$HOME"/.local/share/wakil/traces/<chat-id>.jsonl

# compare two periods
sh scripts/context_usage.sh before/*.jsonl > before.tsv
sh scripts/context_usage.sh after/*.jsonl  > after.tsv
```

(The exact directory name should be confirmed with `wakil doctor` or by
listing `$HOME/.local/share/wakil/` — this session did not open it, per the
task's rule.)

**Safety note for the user:** the output contains tool names and counts only.
Do not add `-x`/`set -x` when running it, and do not pipe it through a tool
that echoes its input.
