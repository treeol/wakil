package counsel

import "testing"

// TestAddProviderCostSumsAttempts: VOID-retry and debate usage merges must sum
// provider-reported dollars, not just tokens.
func TestAddProviderCostSumsAttempts(t *testing.T) {
	a := OracleUsage{InputTokens: 10, ProviderCostUSD: 0.10, HasProviderCost: true}
	b := OracleUsage{InputTokens: 5, ProviderCostUSD: 0.05, HasProviderCost: true}
	got := addProviderCost(a, b)
	if d := got.ProviderCostUSD - 0.15; d > 1e-12 || d < -1e-12 || !got.HasProviderCost {
		t.Fatalf("addProviderCost = %+v, want cost 0.15 with HasProviderCost", got)
	}
	none := addProviderCost(OracleUsage{}, OracleUsage{InputTokens: 3})
	if none.HasProviderCost {
		t.Fatalf("no provider cost on either side must stay absent: %+v", none)
	}
}

// TestAddProviderCostKeepsServedModel: a VOID-retry or debate merge must keep
// the served model so the retry's dollars are still keyed to the model that ran.
func TestAddProviderCostKeepsServedModel(t *testing.T) {
	a := OracleUsage{InputTokens: 10, ProviderCostUSD: 0.10, HasProviderCost: true}
	b := OracleUsage{InputTokens: 12, ProviderCostUSD: 0.05, HasProviderCost: true, ServedModel: "a/m"}
	got := addProviderCost(a, b)
	if got.ServedModel != "a/m" {
		t.Errorf("served model after merge = %q, want a/m", got.ServedModel)
	}
	if d := got.ProviderCostUSD - 0.15; d > 1e-9 || d < -1e-9 {
		t.Errorf("summed cost = %v, want 0.15 (within float tolerance)", got.ProviderCostUSD)
	}
}
