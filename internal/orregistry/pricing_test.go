package orregistry

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
)

func closeEnough(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// TestParsePricing_Units verifies the USD-per-token → USD-per-1M conversion
// (×1e6) and the valid flag.
func TestParsePricing_Units(t *testing.T) {
	p, ok := parsePricing(pricingFields{Prompt: "0.00000045", Completion: "0.0000032"})
	if !ok {
		t.Fatal("parsePricing should accept valid rates")
	}
	if !closeEnough(p.Prompt, 0.45) || !closeEnough(p.Completion, 3.2) {
		t.Errorf("prompt/completion = %v/%v, want 0.45/3.2 (USD/1M)", p.Prompt, p.Completion)
	}
	if !p.Valid {
		t.Errorf("Valid=%v, want true", p.Valid)
	}
}

// TestParsePricing_CacheRates verifies input_cache_read/input_cache_write map
// onto the per-1M cached rates.
func TestParsePricing_CacheRates(t *testing.T) {
	p, ok := parsePricing(pricingFields{Prompt: "0.000003", Completion: "0.000015", InputCacheRead: "0.0000003", InputCacheWrite: "0.0000006"})
	if !ok {
		t.Fatal("parsePricing should accept cache rates")
	}
	if !closeEnough(p.CachedInput, 0.3) || !closeEnough(p.CacheWrite, 0.6) {
		t.Errorf("cached/write = %v/%v, want 0.3/0.6", p.CachedInput, p.CacheWrite)
	}
}

// TestParsePricing_Free verifies a genuine zero rate is Valid (cost is $0.00,
// not "—").
func TestParsePricing_Free(t *testing.T) {
	p, ok := parsePricing(pricingFields{Prompt: "0", Completion: "0"})
	if !ok || !p.Valid {
		t.Errorf("free model: ok=%v Valid=%v, want both true", ok, p.Valid)
	}
}

// TestParsePricing_DynamicSentinel verifies "-1" (OpenRouter dynamic pricing)
// is rejected — the model must be treated as unpriced, never guessed.
func TestParsePricing_DynamicSentinel(t *testing.T) {
	if _, ok := parsePricing(pricingFields{Prompt: "-1", Completion: "0.0000032"}); ok {
		t.Error("dynamic pricing (-1) must be rejected")
	}
	if _, ok := parsePricing(pricingFields{}); ok {
		t.Error("missing prompt/completion must be rejected")
	}
	if _, ok := parsePricing(pricingFields{Prompt: "abc", Completion: "0.0000032"}); ok {
		t.Error("malformed prompt must be rejected")
	}
	if _, ok := parsePricing(pricingFields{Prompt: "0.000003", Completion: "-5"}); ok {
		t.Error("negative completion must be rejected")
	}
	// Non-finite base rates must be rejected, not propagated as valid pricing.
	if _, ok := parsePricing(pricingFields{Prompt: "NaN", Completion: "0.0000032"}); ok {
		t.Error("NaN prompt must be rejected")
	}
	if _, ok := parsePricing(pricingFields{Prompt: "+Inf", Completion: "0.0000032"}); ok {
		t.Error("+Inf prompt must be rejected")
	}
	if _, ok := parsePricing(pricingFields{Prompt: "0.000003", Completion: "1e999"}); ok {
		t.Error("overflow completion must be rejected")
	}
}

// TestParsePricing_MalformedCacheRateDropped verifies a malformed/negative cache
// rate is silently dropped (cached tokens then bill at base input) while the
// base rates stay valid.
func TestParsePricing_MalformedCacheRateDropped(t *testing.T) {
	p, ok := parsePricing(pricingFields{Prompt: "0.000003", Completion: "0.000015", InputCacheRead: "abc", InputCacheWrite: "-0.1"})
	if !ok || !p.Valid {
		t.Fatalf("base rates must stay valid with malformed cache rates: ok=%v Valid=%v", ok, p.Valid)
	}
	if p.CachedInput != 0 || p.CacheWrite != 0 {
		t.Errorf("malformed cache rates must be dropped: CachedInput=%v CacheWrite=%v, want 0/0", p.CachedInput, p.CacheWrite)
	}
}

// TestFetchModelsPopulatesPricing verifies one HTTP response populates both
// context lengths and pricing, and that a model with pricing but no
// context_length still lands in the pricing map.
func TestFetchModelsPopulatesPricing(t *testing.T) {
	ResetCache()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := response{Data: []entry{
			{ID: "priced-only", Pricing: pricingFields{Prompt: "0.000001", Completion: "0.000002"}},
			{ID: "full", ContextLength: 8192, Pricing: pricingFields{Prompt: "0.000003", Completion: "0.000015", InputCacheRead: "0.0000003"}},
			{ID: "dynamic", ContextLength: 4096, Pricing: pricingFields{Prompt: "-1", Completion: "-1"}},
		}}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	restore := SetModelsURLForTest(srv.URL)
	defer restore()

	if _, err := Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	// Full model: context + pricing.
	if cl, ok := Lookup("full"); !ok || cl != 8192 {
		t.Errorf("Lookup(full) = %d,%v want 8192,true", cl, ok)
	}
	if p, ok := LookupPricing("full"); !ok || !closeEnough(p.Prompt, 3.0) {
		t.Errorf("LookupPricing(full) = %+v,%v want Prompt=3.0", p, ok)
	}

	// Priced-only model: no context length, but pricing present.
	if _, ok := Lookup("priced-only"); ok {
		t.Error("Lookup(priced-only) should miss (no context_length)")
	}
	if p, ok := LookupPricing("priced-only"); !ok || !closeEnough(p.Prompt, 1.0) {
		t.Errorf("LookupPricing(priced-only) = %+v,%v want Prompt=1.0", p, ok)
	}

	// Dynamic model: context present, pricing rejected (absent from map).
	if _, ok := Lookup("dynamic"); !ok {
		t.Error("Lookup(dynamic) should hit (context_length present)")
	}
	if _, ok := LookupPricing("dynamic"); ok {
		t.Error("LookupPricing(dynamic) must miss (dynamic pricing rejected)")
	}
}

// TestLookupPricing_ColdMiss verifies a cold cache is a hard miss for pricing
// (never served stale).
func TestLookupPricing_ColdMiss(t *testing.T) {
	ResetCache()
	if _, ok := LookupPricing("anything"); ok {
		t.Error("LookupPricing must miss on a cold cache")
	}
}

// TestSetPricingForTest verifies the test helper seeds a warm pricing cache.
func TestSetPricingForTest(t *testing.T) {
	ResetCache()
	SetPricingForTest(map[string]Pricing{"m": {Prompt: 1.0, Completion: 2.0, Valid: true}})
	p, ok := LookupPricing("m")
	if !ok || !closeEnough(p.Prompt, 1.0) {
		t.Errorf("LookupPricing(m) = %+v,%v want Prompt=1.0,ok=true", p, ok)
	}
	ResetCache()
}
