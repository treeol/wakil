package main

import (
	"regexp"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/tools"
)

func plain(s string) string {
	return regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`).ReplaceAllString(s, "")
}

func TestSearchCostPricing(t *testing.T) {
	// Unpriced by default.
	if _, priced := (config.CostsConfig{}).SearchCost(); priced {
		t.Error("default search cost should be unpriced")
	}
	// Priced when a per-query rate is set.
	c := config.CostsConfig{Search: config.SearchRate{USDPerQuery: 0.002}}
	usd, priced := c.SearchCost()
	if !priced || usd != 0.002 {
		t.Errorf("searchCost = (%v,%v), want (0.002,true)", usd, priced)
	}
}

func TestRecordSearchCost(t *testing.T) {
	app := &agent.App{Cfg: config.DefaultConfig(), Costs: proxy.NewCostTracker()}
	app.Cfg.Costs.Search = config.SearchRate{USDPerQuery: 0.01}
	app.RecordSearchCost()
	app.RecordSearchCost()

	total, rows := app.Costs.Snapshot()
	var search *proxy.CostRow
	for i := range rows {
		if rows[i].Source == proxy.CostSourceSearch {
			search = &rows[i]
		}
	}
	if search == nil || search.Calls != 2 {
		t.Fatalf("expected 2 recorded search calls; rows=%+v", rows)
	}
	if total < 0.0199 || total > 0.0201 {
		t.Errorf("priced total = %v, want ~0.02", total)
	}

	// nil tracker is a no-op (must not panic).
	(&agent.App{Cfg: config.DefaultConfig()}).RecordSearchCost()
}

func TestChipsLine(t *testing.T) {
	out := plain(tools.ChipsLine([]tools.MentionRef{
		{Token: "src/main.go", Ok: true, Note: "2.0 KB"},
		{Token: "missing.go", Ok: false, Note: "not found"},
	}))
	if !strings.Contains(out, "📎 src/main.go (2.0 KB)") {
		t.Errorf("resolved chip missing; got %q", out)
	}
	if !strings.Contains(out, "⚠ missing.go (not found)") {
		t.Errorf("failed chip should use the warning icon; got %q", out)
	}
}

func TestColorHelpersWrapAndReset(t *testing.T) {
	// Only the raw ANSI wrap() helpers are deterministic in a non-TTY test env;
	// dim2 goes through lipgloss, which may emit no codes when color is disabled.
	for _, fn := range []func(string) string{agent.Bold, agent.Red, agent.Dim, agent.Yellow} {
		got := fn("x")
		if !strings.Contains(got, "x") || !strings.HasSuffix(got, "\x1b[0m") {
			t.Errorf("color helper output %q should wrap content and reset", got)
		}
		if plain(got) != "x" {
			t.Errorf("stripping ANSI from %q should yield the bare content", got)
		}
	}
}

func TestHumanSize(t *testing.T) {
	cases := map[int64]string{
		512:           "512 B",
		2048:          "2.0 KB",
		3 * (1 << 20): "3.0 MB",
	}
	for n, want := range cases {
		if got := tools.HumanSize(n); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", n, got, want)
		}
	}
}
