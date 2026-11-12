package counsel

import "testing"

// TestParseOpenRouterSSECost verifies usage.cost is carried through the
// OpenRouter SSE parse (B3, counsel path). Both panel (callOpenRouter) and
// fusion (callFusion) go through orChat → parseOpenRouterSSE, so one parse
// test covers both paths.
func TestParseOpenRouterSSECost(t *testing.T) {
	_, u, _ := parseOpenRouterSSE([]byte(
		`{"choices":[{"delta":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"cost":0.123}}`))
	if u == nil || !u.HasProviderCost || u.ProviderCostUSD != 0.123 {
		t.Errorf("parse cost = %+v, want {0.123,true}", u)
	}
	_, u2, _ := parseOpenRouterSSE([]byte(
		`{"choices":[{"delta":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	if u2 == nil || u2.HasProviderCost {
		t.Errorf("absent cost must not set HasProviderCost: %+v", u2)
	}
	_, u3, _ := parseOpenRouterSSE([]byte(
		`{"choices":[{"delta":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"cost":0}}`))
	if u3 == nil || !u3.HasProviderCost || u3.ProviderCostUSD != 0 {
		t.Errorf("zero cost is present, must set HasProviderCost: %+v", u3)
	}
}

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
