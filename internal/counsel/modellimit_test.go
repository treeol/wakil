package counsel

import (
	"context"
	"strings"
	"testing"
	"time"
)

// populateCache fills the shared cache with test data (avoids network dependency).
func populateCache(t *testing.T, entries map[string]int) {
	t.Helper()
	ResetModelCache()
	sharedModelCache.mu.Lock()
	sharedModelCache.entries = entries
	sharedModelCache.fetched = time.Now().Add(time.Hour) // far future = fresh
	sharedModelCache.mu.Unlock()
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

// TestFitToContextNoAdjustment verifies that a request that fits within the
// model's context window is returned unchanged.
func TestFitToContextNoAdjustment(t *testing.T) {
	sys := "system prompt"
	q := "question"
	briefing := strings.Repeat("x", 100_000) // ~25K tokens
	fit := FitToContext(sys, q, briefing, 4096, 1_000_000)

	if fit.Adjusted {
		t.Error("should not be adjusted — fits within 1M window")
	}
	if fit.MaxTokens != 4096 {
		t.Errorf("max_tokens = %d, want 4096 (unchanged)", fit.MaxTokens)
	}
	if fit.Briefing != briefing {
		t.Error("briefing should be unchanged")
	}
	if fit.Note != "" {
		t.Errorf("note should be empty, got %q", fit.Note)
	}
	if fit.CannotFit {
		t.Error("CannotFit should be false")
	}
}

// TestFitToContextReducesMaxTokens verifies max_tokens is reduced when the
// request would overflow but the briefing is small enough to leave room.
func TestFitToContextReducesMaxTokens(t *testing.T) {
	sys := "sys"
	q := "q"
	briefing := strings.Repeat("x", 100_000) // ~25K tokens
	fit := FitToContext(sys, q, briefing, 8192, 28_000)

	if !fit.Adjusted {
		t.Error("should be adjusted — 25K + 8192 > 28K")
	}
	if !fit.ReducedMaxTokens {
		t.Error("ReducedMaxTokens should be true")
	}
	if fit.MaxTokens >= 8192 {
		t.Errorf("max_tokens should be reduced below 8192, got %d", fit.MaxTokens)
	}
	if fit.MaxTokens < minOutputTokens {
		t.Errorf("max_tokens should not go below minOutputTokens (%d), got %d", minOutputTokens, fit.MaxTokens)
	}
	if !strings.Contains(fit.Note, "reduced max_tokens") {
		t.Errorf("note should mention max_tokens reduction, got %q", fit.Note)
	}
}

// TestFitToContextTruncatesBriefing verifies that when reducing max_tokens to
// the floor is not enough, the briefing is truncated.
func TestFitToContextTruncatesBriefing(t *testing.T) {
	sys := "sys"
	q := "q"
	briefing := strings.Repeat("x", 200_000) // ~50K tokens
	fit := FitToContext(sys, q, briefing, 4096, 10_000)

	if !fit.Adjusted {
		t.Error("should be adjusted")
	}
	if fit.MaxTokens != minOutputTokens {
		t.Errorf("max_tokens should be at floor %d, got %d", minOutputTokens, fit.MaxTokens)
	}
	if len(fit.Briefing) >= len(briefing) {
		t.Error("briefing should be truncated")
	}
	if !strings.Contains(fit.Briefing, "[briefing truncated to fit model context window]") {
		t.Error("briefing should have truncation marker")
	}
	if !fit.TruncatedBriefing {
		t.Error("TruncatedBriefing should be true")
	}
	if !strings.Contains(fit.Note, "truncated briefing") {
		t.Errorf("note should mention briefing truncation, got %q", fit.Note)
	}
}

// TestFitToContextUnknownModel verifies that contextLength=0 returns inputs
// unchanged with no adjustment.
func TestFitToContextUnknownModel(t *testing.T) {
	briefing := strings.Repeat("x", 500_000)
	fit := FitToContext("sys", "q", briefing, 4096, 0)

	if fit.Adjusted {
		t.Error("should not be adjusted when context length is unknown")
	}
	if fit.MaxTokens != 4096 {
		t.Errorf("max_tokens = %d, want 4096", fit.MaxTokens)
	}
	if fit.Briefing != briefing {
		t.Error("briefing should be unchanged")
	}
	if fit.CannotFit {
		t.Error("CannotFit should be false when context is unknown")
	}
}

// TestFitToContextExactFit verifies behavior at the exact boundary.
func TestFitToContextExactFit(t *testing.T) {
	sys := "sys"
	q := "q"
	briefing := "briefing"
	// Total input chars = 3 + 1 + 13(joiner) + 8 = 25 → ~7 tokens.
	// effectiveCtx = 10 * 0.9 = 9; 7 + 2 = 9 → exact fit.
	fit := FitToContext(sys, q, briefing, 2, 10)

	if fit.Adjusted {
		t.Error("should not be adjusted at exact fit")
	}
	if fit.MaxTokens != 2 {
		t.Errorf("max_tokens = %d, want 2", fit.MaxTokens)
	}
}

// TestFitToContextCannotFit verifies CannotFit is set when system+question alone
// exceed the context window.
func TestFitToContextCannotFit(t *testing.T) {
	sys := strings.Repeat("s", 50_000) // ~12.5K tokens
	q := strings.Repeat("q", 50_000)   // ~12.5K tokens
	briefing := ""
	// effectiveCtx = 10K * 0.9 = 9K; sys+q alone = ~25K tokens → way over.
	fit := FitToContext(sys, q, briefing, 4096, 10_000)

	if !fit.CannotFit {
		t.Error("CannotFit should be true — system+question alone overflow")
	}
	if !fit.Adjusted {
		t.Error("Adjusted should be true (max_tokens reduced)")
	}
	if !strings.Contains(fit.Note, "still exceeds") {
		t.Errorf("note should mention still exceeds, got %q", fit.Note)
	}
}

// TestFitToContextZeroMaxTokens verifies that maxTokens=0 is clamped to
// minOutputTokens.
func TestFitToContextZeroMaxTokens(t *testing.T) {
	fit := FitToContext("sys", "q", "brief", 0, 100_000)
	if fit.MaxTokens != minOutputTokens {
		t.Errorf("max_tokens = %d, want %d (clamped from 0)", fit.MaxTokens, minOutputTokens)
	}
}

// TestFitToContextNegativeMaxTokens verifies negative maxTokens is clamped.
func TestFitToContextNegativeMaxTokens(t *testing.T) {
	fit := FitToContext("sys", "q", "brief", -5, 100_000)
	if fit.MaxTokens != minOutputTokens {
		t.Errorf("max_tokens = %d, want %d (clamped from -5)", fit.MaxTokens, minOutputTokens)
	}
}

// TestFitToContextUTF8Safe verifies that truncation doesn't split a multibyte
// UTF-8 rune.
func TestFitToContextUTF8Safe(t *testing.T) {
	// Briefing full of 3-byte runes (é = 0xC3 0xA9 in UTF-8).
	briefing := strings.Repeat("é", 10_000) // 20_000 bytes
	fit := FitToContext("sys", "q", briefing, 4096, 2_000)

	if !fit.TruncatedBriefing {
		t.Fatal("briefing should be truncated")
	}
	// The truncated briefing must be valid UTF-8 (excluding the marker, which is ASCII).
	truncatedPart := strings.TrimSuffix(fit.Briefing, "\n[briefing truncated to fit model context window]")
	if !isValidUTF8(truncatedPart) {
		t.Error("truncated briefing contains invalid UTF-8 (split rune)")
	}
}

// isValidUTF8 checks if s is valid UTF-8. (testing/utf8 isn't imported in tests
// so we do a simple check.)
func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == 0xFFFD {
			return false
		}
	}
	return true
}

// TestFitToContextMarkerBudgeted verifies that the truncation marker is counted
// against the briefing budget (the result should fit within the context window).
func TestFitToContextMarkerBudgeted(t *testing.T) {
	briefing := strings.Repeat("x", 100_000) // ~25K tokens
	fit := FitToContext("sys", "q", briefing, 4096, 8_000)

	if !fit.CannotFit {
		// Even with adjustments, verify the result doesn't claim to fit when it doesn't.
		// If not CannotFit, verify the post-condition holds.
		if fit.InputEstimate+fit.MaxTokens > int(float64(8_000)*contextSafetyMargin) {
			t.Errorf("fit claims to fit but InputEstimate(%d) + MaxTokens(%d) > effectiveCtx(%d)",
				fit.InputEstimate, fit.MaxTokens, int(float64(8_000)*contextSafetyMargin))
		}
	}
}

// TestApproxTokensConsistencyWithProxy verifies the local approxTokens matches
// the proxy package's 4-chars-per-token convention.
func TestApproxTokensConsistencyWithProxy(t *testing.T) {
	cases := []struct {
		chars int
		want  int
	}{
		{0, 0},
		{1, 1},
		{4, 1},
		{5, 2},
		{100, 25},
		{1000, 250},
	}
	for _, c := range cases {
		got := approxTokens(c.chars)
		if got != c.want {
			t.Errorf("approxTokens(%d) = %d, want %d", c.chars, got, c.want)
		}
	}
}
