package tui

import (
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"
)

// TestCacheStatsDisplayed verifies that cache-hit ratio appears in the cost
// segments when the backend reports cached tokens (card #189).
func TestCacheStatsDisplayed(t *testing.T) {
	costs := proxy.NewCostTracker()
	// Record an inference call with cached tokens.
	costs.Record(proxy.CostSourceInference, 1000, 50, 0.12, true, proxy.ConfModeled,
		config.TokenDetail{CachedTok: 800})

	f := &fakeFacade{sid: "sess_cache_test", chatID: "chat_cache"}
	f.info.Costs = costs
	m := tuiModel{facade: f}
	lines := m.costSegments()
	joined := strings.Join(lines, "\n")

	if !strings.Contains(joined, "cache") {
		t.Errorf("expected 'cache' segment in cost lines, got:\n%s", joined)
	}
	if !strings.Contains(joined, "80.0%") {
		t.Errorf("expected '80.0%%' cache ratio (800/1000), got:\n%s", joined)
	}
}

// TestCacheStatsNotDisplayedWhenZero verifies no cache segment when CachedTok is zero.
func TestCacheStatsNotDisplayedWhenZero(t *testing.T) {
	costs := proxy.NewCostTracker()
	costs.Record(proxy.CostSourceInference, 1000, 50, 0.12, true, proxy.ConfModeled)
	// No CachedTok → no cache line.

	f := &fakeFacade{sid: "sess_cache_test2", chatID: "chat_cache2"}
	f.info.Costs = costs
	m := tuiModel{facade: f}
	lines := m.costSegments()
	joined := strings.Join(lines, "\n")

	if strings.Contains(joined, "cache") {
		t.Errorf("did not expect 'cache' segment when no cached tokens, got:\n%s", joined)
	}
}

// TestCacheStatsMultipleRows verifies the ratio aggregates across multiple rows.
// Denominator is ALL InputTok (not just rows with cached tokens), so cache
// misses correctly lower the ratio.
func TestCacheStatsMultipleRows(t *testing.T) {
	costs := proxy.NewCostTracker()
	// Row 1: 800 cached / 1000 input
	costs.Record(proxy.CostSourceInference, 1000, 50, 0.10, true, proxy.ConfModeled,
		config.TokenDetail{CachedTok: 800})
	// Row 2: 200 cached / 1000 input
	costs.Record(proxy.CostSourceMashura, 1000, 20, 0.05, true, proxy.ConfExact,
		config.TokenDetail{CachedTok: 200})

	f := &fakeFacade{sid: "sess_cache_test3", chatID: "chat_cache3"}
	f.info.Costs = costs
	m := tuiModel{facade: f}
	lines := m.costSegments()
	joined := strings.Join(lines, "\n")

	// Total: 1000 cached / 2000 input = 50.0%
	if !strings.Contains(joined, "50.0%") {
		t.Errorf("expected '50.0%%' cache ratio (1000/2000), got:\n%s", joined)
	}
}

// TestFormatCacheStats verifies the helper directly.
func TestFormatCacheStats(t *testing.T) {
	rows := []proxy.CostRow{
		{CachedTok: 800, InputTok: 1000},
		{CachedTok: 200, InputTok: 1000},
	}
	got := formatCacheStats(rows)
	if got != "50.0%" {
		t.Errorf("expected '50.0%%', got %q", got)
	}
}

// TestFormatCacheStatsEmpty verifies nil rows returns empty.
func TestFormatCacheStatsEmpty(t *testing.T) {
	got := formatCacheStats(nil)
	if got != "" {
		t.Errorf("expected empty string for nil rows, got %q", got)
	}
}

// TestFormatCacheStatsNoCached verifies rows with no cached tokens returns empty.
func TestFormatCacheStatsNoCached(t *testing.T) {
	rows := []proxy.CostRow{
		{CachedTok: 0, InputTok: 1000},
		{CachedTok: 0, InputTok: 500},
	}
	got := formatCacheStats(rows)
	if got != "" {
		t.Errorf("expected empty string when no cached tokens, got %q", got)
	}
}

// TestFormatCacheStatsMixedRows verifies the denominator includes ALL rows
// (not just rows with cached tokens). This prevents the ratio from being
// biased upward when cache misses (CachedTok=0) are present.
func TestFormatCacheStatsMixedRows(t *testing.T) {
	rows := []proxy.CostRow{
		{CachedTok: 800, InputTok: 1000}, // 80% on this row
		{CachedTok: 0, InputTok: 1000},   // 0% on this row (cache miss)
	}
	got := formatCacheStats(rows)
	// 800 cached / 2000 total input = 40.0%, NOT 80.0%
	if got != "40.0%" {
		t.Errorf("expected '40.0%%' (800/2000 including cache-miss rows), got %q", got)
	}
}
