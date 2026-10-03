# Proposal: Session-Scoped Progressive Prompt Loading

> **Status: REJECTED — do not implement.** Two-panel Mashūra review (2026-10-02):
> the safety kernel computes to 14–19 KB from §2's own table, failing §8.1's abort
> threshold before implementation; capability-based selection (§5-A) is vacuous
> because memory/shell/async/subagents are all compiled in; §3.3's cache claim was
> probably false. Measured 97.6% cache hit rate makes the economics moot regardless.
> Retained as a record of the analysis. Successor: `docs/prompt-corrective-revisions-proposal.md`.
> **Supersedes:** nothing. Corrects an earlier draft that described *per-request* conditional loading,
> which Mashūra reviewed and rejected (see §7).
> **Date:** 2026-10-02

## 1. Goal

Reduce the always-resident system prompt by loading only the safety kernel on the
first request, then appending topic-specific sections as the session's needs become
known — **monotonically**, so the prompt only ever grows and then stays stable for
the rest of the session.

The agent stays correct at every point: no safety rule is ever absent because a
section was not selected.

## 2. Current state (measured)

- `prompts/agent.txt`: **43,847 bytes / 739 lines / ~11k tokens**, `go:embed`ed
  (`prompts/embed.go:14`), loaded once at startup by `loadAgentPrompt`
  (`internal/wiring/bootstrap.go:459`).
- Precedence: `cfg.AgentPromptPath` file if readable, else the embedded string.
- Sent as `Conv[0]` via `buildPreamble` (`internal/agent/app.go:1447`), pinned
  day-stable by `ensurePreamble` (`app.go:1563`) — re-entrant only when the
  calendar day changes.
- Cache breakpoints: exactly two — `messages[0]` and the last non-null message
  (`computeCacheBreakpoints`, `internal/proxy/client.go:232-249`). Intermediate
  messages are **not** marked. Enabled by config flag or the Anthropic/OpenRouter
  heuristic (`client.go:849`).

Per-section size (measured, `##`-level):

| Section | Bytes | Lines |
|---|---:|---:|
| Global precedence ladder | 742 | 12 |
| Role | 825 | 14 |
| Core loop | 2,544 | 38 |
| Secrets (tier 1) | 3,348 | 52 |
| Shell | 2,029 | 33 |
| Files | 799 | 14 |
| Documentation upkeep | 1,365 | 26 |
| What "verified" means | 789 | 15 |
| Search & Research (incl. Mashūra) | 4,828 | 82 |
| External side-effect gating | 2,014 | 31 |
| File vs. Inline Decision | 593 | 15 |
| Skills | 1,435 | 25 |
| Tool / Cost Discipline | 624 | 11 |
| **Durable Memory** | **8,135** | **148** |
| Honesty Invariants (tier 4) | 1,131 | 19 |
| Truncated results / spill | 393 | 7 |
| Recovery | 277 | 6 |
| **Async execution** | **4,484** | **79** |
| **Subagents** | **6,522** | **95** |
| Interaction & Output | 516 | 9 |

The three largest — Durable Memory, Subagents, Async execution — total
**19,141 bytes (44%)** and are the most self-contained. They are the natural
first candidates for deferral. The remaining ~24.7 KB is the plausible kernel
ceiling before any kernel trimming.

## 3. Design

Three tiers, escalating over the session's lifetime:

- **Tier 0 (turn 1, every request):** safety kernel only. Never selected, never
  omitted. Contents in §4.
- **Tier 1 (sections):** topic blocks appended as system messages when the
  session's needs become known.
- **Tier 2 (escalation):** the remainder, loaded on tool-result-driven discovery
  (see §5).

### 3.1 Monotone growth is the core constraint

The prompt is only ever extended. It is never rewritten, reordered, or removed
within a session. This bounds the number of prefix invalidations to a small
constant per session (typically 2: the initial kernel, then the first append),
independent of turn count. This is what distinguishes the design from the
per-request variant rejected in §7.

### 3.2 Sections must be separate messages, never concatenated into Conv[0]

`ensurePreamble` re-enters only on **calendar-day** change. If Tier-1 sections
were concatenated into `Conv[0]`, each append would require a day-independent
mutation path and would collide with the day-rollover logic.

Appending **separate system messages after `Conv[0]`** avoids this entirely:
`Conv[0]` stays byte-identical for the whole day regardless of how many sections
are loaded, and the existing pinning logic is untouched.

### 3.3 Cache breakpoints should be extended

`computeCacheBreakpoints` marks only index 0 and the last message. Appended
sections are neither, so they receive no cached prefix and are re-billed in full
each turn. Since the set is bounded and written once, extending the breakpoint
set to the loaded-section messages is a small, worthwhile improvement. This is an
optimization, not a correctness requirement.

## 4. Safety kernel — unconditional

Mashūra's sharpest correction to the earlier draft: **safety is
action-dependent, not request-keyword-dependent.** "Fix a test" can reach
credentials, deployment tooling, or destructive cleanup. No safety rule may be
gated by selection of any kind.

Tier 0 therefore always contains:

- the full global precedence ladder and interpretation rules;
- the **complete** secrets policy — external-query sanitization, sensitive
  command output, delegation/evidence screening, accidental-exposure handling;
- external-side-effect and sensitive-read gates, including the approval
  carve-outs (irreversible destructive and production-mutating actions are never
  covered by blanket approval);
- shell approval, destructive-action rules, the production definition, and the
  per-action-confirmation requirement;
- user-work preservation and mutation restrictions;
- honesty, verification, and completion rules, including
  `[inferred — confirm]` labeling;
- the trust treatment for memory and external material (taint is not
  instruction);
- the rule that unavailable or unselected sections may never weaken the kernel,
  and what happens when a requested section cannot be loaded (fall back to the
  full prompt — never silently proceed without it);
- async result/completion semantics, to the extent async tools remain available.

**Kernel trimming is a separate project.** This proposal does not shrink the
kernel; it proposes which sections are *deferrable*. If the honest kernel turns
out to be 8 KB rather than 2 KB, this design yields little and should be
abandoned — that is a measurable go/no-go, not a matter of taste.

## 5. Selection mechanism

Three candidates, for different roles:

- **A — harness-side rules (explicit mode / capability signals).** Preferred
  selection mechanism. Matches on *declared* task mode and *actually enabled*
  capabilities, never on free-text keywords. Unknown or ambiguous cases fall
  back to the full prompt — safe default, never a partial prompt.
- **C — `load_section(name)` tool, as an escape hatch.** For sections whose
  relevance only becomes clear mid-session (a tool result reveals deploy
  tooling, a credential, a subagent). Requires an always-visible catalog with
  one-line descriptions.
- **B — a cheap selector model.** Not recommended: serial latency, extra cost,
  another failure mode, and the selector call itself needs a prompt. Revisit only
  if A demonstrably misfires in production.

**Two constraints on C, from the earlier review:**

1. **Authority.** Tool output is not automatically equivalent to a system
   instruction. Sections whose content must *bind* should arrive in a system-role
   message, not as tool result text. Decide this deliberately.
2. **Compaction.** Loaded sections must survive compaction as pinned content.
   A section compacted away mid-task silently removes a rule the model believed
   it had. This is a correctness risk, not a cost issue.

Sections must be served from a **trusted, versioned, allowlisted catalog** at
compile time — never from arbitrary filesystem paths supplied by model output.

## 6. Acceptance criteria

- Loaded sections are pinned against compaction; a section the model relied on
  cannot silently disappear.
- Selection is capability/mode-driven; free-text keyword matching alone never
  gates a safety rule.
- Unknown, ambiguous, or failed selection falls back to the **full** prompt.
- Section authority is established by role, with tool-output sections explicitly
  reasoned about and documented.
- Subagents and side-question clones receive the same kernel treatment as the
  parent; the kernel must be present before any capability can execute.
- Ordinary non-continuous, TUI, headless, and daemon paths are unaffected.
- Cache invalidations per session are counted and bounded by design, not by
  accident.

## 7. Relationship to the rejected per-request design

An earlier draft proposed selecting sections **per request**. Mashūra reviewed
that design and rejected it: net saving is `0.1pD − 0.9pL`, requiring **D > 9L**,
where `L` includes *conversation history* after the changed prefix — so a
per-request-varying prefix can cost far more than the omitted tokens save, and a
shortened *cold* prompt can cost ~6× a full *warm* one.

That analysis stands **for the per-request design only**. Session-scoped monotone
accumulation reduces prefix invalidations from per-turn to a small constant per
session, which is why this design is materially different and worth reviewing on
its own terms. The D > 9L threshold should not be cited as a rejection of *this*
proposal — but it is the right lens for any future proposal to return to
per-request selection.

## 8. Go/no-go measurement

Before implementation, answer with data rather than estimates:

1. **Kernel size.** Split the file into kernel vs. deferred sections and measure
   the byte/token split. If the honest kernel is ≥8 KB, the deferred remainder
   cannot repay the two extra prefix writes and the project stops here.
2. **Cache-hit rate.** Actual observed hit rate from real traffic. The comments
   in `app.go` express *intended* cache behavior; they are not billing evidence.
3. **Section usage distribution.** How often a typical session actually needs
   Durable Memory / Subagents / Async. If most sessions converge on the full
   prompt, the design earns nothing.

Steps 1 and 3 are cheap and can be done as a pure file restructuring with
**byte-identical assembled output** — no behavior change, no token change, and
it yields the numbers this proposal needs. Note that a packaging-only split must
still be described as "no intended model-input change," not "zero behavior
change": loading, packaging, and failure paths can still regress.

## 9. Open questions for review

1. Is system-role section injection tractable through the current wire format,
   or does it require a proxy/provider change?
2. Should section selection be sticky for the session, or re-evaluated per turn?
   (This proposal argues sticky — §3.1.)
3. If the kernel proves large (§8.1), is the honest answer to abandon deferral, or
   to reconsider whether 11k tokens of rules is itself the problem?
4. Does compaction need a first-class "pinned section" concept, independent of
   the existing leading-system-message pinning?
