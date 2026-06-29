# Context-Limit Awareness for Mashūra (Counsel) Calls

## Problem

The Mashūra counsel path (`callOpenRouter`, `callFusion`, `callAnthropic` in
`internal/counsel/oracle.go`) had no context-limit awareness. Briefings were
capped at a hardcoded 200 KB byte ceiling (`mashuraToolBriefingCap` in
`internal/agent/mashura.go:32`) that was assumed — but never verified — to fit
the target model's context window. For fusion's 1M-token window this is always
safe; for smaller models it can overflow and fail silently or with an opaque API
error.

Meanwhile, the main ilm-proxy path has a full dynamic context-limit resolution
system (`internal/agent/ctxlimit.go`) that probes the backend for `n_ctx` and
scales compaction thresholds accordingly. That system is **not** wired to the
counsel path.

## Solution

A new file `internal/counsel/modellimit.go` adds three pieces:

### 1. FetchModelContextLimits — OpenRouter model metadata cache

Fetches `context_length` for every OpenRouter model from the public
`/api/v1/models` endpoint (no API key required), cached for 1 hour at the
process level.

**Key design decisions (informed by Mashūra panel review):**

- **HTTP I/O outside the cache mutex.** The first version held a `sync.Mutex`
  across the up-to-10s fetch, serializing all concurrent callers. The revised
  version uses a singleflight flag (`inflight`) so only one goroutine fetches at
  a time, and the fetch happens outside the lock. Concurrent callers spin-wait
  (100ms intervals, up to 10s) for the fetcher to finish, then read the result.
  This is acceptable because counsel calls are infrequent and gated.

- **Stale-on-error.** If a refresh fetch fails but stale cached data exists, the
  stale data is returned (with nil error) rather than falling back to the
  conservative default. This prevents a transient OpenRouter outage from causing
  unnecessary truncation.

- **Defensive copy.** `FetchModelContextLimits` returns a `cloneMap` of the
  cached entries so callers cannot mutate the shared cache.

### 2. ResolveContextLength — model → context length resolution

Resolution order:
1. OpenRouter registry (live or cached)
2. `knownModelContexts` fallback table (covers Anthropic direct calls that
   don't go through OpenRouter's catalog, plus the fusion model)
3. `fallbackContextLength` (128K — conservative last resort)

For bare Anthropic model IDs (e.g. `"claude-sonnet-4-6"`), it tries the
`"anthropic/<model>"` prefix form since OpenRouter carries Anthropic models
under that prefix.

### 3. FitToContext — request fitting

Checks whether a request (systemPrompt + question + briefing + maxTokens) fits
within the model's context window and adjusts if needed.

**Adjustment priority:**
1. **Reduce `max_tokens`** (down to `minOutputTokens` = 1024) to make room for
   input. This preserves briefing content (input fidelity) over output budget.
2. **Truncate the briefing** (at a UTF-8 rune boundary) if reducing max_tokens
   to the floor is not enough. The truncation marker is budgeted into the
   calculation so the result doesn't overflow.
3. **Signal `CannotFit`** if the request still doesn't fit after all adjustments
   (e.g., system+question alone exceeds the window).

**Safety margin:** A 90% safety margin (`contextSafetyMargin = 0.90`) is applied
to the context window so the estimate doesn't target the absolute ceiling. This
accounts for:
- Tokenizer variance (4 chars/token undercounts code/JSON)
- Message-protocol overhead (JSON formatting, roles, etc.)
- The `"\n\nContext:\n"` joiner that callers prepend (now included in the
  estimate)

**Edge cases handled:**
- `contextLength <= 0` (unknown): returns inputs unchanged, no fitting
- `maxTokens <= 0`: clamped to `minOutputTokens`
- Empty briefing + oversized other input: truncation skipped, `CannotFit` set
- UTF-8: truncation backs up to a valid rune boundary via
  `utf8.DecodeLastRuneInString`
- Truncation marker length: subtracted from the briefing budget before slicing
- Newline over-truncation: only cuts to the last newline if it preserves ≥80% of
  the budget

### ContextFit struct

```go
type ContextFit struct {
    ContextLength     int    // model's context window (tokens); 0 if unknown
    InputEstimate     int    // estimated input tokens after fitting
    MaxTokens         int    // adjusted max_tokens
    Briefing          string // possibly truncated briefing
    Adjusted          bool   // true if max_tokens or briefing was changed
    ReducedMaxTokens  bool   // true if max_tokens was reduced
    TruncatedBriefing bool   // true if briefing was truncated
    CannotFit         bool   // true if the request cannot fit
    Note              string // human-readable description of adjustments
}
```

## Integration plan (not yet wired into oracle.go)

The three call sites in `internal/counsel/oracle.go` need to be updated:

### callOpenRouter (line ~357)
```go
func callOpenRouter(ctx context.Context, model, apiKey, question, briefing string, ccfg PanelCallConfig) (string, OracleUsage, error) {
    // ... endpoint resolution ...

    contextLength := ResolveContextLength(ctx, model)
    fit := FitToContext(oracleSystemPrompt, question, briefing, ccfg.MaxTokens, contextLength)

    userContent := question
    if fit.Briefing != "" {
        userContent += "\n\nContext:\n" + fit.Briefing
    }

    body, err := json.Marshal(orReq{
        Model:     model,
        MaxTokens: fit.MaxTokens,  // use fitted value
        Messages: []orMsg{
            {Role: "system", Content: oracleSystemPrompt},
            {Role: "user", Content: userContent},
        },
    })
    // ... rest unchanged ...
}
```

### callFusion (line ~439)
Same pattern — resolve context length for `"openrouter/fusion"`, fit, rebuild
`userContent` from `fit.Briefing`, use `fit.MaxTokens`.

### callAnthropic (line ~308)
Same pattern — resolve context length for the Anthropic model (via the
`knownModelContexts` table or OpenRouter registry with `anthropic/` prefix),
fit, rebuild, use `fit.MaxTokens`.

### Surfacing the Note

The `ContextFit.Note` should be surfaced to the user out-of-band (e.g., printed
to stderr or a status line in the TUI), not injected into the LLM input. The
briefing's `[briefing truncated]` marker already signals the model. The `Note`
is for the operator — "your briefing was truncated to fit X's context window."

One approach: add an optional `*ContextFit` field to `PanelMemberResult` so the
caller (`handleMashura` in `internal/agent/mashura.go`) can print it:

```go
type PanelMemberResult struct {
    // ... existing fields ...
    Fit *ContextFit // non-nil when context fitting was applied
}
```

## Testing

`internal/counsel/modellimit_test.go` covers:
- Cache hit / defensive copy (mutations don't affect cache)
- `ResolveContextLength` direct lookup, prefix fallback, known-table fallback,
  unknown model fallback
- `FitToContext`: no adjustment, max_tokens reduction, briefing truncation,
  unknown model (no fitting), exact boundary fit, CannotFit, zero/negative
  maxTokens, UTF-8 safe truncation, marker budgeting
- `approxTokens` consistency with `proxy.ApproxTokens`

All tests pass under `-race -count=3`.

## What the Mashūra panel review caught (and we fixed)

| Issue | Panel consensus | Fix |
|-------|----------------|-----|
| Mutex held across 10s HTTP fetch | All 3 models: serious design flaw | Singleflight + fetch outside lock |
| Returned map mutable by caller | Opus + GPT: fragile invariant | `cloneMap` defensive copy |
| No stale-on-error | Opus + GPT: latency landmine | Serve stale cache on fetch failure |
| Truncation marker not budgeted | All 3: real overflow bug | Subtract `len(marker)` from budget |
| UTF-8 rune splitting | All 3: invalid output | `utf8.DecodeLastRuneInString` boundary check |
| No CannotFit path | GPT + Gemini: missing guarantee | `CannotFit` flag + final verification |
| `maxTokens <= 0` not guarded | Opus + GPT: API rejection | Clamp to `minOutputTokens` |
| No safety margin on estimate | All 3: undercounts code/JSON | 90% safety margin |
| Joiner not in estimate | All 3: measures wrong string | Include `"\n\nContext:\n"` in estimate |
| Anthropic sized from OR catalog | Opus + GPT: wrong source | `knownModelContexts` fallback table |
| Note not surfaced to user | All 3: silent degradation | Structured `Note` field (integration TODO) |

## What the panel flagged that we did NOT address (deferred)

- **Real tokenizer / token-count API.** The panel suggested Anthropic's
  `/v1/messages/count_tokens` endpoint for exact counting. This is accurate but
  adds a network round-trip per counsel call. The 90% safety margin is
  sufficient for now; exact token counting is a future enhancement.

- **Fusion routing risk.** Fusion dispatches to underlying models in parallel;
  a single `context_length` for `openrouter/fusion` may not reflect the model
  actually serving a sub-request. The OpenRouter API reports 1M for fusion
  (verified: `curl -s https://openrouter.ai/api/v1/models | jq '.data[] |
  select(.id=="openrouter/fusion")'`), but if a large prompt routes to a smaller
  underlying model, it could still fail. This is an OpenRouter-side
  responsibility; we can only size for the reported window.

- **`top_provider.max_completion_tokens`.** The OpenRouter `/models` response
  also includes per-provider `max_completion_tokens` which could cap
  `max_tokens` separately. Not yet read; deferred until a concrete issue
  arises.

- **Prompt caching interaction.** Dynamic truncation defeats Anthropic/OpenRouter
  prompt caching. Since counsel calls are infrequent and gated, this is
  acceptable; if counsel volume increases, caching-aware truncation would be
  needed.
