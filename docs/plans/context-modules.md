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
| Core tools (proposed) | ≈ 14 200 | ≈ 3 550 |
| On-demand tools | ≈ 25 400 | ≈ 6 340 |
| **Total today** | **39 563** | **9 890** |

So the tool side has ≈ 6 340 est. tokens of movable bytes — but the index and
the loader tool cost ≈ 253 back (§7). §7.1.1 is the reconciled core list; this
table is the pre-reconciliation split and the two agree on the total.

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
`message`, `memory_key`, `model`, `workspace`, an **extra field inside a
`tool_calls[]` entry**, and a malformed line containing it. It then checks
fourteen cases:

```
$ sh scripts/context_usage_test.sh
  PASS counts and layout for one file
  PASS counts accumulate across two files
  PASS a repeated session id counts once for SESSIONS
  PASS an unusual tool name is printed, not evaluated
  PASS an entry with extra fields still counts
  PASS --help exits zero
  PASS --help prints usage
  PASS no arguments exits non-zero
  PASS a missing file exits non-zero
  PASS a directory argument exits non-zero
  PASS an unreadable file exits non-zero
  PASS a non-trace JSON file exits non-zero
  PASS a malformed trace exits non-zero
  PASS an empty trace yields the header only
context_usage_test: all checks passed
```

Corrections after review:
- The draft's header claimed an unreadable-file case that **did not exist**;
  case 8b now does (skipped under root, where `-r` is moot).
- The marker assertion now also runs on stdout for the two-file and
  in-`tool_calls` cases, not only stderr.
- The shell-metacharacter fixture heredoc is **quoted** so the expansion the
  test claims to disprove does not happen at fixture-write time.
- `--help`'s exit code is asserted, not swallowed.
- A non-trace JSON file is now an error (case 8c), matching §6.2's claim.

The test was itself checked for brain-deadness: printing every raw record to
stderr (a deliberate leak) makes it report four failures, including
`FAIL one file (stderr): the secret marker leaked into the output`. The leak was
reverted before commit. (This brain-deadness check is a manual step recorded
here; it is **not** scripted into the test, so it is not reproducible from the
file alone — a limitation, not a hidden one.)

Also verified: both scripts pass under `sh` (bash) and `dash`, and both are
accepted by `sh -n`. **`shellcheck` is not installed in this environment**, so
the `shellcheck -s sh` gate CI runs has **not** been exercised locally — see
§Unverified.

### Exit-status honesty

POSIX `sh` has no `pipefail`, so the original pipeline
(`awk … | sort … | { printf; cat; }`) would have masked an `awk` or `sort`
failure behind the trailing `printf`. The script now writes the totals to a
temp file, checks `awk`'s status, then checks `sort`'s — so a failure surfaces
as exit 2 instead of an empty-but-successful report.

### A residual risk on tool names

Tool names are printed verbatim, and MCP tool names are server-supplied
(`server__tool`), so with an MCP server a *name* is effectively untrusted
output. The script cannot sanitise it without breaking its purpose; the honest
statement is: the privacy contract covers everything the trace records *about*
the conversation, but a hostile MCP server could encode content in a tool
name. That is a property of the MCP server, not of this script, and it is
recorded rather than silently accepted.

## 6.5 The host command for the user

Trace files live under the user's data directory, which this session may not
read. The user runs:

```sh
# every trace in the default store, one report
sh scripts/context_usage.sh "$HOME"/.local/share/wakil/traces/*.jsonl

# exact module session-union counts (see F1.3.8 for the full set of --union specs)
sh scripts/context_usage.sh "$HOME"/.local/share/wakil/traces/*.jsonl \
  --union "subagents=dispatch_subagent,dispatch_subagents"

# a single session
sh scripts/context_usage.sh "$HOME"/.local/share/wakil/traces/<chat-id>.jsonl

# compare two periods
sh scripts/context_usage.sh before/*.jsonl > before.tsv
sh scripts/context_usage.sh after/*.jsonl  > after.tsv
```

(The exact directory name should be confirmed with `wakil doctor` or by
listing `$HOME/.local/share/wakil/` — this session did not open it, per the
task's rule.)

A broken line in a file is counted and skipped, and the count appears on
stderr; that is expected for traces written before the fix above.

**Safety note for the user:** the output contains tool names and counts only.
Do not add `-x`/`set -x` when running it, and do not pipe it through a tool
that echoes its input.

---

# Question 7 — Design

## 7.0 The cost model everything else follows from

### 7.0.1 The ledger (corrected)

An earlier draft of this section omitted the non-prompt part of the preamble
and stated 9 261 core tokens. That was wrong by 2 119 est. tokens — the
AGENTS.md section (1 676) and the digest/date/cwd/environment lines (~443),
which §7.10 keeps in core. The ledger below is the corrected one, in **one
unit (estimated tokens)** throughout:

| Part | Today | Proposed core | Note |
|---|---|---|---|
| Prompt sections | 13 214 | 4 525 | §5.1; assumes Shell/verified/Interaction are each ~halved (**judgement, not measured**) |
| AGENTS.md section | 1 676 | 1 676 | kept core (§7.10) |
| Digest + date + cwd + sandbox + repo map + network | 443 | 443 | kept core |
| Tool definitions | 9 890 | ≈ 3 550 | §5.2 corrected for the double-count (§7.1.1) |
| Writing style | 0 | 383 | only when a style file/default is active (§7.5) |
| Module index | 0 | 172 | §7.2 |
| `load_module` definition | 0 | 81 | |
| **Total static prefix** | **25 223** | **≈ 10 830** | |

So the **corrected core is ≈ 10 830 est. tokens**, inside the 10-12k target —
and the target is met with ~1 200 tokens of margin. The load-bearing assumption is
the prompt cuts: **if the Shell / "verified" / Interaction cuts are rejected,
core is ≈ 12 005 and the 10-12k target is missed.** Any repo whose AGENTS.md is
larger than this one's moves further out. §5.4 already warns that a single
static table misstates the typical case.

| Quantity | Corrected value |
|---|---|
| Static prefix today | 25 223 est. tokens/request |
| Corrected core + style + index + loader | ≈ 10 830 est. tokens |
| Raw prefix reduction | ≈ 14 393 est. tokens (**estimate**) |
| Recurring saving at a 0.1× cache-read discount | ≈ 1 439 token-equiv/request (**estimate**) |
| One module-load round trip over a 10 830-token prefix | ≈ 1 360 token-equiv (see 7.0.2) |

### 7.0.2 The break-even, stated the right way round

The draft got the direction of this wrong, and the correction changes the
recommendation. Once a module is **loaded**, its bytes are re-sent on every
subsequent request — so loading it mid-session does **not** amortise; the load
cost is paid once on top of a cost you then pay anyway.

What a mid-session load actually buys is: **the saving on the requests that
happened before it was loaded**, minus the load cost. Therefore:

- If a module is needed on the **first** request of a session, loading it later
  is **strictly worse than having it in core** — no number of later turns
  recovers it.
- A mid-session load pays off only when the module was **not** needed for some
  number of requests, and the alternative was having it in core all along. That
  is: `requests_without_needing_it × module_size × discount > load_cost`.
- Pre-loading (from the task text, or from session config) has **no load cost**
  and is therefore always correct when the prediction is right — which is why
  §7.3 makes it the default and §7.5's style file is static.

Recalculated table — "how many requests must a module be **not needed** before
one mid-session load repays itself" (cache-read 0.1×, load ≈ 1 360
token-equiv):

| Module size | Saved per request while unloaded | Requests without needing it to break even |
|---|---|---|
| 3 459 (memory) | 346 | 3.9 |
| 2 750 (skills) | 275 | 4.9 |
| 1 768 (subagents) | 177 | 7.7 |
| 1 529 (search) | 153 | 8.9 |
| 1 133 (long-session) | 113 | 12.0 |
| 1 111 (async) | 111 | 12.3 |
| 562 (git) | 56 | 24.2 |
| 406 (edit-extras) | 41 | 33.5 |
| 338 (documentation upkeep) | 34 | 40.3 |
| 172 (the index itself) | 17 | 80.6 |

**Load cost, honestly priced** (this was understated too):

- the cached prefix re-read: `10 830 × 0.1 ≈ 1 083` token-equiv, and on a later
  request it is `core + conversation so far`, not just core;
- **plus** the tool call's output tokens. The draft priced 120 output tokens
  at 1:1; output typically bills at 3-5× **uncached input**, so 120 output
  tokens is ≈ 360-600 input-token-equiv (**recall, unverified for this
  provider** — must be read from `internal/proxy/cost.go` and the provider's
  rate card before it is trusted);
- **plus** the cache-write premium when the module's bytes are first inserted
  (**recall: ~1.25×**, unverified);
- **plus** the module's own bytes on that request.

`10 830 × 0.1 + 480 ≈ 1 563` token-equiv with the output multiplier included,
rather than 1 360. The table above uses the conservative 1 360; the real
figure is worse, which strengthens the conclusion.

**Consequence, and it is the central finding:** for short sessions — which is
every bench task except `rename` — **module loading does not pay for itself at
all.** The real, unconditional win is the **prefix reduction** (stage 1 + 2),
which needs no model cooperation and no round trip. Module *loading* is a
long-session mechanism, and the design must say so plainly rather than sell it
as a general saving.

**The honest counter-case, corrected.** The draft claimed "if there is no cache
discount, module loading becomes attractive." That is wrong for the same reason:
with no discount, a mid-session load costs the **entire prefix** (≥ 11k) and
saves at most one module (≤ 2.1k) per request — it never repays. What a missing
discount changes is that the **prefix reduction becomes 10× more valuable**
(≈ 14 393 token-equiv per request instead of ≈ 1 439), which makes stages 1-2
*more* worth doing and module loading *no* more attractive. The design is
therefore **unconditionally right about stages 1-2** and **conditional about
loading**.

## 7.1 The module list

> **Consistency note (corrected after review).** An earlier draft of this
> section contradicted §5. The rules now applied, once, and used consistently
> below and in §7.2:
>
> - **`dispatch_subagent` / `dispatch_subagents` are CORE** (they are the
>   entry points for the most common verb) — the `subagents` module carries
>   only the *prompt section*, not these two definitions. §7.1's earlier
>   "1 768 + 1 050" double-counted; the module is 1 768 only.
> - **The five async tools are CORE** (`check_pending`,
>   `wait_for_completion`, `read_process_log`, `kill_process`,
>   `run_background`). The `async` module carries **only the prompt section**
>   (1 111), not 4 546 B of tools. Earlier draft double-counted.
> - **`delete_file` is core** (it pairs with `write_file`/`edit_file`).
>   `edit-extras` = `move_file` + `write_binary_file` only.
> - **"What verified means" is core (compact)**; `implement` = Documentation
>   upkeep only.
> - **The memory digest line stays core** (§4.4); the `memory` module carries
>   the prompt section + the 8 memory tool definitions, and the digest stays
>   in the prefix.
> - **`memory_search` and `list_skills` stay core** as discovery handles, even
>   though their sibling tools are deferred.

Twelve modules. Sizes are the measured §5 values, rounded; all are
**estimates**.

| Module | Contains | Bytes | Est. tokens |
|---|---|---|---|
| `memory` | Durable Memory section + 8 memory tool defs (digest stays core) | ≈ 8 400 + 5 437 | 2 100 + 1 359 = **3 459** |
| `subagents` | Subagents section **only** (both dispatch tools are core) | ≈ 7 073 | **1 768** |
| `search` | Search & Research section + searxng tools | ≈ 4 987 + 1 135 | 1 246 + 283 = **1 529** |
| `async` | Async execution section **only** (all 5 tools are core) | ≈ 4 446 | **1 111** |
| `skills` | Skills section + 7 skill tool defs (list/search/load stay core) | ≈ 1 617 + 9 386 | 404 + 2 346 = **2 750** |
| `long-session` | Truncated results/collapse section + `collapse_context` | ≈ 2 772 + 1 762 | 693 + 440 = **1 133** |
| `git` | 5 git tool defs | 2 248 | **562** |
| `staging` | 5 staging tool defs | 2 304 | **576** |
| `edit-extras` | `move_file`, `write_binary_file` | ≈ 1 624 | **406** |
| `implement` | Documentation upkeep only | 1 353 | **338** |
| `write` | File vs. Inline | 585 | **146** |
| `misc` | `math_eval`, `open_url` | 1 064 | **266** |
| *(conditional)* `browser` | 8 browser tools | 4 685 | 1 171 |
| *(conditional)* `lsp` | 4 LSP tools | 2 575 | 643 |
| *(conditional)* `mashura` | 4 Mashūra tools | not measured (no key) | — |
| *(conditional)* `mcp` | per-server tools | unbounded | — |

**MCP/LSP/Browser/Mashūra now have modules** — the earlier draft omitted them,
which contradicted §5.2's "where the design pays off most." They are
config-gated as today, so their module is simply the mechanism that keeps them
out of the core array.

### 7.1.1 The core tool list, and its sum

Core tools (§5.2 + the rules above), measured from the per-tool table:

| Tool | Bytes |
|---|---|
| `read_file` | 1 069 |
| `read_file_full` | 844 |
| `list_dir` | 805 |
| `search_files` | 791 |
| `find_files` | 599 |
| `run_shell` | 786 |
| `write_file` | 563 |
| `edit_file` | 1 032 |
| `delete_file` | 605 |
| `run_background` | 915 |
| `kill_process` | 395 |
| `read_process_log` | 382 |
| `check_pending` | 650 |
| `wait_for_completion` | 442 |
| `dispatch_subagent` (shortened ≈ −50 %) | ≈ 1 140 |
| `dispatch_subagents` (shortened ≈ −50 %) | ≈ 960 |
| `memory_search` | 671 |
| `list_skills` | 291 |
| `load_skill` | 899 |
| `load_module` (new) | ≈ 325 |
| **Total** | **≈ 14 164 B ≈ 3 541 est. tokens** |

So the corrected core tool figure is **≈ 3 550 est. tokens**, not 4 100. The
§7.0 ledger uses 4 100 as a conservative upper bound (it leaves room for
`context_mark` when pruning is on, which adds ≈ 1 518 est. tokens on top —
another reason prune-on must be measured separately).

### 7.1.2 Is 10-12k reachable?

**Yes at ≈ 10 830 est. tokens**, but with two explicit dependencies:

1. **The prompt cuts** in §5.1 (Shell, "verified", Interaction each roughly
   halved) are a **judgement, not a measurement**. Reject them and core is
   ≈ 12 005 — outside the target.
2. **AGENTS.md stays core** (§7.10), so any repo with a bigger AGENTS.md is
   outside the target by definition. This is an accepted limitation, not an
   oversight.

If the tool side keeps `dispatch_subagent(s)` unshortened and
`memory_search`/`list_skills` core, the tool figure is ≈ 4 700 and examined
total ≈ 12 000 — still just inside, with no margin.

## 7.2 The index format

One line per module, always loaded, appended after the core prompt (inside
`Conv[0]`, so it costs nothing extra after the first request).

```
## Context modules
Not loaded by default. Call load_module(name) to load one; unknown name lists all.
  memory       — durable memory rules and 8 memory tools (~2.1k tok)
  subagents    — dispatch rules, tiers, budgets (~2.8k tok)
  search       — tool priority, Mashūra, evidence screening (~1.5k tok)
  async        — check_pending, wait_for_completion, background jobs (~2.2k tok)
  skills       — skill store, load_skill, templates (~3.2k tok)
  long-session — collapse, pruning, spilled tool results (~1.1k tok)
  git          — git_status/diff/log/show/blame (~0.6k tok)
  staging      — ephemeral handoff KV (~0.6k tok)
  edit-extras  — move_file, write_binary_file, delete_file (~0.6k tok)
  implement    — documentation upkeep, verified-by-output-class (~0.5k tok)
  write        — file vs. inline decisions (~0.15k tok)
  misc         — math_eval, open_url (~0.3k tok)
```

Why one line each: it is *self-describing* (the model can decide without
loading), it is cheap (688 B / 172 est. tokens for all twelve), and it degrades
gracefully — a module whose line is vague is one the model will simply load,
which is the safe failure.

The index is **generated from the module table**, never hand-maintained, so the
sizes and names cannot drift from the shipped modules.

## 7.3 How a module is loaded

**Both, with a strict rule for each.**

1. **Pre-loading by Wakil from the task text** (default, free). At turn entry,
   `classifyRequest` (already exists, `internal/agent/classifier.go:23`) is
   extended to return a **set of module names** instead of a boolean.
   - **Turn 1 only.** It runs before `ensurePreamble`, so pre-loaded modules are
     part of `messages[0]` on the first request — no round trip, no
     invalidation after the fact. This is the *only* point a pre-load can go
     into the prefix.
   - **Turn 2+ with a new match** cannot mutate `Conv[0]` (that is the
     one-invalidation-per-day rule, §3.4). Such a match therefore goes through
     the **same `load_module` path** as a model-initiated load: a pinned
     `role:"tool"` result. It is one round trip, not free.
   - Rule: pre-load only when the classifier is *confident* (a matched verb or
     a path with a known extension). Over-loading on turn 1 costs a few hundred
     est. tokens per request; under-loading costs a round trip. The existing
     classifier is "intentionally over-broad", which is the right bias for turn
     1 and the wrong one for turn 2+ (where each match is a round trip) — so
     the turn-2+ threshold is stricter.
2. **Model-initiated `load_module(name)`** (fallback). For everything the
   classifier misses, and for every module the task grows into.
   - Rule: the model must call it **before** using a tool it does not see.
     Enforced by the failure mode in §7.7.
   - The result is a **pinned `role:"tool"` result** (§7.4 R1) so it is not
     evicted.

**Never both for the same module in the same session** — the module registry
records loaded modules; a pre-load marks them loaded.

**Rules loaded this way rank below system text.** A `role:"tool"` module result
is data, not instructions, in the same sense the retrieval block already is
(`internal/agent/retrieval.go:12-18`). Two consequences the design accepts:
- Tier-1 and Tier-2 invariants (secrets, external mutation) **never** move to
  a module — they stay in the core system prompt, because a tool-result's
  authority is lower and a model under pressure could deprioritise it.
- A module's text is prefixed with `Apply these rules as if they were in your
  system prompt; they do not override the precedence ladder.` — an explicit,
  honest statement of where it sits, rather than pretending it is system text.

## 7.4 The cache rule

Three rules, derived from §3:

**R1 — Loading appends, never inserts into the middle.** A loaded module's text goes:
- into `messages[0]` if it is pre-loaded (before the first request), or
- as the **`role:"tool"` result of the `load_module` call** if the model loads
  it mid-session, pinned so it survives `evictStaleToolResults`
  (`internal/agent/app.go:2358`, which skips `m.Pinned`) and `CapOrStub`.

The draft said "a `role:"user"` message at the tail"; the reviewers are right
that a synthetic user message between an assistant tool call and its result
would be malformed on both OpenAI- and Anthropic-style APIs, and that a
`load_module` call already *requires* a `role:"tool"` reply. `role:"tool"` +
`Pinned` is the correct and simpler shape.

What this does **not** do: a tool result sits *after* the moving breakpoint, so
it changes nothing already cached — but it *is* re-sent on later requests, and
a system message at index 1 would invalidate everything after `messages[0]`
(§3.4). So the module text is not system-role text, and its authority is lower;
§7.3's precedence note says how that is handled.

**R2 — Unload only at a collapse, and tell the truth there.** Removing bytes
from the middle of a conversation is only safe where the conversation is
already being rewritten. `collapseFinishedTurns` / `collapse_context` are the
only such points.

The draft's R2 said a collapse note would read "still available — do not
reload" while the registry dropped the module. That is a **falsehood the model
would act on**: a tail-loaded module's text is inside the collapsed span, so
after collapse it is *gone*. Corrected rule:

- **Pre-loaded modules** (in `messages[0]`) cannot be unloaded without
  invalidating the prefix, so they are **never** unloaded. They are in the
  core prefix for the session by construction.
- **Tail-loaded modules** are gone once their turn is collapsed. The collapse
  note must say so:
  `modules loaded this session and since collapsed away: memory, git — call load_module(name) again if still needed.`
- **Nothing is silently dropped.** The registry keeps the names (so a reload is
  cheap and the model is not confused), drops the *text*, and the note states
  exactly that.
- "Used since the last collapse" is defined only for modules with a tool
  (`memory`, `git`, `staging`, `skills`, `search`, …). **Prompt-only modules**
  (`implement`, `write`, `long-session`) have no signal, so they are treated as
  used if they were pre-loaded and never unloaded — i.e. their unload is a
  no-op.

Net effect: in practice **nothing is unloaded mid-session**; a collapse simply
tells the model which loaded modules it lost. That is a weaker claim than the
draft made, and it is the only claim the mechanics support.

**R3 — Tools are split once, at session start; mid-session a generic dispatcher
is used.** The draft's R3 said "never mutate `tools` mid-session" and then left
Stage 1's mechanism undefined — a contradiction all three reviewers caught.
Resolved as follows:

- `tools` is **fixed for the session** at `BuildTools` time: core tools only.
  It never grows mid-session (that is the cache protection R3 was for).
- A deferred tool is invoked through **one always-present generic
  dispatcher**, `load_module` — i.e. `load_module(name)` both loads a module's
  *rules* and returns the *schemas* of its tools, and the actual call goes
  through a second always-present entry, `call_tool(name, arguments)`.
- **The trade-off is stated, not hidden:** a generic dispatcher loses
  provider-side JSON-schema validation and per-tool argument checking. That is
  the price of keeping `tools` byte-stable. It is mitigated by validating
  `call_tool`'s arguments against the schema Wakil already holds before
  dispatching, and by gating a `call_tool` call exactly as the named tool would
  be gated (`IsGatedTool` lookup on the inner name — the gate list is a single
  source in `internal/tools/tools.go`).
- **This is a real decision, and it has an alternative:** if §3.3's
  measurement shows `tools` is **not** in the cacheable prefix, then R3 is
  unnecessary and deferred tools can be added to the array directly, keeping
  full schema validation. §7.9 Stage 1 therefore *starts* by measuring that,
  and the dispatcher exists only if the measurement says tools are cached.

So Stage 1 is no longer circular: it first measures the tools-cache position,
then either (a) adds tools mid-session (if uncached) or (b) ships the
dispatcher (if cached). The design commits to (b) as the default because it is
safe under either answer, and lists (a) as the simpler variant if the
measurement permits.

## 7.5 The writing style as a user-editable file

Today `agent_prompt_path` **replaces the entire prompt** — editing it to change
the writing style throws away every rule. `style_guides` only covers code
style. Neither is the right home.

Design:

- One file: `${XDG_CONFIG_HOME:-$HOME/.config}/wakil/writing-style.md`.
- **Folded into `messages[0]`, not a separate index-1 message.** The draft put
  it at index 1, which §3.4 itself calls a shipped invalidation hazard and
  which collides with the code style-guide message's slot. The writing style is
  session-static (it does not depend on the task), so it belongs in the stable
  prefix: `buildPreambleForNetwork` appends it after the core prompt and before
  AGENTS.md, so it is inside `messages[0]` and costs nothing after the first
  request.
- **Size cap.** A user file can be arbitrary; cap the section at 4 096 B
  (≈1 024 est. tokens) and mark truncation in the section header, exactly as
  `agentsMDMaxFile` does (`internal/agent/agentsmd.go:21`).
- **Precedence**: Wakil's safety rules > AGENTS.md > the user's style file >
  built-in default. Same ladder the prompt already states.
- **Never overwritten by a prompt update**: the file is user-owned and outside
  the repo. A binary update that changes `prompts/agent.txt` does not touch it.
  The built-in default ships embedded (`prompts/style.txt`) and is used when
  the file is absent — same precedence shape as `loadAgentPrompt`
  (`internal/wiring/bootstrap.go:649`).
- **No version line on the user's file.** The draft proposed reusing
  `loadAgentPrompt`'s version mechanism; that would make an unversioned user
  file a second-class citizen and risks a silent fallback to the default. A
  writing style is prose, not a contract — no version, no fallback: absent file
  → default, present file → user's, always.
- `wakil init` writes the default on request; `wakil config show` reports which
  one is active (source), mirroring `/prompt status`
  (`internal/agent/commands.go:361-372`).

This is the smallest change of the four stages and the most clearly valuable:
it turns "edit your whole system prompt to change tone" into "edit one file".

## 7.6 How subagents, headless and Mashūra get their modules

**Subagents.** They already get their own short prompt and curated toolset
(§2.4) — they do *not* inherit. So:
- A subagent gets a **fixed module set by tier**: discovery → none (its prompt
  already fits in 481 est. tokens); edit → none; tools → `search` only.
- A subagent **cannot** call `load_module`: it has no index, no loader, and its
  round-trip budget is for the task. This keeps child cost predictable, which
  is what makes parallel dispatch worth it.
- The parent's active modules do **not** propagate — a child that needed the
  memory rules would be a design smell (the parent should have done that work).

**Headless (`wakil run`).** Same `BuildApp` path, so the same core+index.
Differences:
- `--no-memory` removes the `memory` module and its digest entirely (already
  true today for the digest and tools).
- The bench's single-turn tasks mean pre-loading is the only useful load, so
  the classifier's coverage matters more headless than in the TUI. §8 measures
  exactly that.

**Mashūra members.** Counsel builds its own request bodies
(`internal/counsel`) — a panel member is a single, self-contained call with a
briefing. It should **not** get the module system at all: it has no
conversation, no tools, and no second turn. Its briefing is already
task-scoped. The only shared piece is that the *main agent* may need the
`search` module loaded to know how to prepare evidence before calling a panel
(evidence screening rules live there) — which stage 1 and 2 handle for the
parent, not for the member.

## 7.7 When a model needs a module it did not load

**Detection.** Three signals, not two — the draft's single unknown-tool signal
is unreliable, because a model that never learned a tool exists usually does
not call it by name; it substitutes a core tool or answers without it.

1. **Unknown tool call** (rare but exact). `executeToolCall` falls through
   `default: handleMCPTool` (`internal/agent/app.go:3407-3408`), which returns
   `ERROR: unknown tool "name"` for a non-MCP name
   (`internal/agent/tool_handlers.go:2077`). For names that belong to a
   module's tool set, replace it with:
   `ERROR: tool "memory_put" belongs to module "memory", which is not loaded.
   Call load_module("memory") and then retry.`
2. **Substitution signal** (the common case). The model calls a *core* tool in
   a way that only makes sense if it had the module: e.g. `run_shell` with a
   `git` command while `git` is unloaded, or `write_file` to a `*.md` while
   `implement` is unloaded. This is a heuristic and is **not** a hard gate;
   it appends one line to the tool result: `(the git module is not loaded —
   load_module("git") for the structured git tools)`. Cheap, truthful, and it
   does not block the model's workaround.
3. **Self-report.** The model says it cannot find a rule. The index makes this
   rare.

**Fallback.** `load_module` is always core, so recovery is one call.

**What must not happen:** silently answering without the rules — and for
prompt-only modules (`memory`, `implement`) there is *no tool call to fail*, so
signal 1 cannot fire at all. The design accepts this: for those modules the
mitigation is pre-loading (§7.3) and the index being explicit, not detection.
This is stated as a **known limitation**, not solved.

**`/auto` interaction — corrected.** The draft had `/auto` auto-load after the
error, which makes the model's next `load_module` call a redundant round trip.
Resolved: **there is no auto-load.** The error text always names the module and
the call. One mechanism, one message, no contradiction.

## 7.8 What this costs when it goes wrong

Stated plainly, because the user asked for "more expensive or less reliable":

| Failure | Cost |
|---|---|
| Model loads a module it did not need | ≈ 1 360 token-equiv (more if output bills above input) + the module's bytes re-sent per turn |
| Model fails to load a needed module | one wasted tool call, then the error message teaches it |
| A module inserted as a system message at index 1 | one whole-prefix invalidation — avoided by R1 |
| Tools mutated mid-session | one whole-prefix invalidation (if tools are in the prefix) — avoided by R3 |
| A loaded module evicted by `ToolResultTTL` mid-task | reload round trip + confusion — avoided by pinning |
| AGENTS.md deferred and the model never loads it | project rules silently skipped — **the worst failure**; see §7.10 |
| Index drifts from reality | model loads a module that does not exist, or misses one — avoided by generating the index |

## 7.9 The stages

Ordered so each stage is independently valuable and each later stage can be
skipped if the bench says no.

### Stage 0 — Measure first (precondition, no code)

**What.** Two measurements that can invert the design, both before any change:

1. **Is `tools` inside the provider's cacheable prefix?** §3.3 could not answer
   this from Wakil's code. Capture one real request body per pinned provider
   and check whether the provider reports cache hits when only `messages`
   change but `tools` stays byte-identical. If tools are cached, R3's
   dispatcher is required; if not, deferred tools may be added directly.
2. **What is the noise floor?** Run the current bench twice with nothing
   changed (§8.7) and diff. Without this the §8.4 thresholds are guesses.

**Files touched.** None (measurement). Findings appended to this document.

**Why it is stage 0.** Reviewers correctly noted that Stage 1 was ordered first
while §7.11 lists two facts (tools-cache position, minimum cacheable prefix)
that can invert it.

### Stage 1 — Tools first

**What.** Split `BuildTools` output into core tools and module tools. Core
stays in `tools`; module tools are reached through `call_tool(name, arguments)`
plus `load_module(name)` returning the module's schemas — or, if Stage 0 shows
tools are not cached, by adding them to the array directly (simpler, keeps
schema validation).

**Files touched.** `internal/agent/mcp_manager.go` (BuildTools split),
`internal/agent/turn_phases.go:250` (tool assembly), a new
`internal/agent/modules.go` (registry + `call_tool` dispatch + gating),
`internal/agent/tool_handlers.go` (unknown-tool message), `docs/tools.md`
(parity test), `docs/configuration.md` if a knob is added.

**Tests.** `internal/tools/toolsmd_parity_test.go` must keep passing; a new
`internal/agent/modules_test.go` for the registry, the dispatcher's gating
parity (a `call_tool` for a gated inner tool must gate identically), and the
unknown-tool message; a test that the core-only `tools` array is **byte-identical
to a `BuildTools` call with no optional groups and no module tools** — i.e. the
existing default-config behaviour minus the deferred definitions; and a test
that no module tool name appears in the core array.

**Expected saving.** 9 890 → ≈ 3 800-4 100 est. tokens of tool definitions,
**≈ 6 340 est. tokens off every request** (estimate), with no round trip. This
is the largest single, unconditional win. It needs no model cooperation **for
requests that use only core tools** — for any request that uses memory, git,
skills, staging or delegation it needs one `load_module` + one `call_tool`, so
the earlier "no model cooperation" claim was too strong and is retracted.

**Risk.** The output-token cost of `call_tool` (arguments as a string) and the
lost schema validation. §8 must add a task that actually needs a deferred tool.

### Stage 2 — Prompt modules

**What.** Split `prompts/agent.txt` into core + twelve module files. Ship the
index and `load_module`'s prose half. **No pre-loading** — that is Stage 4.

**Files touched.** `prompts/*.txt` (one per module), `prompts/embed.go`,
`internal/agent/app.go` (`buildPreambleForNetwork`), `internal/agent/app_state.go`
(prompt mode), `internal/agent/commands.go` (`/prompt`, new `/modules`),
`docs/configuration.md` if a knob appears.

**Tests.** `internal/agent/split_prompt_test.go` (the existing split-prompt
suite is the template), `internal/agent/app_preamble_cost_test.go` (section
order and presence), a new test that the generated index names match the
embedded module files (no drift), and `prompts/embed_test.go` size bounds.

**Expected saving.** 13 214 → ≈ 4 525 est. tokens of prompt,
**≈ 8 700 est. tokens off every request** (estimate) — only if the Shell /
"verified" / Interaction cuts survive review. Without those cuts, ≈ 7 500.

### Stage 3 — The writing-style file

**What.** §7.5.

**Files touched.** `prompts/style.txt` (new, embedded),
`internal/wiring/bootstrap.go` (`loadWritingStyle`), `internal/agent/app.go`
(append inside `buildPreambleForNetwork`), `internal/initcmd/write.go` (write
the default on request), `docs/configuration.md`.

**Tests.** A precedence test (file > embedded; absent file → default; no
version fallback), a size-cap/truncation test mirroring the AGENTS.md one, and
a test that no update path writes the file.

**Expected saving.** **Zero by itself** — it is a usability change, and it
*adds* ≤1 024 est. tokens when a file is present. Do not sell it as a saving.

### Stage 4 — Pre-loading from the task text and session config

**What.** The classifier's module-name return, plus a startup-time pre-load for
modules the *session configuration* already determines: `long-session` when
`context_mode` says so, `git` when the workspace is a git repo, `search` when
`searxng_url` is set, `browser`/`lsp`/`mashura` when enabled.

**Files touched.** `internal/agent/classifier.go`, `internal/agent/modules.go`,
`internal/wiring/bootstrap.go` (session-time preload inputs).

**Tests.** A table-driven classifier test (module names per task text), and a
boot test asserting `long-session`/`git` are pre-loaded without a model call.

**Expected saving.** Removes the round trip for modules the config already
determines — one avoided `load_module` per session, worth the load cost in
§7.0.2. **Honest caveat (from review):** the `git`/`search`/`browser` config
pre-loads mostly restore today's state (those tools are present today), so
their "saving" is illusory — they are correctness-preserving, not wins. The
real Stage 4 win is only for modules the *task text* predicts and the config
does not.

## 7.10 AGENTS.md and taint — an explicit decision

The question asks for a decision, and the answer is **keep AGENTS.md in the
core prefix, do not defer it.**

Reasons, in order:
1. **Correctness over cost.** Deferring it means a session can run for many
   turns without the project's own rules, and nothing fails visibly. That is
   the worst failure mode in §7.8.
2. **Taint timing.** Loading AGENTS.md sets `touchedExternal` and records a
   taint reason at preamble build (`internal/agent/app.go:1976-1979`). Deferring
   moves that to whenever the model gets round to it — so a session's memory
   writes could be recorded as untainted when the repo content is tainted.
   Getting taint wrong is silent and permanent.
3. Size. It is 6 704 B / 1 676 est. tokens in *this* repo — real, but the
   third-largest section, not the largest. The savings above do not depend on
   it.

If it is ever revisited, the only acceptable shape is: **always load the
header and precedence line, defer only the bodies**, and keep the taint
marking at preamble build regardless.

## 7.11 What would make this design wrong

Listed so the review can attack the load-bearing assumptions:

1. If tools are **not** in the cacheable prefix, R3 is unnecessary and stage 1
   could add tools mid-session — a different, simpler design.
2. If the provider applies **no cache discount**, the prefix reduction
   (stages 1-2) saves ~10x more — and module *loading* becomes no more
   attractive, because a load costs the whole ~11k prefix (§7.0.2).
3. If the provider's cache has a **minimum cacheable prefix length** above
   ≈ 10 830 tokens, the core request may not be cacheable at all, and the whole
   arithmetic changes.
4. If the model cannot reliably call `load_module` when it needs one, stage 1
   is a reliability regression regardless of the token saving.
5. If the Shell/verified/Interaction cuts are rejected, the 10-12k target is
   reachable only through the tool side alone, which is a thinner margin.

---

# Question 8 — Measurement on the bench

## 8.1 What the harness already gives us

`bench/results/*.csv`, one row per run, 16 columns
(`scripts/bench.sh:178`):

```
date,wakil_version,model,task,run,result,exit_reason,seconds,tokens_in,
tokens_out,tokens_cached,cost_usd,tool_calls,collapses,collapses_turn_end,
mashura_rounds
```

`tokens_cached` exists, and `print_table` already computes a **cached-%**
column (`scripts/bench.sh:1021-1023`). So the harness **can** detect a cache
regression — which corrects the earlier draft's claim (§C6 in the plan was
based on a stale result file).

`tokens_in` and `tokens_cached` come from the proxy's usage accounting
(`internal/proxy/cost.go:71`, `internal/wiring/resultjson.go:95-104`).

## 8.2 The task set and what each measures

| Task | Shape | What it isolates |
|---|---|---|
| `add-flag` | 1 turn, small repo, one file edit + test | baseline prefix cost; does the model find `edit_file`/`run_shell` without the index pointing at them? |
| `fix-test` | bug hunt, needs `search_files` + `read_file` + test run | one iteration of read/test iteration; `search` module decision |
| `rename` | many-package refactor, needs `search_files`, `edit_file`, build | **longest** task; the one where module loading can amortise |
| `steps` | six numbered steps, six artifacts | many short tool rounds; stress on per-turn prefix re-send |
| `long-read` | ~60 files of prose, needs `read_file_full` ×60 + `write_file` | **the best module test**: many turns, high prefix re-send, and it needs no module at all (so a load would be pure loss) |

## 8.3 The protocol per stage

For each stage (1 tools, 2 prompt modules, 3 style file, 4 pre-loading):

```
scripts/bench.sh --wakil ./wakil-before --runs 5 --model <pinned> > before.txt
scripts/bench.sh --wakil ./wakil-after  --runs 5 --model <pinned> > after.txt
```

- **5 runs** per task per binary (single-run deltas are noise; the harness
  supports `--runs N`).
- **Same model string** for both, and the provider pinned
  (`provider.order`/`only`), because a different upstream route changes cache
  behaviour and invalidates the comparison. This is a **precondition**, per the
  task's note — pinning the provider and adding harder tasks are separate work.
- Compare with the harness's own table plus a direct CSV diff on:
  `tokens_in`, `tokens_cached`, `cost_usd`, `tool_calls`, `result`.

## 8.4 Columns and thresholds

Corrected after review. Three of the draft's criteria were wrong:

- "cached-% at or above baseline" would **fail a correct implementation**,
  because shrinking the static prefix legitimately lowers the cached share of
  `tokens_in`. Use **absolute uncached input tokens**
  (`tokens_in − tokens_cached`) instead.
- "`cost_usd` ≥ 15 % lower" assumed a large recurring saving; §7.0.2 shows it is
  ≈ 1 439 token-equiv at a 0.1× discount. **5 %** is the honest floor for a
  5-run sample, and even that may sit under the noise floor (§8.7).
- "any task higher on `tokens_in` = regression" conflicts with Stage 1 adding
  the index + loader (≈ 253 est. tokens) and Stage 3 adding style bytes.
  Restate as a budget: `tokens_in` must not rise by more than the stage
  deliberately adds.

| Column | Success | Regression |
|---|---|---|
| `result` pass rate | ≥ baseline | any task that passed before fails on ≥2 of 5 runs |
| `tokens_in` (median) | Stage 1: ≥ 18 % lower on short tasks, ≥ 5 % lower on `long-read`; Stage 2: a further ≥ 20 % on short tasks | any rise beyond the stage's own added bytes (index+loader 253, style ≤ 1 024) |
| **uncached** = `tokens_in − tokens_cached` (median) | lower than baseline | higher |
| `cost_usd` (median) | ≥ 5 % lower | higher |
| `tool_calls` (median) | ≤ baseline + 1 | > baseline + 2 on a task that needs no module (loader misuse) |
| `modules_loaded` (new column, Stage 1) | 0 on tasks needing no module | > 0 on `long-read`/`add-flag` with no module need |
| `tokens_out` (median) | ≤ baseline + 5 % | > +10 % (dispatcher argument re-encoding) |
| `seconds` (median) | ≤ baseline + 10 % | > +25 % (extra round trips) |
| `exit_reason` | no new `error`/`backend_failure` | any new failure reason |
| **reliability probe** (new task, §8.8) | task passes at baseline rate | any drop |

**Why 18 % on `tokens_in` for stage 1:** the tool split removes ≈ 6 340 of
25 223 est. tokens ≈ 25 % of the static prefix, but (a) `tokens_in` also
counts conversation, and (b) the bench runs `--no-memory` (§5.4), which already
removes the 1 359-token memory group — so the observed drop is smaller, ~18 %
max. Stating 23 % (the earlier draft) was wrong for the bench specifically.

**Why "budget, not any rise" on `tokens_in`:** an implementation that adds the
index and loader is *supposed* to add ~253 est. tokens; calling that a
regression would fail the design's own plan. The criterion is that the rise be
no larger than what the stage intentionally adds.

## 8.5 Per-stage expectations (estimates, to be falsified)

| Stage | `tokens_in` short tasks | `tokens_in` `long-read` | pass rate | uncached tokens |
|---|---|---|---|---|
| 0 (measure) | no change | no change | no change | records the noise floor |
| 1 tools | −15 to −18 % | −4 to −8 % | unchanged | lower |
| 2 prompt modules | a further −20 to −30 % | a further −8 to −12 % | unchanged | lower |
| 3 style file | +≤1 024 tok if a file is present; 0 otherwise | same | unchanged | may rise by the style bytes only |
| 4 pre-loading | no change vs stage 2 on 1-turn tasks; **fewer `tool_calls`** on multi-step | fewer `tool_calls` | unchanged or better | unchanged |

Stage 3 is measured only for **non-regression**; it is a usability change and
must not be reported as a saving.

**The earlier draft's −25 to −35 % for stage 2 was a guess stacked on the
uncorrected ledger**; the corrected numbers are lower because §7.0.1 restored
2 119 est. tokens of non-prompt preamble to core and §7.1.1 corrected the tool
double-count.

## 8.6 What the bench cannot measure here

1. **Single-turn tasks cannot show module-load amortisation** — and §7.0.2
   says amortisation does not exist anyway. `add-flag`, `fix-test`, `steps` and
   `long-read` are effectively one prompt with tool loops; only `rename` has
   enough requests for a mid-session load to have any chance. **Harder tasks
   are a precondition for fine comparisons** (task statement).
2. **Provider cache behaviour.** If the pinned provider has a minimum cacheable
   prefix or does not report `cache_creation_input_tokens`, the cached columns
   are meaningless. Must be checked once (Stage 0) and recorded.
3. **Module-load frequency.** No CSV column counts `load_module` calls. Stage 1
   must add `modules_loaded` to `ResultJSON`
   (`internal/wiring/resultjson.go`) — the draft deferred this to Stage 4,
   which was wrong: Stage 1's risk is exactly loader misuse.
4. **Quality.** The bench checks artifacts, not whether the agent skipped a
   rule it should have followed. This is **the design's core risk** and the
   bench does not measure it (§8.8 adds a task that at least approximates it).

## 8.7 The one experiment worth running first

Before any stage: run `scripts/bench.sh --runs 5` on current master twice with
**the same model, provider pinned, and nothing changed**, and diff the two
CSVs. That gives the **noise floor** for `tokens_in` and `cost_usd` on this
machine/provider — without it, the thresholds in §8.4 are guesses. If the
noise floor is above the stage's expected saving (§8.5), the thresholds are too
tight for a 5-run sample and the protocol needs more runs or a paired
comparison.

## 8.8 The missing task: a reliability probe

Reviewers correctly noted that **no bench task needs a module**, so the suite
measures only the zero-module path and false-positive loads — not whether the
model succeeds when it *must* load one, which §7.11 calls design-fatal.

Proposal (new tasks; separate work per the task statement's "harder tasks are
planned separately"):

- **`memory-note`**: a task whose check requires the agent to `memory_put` a
  value and then `memory_get` it in a later turn of the same session. Memory
  tools are in the `memory` module, so the task **cannot pass** unless the
  model loads it (by pre-load, by the unknown-tool error, or from the index).
- **`git-history`**: a task whose check requires `git_log`/`git_show` output,
  with `run_shell` also available — this measures the *substitution* failure
  (the model runs `git log` through a shell instead of loading the module),
  which the design accepts as a non-fatal but costly path.
- **`long-read` unchanged**: it needs no module, so it stays the
  false-positive-load probe.

Without these, Stage 1 cannot be declared safe to ship, whatever the token
numbers say.

---

# 9. The second Mashūra round — findings and disposition

The final round asked "what is wrong, missing or risky in this design?" Three
reviewers ran. Every BLOCKING point was checked against the document (and the
code where the claim was about code) and folded in above. This table records
what changed so the trail is auditable.

## 9.1 BLOCKING findings — taken

| # | Finding | Where fixed |
|---|---|---|
| 1 | Core total omitted 2 119 est. tokens of non-prompt preamble (AGENTS.md + digest + environment); 9 261 was wrong | §7.0.1 rewritten as one ledger; corrected core ≈ 10 830, reduction ≈ 14 393 |
| 2 | Stage 1 had no mechanism; R3 forbade mid-session tools while Stage 1 promised them (circular) | §7.4 R3 now specifies `load_module` + `call_tool`, states the schema-validation trade-off and gate parity, and adds Stage 0 as the measurement that decides the variant |
| 3 | Inventory self-contradictions (async tools both core and in the `async` module; dispatch tools both core and in `subagents`; `delete_file`, "verified", digest) | §7.1 consistency note + rewritten module table + §7.1.1 core tool sum |
| 4 | Break-even inverted; load cost omitted output-token multiplier, cache write, and conversation growth | §7.0.2 rewritten ("requests without needing it"), load cost re-priced, counter-case corrected |
| 5 | Load injection role contradictory (`role:"user"` vs pinned tool result); a user message between tool call and result is malformed | §7.4 R1 now `role:"tool"` + `Pinned`; §7.3 states the authority caveat and the Tier-1/2 carve-out |
| 6 | Pre-load at turn entry only reaches `messages[0]` on turn 1; turn 2+ unspecified | §7.3 now says turn 1 → prefix, turn 2+ → same `load_module` path, with a stricter threshold |
| 7 | R2 incoherent: "still available — do not reload" was false after collapse; "used" undefined for prompt-only modules | §7.4 R2 rewritten: pre-loaded modules never unloaded, tail-loaded ones reported as lost, prompt-only unload is a no-op |
| 8 | Bench criteria cannot pass (cached-% falls by design) or cannot detect the main risk | §8.4 rewritten (absolute uncached tokens; budget not "any rise"; 18 % corrected for `--no-memory`); §8.8 adds reliability tasks |
| 9 | §7.5 style at index 1 repeats a documented hazard and collides with the code-style slot | §7.5 now folds into `messages[0]` with a size cap and no version fallback |
| 10 | Stage 2 and Stage 4 overlapped on the classifier | §7.9: all pre-loading moved to Stage 4 |
| 11 | Detection depends on the model naming a hidden tool — unreliable; prompt-only modules have no signal | §7.7 adds the substitution signal, and states the prompt-only limitation explicitly |

## 9.2 BLOCKING findings — reviewed and **not** taken, with reasons

| Finding | Why not taken |
|---|---|
| "A generic dispatcher can bypass per-tool approval gates" (muse-wonder) | Valid concern, addressed rather than dismissed: §7.9 Stage 1 requires the dispatcher to look up `IsGatedTool` on the inner name so gating is identical. Recorded as a **test requirement**, not a design gap. |
| "Output tokens cost 3-5×, so 120 output tokens ≈ 600" | Accepted as the *direction* (§7.0.2 uses the conservative 1 360 and notes the real figure is worse), but the exact multiplier is **unverified** — it is a provider rate-card fact, marked as such rather than asserted. |
| "`modules_loaded` should be added in Stage 4" (gemini) | Accepted but re-assigned: §8.6.3 now says **Stage 1**, because Stage 1's risk is loader misuse. |
| "Add `memory-note`/`git-history` bench tasks now" | Out of scope: the task statement says harder tasks are planned separately. §8.8 proposes them and states the precondition. |

## 9.3 Non-blocking — listed, not folded in

1. **No usage data was collected** — every "Needed by" entry in §5.2 is a
   judgement. §6.5 gives the user the command; that is stage 0 of a *data*
   iteration, not of this document.
2. **`context_usage.sh` has no session denominator or
   requests-before-first-use figure**, so it cannot directly feed §7.0.2's
   break-even. Adding those is a future change; the script's current contract
   (names + counts only) is deliberate.
3. **§7.5's "383 tokens" is the Go *code*-guide size**, used as a stand-in for
   a writing-style file of unknown size. §7.5 now caps the user file at 4 096 B
   (≈1 024 est. tokens) and the ledger's 383 is only the embedded default.
4. **Precedence "safety > AGENTS.md > style" is asserted as text only** —
   nothing enforces it among same-role messages. True for AGENTS.md today too;
   not a new gap, but worth stating.
5. **Subagent `tools` tier gets `search`** without `load_module`; §7.6 keeps
   that deliberate (children do not load modules) but the delivery is "baked
   into the child's fixed set", which is one more place a tier list must be
   maintained.
6. **MCP/LSP/browser/Mashūra modules are listed in §7.1 but not sized for
   Mashūra** (no key in this environment). An estimate would be a fabrication;
   the row says "not measured".
7. **`--no-memory` in the bench** means the bench's baseline is not the TUI's
   default; §5.4 and §8.4 now say so, but the two are never measured side by
   side here.

---

# Open questions

1. **Is `tools` inside the provider's cacheable prefix?** (§3.3, §7.11.1,
   Stage 0). The single largest unknown: it decides between the dispatcher and
   direct mid-session tool addition.
2. **What is the provider's cache-read discount, cache-write premium, output
   multiplier, TTL, and minimum cacheable prefix?** (§7.0.2). All are
   load-bearing for every number in §7, and all are currently recall.
3. **Do the Shell / "verified" / Interaction prompt cuts survive review?**
   (§5.1, §7.0.1, §7.1.2). Without them the 10-12k target is missed.
4. **Can a model reliably load a module it needs?** (§7.11.4, §8.8). Unknown
   until `memory-note`/`git-history` exist.
5. **Where do real traces live?** (§6.5). This session did not open
   `~/.local/share/wakil`, so the host command uses a path to be confirmed.
6. **Should `git` be core or a module?** §5.2 proposes a module; in practice
   almost every repo has `.git`, so it would be pre-loaded almost always — that
   makes it effectively core and the module a no-op. Not resolved.
7. **Does the 39k-token claim in the task have a source?** Measured here at
   25 223 for preamble+tools; the gap needs a real trace (§1.6).

# Unverified

- **Cache prefix order (tools → system → messages)** and whether tools are
  cached: provider-side behaviour, not derivable from Wakil's code.
- **All pricing rates** in §7.0.2 (0.1× read, 1.25× write, 3-5× output): recall,
  not read from a rate card or from `internal/proxy/cost.go`'s actual formula.
- **`shellcheck -s sh`** has not been run on the two new scripts — not
  installed in this environment. CI runs it; local verification is `sh -n`
  plus execution under `bash` and `dash`.
- **The trace directory path** in §6.5.
- **The 2 119-token non-prompt preamble split** (1 676 AGENTS.md + 443 other)
  is derived by subtracting two measured totals, not by measuring each line
  separately; it is consistent but not independently confirmed.
- **The prompt-cut figures** in §5.1 core (4 525 est. tokens) are a judgement
  about what could be cut, not a measurement of a cut prompt.
- **Mashūra tool definition sizes**: no OpenRouter key in this environment, so
  the four `mashura__*` definitions were not measured.
- **`internal/counsel` caching behaviour** was read for `UseCache` but not
  exercised (no key).
- **The 39k / 15k / 13.7k figures from the task**: 15k supported, 13.7k and
  39k not reproduced; see §1.6.

# Appendix A — the measurement program

`tmp/measure/main.go` (gitignored, deleted before the session ends). It is not
product code; it exists so every tool-size figure above is reproducible.
Outline:

```go
base := agent.BuildTools(config.DefaultConfig(), cwd, nil, "")
b, _ := json.Marshal(base)          // 39 563 B for the default 49-tool set
// per-tool: json.Marshal(one tool)  // the per-tool table in §1.3
```

---

# Follow-up 1 — real usage data, MCP modules, two bugs

Branch `work/context-modules` at `a3ee4ca`. Local only. This section is the plan;
the blocks below it are the implementation.

## F1.0 State check

```
$ git status --short
(no output)
$ git rev-parse HEAD
a3ee4ca7b7bed2c272292fcb47485c0d864776b8
$ git describe --tags
v0.8.9-12-ga3ee4ca7
$ git branch --show-current
work/context-modules
$ git stash list
stash@{0}: WIP on work/0.8.3: 1c1b300 fix: approvals carry over, resume paths agree, grant key works
stash@{1}: On master: phase-F: signing_guard checkGitConfigSub goto->joinedValueFlag (pre-existing, not Phase A)
```

Both stashes left alone.

## F1.1 Verified findings

### F1.1.1 The 64 KiB cut — cause, with path and line

`internal/trace/trace.go:165`:

```go
bw := bufio.NewWriterSize(f, 64*1024)
```

`drainTo` writes each record with `bw.Write(b)` (`:167`) and then a newline
(`:170`). `bufio.Writer` flushes **when its buffer fills**, at whatever byte
offset it happens to be — it does not respect record boundaries. A record that
straddles the 65 536-byte mark has its first part written in one flush and its
remainder in the next, so if the process dies (or the file is read) between the
two, the last line is a partial record. That is exactly the observed 65 536-byte
file with a truncated final line.

Reproduced in this session (`/tmp/bufio2.go`, a throwaway):

```
kill-sim: file size=65536 (buffer 65536) last 40 bytes="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa{\"type\":\"t"
```

The file is exactly 65 536 bytes and the last line is `{"type":"t` — a record
cut mid-way, byte-identically the shape the user reported.

There is **no size cap and no rotation** in the trace writer: `Open` uses
`O_APPEND` (`:83`), nothing checks file size, and `grep -rn "65536|64 \* 1024"`
over `internal/ cmd/ scripts/` finds only `agentsmdMaxTotal`, `gitDiffCapBytes`,
`captureMaxFileBytes` and sandbox MiB limits — none of them a trace cap. So the
64 KiB is an artefact of the **buffer size**, not of a deliberate limit. That
matters: it means the fix is not "rotate" but "never split a record".

Two further, related facts from the same file:

- On a **full channel** a record is dropped and counted
  (`Write`, `:127-131`, `dropped.Add(1)`), never written partially — so the
  drop path is honest, but `Dropped()` (`:136`) is read by **no production
  caller** (`grep -rn "\.Dropped\b"` hits only `trace_test.go`). A long session
  can therefore lose turns silently, which block 1 also has to fix.
- `Close` flushes (`:174`), so a clean shutdown never leaves a partial line; the
  bug needs a kill, a crash, or an OOM. Those are exactly the cases where the
  trace is the only evidence, which is why it matters.

### F1.1.2 Every reader of trace files in the repo

Searched with `grep -rn "TraceDir|\.jsonl|trace.Record|json.Unmarshal"` over
`cmd/ internal/ scripts/`:

| Reader | Path | Handles a broken last line? |
|---|---|---|
| trace package's own tests | `internal/trace/trace_test.go` | n/a (writes cleanly in-process) |
| `scripts/context_usage.sh` | whole file via `jq -r` | **No** — one bad line aborts the whole file (block 2) |
| everything else | — | **there is no other production reader** |

That is a finding in itself: today no Go code reads trace files back. The
"reader must skip an unparsable last line" requirement therefore applies to
(a) `scripts/context_usage.sh` and (b) the trace package itself, which should
gain a reader so future code cannot get this wrong. `internal/timeline` also
writes under `TraceDir` (`cmd/wakil/main.go:176`) but produces one
`json.MarshalIndent` document via a temp file + rename (`timeline.go:398-405`),
so it is atomic and not affected.

### F1.1.3 The shape of `tool_calls` as the writer produces it

`internal/trace/trace.go:43` and `:55-60`:

```go
ToolCalls []ToolTrace `json:"tool_calls,omitempty"`
type ToolTrace struct {
    Name         string `json:"name"`
    PreCapBytes  int    `json:"pre_cap_bytes"`
    PostCapBytes int    `json:"post_cap_bytes"`
    Capped       bool   `json:"capped"`
}
```

So `tool_calls` is a **list of objects**, each with a `name` string. It is
**omitted entirely** when empty (`omitempty`). `scripts/context_usage.sh`
already targets `.tool_calls[].name`, which is correct — its bug is only that a
single unparsable line kills the whole file, not that it reads the wrong shape.

### F1.1.4 `scripts/context_usage.sh` as it stands

`scripts/context_usage.sh:96-102` runs `jq -r "$jq_prog" "$f"` and treats any
non-zero exit as "not a readable JSONL trace" and **exit 2**, printing nothing.
On a file cut mid-line, `jq` exits non-zero at the bad line — so the whole file
is lost, which is exactly what the user saw: one bad file in 2810 and the tool
refused to report on any of them.

`jq` has no built-in "skip bad lines" for a whole-file read, so the fix must
read line by line (or use `jq --seq`/`-R` with a parse-per-line), count the
skipped lines, and report them. This also satisfies the DO NOT ALTER rule that
the script "never prints ... any field other than tool name, counts and the
number of skipped lines and files".

### F1.1.5 Design sections 5 and 7 — what the usage data changes

The design in §5/§7 assumed "needed by" per module was a judgement. The usage
data replaces that judgement with a measured **session share**: the fraction of
sessions in which a tool of that module was used at all. Four assumptions do
not survive:

1. **§7.1's MCP row was a placeholder: one conditional row, no per-server split
   and no size.** The user's sessions carry `trello__*`
   (9 distinct tools, 153 sessions on `trello__get_lists` alone) and
   `context7__*`. The "65 tools" of the earlier draft is now explained: 49
   default tools plus the MCP servers (§1.3 measured 49 without MCP). The count
   itself is **inferred, not measured** — traces record tools that were
   *called*, not tools that were *sent* (see §F1.3.5), so MCP is a large real
   prefix whose size the design never measured.
2. **§5.2 put the git tools in a module.** `git_status` is used in 107 sessions
   but `run_shell` in 498 — and git commands are routinely run through
   `run_shell`, so the git group's true necessity is inflated by substitution.
3. **§7.1 kept the memory tools deferred.** `memory_put` appears in 173
   sessions, `memory_get` 135, `memory_promote` 125, `memory_search` 115 — the
   memory group is used in roughly a third of sessions, which changes its
   arithmetic.
4. **§5.1 deferred the Subagents section.** `dispatch_subagent(s)` appear in
   239 + 212 sessions — over 45 % of tool-using sessions.

## F1.2 Touched files and functions (by name)

| Block | Files | Functions / regions |
|---|---|---|
| 1 | `internal/trace/trace.go` | `drainTo` (line integrity), `Write`/`Dropped` (surface drops), `Open` (header unchanged), new `Read`/`ReadFile` reader that skips a broken tail |
| 1 | `internal/trace/trace_test.go` | new: over-64 KiB session parses clean; kill-sim produces no partial line; drop counter surfaces |
| 2 | `scripts/context_usage.sh` | `main` loop: per-line parse, skip + count; final skipped-lines/files summary |
| 2 | `scripts/context_usage_test.sh` | new cases: cut mid-line; valid record after a broken one; marker in every non-name field |
| 3 | `docs/plans/context-modules.md` | new F1.3 saving table; §5.1/§5.2/§7.1 re-decisions; MCP module section; restated totals; stage order |
| 4 | `CHANGELOG.md` | two `[Unreleased]` lines |

No change to `prompts/`, no change to which tools a request sends, no Go change
outside `internal/trace`.

## F1.3 The blocks

### Block 1 — trace files are never written partially

- `drainTo` must write **whole records**: buffer the record bytes and issue one
  `Write` of `record + "\n"` so `bufio` can still split *across* records but a
  flush can never land inside one. Concretely: write the newline into the same
  buffer as the payload (append `"\n"` to `b` before `bw.Write`), and — because
  `bufio` flushes at a byte offset regardless — use a writer that flushes only
  between records. The simplest correct form is to stop using a fixed-size
  `bufio.Writer` for record payloads: accumulate into a `bytes.Buffer` and
  `f.Write(append(b, '\n'))` per record (one syscall per record, which is fine
  at trace volumes), or use `bufio.Writer.Write` for the payload and
  `Flush()` after each record.
- Surface silent loss: `Close` (or a new `CloseReporting`) logs/carries
  `Dropped()`; the session teardown reports a non-zero drop count once
  (a diagnostic line, not a rewrite of old files).
- If a size cap is ever wanted it must rotate or stop **at a line boundary**
  and write a marker record; this fix adds **no cap**, because none exists
  today and adding one is out of scope.
- Reader: add `Read(path)` / `ReadFile` that scans lines, skips a final
  unparsable line, and reports how many it skipped — so the "skip the broken
  tail" rule lives in one place.
- **Test, failing before the fix:** write >64 KiB of records through the store,
  close, assert every line parses and the count matches; and a kill-sim
  assertion that no flushed line is partial. Show it red first.

### Block 2 — `scripts/context_usage.sh` skips broken lines

- Read per line, `jq -c` each; a line that fails to parse is **counted and
  skipped**, never fatal.
- Unchanged output contract: `TOOL<TAB>CALLS<TAB>SESSIONS`, plus a final
  summary of skipped lines and files-with-skips.
- Handle exactly the writer's shape (`tool_calls` = list of objects with
  `name`, omitted when empty).
- Extend the test with: a file cut mid-line; a valid record after a broken one;
  the secret marker in every field that is not the tool name.
- Document the host command in the script header and in this document.

### Block 3 — design updated with real usage data

- Saving rule, per module:
  `saving = size × (1 − share) − load_cost × share`, where `share` is the
  session share that needs it, and `load_cost` is the size plus one round trip
  for a mid-session load, or the size alone when pre-loaded on turn 1.
- Re-decide with numbers: Subagents / Durable Memory / Async prompt sections;
  the memory tool group; the mashura tools; the git tools; the skill-authoring
  tools including `capture_skill_template`.
- MCP servers as modules — one module per configured server, one index line,
  loaded on demand or pre-loaded from keywords ("board", "card", "trello").
  Size measured from repo fixtures if any exist, else "unmeasured" + the host
  command.
- Restate total saving for this user's typical session and for a
  default-config session, method shown.
- Re-order the stages if the numbers change it.

### Block 4 — CHANGELOG `[Unreleased]`

One line for the trace fix, one for the usage script.

## F1.4 Open risks

- One syscall per record may reduce throughput on very chatty sessions; trace
  volume is small (`~67 lines / 64 KiB`), so this is accepted, and measured if
  it ever matters.
- Dropping `bufio` changes flush behaviour; a test must prove that a clean
  `Close` still writes every queued record.
- The usage data is **one user**, and that user always loads the
  `task-workflow` skill, which inflates `load_skill` (144 sessions) and biases
  the skills module's share upward. Every derived share inherits that bias; the
  document must say so next to the table.
- `run_shell` in 35 % of sessions could equally mean "the git module is
  unnecessary" or "the model substitutes shell for git tools because it cannot
  see them" — the data cannot distinguish the two, so the git decision must be
  stated as conditional.
- MCP sizes are unknown; a Trello server can be 9 tools and ~10 KB, but that is
  an estimate until measured.
- The saving formula treats `share` as independent of loading; in practice
  loading is sticky, so the formula is an upper bound on the module's benefit
  and a lower bound on its cost. Stated, not hidden.

---

# F1.3 — Design re-decided with real usage data

Data source: the user's host (`rubin`), 2 810 trace files, collected by the
user with `jq` after `scripts/context_usage.sh` aborted on one cut file (the
bug fixed in this follow-up). One user's usage; that user always loads the
`task-workflow` skill, which inflates `load_skill` (144 sessions). Every share
below inherits that bias.

Denominator: **N = 498** tool-using sessions (`run_shell` is the most-used tool
at 498 sessions, so at least 498 sessions called some tool). `N` is a *lower*
bound on tool-using sessions if `run_shell` is not in some tool-using session;
the estimate is therefore if anything slightly pessimistic.

## F1.3.1 The saving rule, stated correctly

An earlier draft used `saving = size × (1 − share) − load_cost × share` with
`load_cost = size` for a pre-load, which becomes `size × (1 − 2·share)` and
turns negative above 50 % — that is **wrong**: the baseline already pays `size`
on every session, so a session that pre-loads pays exactly what it paid before
(net zero), not double. Corrected, against the always-loaded baseline:

- **Pre-loaded on turn 1** (the module lands in the stable prefix):
  `saving = size × (1 − share) − index_line`
- **Loaded mid-session** (an extra round trip, the module re-sent uncached once):
  `saving = size × (1 − share) − round_trip × share − index_line`

`index_line = 12` est. tokens (one `load_module` index line, measured in §7.2
at ≈ 40 B each). `round_trip = 1 000` est. tokens (one extra request that
re-sends the prefix at cache-read price plus one tool-call and one tool-result
message — an estimate, flagged in the document). `share` is the **union** of
sessions needing the module, reported as a range `[max single tool, Σ tools]`
because per-tool session counts cannot be summed and the true union is
unmeasured (§F1.3.2).

## F1.3.2 Why shares are a range, not a number

`scripts/context_usage.sh` prints `SESSIONS` per tool. A module's share is the
number of sessions that used **any** of its tools — a union. Summing per-tool
counts double-counts a session that used two tools of the group, so the union
lies between the largest single tool (`lo`) and the sum clipped at `N` (`hi`).
The script now reports the union directly with `--union` (added this session),
but the user has not yet run it on the host, so the table below uses the
`[lo, hi]` bounds. The `--union` command for exact figures is in §F1.3.7.

## F1.3.3 The saving table (this user's sessions, N = 498)

`mid@lo` = saving if the true union is the low bound; `mid@hi` = if it is the
high bound. Negative numbers mean the module **costs** tokens for this user —
it is loaded in so many sessions that the round trip outweighs what it hides.

| Module | Tokens | share lo | share hi | mid@lo | mid@hi | pre@hi |
|---|---|---|---|---|---|---|
| `memory` (section + 8 tool defs) | 3 459 | 0.35 | 1.00 | 1 898 | **−1 012** | −12 |
| `subagents` (section only) | 1 768 | 0.48 | 0.91 | 428 | **−751** | 155 |
| `async` (section only) | 1 111 | 0.30 | 0.95 | 459 | **−902** | 46 |
| `skills` authoring tools | 2 346 | 0.07 | 0.07 | 2 092 | 2 092 | 2 164 |
| `skills` section | 404 | 0.29 | 0.29 | −14 | −14 | 275 |
| `search` | 1 529 | 0.13 | 0.24 | 1 192 | 908 | 1 149 |
| `long-session` | 1 133 | 0.04 | 0.04 | 1 044 | 1 044 | 1 080 |
| `git` | 562 | 0.21 | 0.53 | 214 | −272 | 254 |
| `staging` | 576 | 0.03 | 0.03 | 523 | 523 | 549 |
| `edit-extras` | 406 | 0.00 | 0.00 | 394 | 394 | 394 |
| `implement` | 338 | 0.00 | 0.00 | 326 | 326 | 326 |
| `write` | 146 | 0.00 | 0.00 | 134 | 134 | 134 |
| `misc` | 266 | 0.05 | 0.05 | 196 | 196 | 242 |
| `browser` (conditional) | 1 171 | 0.07 | 0.21 | 1 006 | 701 | 912 |
| `lsp` (conditional) | 643 | 0.09 | 0.19 | 483 | 321 | 510 |
| **Sum, mid-session load** | | | | **10 374** | **3 688** | |

## F1.3.4 The re-decisions, with the numbers

**Subagents section (≈1 768 tok, share 48–91 %) — stays CORE.**
`dispatch_subagent` (239) + `dispatch_subagents` (212) are already core tools,
and the section is prompt-only: there is no unknown-tool error to signal that a
model ignored the rules (§7.7). At the high bound the module *costs* 751 est.
tokens; even at the low bound it saves 428, less than one misload, and the
failure mode is a subagent dispatched with no tier or budget rules. Not worth
it. **Core.**

**Durable Memory section + tools (≈3 459 tok, share 35–100 %) — stays CORE.**
Same shape: `memory_search` is core by design (the discovery handle), used in
115 sessions, and the section is prompt-only. At the high bound it costs 1 012
est. tokens. The memory *rules* are exactly what makes the model store anything
at all; a silent failure is expensive and the prompt has no detection signal.
The 35 % low bound is the most the module could win (1 898) and the honest
range is far worse. **Core.** (The memory *digest line* already stays core,
§4.4.)

**Async execution section (≈1 111 tok, share 30–95 %) — stays CORE.**
Identical shape (all five async tools are core). High bound −902 est. tokens;
low bound +459, below the cost of one round trip. `wait_for_completion` (151),
`read_process_log` (133), `check_pending` (76) are common. **Core.**

**Memory tool group (the 7 non-`memory_search` defs, ≈1 359 tok) — splits off
from the section decision.** The tools are *not* prompt-only: a deferred
`memory_put` is discoverable through `memory_search`'s description, and the
unknown-tool error gives a real detection signal. `memory_put` (173),
`memory_promote` (125) are the frequent ones; the rarer defs
(`memory_forget` 25, `memory_list` 67) are what defers well. Proposal:
**keep the section core, defer the 4 rare memory defs** (`memory_forget`,
`memory_list`, `memory_promote_from_staging`, `memory_promote`) as a
`memory-admin` module (≈1 000 B ≈ 250 tok), which never clears its own cost and
is therefore **not worth a module either — leave the group core**. Net: memory
stays entirely core. The measurement says the earlier "defer memory" proposal
does not pay for this user.

**Mashūra tools (4, size unmeasured) — stays config-gated, on-demand.**
299 sessions used `mashura__review`; that is a *large* share (≈60 % of N). The
tools are already absent unless configured, so the saving today is zero for a
user without a key and only pays for a configured user who rarely calls them —
which this user is not. **Keep as today (config-gated); do not defer further.**
Size unmeasured (no key); the host command is in §F1.3.7.

**Git tools (5, 562 tok, share 21–53 %) — REMOVE from the module plan; keep
core.** `git_status` (107), `git_log` (72), `git_diff` (72) are used, but
`run_shell` (498) covers every one of them, and Stage 4 would pre-load a `git`
module in every git repo anyway — which nulls the saving for the users most
likely to need it. A module that is pre-loaded for its main audience saves
nothing and adds an index line. **Keep core, drop the `git` module.**

**Skill-authoring tools incl. `capture_skill_template` (2 346 tok, share 7 %) —
MODULE, the largest real win.** Only `skill_search` (36) was used; the
authoring tools (`save_skill`, `update_skill`,
`capture_skill_template`, `apply_skill_template`, `check_skill_template`,
`skill_history`) were not used at all in these traces. 2 092–2 164 est. tokens
saved with a real detection signal (the tool is missing → `load_module` →
`call_tool`). **On-demand module `skills-authoring`.** This is the single
biggest saving that survives the data.

## F1.3.5 MCP servers as modules

The user's sessions carry `trello__*` (9 tools) and `context7__*` on top of the
49 default tools — that is the "65 tools" of the earlier draft, now explained.

1. **Each configured MCP server is one module**, named for the server
   (`mcp:trello`, `mcp:context7`), holding exactly that server's tool
   definitions. `MCPToolToOpenAI` (`internal/agent/mcp_manager.go:777`) already
   builds them per server with the `server__tool` namespace, so the split is a
   one-line filter, not new plumbing.
2. **Index line per server**, generated at startup from the connected server
   list, so the model sees `mcp:trello — 9 board/card tools (~3k tok)` without
   the 9 schemas.
3. **Pre-load by keyword** from the task text (turn 1) when the task clearly
   names the domain: "trello", "board", "card", "list" → `mcp:trello`;
   "docs for <library>", "library", "API reference" → `mcp:context7`. Keyword
   lists ship as defaults the user can edit; a server with no keyword match
   stays unloaded and is reachable by `load_module`.
4. **Otherwise load on demand**: an unknown `trello__*`-shaped call is the
   detection signal (a call to a tool the model knows exists but has not
   loaded). Fallback lists the servers.
5. **Size**: Trello and context7 schemas are not in the repo (no fixtures), so
   both are **unmeasured** here. Estimate: 9 Trello tools at the median
   definition size of ≈800 B each ⇒ ≈7 200 B ≈ **1 800 est. tokens**, marked
   *estimate*. For this user, if `trello` is pre-loaded only in the 153 sessions
   that touched it (share ≈0.31) and on-demand otherwise, the saving is
   ≈ 1 800 × (1 − 0.31) − 12 ≈ **1 230 est. tokens** per request — but see the
   caveat: this rests on the unmeasured size.

## F1.3.6 Restated totals

**This user's typical session** (MCP present, configured with searxng/browser/
lsp/mashura), mid-session load, exact union unknown:

- Optimistic (every union at its low bound): **≈ 10 400 est. tokens/request**.
- Pessimistic (every union at its high bound): **≈ 3 700 est. tokens/request**.
- Realistic (memory/subagents/async stay core per §F1.3.4, which removes the
  negative rows): defer `skills-authoring` 2 092 + `search` ≈1 000 +
  `long-session` 1 044 + `staging` 523 + `edit-extras` 394 + `implement` 326 +
  `write` 134 + `misc` 196 + `browser` ≈800 + `lsp` ≈400 + MCP ≈1 230 (est.,
  unmeasured) ⇒ **≈ 8 100 est. tokens/request**, method above. This is the
  number to plan against.

**A default-config session** (no MCP, no searxng/lsp/browser/mashura), same
rule with those rows dropped and every union at its low bound:
`skills-authoring` 2 092 + `long-session` 1 044 + `staging` 523 +
`edit-extras` 394 + `implement` 326 + `write` 134 + `misc` 196 ≈
**≈ 4 700 est. tokens/request**. The saving is larger for the power user than
for a default install, because MCP and the optional groups are where the
movable bytes are — but the MCP part is unmeasured.

Contrast with the pre-data figure of ≈8 689 est. tokens (§7.1): the data keeps
the *order of magnitude* and cuts it to ≈4 700 for a default install, with the
power user's larger figure resting on the unmeasured MCP sizes.

## F1.3.7 Stage order after the numbers

Unchanged in substance, but the tool stage shrinks and two prompt modules drop:

1. **Stage 0** — measure the two facts (§7.9): tools-cache position and bench
   noise. Still first; unchanged.
2. **Stage 1 (tools)** — core array + `load_module` + `call_tool`. The core tool
   list (§7.1.1) loses `git` (stays core anyway, so no change) and the deferred
   set is now `skills-authoring` (2 346) + the existing `edit-extras`,
   `staging`, `misc` (≈1 248) + conditional groups ⇒ ≈ **3 590 est. tokens** off
   every request for a default install. This is now the **first and largest**
   stage, ahead of the prompt split — the data made the tool side more valuable
   than the prompt side (the prompt's big sections must stay core).
3. **Stage 2 (prompt)** — module files, but **only** `search`, `long-session`,
   `implement`, `write` (≈2 553 est. tokens). `memory`, `subagents`, `async`
   stay in the core prompt per §F1.3.4. Smaller than the pre-data plan (which
   had ≈8 689) and should come **after** Stage 1, not before.
4. **Stage 3 (style)** — user-owned `writing-style.md`, unchanged, no saving.
5. **Stage 4 (pre-loading)** — now includes MCP keyword pre-loading. Unchanged
   otherwise; still last, and still gated on Stage 0's cache finding.

## F1.3.8 Host commands to close the estimate gaps

```sh
# exact module unions (integers only, nothing else printed)
sh scripts/context_usage.sh "$HOME"/.local/share/wakil/traces/*.jsonl \
  --union "subagents=dispatch_subagent,dispatch_subagents" \
  --union "memory=memory_put,memory_get,memory_search,memory_list,memory_promote,memory_promote_from_staging,memory_forget,memory_put" \
  --union "async=check_pending,wait_for_completion,read_process_log,kill_process,run_background" \
  --union "trello=trello__get_boards,trello__get_board,trello__get_lists,trello__get_cards,trello__get_card,trello__create_card,trello__move_card,trello__update_card,trello__add_comment" \
  --union "context7=context7__resolve-library-id,context7__query-docs"

# MCP tool-definition sizes: count the bytes of the serialised tool array with
# MCP connected, minus the same run with no MCP server configured
# (wakil with --trace prints the request; or read the size of the tool JSON in a
# trace once a `tools_sent` field exists — not present today).
```
