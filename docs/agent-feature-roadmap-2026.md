# Wakil Feature Roadmap Proposal — Mid-2026 CLI Agent Landscape

## Overview

This document surveys 35+ CLI coding agents as of mid-2026, catalogs 38 distinct
features, assesses Wakil's current status on each, and proposes a ranked top 10
features to consider adding. The proposal was discussed with Mashūra (two external
AI panelists: GPT-6-Astra and Claude Fable-5.1) whose feedback shaped the ranking.

---

## Research Methodology

Web research across comparison articles, official docs, and community sources dated
June–September 2026. Primary sources:

| Source | URL | Focus |
|--------|-----|-------|
| State of CLI Coding Agents, Mid-2026 | blog.arcbjorn.com/state-of-cli-coding-agents-2026 | Comprehensive landscape survey |
| Best Terminal AI Coding Agents 2026 | amux.io/blog/best-terminal-ai-coding-agents-2026 | Top 8 comparison |
| AI Agent Harness Comparison 2026 | winder.ai/ai-agent-harness-comparison | 7 harnesses compared |
| Top CLI-based AI Coding Agents | pinggy.io/blog/top_cli_based_ai_coding_agents | Top 5 with model choice |
| Claude Code Alternatives | datacamp.com/blog/claude-code-alternatives | 8 tools compared |
| awesome-cli-coding-agents | github.com/bradagi/awesome-cli-coding-agents | Curated directory |
| Claude Code Guide 2026 | marktechpost.com (June 2026) | 25 features list |

**Caveat**: Several specific figures (star counts, benchmark scores, product
capabilities) come from secondary sources and could not all be verified against
primary documentation. Treat competitor-specific claims as unconfirmed. The
feature inventory itself — what capabilities exist in the ecosystem — is
well-supported across multiple sources.

---

## Agents Surveyed

### Lab/Vendor Agents (16)
| Agent | Vendor | Key Differentiator |
|-------|--------|-------------------|
| Claude Code | Anthropic | Category template; deepest orchestration; agent teams |
| Codex CLI | OpenAI | Rust core; OS-level sandboxing; plugin marketplace |
| Gemini CLI / Antigravity CLI | Google | Free preview; async multi-agent; shares harness with desktop |
| Grok Build | xAI | Up to 8 parallel subagents in isolated git worktrees |
| GitHub Copilot CLI | GitHub | Auto-delegating specialist agents; cloud agent via `&` |
| Cursor CLI | Anysphere | Same config across IDE, terminal, CI |
| Kimi Code CLI | Moonshot | Built-in coder/explore/plan subagents; ACP |
| Mistral Vibe | Mistral AI | Remote async agents; ACP; European option |
| Qwen Code | Alibaba | Gemini CLI fork for open-weight coders |
| Amp | Amp Inc. | Opinionated; ad-funded free tier |
| Auggie | Augment Code | Whole-repo context indexing before first prompt |
| Droid | Factory AI | Specialist agents; Slack/ticketing hooks |
| Junie | JetBrains | ACP-native; agentic debugging; IDE + DB integration |
| Qoder | Alibaba | Quest mode for spec-driven autonomous tasks |
| CodeBuddy | Tencent | Skills, plan mode, ACP, sandboxed execution |
| Devin CLI | Cognition | Plugins contribute rules, hooks, MCP servers, subagents |

### Open-Source Harnesses (12)
| Agent | Key Differentiator |
|-------|-------------------|
| OpenCode (182k★) | 75+ providers; TUI + desktop + IDE; agents and skills |
| Crush (26k★) | Best-crafted TUI; LSP context; MCP |
| Goose (51k★) | MCP-native; foundation-governed (AAIF) |
| Aider (47k★) | Git-native pair programming; repo map; atomic commits |
| Cline CLI (64k★) | Open SDK; parallel agents; headless CI |
| Kilo CLI (26k★) | 500+ models; Memory Bank; Architect/Code/Debug modes |
| DeepSeek-Reasonix (26k★) | Cache-first; extreme budget efficiency; checkpoints/rewind |
| OpenHands CLI (79k★) | Agent SDK with event-sourced replay |
| Pi (67k★) | Minimal: 4 tools, sub-1K-token system prompt |
| Codebuff (7k★) | Explicit agent roles: finder, planner, editor, reviewer |
| ForgeCode (7k★) | Rust, shell-native, semantic codebase search |
| Nanocoder | Community-owned, no telemetry, local-first |

---

## Comprehensive Feature Inventory (38 Features)

Status legend: ✅ Present · ⚠️ Partial · ❌ Absent · ❓ Unknown (needs code audit)

### Core Agent Capabilities

| # | Feature | Ecosystem Status | Wakil Status | Notes |
|---|---------|-----------------|--------------|-------|
| 1 | Agentic loop (plan→act→observe) | Universal | ✅ | Core loop in operating instructions |
| 2 | Subagents / multi-agent orchestration | Mainstream | ✅ | Discovery/edit/tools tiers; parallel dispatch |
| 3 | Plan mode | Near-universal | ✅ | plan/current in memory; step lists |
| 4 | MCP (Model Context Protocol) | Near-universal | ✅ | Trello, Telegram, invoicely, context7, browser |
| 5 | Skills / custom instructions | Widespread | ✅ | Global skill store with history |
| 6 | Headless / CI mode | Near-universal | ✅ | --auto flag |
| 7 | Session resume / handoff | Common | ✅ | plan/current, durable memory, handoff summaries |
| 8 | Memory / project context | Universal | ✅ | Durable + mid-tier + staging; anchors; provenance |
| 9 | LSP integration | Growing | ✅ | lsp_definition, lsp_references, lsp_hover, lsp_symbols |
| 10 | Thinking / extended reasoning | Growing | ✅ | Recently added; inherited by subagents |
| 11 | Browser tool | Growing | ✅ | Full headless browser suite |
| 12 | Notifications / multi-channel output | Growing | ✅ | Telegram bridge, Trello |
| 13 | User-configurable permission system | Common | ✅ | Precedence ladder; per-action + blanket approval |
| 14 | Automated testing / verification loops | Common | ✅ | "Strongest cheap check" methodology |
| 15 | Diff preview / change review | Common | ✅ | git_diff, edit_file old/new strings |

### Partial — Already Started But Incomplete

| # | Feature | Ecosystem Status | Wakil Status | Notes |
|---|---------|-----------------|--------------|-------|
| 16 | Hooks / lifecycle events | Widespread | ⚠️ | Permission gates exist but no user-configurable hooks |
| 17 | OS-level sandboxing | Growing | ⚠️ | Docker available but no automatic action isolation |
| 18 | Code review agent | Growing | ⚠️ | Mashūra review exists but no built-in review workflow |
| 19 | Multi-model / BYOK | Common | ⚠️ | Per-dispatch model override in subagents; main-model swap unclear |
| 20 | Semantic code search | Growing | ⚠️ | LSP gives semantic navigation; no whole-repo semantic search |
| 21 | Context compaction / window management | Important | ⚠️ | Spill files for large outputs; active compaction unclear |
| 22 | Self-improvement / learning from corrections | Emerging | ⚠️ | Memory can store corrections; no explicit learning loop |
| 23 | Continuous autonomous mode | Growing | ⚠️ | Agentic loop runs per-turn; no multi-turn autonomy |
| 24 | Spec-driven development | Emerging | ⚠️ | Plan mode + skills approximate; no formal spec workflow |

### Absent — Gaps in Wakil

| # | Feature | Ecosystem Status | Wakil Status | Notes |
|---|---------|-----------------|--------------|-------|
| 25 | Checkpoint / undo / rewind | Growing | ❌ | No snapshot/rollback mechanism |
| 26 | Session replay / deterministic replay | Rare | ❌ | No replay system |
| 27 | Parallel worktree execution | Emerging | ❌ | Subagents share one workspace |
| 28 | Plugin / extension marketplace | Emerging | ❌ | Skills are reference docs, not executable plugins |
| 29 | Remote / cloud execution | Growing | ❌ | Local/sandbox only |
| 30 | AGENTS.md / cross-agent config standard | Emerging standard | ❌ | Uses own instruction format |
| 31 | Cost tracking / token budgets | Growing | ❌ | Cost-conscious instructions but no programmatic tracking |
| 32 | Repo indexing / context engine | Growing | ❌ | On-demand file reading only |
| 33 | ACP (Agent Client/Communication Protocol) | Emerging | ❌ | Not supported |
| 34 | Voice / multimodal input | Rare in CLI | ❌ | Not applicable to CLI-first |
| 35 | Cache-first / budget-efficient design | Niche | ❌ | No cache-optimized prompt engineering |
| 36 | Cross-agent config portability | Emerging | ❌ | Own format, not portable |
| 37 | Scheduled / cron-like automation | Emerging | ❌ | No built-in scheduler |
| 38 | Multi-agent messaging / agent teams | Experimental | ❌ | One-way dispatch only |

### Deliberate Design Stances (Not Gaps)

| # | Feature | Notes |
|---|---------|-------|
| — | Autonomous PR / commit / push | Intentionally requires explicit user request |
| — | IDE integration | Wakil is terminal-first by design |

---

## Top 10 Proposed Features for Wakil

This ranking synthesizes the Mashūra panelists' recommendations with corrections.
Ranking criteria: user impact (frequency × severity), implementation effort,
dependency on existing Wakil infrastructure, positioning fit, and risk.

**Effort estimates** are in developer-days, assuming one Go developer familiar with
the codebase. All estimates include tests and documentation. These are preliminary
— a code audit of the relevant packages would refine them.

---

### 1. Checkpoint / Rewind (Filesystem Snapshots)

**User problem**: An agent edit goes wrong 3 turns in. The user must manually
reconstruct state — revert files, figure out what was changed, redo good work
that got mixed with bad. Diff preview helps *before* applying; nothing covers
*after*.

**MVP scope**: Shadow-snapshot workspace files before each mutating tool call
(shadow git repo or content-addressed copy). `/rewind N` restores files to the
Nth checkpoint and truncates conversation history to that point. Memory tiers
get a matching marker. Non-reversible side effects (shell commands, API calls)
are warned about but not undone.

**Rationale**: Directly extends Wakil's existing safety story (permissions, diff
preview, verification). The "undo" capability is the #1 user-requested feature
across the ecosystem. Claude Code, OpenHands, and DeepSeek-Reasonix all have
variants. Low architectural risk — operates at the filesystem layer, not the
agent loop.

**Effort**: 5–10 days

**Dependencies/risks**: Interaction with durable/mid-tier memory (should memory
rewind too? — decide explicitly). Large binary files need exclusion strategy.
Non-git directories need a copy-based fallback. Must not conflict with concurrent
user edits.

**Acceptance criteria**:
- Edit → rewind restores byte-identical files
- Rewind of a turn that ran a shell command warns that side effects aren't undone
- Works in a non-git directory (copy-based fallback)
- Dirty, new, deleted, and binary files handled explicitly
- Concurrent user edits are protected (not silently overwritten)

---

### 2. Context Compaction / Stale-Output Pruning

**User problem**: Long sessions degrade or hard-fail on context limits. The agent
loses track of early constraints, forgets the plan, or simply can't continue.

**MVP scope**: (a) Prune old tool outputs to a one-line stub once N turns old;
(b) threshold-triggered summarization of the transcript into a handoff-style
summary, reusing existing handoff machinery; (c) `/compact` manual trigger.
Summaries must preserve: plan/current, open verification state, permission
constraints, and provenance of untrusted content.

**Rationale**: Prerequisite for continuous autonomous mode and multi-hour tasks.
Wakil already has handoff summary code that could be reused. Every serious agent
with long sessions has solved this. DeepSeek-Reasonix's "stale tool output pruning
before compaction" is the state of the art approach.

**Effort**: 3–8 days (less if handoff summary code is reusable)

**Dependencies/risks**: Summaries must not discard user constraints or elevate
untrusted content into instructions. Pruning must not break cache prefix (see #7).
Must preserve plan/current and pending verification state. **Verify first — may
partially exist in Wakil's codebase.**

**Acceptance criteria**:
- Synthetic 200-turn session stays under the model's context limit
- A task that references a pruned file still succeeds via re-read
- Summary includes current plan step and open verification state
- Long-session tests preserve user constraints, permissions, and provenance

---

### 3. Token/Cost Tracking + Budgets

**User problem**: Users can't see what a task cost or bound a runaway loop. A
subagent-heavy task can spend $20 before anyone notices.

**MVP scope**: Parse provider usage fields into a per-session ledger (input,
output, cache tokens, $ via a pricing table). `--budget` flag halts with a
handoff when exceeded. Per-subagent cost attribution rolls up to the parent.
End-of-session summary prints total tokens and cost.

**Rationale**: Codex has "Goals with token budgets" as a flagship feature.
DeepSeek-Reasonix made cost an engineering first principle. Wakil already has
cost-consciousness in its instructions — making it programmatic is a natural
extension. The session ledger also enables #7 (cache-hit measurement).

**Effort**: 2–5 days

**Dependencies/risks**: Pricing tables go stale (need a refresh mechanism).
Usage field shapes differ per provider (audit which providers Wakil's adapters
support). Parallel requests, retries, and incomplete provider usage reporting
complicate hard caps. Unknown prices should be labeled, and possible overshoot
bounded and disclosed.

**Acceptance criteria**:
- End-of-session summary prints tokens and cost
- Budget breach stops before the next model call and writes handoff
- Subagent costs roll up to parent
- Concurrent reservation and stopping behavior tested under -race

---

### 4. AGENTS.md Ingestion

**User problem**: Teams maintaining AGENTS.md for Claude Code, Codex, and
Copilot CLI get no benefit in Wakil. The emerging standard (Linux Foundation
AAIF, backed by Anthropic, OpenAI, Google, Microsoft, AWS) is becoming the
universal project-instructions file.

**MVP scope**: Read `AGENTS.md` (root + nearest ancestor of touched files) into
the system context alongside Wakil's own instruction format. Documented
precedence rule (Wakil's own instructions take precedence; AGENTS.md is
advisory). Content is tainted per Wakil's existing taint tracking (untrusted
repo content).

**Rationale**: Cheapest win on the list. Zero architectural change — just file
reading + prompt assembly. The standard is backed by every major vendor. Not
supporting it means teams with mixed tooling can't use Wakil without maintaining
duplicate instructions.

**Effort**: 0.5–2 days

**Dependencies/risks**: Prompt-injection surface — AGENTS.md is untrusted repo
content and must be tainted. Nested file precedence needs documented scope.
Token budget — large AGENTS.md files add to system prompt (interacts with #2).

**Acceptance criteria**:
- Repo with AGENTS.md instruction "use pnpm" → agent uses pnpm
- Nested AGENTS.md overrides root for its subtree
- AGENTS.md content is marked tainted in provenance
- Wakil's own instructions take precedence over conflicting AGENTS.md directives

---

### 5. Lifecycle Hooks (User-Configurable)

**User problem**: Users want `gofmt`/lint after every edit, a Telegram ping on
completion, or a pre-commit hook before any file mutation — without editing agent
code or asking the agent to remember.

**MVP scope**: Config-defined shell commands triggered on lifecycle events:
`pre_tool`, `post_tool`, `session_start`, `session_end`, `on_stop`. Pre-hook exit
code can block a tool; post-hook output can inject a message into context. Hooks
sit *under* the permission ladder — they cannot bypass permission denials.

**Rationale**: Claude Code, Codex, and Copilot CLI all have hooks. Wakil already
has the permission infrastructure; hooks are the user-facing extension point on
top of it. The combination of hooks + skills + MCP gives Wakil the full
extensibility story without a plugin marketplace.

**Effort**: 3–6 days

**Dependencies/risks**: Hooks run with user privileges — must sit under the
permission ladder, not bypass it. Timeout handling. Hook output size (spill
files apply). Repository-controlled hooks introduce a trust boundary (must be
explicitly enabled, not auto-discovered). Deterministic ordering; defined
failure behavior.

**Acceptance criteria**:
- Post-edit hook running a formatter results in formatted file before next model turn
- Failing pre-hook blocks the tool with a visible reason
- Hook timeout is enforced
- Hooks cannot override permission denials
- Repository-controlled hooks require explicit opt-in

---

### 6. Built-in Review Workflow

**User problem**: "Review my changes" today means ad hoc prompting or invoking
the external Mashūra tool. A structured, repeatable review process doesn't exist
as a first-class command.

**MVP scope**: `/review [ref]` runs a discovery-tier subagent over `git diff`
with a fixed rubric: correctness, tests, security, style vs AGENTS.md. Outputs
structured findings with file:line references. Reviewer is strictly read-only —
zero file mutations. Large diffs are chunked (depends on #2 compaction).

**Rationale**: Nearly all pieces exist (subagents, git_diff, LSP, Mashūra for
escalation). Copilot CLI has a review specialist; Codebuff has a reviewer agent
role; Droid has code review. Low marginal cost — this is assembling existing
primitives into a workflow, not building new infrastructure.

**Effort**: 2–4 days

**Dependencies/risks**: Reviewer must not edit (enforce via discovery tier).
Large diffs need chunking (depends on #2). Review quality depends on diff
context available — very large changes may need #8 (repo map) for surrounding
context.

**Acceptance criteria**:
- Seeded bug in a diff is flagged with correct file:line
- Review on a 2k-line diff completes without context overflow
- Reviewer makes zero file mutations (assert via #1 snapshot if available)
- Findings include severity levels and actionable recommendations

---

### 7. Cache-Stable Prompt Layout

**User problem**: Every turn, the model re-processes the full system prompt,
tool schemas, and context — paying for tokens it already saw. This is the
dominant cost driver for long sessions.

**MVP scope**: Freeze system prompt + tool schemas + AGENTS.md as an immutable
prefix. Move volatile items (timestamp, memory KV, plan) to the end.
Deterministic tool ordering. Measure cache-hit ratio via #3. Provider-specific
cache control (Anthropic's `cache_control` markers vs automatic prefix caching
elsewhere — confirm against each provider's current docs).

**Rationale**: DeepSeek-Reasonix achieved 99.82% cache hits by engineering for
it. The technique: stable env summaries at startup, stale tool output pruned
before compaction, planner and executor on separate cache-stable threads.
Wakil's system prompt and tool schemas are stable — they just need to be
ordered and marked correctly. Pure cost win with no capability change.

**Effort**: 2–5 days

**Dependencies/risks**: Requires #3 to measure. Provider cache semantics
differ — must confirm against each provider's current documentation. The
"99.82%" figure is from a secondary source and unverified for Wakil's context.
Set your own baseline first. Must not change task success rate.

**Acceptance criteria**:
- Cache-read tokens as % of input rises measurably on a 20-turn benchmark
- No change in task success on a small eval set
- Works across at least 2 providers with different caching semantics
- Baseline and post-optimization measurements documented

---

### 8. Repo Map (Lightweight Indexing)

**User problem**: Cold-start on a large repo burns 5–10 turns on discovery
subagents just finding where things are. The agent doesn't know the layout and
has to explore before it can work.

**MVP scope**: On first run, build a symbol-level outline (via existing LSP
`lsp_symbols` or tree-sitter) ranked by reference count. Inject top-K symbols
into context. Refresh incrementally on file changes. Skip embeddings — not
needed for v1.

**Rationale**: Aider's repo map and Auggie's whole-repo index both solve this.
Wakil already has LSP tools — extending them to build a repo-level overview is
a natural extension. The discovery subagent dispatch pattern already works for
exploration; a pre-built map just makes the first turn faster.

**Effort**: 5–10 days

**Dependencies/risks**: LSP availability per language (not all languages have
servers configured). Index staleness — needs invalidation strategy. Context
cost (conflicts with #7 if not placed in the volatile tail). Index build time
must be bounded (< 60s for a 50k-LOC repo).

**Acceptance criteria**:
- On a 50k-LOC repo, first-turn "where is X handled" resolves with ≤1 search call
- Index build < 60s
- Index refresh on file change is incremental (not full rebuild)
- Works for at least Go, TypeScript, and Python

---

### 9. Worktree Isolation for Parallel Edit Subagents

**User problem**: Parallel edit-tier subagents in one workspace can clobber each
other's changes. The semaphore prevents concurrent edits, but if relaxed,
silent overwrites occur.

**MVP scope**: Each parallel edit dispatch gets `git worktree add`. Results
returned as patches. Parent applies/merges with conflict report. Worktrees
cleaned up on exit or crash. Only valuable if parallel edit dispatch is actually
used — check usage patterns first.

**Rationale**: Grok Build made this their flagship feature (8 parallel in
isolated worktrees). Claude Code also supports it. For Wakil, it would unlock
true parallel implementation — e.g., refactoring 3 independent packages
simultaneously. The infrastructure (subagents, git tools) already exists; the
missing piece is workspace isolation.

**Effort**: 5–10 days

**Dependencies/risks**: Git-only (doesn't work in non-git directories). Merge
conflicts push complexity onto the parent. Shared services, credentials, ports,
and caches remain concerns (worktrees separate files, not security boundaries).
Only valuable if parallel edit dispatch is actually used — verify demand first.

**Acceptance criteria**:
- Two subagents editing the same file produce a conflict report, not silent overwrite
- Worktrees are cleaned up on normal exit and crash
- Task-local workspace is propagated (env, config, etc.)
- Non-git directories fall back to serialized execution with a warning

---

### 10. Correction-Capture Learning Loop

**User problem**: Users repeat the same corrections across sessions. "Don't use
var, use const." "Run tests with `make test` not `go test`." The agent forgets
between sessions because no one captures the pattern.

**MVP scope**: Detect correction patterns (user reverts agent edit, explicit
"no, do X") → propose a durable memory or skill entry with provenance → user
confirms. Leverages Wakil's existing memory system, which is its strongest
differentiator. Needs #1 (checkpoints) to reliably detect reverts.

**Rationale**: Wakil's memory system (durable + mid-tier + staging, with
provenance and taint tracking) is already more sophisticated than most agents'
memory. What's missing is the *loop*: detect → propose → confirm → store →
auto-apply next time. This turns Wakil's memory from passive storage into
active learning. No competitor does this well yet — it's a differentiation
opportunity, not just parity.

**Effort**: 3–7 days

**Dependencies/risks**: False positives (not every correction is a pattern).
Must go through taint/provenance system. Needs #1 to detect reverts reliably.
User must explicitly confirm — never auto-store corrections without consent
(respects Wakil's "never claim to have learned anything" rule).

**Acceptance criteria**:
- After a user correction in session 1, session 2 on a similar task applies it without re-prompting
- The memory entry shows its origin (which session, which correction)
- False positive rate < 20% (user rejects fewer than 1 in 5 proposals)
- Corrections are never stored without explicit user confirmation

---

## Features Recommended to Defer or Skip

These features are overhyped, premature, or unnecessary for Wakil's positioning
as a terminal-first coding agent. Mashūra panelists strongly agreed on these:

| Feature | Reason to Defer |
|---------|-----------------|
| Plugin marketplace | Distribution problem, not capability. MCP + skills already cover extension. Revisit only with external contributors. |
| ACP | Only matters when editor integration is requested. Zero user pull evidenced. Naming is also ambiguous (Agent Client Protocol vs Agent Communication Protocol). |
| Remote / cloud execution | Requires infra Wakil doesn't own. Headless mode + CI gets 80% there. High operational and security cost. |
| Voice / multimodal input | Rare in CLI agents. Low default priority. Don't conflate with useful image/screenshot input (Wakil already has browser screenshots). |
| Agent teams / inter-agent messaging | Experimental everywhere. Parallel dispatch + review subagent covers realistic cases. Adds orchestration complexity without proven benefit. |
| Spec-driven development | A skill on top of plan mode, not a feature. Write the skill. |
| Scheduled / cron automation | `cron` + headless mode already works — document the recipe instead of building a scheduler. |
| Semantic (embedding) search | Repo map + LSP + grep covers most needs at far lower complexity. Embeddings are overkill for v1. |
| Autonomous PR / commit / push | Deliberate design stance. Not a gap. |
| 500+ model routing | BYOK for a handful of providers is enough. If main-model swap isn't already a config key, that's a 1-day fix, not a feature. |
| OS-level sandboxing | Real value but high cross-platform effort. Permission ladder + hooks + checkpoints cover more risk per day of effort. Defer. |
| Cross-agent config portability | No demand evidenced. AGENTS.md ingestion (#4) covers the important case. |

---

## Implementation Priority Order vs. Ranking

The ranking above is by value. The implementation order should respect
dependencies:

1. **#4 AGENTS.md** (0.5–2 days, no deps) — ship immediately
2. **#3 Cost tracking** (2–5 days, no deps) — ship immediately
3. **#2 Context compaction** (3–8 days, no deps) — ship next, enables #7
4. **#5 Hooks** (3–6 days, no deps) — independent, can parallelize
5. **#6 Review workflow** (2–4 days, depends on #2 for large diffs) — after #2
6. **#1 Checkpoints** (5–10 days, no deps) — independent, high value
7. **#7 Cache-stable prompts** (2–5 days, depends on #3) — after #3
8. **#10 Correction capture** (3–7 days, depends on #1) — after #1
9. **#8 Repo map** (5–10 days, depends on #2) — after #2
10. **#9 Worktree isolation** (5–10 days, independent but verify demand first)

---

## Before Acting on This Proposal

1. **Audit Wakil's codebase** for features marked ⚠️ or ❓ — especially
   context compaction (#21) and multi-model config (#19). Two ranked items
   depend on knowing the current state.
2. **Pull actual user pain** from session logs, correction memory, and failure
   patterns to validate or challenge this ordering.
3. **Attach source URLs** to any competitor-specific claim before using it in
   implementation decisions.
4. **Define Wakil's positioning** explicitly — solo interactive user vs.
   unattended CI vs. team workflows changes the ranking materially.