package counsel

import (
	"context"
	"testing"

	"github.com/treeol/wakil/internal/orregistry"
)

// populateCache fills the shared cache with test data (avoids network dependency).
func populateCache(t *testing.T, entries map[string]int) {
	t.Helper()
	ResetModelCache()
	orregistry.SetCacheForTest(entries)
}

// TestFetchModelContextLimits verifies that cached entries are returned and
// the caller gets a defensive copy (mutations don't affect the cache).
func TestFetchModelContextLimits(t *testing.T) {
	original := map[string]int{
		"openrouter/fusion":           1_000_000,
		"anthropic/claude-sonnet-4-6": 200_000,
		"google/gemini-2.5-pro":       2_000_000,
	}
	populateCache(t, original)
	defer ResetModelCache()

	limits, err := FetchModelContextLimits(context.Background())
	if err != nil {
		t.Fatalf("FetchModelContextLimits: %v", err)
	}
	if limits["openrouter/fusion"] != 1_000_000 {
		t.Errorf("fusion context_length = %d, want 1000000", limits["openrouter/fusion"])
	}
	if limits["anthropic/claude-sonnet-4-6"] != 200_000 {
		t.Errorf("sonnet context_length = %d, want 200000", limits["anthropic/claude-sonnet-4-6"])
	}

	// Verify the returned map is a copy: mutating it must not affect the cache.
	limits["openrouter/fusion"] = 999
	limits2, _ := FetchModelContextLimits(context.Background())
	if limits2["openrouter/fusion"] != 1_000_000 {
		t.Error("caller mutated the cached map — FetchModelContextLimits must return a copy")
	}
}

// TestResolveContextLength verifies resolution with prefix fallback for bare
// Anthropic model IDs, and the known-model fallback table.
func TestResolveContextLength(t *testing.T) {
	populateCache(t, map[string]int{
		"openrouter/fusion":           1_000_000,
		"anthropic/claude-sonnet-4-6": 200_000,
	})
	defer ResetModelCache()

	ctx := context.Background()

	// Direct lookup.
	if cl := ResolveContextLength(ctx, "openrouter/fusion"); cl != 1_000_000 {
		t.Errorf("fusion: got %d, want 1000000", cl)
	}

	// Bare Anthropic model ID → tries "anthropic/<model>" prefix.
	if cl := ResolveContextLength(ctx, "claude-sonnet-4-6"); cl != 200_000 {
		t.Errorf("bare sonnet: got %d, want 200000", cl)
	}
}

// TestResolveContextLengthKnownTableFallback verifies the known-model table is
// used when the OpenRouter registry doesn't have the model.
func TestResolveContextLengthKnownTableFallback(t *testing.T) {
	// Cache has no entry for claude-opus-4-8.
	populateCache(t, map[string]int{
		"openrouter/fusion": 1_000_000,
	})
	defer ResetModelCache()

	// Should find it in knownModelContexts.
	cl := ResolveContextLength(context.Background(), "anthropic/claude-opus-4-8")
	if cl != 200_000 {
		t.Errorf("known table fallback: got %d, want 200000", cl)
	}
}

// TestResolveContextLengthBareKnownTable verifies bare ID lookup in the known
// table.
func TestResolveContextLengthBareKnownTable(t *testing.T) {
	populateCache(t, map[string]int{})
	defer ResetModelCache()

	cl := ResolveContextLength(context.Background(), "claude-opus-4-8")
	if cl != 200_000 {
		t.Errorf("bare known table: got %d, want 200000", cl)
	}
}

// TestResolveContextLengthUnknownModel verifies that a truly unknown model gets
// the conservative fallback.
func TestResolveContextLengthUnknownModel(t *testing.T) {
	populateCache(t, map[string]int{})
	defer ResetModelCache()

	cl := ResolveContextLength(context.Background(), "unknown/model-xyz")
	if cl != fallbackContextLength {
		t.Errorf("unknown: got %d, want %d", cl, fallbackContextLength)
	}
}

// TestResolveContextLengthFallbackOnEmpty verifies that an empty cache and a
// failed fetch (simulated by not populating) still returns a positive value.
func TestResolveContextLengthFallbackOnEmpty(t *testing.T) {
	ResetModelCache()
	defer ResetModelCache()

	cl := ResolveContextLength(context.Background(), "openrouter/fusion")
	// Should find fusion in knownModelContexts even without a live fetch.
	if cl != 1_000_000 {
		t.Errorf("fusion via known table: got %d, want 1000000", cl)
	}
}
