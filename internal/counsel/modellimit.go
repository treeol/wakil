package counsel

import (
	"context"
	"strings"

	"github.com/treeol/wakil/internal/orregistry"
)

// Context-limit awareness for counsel (mashūra) calls.
//
// FetchModelContextLimits and ResolveContextLength supply the model context
// window used by the whole-or-fail reviewer dispatcher (review.go), which sizes
// every request against it and either sends the document whole, in labelled
// parts, or fails loudly. Nothing here truncates content. A per-model
// override (config mashura_models.context_tokens) wins over this table.

const (
	// fallbackContextLength is the last-resort assumption when a model's context
	// length cannot be determined from any source. Deliberately conservative.
	fallbackContextLength = 128_000
)

// knownModelContexts is a small fallback table for common models when the
// OpenRouter registry is unreachable or doesn't list the model. This covers
// Anthropic direct calls (which don't go through OpenRouter's catalog).
//
// Values are in tokens. Update when new model generations ship.
var knownModelContexts = map[string]int{
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
	// 1. Static table — instant, covers all Anthropic models.
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

// ResetModelCache clears the shared model metadata cache. For testing only.
func ResetModelCache() {
	orregistry.ResetCache()
}
