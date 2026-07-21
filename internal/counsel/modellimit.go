package counsel

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/treeol/wakil/internal/orregistry"
	"github.com/treeol/wakil/internal/proxy"
)

// Context-limit awareness for counsel (mashūra) calls.
//
// The main ilm-proxy path resolves n_ctx dynamically via ctxlimit.go, but the
// counsel path — callOpenRouter, callFusion, callAnthropic — had no such check.
// Briefings were capped at a hardcoded 200 KB byte ceiling that was assumed (but
// never verified) to fit the target model's context window. For fusion's 1M-token
// window this is always safe; for smaller models it can overflow and fail.
//
// This file adds three pieces:
//
//  1. FetchModelContextLimits — fetches context_length for every OpenRouter
//     model from the public /api/v1/models endpoint (no API key required),
//     cached for 1 hour at the process level. HTTP I/O happens outside the
//     cache mutex; concurrent cold callers share one fetch via a singleflight
//     pattern, and stale data is served on fetch failure.
//  2. ResolveContextLength — resolves a single model's context length, trying
//     the OpenRouter registry first, then a small known-model table, then a
//     conservative fallback.
//  3. FitToContext — estimates the input token count and, if the request would
//     overflow the model's context window, reduces max_tokens (down to a floor)
//     and/or tail-truncates the briefing to fit. Guarantees that the result
//     either fits or signals CannotFit.

const (
	// fallbackContextLength is the last-resort assumption when a model's context
	// length cannot be determined from any source. Deliberately conservative.
	fallbackContextLength = 128_000

	// minOutputTokens is the floor for max_tokens after auto-reduction. Below
	// this the oracle answer is too likely to be truncated to be useful; if even
	// this doesn't fit, the briefing is truncated instead.
	minOutputTokens = 1024

	// charsPerToken is the byte-to-token approximation used for input estimation.
	// Consistent with proxy.ApproxTokens; not exact but sufficient for fitting.
	charsPerToken = 4

	// contextSafetyMargin is the fraction of the context window to target —
	// leaving headroom for estimation error (4 chars/token undercounts code/
	// JSON), message-protocol overhead, and tokenizer variance.
	contextSafetyMargin = 0.90
)

// knownModelContexts is a small fallback table for common models when the
// OpenRouter registry is unreachable or doesn't list the model. This covers
// Anthropic direct calls (which don't go through OpenRouter's catalog) and
// provides a sensible default for the fusion model.
//
// Values are in tokens. Update when new model generations ship.
var knownModelContexts = map[string]int{
	"openrouter/fusion":           1_000_000,
	"anthropic/claude-sonnet-4-6": 200_000,
	"anthropic/claude-opus-4-8":   200_000,
	"anthropic/claude-3.7-sonnet": 200_000,
	"anthropic/claude-3.5-sonnet": 200_000,
	"anthropic/claude-3-opus":     200_000,
	"anthropic/claude-3-haiku":    200_000,
}

// FetchModelContextLimits retrieves model context lengths from OpenRouter's
// public /api/v1/models endpoint (no API key required). The fetch, cache, and
// singleflight logic live in internal/orregistry (shared with the agent's
// context-limit resolution); this is a thin delegation kept for existing
// counsel callers.
func FetchModelContextLimits(ctx context.Context) (map[string]int, error) {
	return orregistry.Fetch(ctx)
}

// ResolveContextLength returns the context length (in tokens) for the given
// model ID. Resolution order:
//  1. knownModelContexts static table (instant, no network)
//  2. OpenRouter registry cache if already warm (no new network I/O)
//  3. fallbackContextLength (conservative default)
//
// For bare Anthropic model IDs (e.g. "claude-sonnet-4-6"), the
// "anthropic/<model>" prefix is tried automatically in each step.
//
// Oracle callers must not block on a cold-cache network fetch; this function
// only reads what is already available. To prime the cache for OpenRouter
// models, call FetchModelContextLimits explicitly at session start.
func ResolveContextLength(_ context.Context, modelID string) int {
	// 1. Static table — instant, covers all Anthropic and fusion models.
	if cl, ok := knownModelContexts[modelID]; ok {
		return cl
	}
	if !strings.Contains(modelID, "/") {
		if cl, ok := knownModelContexts["anthropic/"+modelID]; ok {
			return cl
		}
	}

	// 2. If the OpenRouter cache is warm, read it without triggering a fetch.
	if cl, ok := orregistry.Lookup(modelID); ok {
		return cl
	}
	if !strings.Contains(modelID, "/") {
		if cl, ok := orregistry.Lookup("anthropic/" + modelID); ok {
			return cl
		}
	}

	return fallbackContextLength
}

// ContextFit holds the result of fitting a request to a model's context window.
type ContextFit struct {
	ContextLength     int    // the model's context window (tokens); 0 if unknown
	InputEstimate     int    // estimated input tokens after fitting
	MaxTokens         int    // adjusted max_tokens (may be reduced from the input)
	Briefing          string // possibly truncated briefing (may be shorter than input)
	Adjusted          bool   // true if max_tokens or briefing was changed
	ReducedMaxTokens  bool   // true if max_tokens was reduced
	TruncatedBriefing bool   // true if briefing was truncated
	CannotFit         bool   // true if the request cannot fit even with max reduction
	Note              string // human-readable description of adjustments, "" if none
}

// FitToContext checks whether a request (systemPrompt + question + briefing +
// maxTokens) fits within the model's context window and adjusts if needed.
//
// Adjustment priority:
//  1. Reduce max_tokens (down to minOutputTokens) to make room for input.
//  2. If max_tokens is at the floor and still over, tail-truncate the briefing
//     (at a UTF-8 rune boundary) to fit within the remaining budget.
//  3. If the request still cannot fit (system+question alone overflows), sets
//     CannotFit=true.
//
// If contextLength is 0 (unknown), returns the inputs unchanged — the call
// proceeds as before, with no fitting.
//
// A safety margin (contextSafetyMargin) is applied to the context window so the
// estimate doesn't target the absolute ceiling — this accounts for tokenizer
// variance and protocol overhead.
func FitToContext(systemPrompt, question, briefing string, maxTokens, contextLength int) ContextFit {
	if contextLength <= 0 {
		return ContextFit{
			MaxTokens: maxTokens,
			Briefing:  briefing,
		}
	}

	// Apply safety margin to the effective context window.
	effectiveCtx := int(float64(contextLength) * contextSafetyMargin)

	// Guard: maxTokens must be positive.
	if maxTokens <= 0 {
		maxTokens = minOutputTokens
	}

	// Estimate input tokens. Include the "\n\nContext:\n" joiner that callers
	// prepend when assembling the user message.
	const joiner = "\n\nContext:\n"
	inputChars := len(systemPrompt) + len(question) + len(joiner) + len(briefing)
	inputEstimate := approxTokens(inputChars)

	if inputEstimate+maxTokens <= effectiveCtx {
		return ContextFit{
			ContextLength: contextLength,
			InputEstimate: inputEstimate,
			MaxTokens:     maxTokens,
			Briefing:      briefing,
		}
	}

	// Over budget — reduce max_tokens first.
	adjustedMax := effectiveCtx - inputEstimate
	if adjustedMax < minOutputTokens {
		adjustedMax = minOutputTokens
	}

	adjusted := false
	reducedMax := false
	truncatedBriefing := false
	var notes []string

	if adjustedMax < maxTokens {
		maxTokens = adjustedMax
		adjusted = true
		reducedMax = true
		notes = append(notes, fmt.Sprintf("reduced max_tokens to %d to fit context window (%d tokens, %.0f%% safety margin)",
			maxTokens, contextLength, contextSafetyMargin*100))
	}

	// Re-check with the adjusted max_tokens: if still over, truncate the briefing.
	inputEstimate = approxTokens(len(systemPrompt) + len(question) + len(joiner) + len(briefing))
	if inputEstimate+maxTokens > effectiveCtx {
		otherChars := len(systemPrompt) + len(question) + len(joiner)
		// maxBriefingChars = (effectiveCtx - maxTokens) * charsPerToken - otherChars
		maxBriefingChars := (effectiveCtx-maxTokens)*charsPerToken - otherChars

		const marker = "\n[briefing truncated to fit model context window]"
		// Budget for the marker so the result doesn't overflow.
		budget := maxBriefingChars - len(marker)
		if budget < 0 {
			budget = 0
		}

		if budget < len(briefing) {
			truncated := briefing[:budget]
			// Back up to a valid UTF-8 rune boundary so we don't split mid-rune.
			// DecodeLastRuneInString returns RuneError for incomplete sequences
			// at the end; trim until the last rune is complete. At most 3
			// iterations (max rune size is 4 bytes).
			for len(truncated) > 0 {
				r, size := utf8.DecodeLastRuneInString(truncated)
				if r != utf8.RuneError || size != 1 {
					break
				}
				truncated = truncated[:len(truncated)-1]
			}
			// Further back up to a newline if one is nearby (don't over-truncate).
			if nl := strings.LastIndexByte(truncated, '\n'); nl >= 0 && nl > len(truncated)*4/5 {
				truncated = truncated[:nl]
			}
			briefing = truncated + marker
			adjusted = true
			truncatedBriefing = true
			notes = append(notes, fmt.Sprintf("truncated briefing to fit context window (%d tokens)", contextLength))
			inputEstimate = approxTokens(len(systemPrompt) + len(question) + len(joiner) + len(briefing))
		}
	}

	// Final verification: if still over, signal CannotFit.
	cannotFit := false
	if inputEstimate+maxTokens > effectiveCtx {
		cannotFit = true
		notes = append(notes, "request still exceeds context window after all adjustments")
	}

	return ContextFit{
		ContextLength:     contextLength,
		InputEstimate:     inputEstimate,
		MaxTokens:         maxTokens,
		Briefing:          briefing,
		Adjusted:          adjusted,
		ReducedMaxTokens:  reducedMax,
		TruncatedBriefing: truncatedBriefing,
		CannotFit:         cannotFit,
		Note:              strings.Join(notes, "; "),
	}
}

// approxTokens delegates to proxy.ApproxTokens — the single source of truth for
// the char→token estimation. The previous local copy was duplicated "to avoid
// an import cycle" (per the old comment), but counsel does not import proxy
// and proxy does not import counsel, so no cycle exists. Returns int for
// local call-site convenience (proxy.ApproxTokens returns int64).
func approxTokens(chars int) int {
	return int(proxy.ApproxTokens(chars))
}

// ResetModelCache clears the shared model metadata cache. For testing only.
func ResetModelCache() {
	orregistry.ResetCache()
}
