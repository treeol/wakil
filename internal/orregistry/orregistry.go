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
	"net/http"
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
	ID            string `json:"id"`
	ContextLength int    `json:"context_length"`
}

// response is the top-level response from OpenRouter's models endpoint.
type response struct {
	Data []entry `json:"data"`
}

// cache holds context lengths fetched from OpenRouter, shared process-wide.
// Entries expire after cacheTTL.
type cache struct {
	mu       sync.Mutex
	entries  map[string]int // model ID → context_length (tokens)
	fetched  time.Time
	inflight bool // singleflight: one goroutine fetches at a time
	fetchErr error
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
				// Fetch completed but failed; release lock and fall through to our own fetch.
				c.mu.Unlock()
				break
			}
			c.mu.Unlock()
		}
		// Re-acquire to claim the fetch slot below.
		c.mu.Lock()
	}

	// Claim the fetch slot.
	c.inflight = true
	c.mu.Unlock()

	// Fetch outside the lock so other callers aren't blocked on I/O.
	entries, err := fetchModels(ctx)

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

// fetchModels does the actual HTTP fetch and parse.
func fetchModels(ctx context.Context) (map[string]int, error) {
	fctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(fctx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build models request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch models: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch models: %s", resp.Status)
	}

	// The models list is large (~hundreds of entries) but well under 16 MB.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("read models response: %w", err)
	}

	var result response
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("parse models response: %w", err)
	}

	entries := make(map[string]int, len(result.Data))
	for _, m := range result.Data {
		if m.ContextLength > 0 {
			entries[m.ID] = m.ContextLength
		}
	}
	return entries, nil
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
	shared.fetched = time.Time{}
	shared.inflight = false
	shared.fetchErr = nil
	shared.mu.Unlock()
}
