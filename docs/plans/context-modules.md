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
