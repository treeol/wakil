package agent

import (
	"math"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/orregistry"
	"github.com/treeol/wakil/internal/proxy"
)

func approxEq(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// TestRecordInferenceCost_FetchedOpenRouterPricing verifies that an external
// OpenRouter call with NO config override is priced from the OpenRouter
// registry (via a seeded test cache), including cache-read/write split rates.
func TestRecordInferenceCost_FetchedOpenRouterPricing(t *testing.T) {
	orregistry.ResetCache()
	orregistry.SetPricingForTest(map[string]orregistry.Pricing{
		"anthropic/claude-opus-4-8": {
			Prompt: 3.0, Completion: 15.0, CachedInput: 0.3, CacheWrite: 3.75, Valid: true,
		},
	})
	defer orregistry.ResetCache()

	app := &App{
		Cfg: config.DefaultConfig(), // no [costs] override
		Client: &proxy.Client{Model: "anthropic/claude-opus-4-8"},
		Costs:  proxy.NewCostTracker(),
		BackendList: []BackendInfo{{Name: "openrouter", External: true}},
	}
	app.Client.SetUsage(proxy.UsageStat{
		InputTok: 1_000_000, OutputTok: 100_000, CachedTok: 400_000, CacheWriteTok: 200_000, Exact: true,
	})
	app.Client.SetLastUsedBackend("openrouter")
	app.RecordInferenceCost()

	_, rows := app.Costs.Snapshot()
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d: %+v", len(rows), rows)
	}
	row := rows[0]
	// 600k/1e6*3 + 400k/1e6*0.3 + 200k/1e6*3.75 + 100k/1e6*15
	// = 1.8 + 0.12 + 0.75 + 1.5 = 4.17
	if !approxEq(row.CostUSD, 4.17) {
		t.Errorf("CostUSD = %v, want 4.17 (fetched split-rate)", row.CostUSD)
	}
	if !row.Priced {
		t.Error("fetched pricing must set Priced=true")
	}
	if row.Confidence != proxy.ConfExact {
		t.Errorf("Confidence = %q, want %q", row.Confidence, proxy.ConfExact)
	}
}

// TestRecordInferenceCost_ConfigOverridesFetched verifies a configured
// [costs].inference_backends rate wins over registry pricing.
func TestRecordInferenceCost_ConfigOverridesFetched(t *testing.T) {
	orregistry.ResetCache()
	orregistry.SetPricingForTest(map[string]orregistry.Pricing{
		"anthropic/claude-opus-4-8": {Prompt: 3.0, Completion: 15.0, Valid: true},
	})
	defer orregistry.ResetCache()

	app := &App{
		Cfg: config.Config{
			Costs: config.CostsConfig{
				InferenceBackends: map[string]config.ModelRate{
					"openrouter/anthropic/claude-opus-4-8": {InputUSDPer1M: 99, OutputUSDPer1M: 99},
				},
			},
		},
		Client: &proxy.Client{Model: "anthropic/claude-opus-4-8"},
		Costs:  proxy.NewCostTracker(),
		BackendList: []BackendInfo{{Name: "openrouter", External: true}},
	}
	app.Client.SetUsage(proxy.UsageStat{InputTok: 1_000_000, OutputTok: 0, Exact: true})
	app.Client.SetLastUsedBackend("openrouter")
	app.RecordInferenceCost()

	_, rows := app.Costs.Snapshot()
	if rows[0].CostUSD != 99.0 {
		t.Errorf("CostUSD = %v, want 99.0 (config override wins)", rows[0].CostUSD)
	}
}

// TestRecordInferenceCost_NonOpenRouterNoRegistry verifies a non-OpenRouter
// external backend never picks up registry pricing.
func TestRecordInferenceCost_NonOpenRouterNoRegistry(t *testing.T) {
	orregistry.ResetCache()
	orregistry.SetPricingForTest(map[string]orregistry.Pricing{
		"anthropic/claude-opus-4-8": {Prompt: 3.0, Completion: 15.0, Valid: true},
	})
	defer orregistry.ResetCache()

	app := &App{
		Cfg: config.DefaultConfig(),
		Client: &proxy.Client{Model: "anthropic/claude-opus-4-8"},
		Costs:  proxy.NewCostTracker(),
		BackendList: []BackendInfo{{Name: "groq", External: true}},
	}
	app.Client.SetUsage(proxy.UsageStat{InputTok: 1_000_000, OutputTok: 0, Exact: true})
	app.Client.SetLastUsedBackend("groq")
	app.RecordInferenceCost()

	_, rows := app.Costs.Snapshot()
	if rows[0].Priced {
		t.Error("non-OpenRouter backend must not be priced from the registry")
	}
}

// TestRecordInferenceCost_DirectOpenRouterEndpoint verifies a direct
// openrouter.ai endpoint (usedBackend empty) still gets registry pricing via
// the endpoint host check.
func TestRecordInferenceCost_DirectOpenRouterEndpoint(t *testing.T) {
	orregistry.ResetCache()
	orregistry.SetPricingForTest(map[string]orregistry.Pricing{
		"anthropic/claude-opus-4-8": {Prompt: 3.0, Completion: 15.0, Valid: true},
	})
	defer orregistry.ResetCache()

	cfg := config.DefaultConfig()
	cfg.Endpoint = config.EndpointConfig{
		Kind: config.EndpointKindOpenAI, BaseURL: "https://openrouter.ai/api", Model: "anthropic/claude-opus-4-8",
	}

	app := &App{
		Cfg: cfg,
		Client: &proxy.Client{Model: "anthropic/claude-opus-4-8"},
		Costs:  proxy.NewCostTracker(),
	}
	app.Client.SetUsage(proxy.UsageStat{InputTok: 1_000_000, OutputTok: 0, Exact: true})
	// No SetLastUsedBackend → usedBackend == "" (direct endpoint)
	app.RecordInferenceCost()

	_, rows := app.Costs.Snapshot()
	if !rows[0].Priced || rows[0].CostUSD != 3.0 {
		t.Errorf("direct OR endpoint: Priced=%v CostUSD=%v, want Priced=true 3.0", rows[0].Priced, rows[0].CostUSD)
	}
}

// TestRecordInferenceCost_FreeModelRendersZero verifies a free OpenRouter model
// (rate "0") is priced at $0.00, not "—".
func TestRecordInferenceCost_FreeModelRendersZero(t *testing.T) {
	orregistry.ResetCache()
	orregistry.SetPricingForTest(map[string]orregistry.Pricing{
		"dots-studio/dots-3-note-preview:free": {Prompt: 0, Completion: 0, Valid: true},
	})
	defer orregistry.ResetCache()

	app := &App{
		Cfg: config.DefaultConfig(),
		Client: &proxy.Client{Model: "dots-studio/dots-3-note-preview:free"},
		Costs:  proxy.NewCostTracker(),
		BackendList: []BackendInfo{{Name: "openrouter", External: true}},
	}
	app.Client.SetUsage(proxy.UsageStat{InputTok: 1_000_000, OutputTok: 0, Exact: true})
	app.Client.SetLastUsedBackend("openrouter")
	app.RecordInferenceCost()

	_, rows := app.Costs.Snapshot()
	if !rows[0].Priced {
		t.Error("free model must be Priced=true (renders $0.00, not —)")
	}
	if rows[0].CostUSD != 0 {
		t.Errorf("CostUSD = %v, want 0", rows[0].CostUSD)
	}
}

// TestRecordInferenceCost_UnknownModelUnpriced verifies an unknown/aliased
// model falls through to unpriced rather than another model's rate.
func TestRecordInferenceCost_UnknownModelUnpriced(t *testing.T) {
	orregistry.ResetCache()
	orregistry.SetPricingForTest(map[string]orregistry.Pricing{
		"anthropic/claude-opus-4-8": {Prompt: 3.0, Completion: 15.0, Valid: true},
	})
	defer orregistry.ResetCache()

	app := &App{
		Cfg: config.DefaultConfig(),
		Client: &proxy.Client{Model: "some/unknown-model"},
		Costs:  proxy.NewCostTracker(),
		BackendList: []BackendInfo{{Name: "openrouter", External: true}},
	}
	app.Client.SetUsage(proxy.UsageStat{InputTok: 1_000_000, OutputTok: 0, Exact: true})
	app.Client.SetLastUsedBackend("openrouter")
	app.RecordInferenceCost()

	_, rows := app.Costs.Snapshot()
	if rows[0].Priced {
		t.Error("unknown model must stay unpriced")
	}
}

// TestExternalInferenceCost_PrefixStripping verifies a model string already
// carrying the "openrouter/" prefix is stripped before registry lookup.
func TestExternalInferenceCost_PrefixStripping(t *testing.T) {
	orregistry.ResetCache()
	orregistry.SetPricingForTest(map[string]orregistry.Pricing{
		"anthropic/claude-opus-4-8": {Prompt: 3.0, Completion: 15.0, Valid: true},
	})
	defer orregistry.ResetCache()

	app := &App{
		Cfg: config.DefaultConfig(),
		Client: &proxy.Client{Model: "openrouter/anthropic/claude-opus-4-8"},
		Costs:  proxy.NewCostTracker(),
		BackendList: []BackendInfo{{Name: "openrouter", External: true}},
	}
	app.Client.SetUsage(proxy.UsageStat{InputTok: 1_000_000, OutputTok: 0, Exact: true})
	app.Client.SetLastUsedBackend("openrouter")
	app.RecordInferenceCost()

	_, rows := app.Costs.Snapshot()
	if !rows[0].Priced || rows[0].CostUSD != 3.0 {
		t.Errorf("prefix-stripped lookup: Priced=%v CostUSD=%v, want Priced=true 3.0", rows[0].Priced, rows[0].CostUSD)
	}
}

// TestRecordInferenceCost_PrefixedModelConfigOverride verifies that a prefixed
// model string does NOT bypass a config override — the override still wins.
func TestRecordInferenceCost_PrefixedModelConfigOverride(t *testing.T) {
	orregistry.ResetCache()
	orregistry.SetPricingForTest(map[string]orregistry.Pricing{
		"anthropic/claude-opus-4-8": {Prompt: 3.0, Completion: 15.0, Valid: true},
	})
	defer orregistry.ResetCache()

	app := &App{
		Cfg: config.Config{
			Costs: config.CostsConfig{
				InferenceBackends: map[string]config.ModelRate{
					"openrouter/anthropic/claude-opus-4-8": {InputUSDPer1M: 99, OutputUSDPer1M: 99},
				},
			},
		},
		Client: &proxy.Client{Model: "openrouter/anthropic/claude-opus-4-8"},
		Costs:  proxy.NewCostTracker(),
		BackendList: []BackendInfo{{Name: "openrouter", External: true}},
	}
	app.Client.SetUsage(proxy.UsageStat{InputTok: 1_000_000, OutputTok: 0, Exact: true})
	app.Client.SetLastUsedBackend("openrouter")
	app.RecordInferenceCost()

	_, rows := app.Costs.Snapshot()
	if rows[0].CostUSD != 99.0 {
		t.Errorf("CostUSD = %v, want 99.0 (config override must win even with prefixed model)", rows[0].CostUSD)
	}
}

// TestRecordInferenceCost_DirectNonOREndpointUnpriced verifies a direct
// NON-OpenRouter endpoint with empty usedBackend stays unpriced (the host
// check is the only gate and must not false-positive).
func TestRecordInferenceCost_DirectNonOREndpointUnpriced(t *testing.T) {
	orregistry.ResetCache()
	orregistry.SetPricingForTest(map[string]orregistry.Pricing{
		"anthropic/claude-opus-4-8": {Prompt: 3.0, Completion: 15.0, Valid: true},
	})
	defer orregistry.ResetCache()

	cfg := config.DefaultConfig()
	cfg.Endpoint = config.EndpointConfig{
		Kind: config.EndpointKindOpenAI, BaseURL: "http://llama-host:11400", Model: "qwen3.6-35b",
	}

	app := &App{
		Cfg: cfg,
		Client: &proxy.Client{Model: "qwen3.6-35b"},
		Costs:  proxy.NewCostTracker(),
	}
	app.Client.SetUsage(proxy.UsageStat{InputTok: 1_000_000, OutputTok: 0, Exact: true})
	app.RecordInferenceCost()

	_, rows := app.Costs.Snapshot()
	if rows[0].Priced {
		t.Error("direct non-OpenRouter endpoint must stay unpriced")
	}
}

// TestRecordInferenceCost_CacheFallbackToBaseInput verifies that fetched pricing
// with NO cache-discount rate bills cached tokens at the base input rate (the
// absent-rate fallback), not $0 or a skipped count.
func TestRecordInferenceCost_CacheFallbackToBaseInput(t *testing.T) {
	orregistry.ResetCache()
	orregistry.SetPricingForTest(map[string]orregistry.Pricing{
		"anthropic/claude-opus-4-8": {Prompt: 3.0, Completion: 15.0, Valid: true}, // no cache rates
	})
	defer orregistry.ResetCache()

	app := &App{
		Cfg: config.DefaultConfig(),
		Client: &proxy.Client{Model: "anthropic/claude-opus-4-8"},
		Costs:  proxy.NewCostTracker(),
		BackendList: []BackendInfo{{Name: "openrouter", External: true}},
	}
	// 1M input (400k cached) + 0 output → all input billed at base 3.0.
	app.Client.SetUsage(proxy.UsageStat{InputTok: 1_000_000, OutputTok: 0, CachedTok: 400_000, Exact: true})
	app.Client.SetLastUsedBackend("openrouter")
	app.RecordInferenceCost()

	_, rows := app.Costs.Snapshot()
	if !approxEq(rows[0].CostUSD, 3.0) {
		t.Errorf("CostUSD = %v, want 3.0 (cached tokens billed at base input when no cache rate)", rows[0].CostUSD)
	}
}
