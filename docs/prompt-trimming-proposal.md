# Proposal: Trim the Static Agent Prompt

> **Status: REJECTED — do not implement.** Two-panel Mashūra review (2026-10-02):
> measured 97.6% OpenRouter cache hit rate gives an effective input multiplier of
> 0.1216, so a 10 KB trim saves ~1.2k token-equivalents/request — noise in dollars.
> The ~4.1 KB estimate was also a large underestimate (realistic 8–12 KB) built on a
> misreading of which Core-loop copy carried the "not proof" caveat. Valon decided
> not to touch the prompt for size. Retained as a record. Successor:
> `docs/prompt-corrective-revisions-proposal.md` (correctness fixes only).
> **Date:** 2026-10-02
> **Supersedes:** nothing. Succeeds the approach of `docs/session-scoped-prompt-proposal.md`,
> which a two-panel Mashūra review rejected on measured grounds (§7).

## 1. Goal

Reduce `prompts/agent.txt` (43,847 bytes / 739 lines / ~11k tokens) by removing
prose the model does not need to act correctly, **while keeping it static**.

This is deliberately the low-risk project. It targets the same concern as
progressive loading — 11k tokens of rules is too much — without any of the
architecture that proposal required.

## 2. Why trimming rather than deferral

The progressive-loading proposal was rejected by two independent panels. Its
own §2 byte table decided the question: the safety kernel it needed was
**14–19 KB** against a self-imposed 8 KB abort threshold, so it failed its own
go/no-go before implementation. Full reasoning is in §7; the operative points:

- Deferral's upside was capped and small: the prompt is already ~90% cache-hit
  (~0.1× price), so the design targets the residual of 44% of 11k tokens —
  single-digit percent of input cost, and much less of total cost.
- Deferral's downside was unbounded: one section inserted mid-session against a
  large history re-writes the prefix, and could exceed a whole session's savings.

Trimming strictly dominates. Cutting 10 KB of prose saves more than deferring
19 KB, and requires no section catalog, no `load_section` tool, no pinning
semantics, no persistence of pin state, no fallback path, and no multi-endpoint
wire-format testing.

## 3. Evidence: specific trimmable prose

Four categories, each verified against the code.

### 3.1 Implemented-in-code behavior described as instruction

`### Correction-Capture Learning Loop` (`agent.txt:499-524`, ~1.5 KB) describes
a loop that **already runs automatically in the parent agent**. Verified:
`internal/agent/correction.go:279` prints *"· correction proposal declined —
not stored"* and `:300` prints *"· correction stored as proposed memory
entry"*. The prompt text's own words: *"You do not control this loop — it runs
automatically in the parent agent before your turn starts… You should not
attempt to trigger or suppress it; just do your work as normal."*

The model is told about a notification it does not produce and cannot act on. It
is documentation of harness behavior, not operating instruction. **Remove, or
reduce to a one-line acknowledgement that corrections may be auto-proposed.**

### 3.2 Duplicated content

`## Core loop` step 2 (`agent.txt:39-55`, ~1.4 KB) restates memory-retrieval
policy — `memory_get("plan/current")`, `memory_search` with focused terms for
environment/architecture tasks, and the "relevant-sounding keys" digest rule.
`## Durable Memory` → `### When to retrieve memory` (`:413-435`) states the
same policy with more detail and the correct authority ("use as prior context,
not proof"). Two copies of one rule, one of them the staler duplicate.

**Keep the `## Durable Memory` version. Compress the Core-loop step 2 reference
to a pointer.**

### 3.3 Cross-references that constrain future splitting

`## Role` and `## Secrets` both dispatch readers to `## Subagents` as the
canonical rule; `## Core loop`, `## Search & Research`, and `## Mashūra` all
point at `## Async execution` (7 references total). These are correct today and
should stay. They are recorded here because they are what made progressive
loading structurally hard — the prompt is a web of cross-references, not a set
of independent modules.

### 3.4 Candidate size (to be validated, not assumed)

| Target | Est. bytes | Confidence |
|---|---:|---|
| Correction-Capture → one line | ~1,400 | High — behavior is code, not instruction |
| Core-loop step 2 → pointer | ~1,200 | High — duplicate of a better-placed rule |
| `## Search & Research` prose tightening | ~1,000 | Medium — needs a read for genuine redundancy |
| `## Skills` tightening | ~500 | Medium |
| **Total** | **~4,100** | |

This is a **floor, not a ceiling**. Honest caveat: ~4 KB is roughly 36% of the
19 KB deferral would have touched, at a fraction of the risk. If that is not
worth doing, deferral was certainly not.

## 4. Explicit non-goals

- **No architectural change.** The prompt stays static, day-stable, embedded.
  `buildPreamble`, `ensurePreamble`, and `computeCacheBreakpoints` are untouched.
- **No safety-rule removal.** Every rule in the kernel identified by the prior
  review — secrets, side-effect gating, destructive-action confirmation, honesty,
  verification labeling, user-work preservation — stays. Trim *explanation and
  duplication*, never obligations.
- **No deferral, catalog, or `load_section` tool.**
- **No byte-identical split.** That was part of the rejected proposal's staging
  and served its measurement; it is not needed here.

## 5. Method

1. Inventory every candidate against the rule inventory in §3, and confirm with
   grep that each removed behavior is code-enforced, not model-enforced.
2. Edit `prompts/agent.txt` directly. Trimming is the point; preserving byte
   identity would defeat it.
3. Behavioral check, not just byte check: confirm the agent still retrieves
   memory at planning time, still escalates per §3.3 references, and still
   honors every kernel obligation. A prompt that is smaller but no longer
   *instructs* the behavior is a regression.
4. Measure the resulting token count and record before/after.

**Risk to watch:** removing descriptive prose may reduce behavior that was
implicitly relying on the model reading it. The §3.1 case is safe (code-driven);
§3.2 and §3.4 need behavioral verification, not just a passing test suite.

## 6. Acceptance criteria

- Every removed rule is either code-enforced or duplicated at its canonical
  location, evidenced by grep.
- No safety obligation is removed.
- No dangling `see ## X` reference is introduced (7 currently exist; the count
  must not increase).
- Before/after byte and token counts recorded.
- Full test suite passes; `agent.txt` has no test asserting exact byte identity
  that would block the edit (verify before editing).
- Behavioral spot-check for memory retrieval, delegation triggers, and async
  semantics.

## 7. The rejected alternative, in one paragraph

`docs/session-scoped-prompt-proposal.md` proposed loading a safety kernel first
and appending topic sections as a session's needs emerge. Two Mashūra panels
rejected it. Astra: monotone accumulation "deserves measurement, not the old
blanket rejection — but it is not ready for implementation," and corrected three
of its claims (prefix invalidations are bounded by catalog size, not "~2";
unmarked sections likely *are* covered by the moving breakpoint; the 8 KB cutoff
had no derivation). Fable was blunter: the kernel computes to **14–19 KB** from
the proposal's own byte table, so §8.1's "≥8 KB → abandon" already failed, and
the honest recommendation was "trim the prompt, keep it static, revisit
deferral only if trimming hits a floor." A capability-driven selection
mechanism was also found **vacuous** — memory, shell, async, and subagents are
all compiled in for the main agent, so capability-based selection loads
everything on turn 1 by construction.

That proposal is retained in `docs/` as a record of the analysis and its
rejection, with §§3.1, 3.3, 7, and 8 to be corrected.

## 8. Open questions

1. Is `~4.1 KB` the honest ceiling, or does a careful read find substantially
   more redundancy? The estimate is a floor; a reviewer with the full text may
   see more.
2. Should §3.1 be deleted outright or reduced to one line? Deleting loses the
   explanation of a loop whose notifications the user will see; one line is
   safer for user comprehension but is still instruction the model cannot act on.
3. Does the test suite assert on `agent.txt` contents in a way that blocks
   editing? Verify before starting.
4. Is a ~4 KB saving worth the behavioral-verification cost at all — or is this
   project only worth doing if §8.1 finds materially more redundancy?
