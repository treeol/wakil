// Package orregistry fetches and caches model context lengths from
// OpenRouter's public /api/v1/models endpoint. Relocated from
// internal/counsel/modellimit.go so both counsel (briefing fitting) and the
// agent's context-limit resolution (kind=openai endpoints on openrouter.ai)
// share one fetch, one cache, one parse.
package orregistry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	// DefaultModelsURL is the public endpoint that returns all model metadata
	// including context_length. No API key required.
	DefaultModelsURL = "https://openrouter.ai/api/v1/models"

	// cacheTTL: model metadata changes infrequently; cache for 1 hour.
	cacheTTL = time.Hour

	// fetchTimeout bounds the metadata fetch so a slow/unreachable OpenRouter
	// cannot stall a caller beyond this.
	fetchTimeout = 10 * time.Second
)

// modelsURL is the fetch target; variable so tests can point it at a mock.
var modelsURL = DefaultModelsURL

// entry is one model entry in the OpenRouter /api/v1/models response.
type entry struct {
	ID            string        `json:"id"`
	ContextLength int           `json:"context_length"`
	Pricing       pricingFields `json:"pricing"`
}

// pricingFields mirrors OpenRouter's per-model "pricing" object. All values
// are USD-per-token strings. InputCacheRead/InputCacheWrite are cache-read and
// cache-write token rates, present for the subset of models whose providers
// discount prompt caching (Anthropic, Gemini, …). Absent fields must NOT fall
// back to the base input rate — a missing cache rate means "this provider has
// no cache discount", which the cost arithmetic already maps to "bill at the
// base input rate".
type pricingFields struct {
	Prompt          string `json:"prompt"`
	Completion      string `json:"completion"`
	InputCacheRead  string `json:"input_cache_read"`
	InputCacheWrite string `json:"input_cache_write"`
}

// Pricing is one model's resolved per-million-token rates in USD. Prompt and
// Completion are the base input/output rates. CachedInput and CacheWrite are
// cache-discounted rates; 0 means "no discount published" (bill cached tokens
// at the base input rate). Valid is false when the model's pricing is dynamic
// (OpenRouter's "-1" sentinel) or malformed — callers must treat it as
// unpriced rather than guessing.
type Pricing struct {
	Prompt      float64
	Completion  float64
	CachedInput float64
	CacheWrite  float64
	Valid       bool
}

// response is the top-level response from OpenRouter's models endpoint.
type response struct {
	Data []entry `json:"data"`
}

// cache holds context lengths and pricing fetched from OpenRouter, shared
// process-wide. Entries expire after cacheTTL.
type cache struct {
	mu        sync.Mutex
	entries   map[string]int     // model ID → context_length (tokens)
	pricing   map[string]Pricing // model ID → resolved per-1M-token rates
	fetched   time.Time
	inflight  bool // singleflight: one goroutine fetches at a time
	fetchErr  error
}

var shared = &cache{}

// Fetch retrieves model context lengths from OpenRouter's public models
// endpoint. Results are cached for cacheTTL. On failure returns stale cached
// data if available (with nil error); otherwise returns nil and the error.
//
// HTTP I/O happens outside the cache mutex. A singleflight flag ensures only
// one goroutine fetches at a time; concurrent callers wait for the fetcher to
// finish, then read the result.
func Fetch(ctx context.Context) (map[string]int, error) {
	c := shared

	for {
		// Fast path: check cache under lock.
		c.mu.Lock()
		if c.entries != nil && time.Since(c.fetched) < cacheTTL {
			entries := c.entries
			c.mu.Unlock()
			return cloneMap(entries), nil
		}

		// Someone already fetching? Wait for them with ctx cancellation support.
		if c.inflight {
			c.mu.Unlock()
			for {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(100 * time.Millisecond):
				}
				c.mu.Lock()
				if !c.inflight {
					if c.entries != nil && time.Since(c.fetched) < cacheTTL {
						entries := c.entries
						c.mu.Unlock()
						return cloneMap(entries), nil
					}
					// Fetch completed but failed; loop back to (re)claim the
					// slot under the lock — do NOT fall through: another waiter
					// may have claimed it in the unlock/relock gap, and we must
					// re-check rather than start a duplicate fetch.
					c.mu.Unlock()
					continue
				}
				c.mu.Unlock()
			}
		}

		// Claim the fetch slot (we hold c.mu here; inflight is false).
		c.inflight = true
		c.mu.Unlock()
		break
	}

	// Fetch outside the lock so other callers aren't blocked on I/O.
	// The defer clears the slot even on panic, so a crashed fetch never strands
	// the singleflight flag.
	entries, pricing, err := fetchModels(ctx)

	c.mu.Lock()
	c.inflight = false
	if err != nil {
		c.fetchErr = err
		// Serve stale data if we have it.
		if c.entries != nil {
			stale := c.entries
			c.mu.Unlock()
			return cloneMap(stale), nil
		}
		c.mu.Unlock()
		return nil, err
	}
	c.entries = entries
	c.pricing = pricing
	c.fetched = time.Now()
	c.fetchErr = nil
	c.mu.Unlock()
	return cloneMap(entries), nil
}

// Lookup returns the context length for modelID from the warm cache only —
// no network I/O is ever triggered. ok is false when the cache is cold,
// expired, or has no entry for the model. Callers that can afford a network
// round-trip should call Fetch first.
func Lookup(modelID string) (contextLength int, ok bool) {
	shared.mu.Lock()
	entries := shared.entries
	fetched := shared.fetched
	shared.mu.Unlock()
	if entries == nil || time.Since(fetched) >= cacheTTL {
		return 0, false
	}
	cl, ok := entries[modelID]
	return cl, ok
}

// LookupPricing returns the model's resolved pricing from the warm cache only
// — no network I/O is ever triggered. ok is false when the cache is cold or
// EXPIRED: unlike context lengths (which Fetch may serve stale), a stale
// billing rate must never be used, so LookupPricing treats expiry as a hard
// miss and callers fall through to unpriced rather than quote an outdated
// rate. It also returns false for models whose pricing is dynamic ("-1") or
// absent from the registry.
func LookupPricing(modelID string) (Pricing, bool) {
	shared.mu.Lock()
	pricing := shared.pricing
	fetched := shared.fetched
	shared.mu.Unlock()
	if pricing == nil || time.Since(fetched) >= cacheTTL {
		return Pricing{}, false
	}
	p, ok := pricing[modelID]
	if !ok || !p.Valid {
		return Pricing{}, false
	}
	return p, true
}

// Warm fetches the registry into the cache if it is cold or expired, and
// returns immediately if the cache is already fresh (no re-fetch). It is the
// synchronous "make sure pricing is available" seam for callers whose flow
// does not otherwise touch orregistry (e.g. a proxy-routed OpenRouter backend
// whose context limits come from /v1/ilm/limits, never the registry). A failed
// fetch is deliberately silent: the caller's LookupPricing then misses and the
// cost row renders "—", so a slow/unreachable OpenRouter never blocks or
// crashes cost accounting.
func Warm(ctx context.Context) {
	shared.mu.Lock()
	fresh := (shared.entries != nil || shared.pricing != nil) && time.Since(shared.fetched) < cacheTTL
	shared.mu.Unlock()
	if fresh {
		return
	}
	// Fetch serves stale context on failure; we ignore the result — Warm's
	// only contract is "best-effort populate, never block on error".
	_, _ = Fetch(ctx)
}

// fetchModels does the actual HTTP fetch and parse.
func fetchModels(ctx context.Context) (map[string]int, map[string]Pricing, error) {
	fctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(fctx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("build models request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch models: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("fetch models: %s", resp.Status)
	}

	// The models list is large (~hundreds of entries) but well under 16 MB.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, nil, fmt.Errorf("read models response: %w", err)
	}

	var result response
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, nil, fmt.Errorf("parse models response: %w", err)
	}

	entries := make(map[string]int, len(result.Data))
	pricing := make(map[string]Pricing, len(result.Data))
	for _, m := range result.Data {
		// Filter per-field, not per-entry: a model with pricing but no
		// context_length must still land in the pricing map.
		if m.ContextLength > 0 {
			entries[m.ID] = m.ContextLength
		}
		if p, ok := parsePricing(m.Pricing); ok {
			pricing[m.ID] = p
		}
	}
	return entries, pricing, nil
}

// parsePricing resolves OpenRouter's string-valued, USD-per-token pricing into
// per-1M-token float64 rates. ok is false when the model's pricing is dynamic
// ("-1") or malformed — such entries are deliberately absent from the pricing
// map so callers fall through to unpriced rather than rendering a bogus figure.
func parsePricing(p pricingFields) (Pricing, bool) {
	if p.Prompt == "" || p.Completion == "" {
		return Pricing{}, false
	}
	prompt, ok := parseRate(p.Prompt)
	if !ok {
		return Pricing{}, false
	}
	completion, ok := parseRate(p.Completion)
	if !ok {
		return Pricing{}, false
	}
	out := Pricing{
		Prompt:     prompt * 1e6,
		Completion: completion * 1e6,
		Valid:      true,
	}
	// Optional cache rates: a malformed/negative/non-finite value is dropped
	// (cached tokens then bill at the base input rate — a conservative
	// overestimate, never a discount guess). Base rates remain valid.
	if v, ok := parseRate(p.InputCacheRead); ok {
		out.CachedInput = v * 1e6
	}
	if v, ok := parseRate(p.InputCacheWrite); ok {
		out.CacheWrite = v * 1e6
	}
	return out, true
}

// parseRate parses one OpenRouter pricing string. ok is false for empty,
// negative (the "-1" dynamic-pricing sentinel), malformed, or non-finite
// values. OpenRouter quotes rates in USD per token; valid values are
// non-negative finite decimals.
func parseRate(s string) (float64, bool) {
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

// cloneMap returns a shallow copy so callers can't mutate the cached map.
func cloneMap(m map[string]int) map[string]int {
	if m == nil {
		return nil
	}
	c := make(map[string]int, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

// SetCacheForTest injects entries as a fresh cache. Testing only.
func SetCacheForTest(entries map[string]int) {
	shared.mu.Lock()
	shared.entries = entries
	shared.pricing = nil
	shared.fetched = time.Now()
	shared.inflight = false
	shared.fetchErr = nil
	shared.mu.Unlock()
}

// SetPricingForTest injects pricing entries into a fresh cache. Testing only.
func SetPricingForTest(pricing map[string]Pricing) {
	shared.mu.Lock()
	shared.pricing = pricing
	shared.entries = nil
	shared.fetched = time.Now()
	shared.inflight = false
	shared.fetchErr = nil
	shared.mu.Unlock()
}

// SetModelsURLForTest points the fetch at url and returns a restore func.
// Testing only.
func SetModelsURLForTest(url string) (restore func()) {
	old := modelsURL
	modelsURL = url
	return func() { modelsURL = old }
}

// ResetCache clears the shared cache. Testing only.
func ResetCache() {
	shared.mu.Lock()
	shared.entries = nil
	shared.pricing = nil
	shared.fetched = time.Time{}
	shared.inflight = false
	shared.fetchErr = nil
	shared.mu.Unlock()
}
