package agent

import (
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"
)

// TestRecordInferenceCostCarriesCachedTokens is the end-to-end proof for the
// UsageStat.CachedTok → RecordInferenceCost → costEntry chain: an external
// backend's usage report with cached_tokens must both (a) show up as
// CostRow.CachedTok and (b) be priced at the configured cached rate rather
// than the full input rate.
func TestRecordInferenceCostCarriesCachedTokens(t *testing.T) {
	app := &App{
		Cfg: config.Config{
			Costs: config.CostsConfig{
				InferenceBackends: map[string]config.ModelRate{
					"openrouter/openai/gpt-4o": {InputUSDPer1M: 10, OutputUSDPer1M: 30, CachedInputUSDPer1M: 2},
				},
			},
		},
		Client: &proxy.Client{Model: "openai/gpt-4o"},
		Costs:  proxy.NewCostTracker(),
		BackendList: []BackendInfo{
			{Name: "openrouter", External: true},
		},
	}

	app.Client.SetUsage(proxy.UsageStat{InputTok: 1_000_000, OutputTok: 100_000, CachedTok: 400_000, Exact: true})
	app.Client.SetLastUsedBackend("openrouter")
	app.RecordInferenceCost()

	_, rows := app.Costs.Snapshot()
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d: %+v", len(rows), rows)
	}
	row := rows[0]
	if row.CachedTok != 400_000 {
		t.Errorf("CachedTok = %d, want 400000", row.CachedTok)
	}
	// 600k/1e6*10 + 400k/1e6*2 + 100k/1e6*30 = 6 + 0.8 + 3 = 9.8
	if row.CostUSD != 9.8 {
		t.Errorf("CostUSD = %v, want 9.8 (split-rate arithmetic)", row.CostUSD)
	}
	if !row.Priced {
		t.Error("expected Priced=true")
	}
}

// TestRecordInferenceCostCachedTokensGoldenWithoutRate verifies that when no
// CachedInputUSDPer1M is configured, RecordInferenceCost's cost output is
// byte-identical to what it computed before cached-token accounting existed,
// even though the usage report carries a nonzero CachedTok — the "unconfigured
// stays unchanged" guarantee at the App layer, not just the config layer.
func TestRecordInferenceCostCachedTokensGoldenWithoutRate(t *testing.T) {
	cfg := config.Config{
		Costs: config.CostsConfig{
			InferenceBackends: map[string]config.ModelRate{
				"openrouter/openai/gpt-4o": {InputUSDPer1M: 10, OutputUSDPer1M: 30},
			},
		},
	}

	withoutCache := &App{Cfg: cfg, Client: &proxy.Client{Model: "openai/gpt-4o"}, Costs: proxy.NewCostTracker(),
		BackendList: []BackendInfo{{Name: "openrouter", External: true}}}
	withoutCache.Client.SetUsage(proxy.UsageStat{InputTok: 1_000_000, OutputTok: 100_000, Exact: true})
	withoutCache.Client.SetLastUsedBackend("openrouter")
	withoutCache.RecordInferenceCost()

	withCache := &App{Cfg: cfg, Client: &proxy.Client{Model: "openai/gpt-4o"}, Costs: proxy.NewCostTracker(),
		BackendList: []BackendInfo{{Name: "openrouter", External: true}}}
	withCache.Client.SetUsage(proxy.UsageStat{InputTok: 1_000_000, OutputTok: 100_000, CachedTok: 400_000, Exact: true})
	withCache.Client.SetLastUsedBackend("openrouter")
	withCache.RecordInferenceCost()

	_, rowsA := withoutCache.Costs.Snapshot()
	_, rowsB := withCache.Costs.Snapshot()
	if rowsA[0].CostUSD != rowsB[0].CostUSD {
		t.Errorf("cost with CachedTok=400000 (no cached rate configured) = %v, want %v (golden-identical to the no-cache usage report)",
			rowsB[0].CostUSD, rowsA[0].CostUSD)
	}
}
