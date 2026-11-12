package counsel

import (
	"context"
	"testing"
)

// realShapeUsageChunk is the usage event of a captured OpenRouter stream for
// openrouter/auto (the router resolved to a different model). Values are
// rounded and identifiers are generic; only the structure the parser reads is
// kept: the final chunk has finish_reason "stop", an empty choices-free usage
// object, a numeric usage.cost, and the served model on the chunk.
const realShapeUsageChunk = `{"model":"anthropic/served-model","choices":[{"index":0,"delta":{"content":""},"finish_reason":"stop"}],"usage":{"prompt_tokens":4193,"completion_tokens":3288,"cost":0.002168025,"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":0}}}`

// TestReviewKeepsProviderCostAndServedModel is the regression for the lost
// panel cost: dispatchReview used to copy only token fields from each send, so
// usage.cost and the served model never reached the review outcome. The usage
// here is parsed from the real chunk shape, not hand-built.
func TestReviewKeepsProviderCostAndServedModel(t *testing.T) {
	_, parsed, _ := parseOpenRouterSSE([]byte(realShapeUsageChunk))
	if parsed == nil || !parsed.HasProviderCost {
		t.Fatalf("parser did not read the cost from the real chunk shape: %+v", parsed)
	}
	send := func(ctx context.Context, q, b, ap string, maxTok int) (string, OracleUsage, error) {
		return "Looks fine. No findings.", OracleUsage{
			InputTokens:     parsed.InputTokens,
			OutputTokens:    parsed.OutputTokens,
			ProviderCostUSD: parsed.ProviderCostUSD,
			HasProviderCost: parsed.HasProviderCost,
			ServedModel:     parsed.ServedModel,
			FinishReason:    parsed.FinishReason,
		}, nil
	}
	out := dispatchReview(context.Background(), "openrouter/auto", 128000, 4096, 4096, "Review.", "ctx", "", send)
	if !out.Usage.HasProviderCost || out.Usage.ProviderCostUSD != parsed.ProviderCostUSD {
		t.Errorf("review outcome lost the provider cost: %+v", out.Usage)
	}
	if out.Usage.ServedModel != "anthropic/served-model" {
		t.Errorf("review outcome lost the served model: %q", out.Usage.ServedModel)
	}
}

// addUsage sums every field of a second call into the first, including cost.
func TestAddUsageSumsEveryField(t *testing.T) {
	a := OracleUsage{InputTokens: 10, OutputTokens: 2, ReasoningTokens: 1, WebSearchRequests: 1, ProviderCostUSD: 0.10, HasProviderCost: true}
	b := OracleUsage{InputTokens: 5, OutputTokens: 3, ReasoningTokens: 2, WebSearchRequests: 2, ProviderCostUSD: 0.05, HasProviderCost: true, ServedModel: "m/x"}
	got := addUsage(a, b)
	if got.InputTokens != 15 || got.OutputTokens != 5 || got.ReasoningTokens != 3 || got.WebSearchRequests != 3 {
		t.Errorf("token fields not summed: %+v", got)
	}
	if d := got.ProviderCostUSD - 0.15; d > 1e-9 || d < -1e-9 {
		t.Errorf("cost = %v, want 0.15", got.ProviderCostUSD)
	}
	if got.ServedModel != "m/x" {
		t.Errorf("served model = %q, want m/x", got.ServedModel)
	}
}
