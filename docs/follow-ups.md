# Open follow-ups

Known deferred work items, kept here so deleting their original proposal docs
doesn't lose them. Each entry lists the decision that closed the original
proposal and what remains.

## Prompt architecture

- **Redaction tool** (from the prompt-corrective-revisions review, §2.1(b)):
  the agent.txt screening rules say "a tested redaction tool is the only
  sanctioned redactor; until one exists, withhold". `internal/scrub`
  (PatternScrubber) exists but is the store write-path scrubber, not an
  agent-exposed tool. Until a dedicated redaction tool exists and is tested,
  the withhold rule stays in force.
- **Harness-enforcement follow-ons** (same review, §5): encode the
  non-emitting screening contract (grep -c / exit-status-only checks) as
  harness tests rather than relying on prompt compliance alone.
- Prompt **trimming** (docs/prompt-trimming-proposal.md, deleted) and
  **session-scoped progressive prompt loading**
  (docs/session-scoped-prompt-proposal.md, deleted) were both **rejected**
  (measured 97.6% prompt-cache hit rate made the economics moot; the safety
  kernel computes to 14–19 KB, above the go/no-go threshold). Note: the
  separately shipped `/prompt core` mode (lean preamble + lazy-loaded
  extension, `prompt_mode` config) is a different, implemented design.
  Any future prompt-size proposal must start from the go/no-go measurement
  template of the deleted session-scoped proposal: kernel size, cache-hit
  rate, section usage distribution (recoverable via
  `git log --follow -- docs/session-scoped-prompt-proposal.md`).

## TUI rotation

- **Handoff stage labels** ("generating summary…"): needs a progress callback
  on `RunHandoffPipeline` → `ConversationManager` interface change →
  stale-stage guard. Deferred — the pulsing indicator conveys ~90% of the
  value; revisit only if "frozen" reports persist.
- **Rotation timeout**: `beginRotate` (internal/tui/tui.go) passes
  `context.Background()` and `RunHandoffPipeline` does not add the 120s
  timeout its legacy path had — a hung backend can block rotation
  indefinitely. Fix is a separate change.

## Long-session mode

- **Daemon parity**: `context_mode` in the proto `SessionState`, a
  `SetContextMode` RPC, `RemoteFacade.Info()` filling `ContextMode` (note:
  the same `Info()` currently omits `RawTools` too).
- **Tool-cache retention**: age-based retention exists
  (`tool_cache_retention_days`, default 30, pruned once at startup; 0 =
  never). Size-based cleanup of the tool cache remains open.
- **Model-proposed partial keep** (`context_keep`-style tool with
  tool-call/sibling closure) — collapse is currently deterministic.
- **Rolling summarization of old answers** so the base stays bounded on very
  long sessions (tail-cap degrade bounds the collapsed-turn answer
  component only; uncapped questions, kept turns and never-collapsed turns
  still grow).

## Continuous mode

- CM2 (coordinator-owned goal completion), CM3 (durable state, leases,
  reconciliation), CM4 (dispatch-time side-effect ledger) — see
  [continuous-mode.md](continuous-mode.md).
- Remote `SaveSession` is a no-op in `internal/remote/facade.go` — needs a
  focused follow-up before remote/daemon scope.

## ILM emitter

- `wakil-emitter.md` (the served-contract reference) has no substantive
  **assist-mode section**: the shadow-mode guarantee ("no proposal reaches
  Wakil") does not hold in assist mode, where the AssistClient reads
  proposals (`internal/ilm/assist.go`). When updating the contract, remember
  the **served artifact** (GET /v1/contract, sha-pinned) must be regenerated
  in lockstep — updating the repo copy alone does not change what the
  endpoint serves.
