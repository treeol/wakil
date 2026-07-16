package orregistry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestFetch_CacheHit verifies that a second Fetch call within the TTL
// does not make another HTTP request.
func TestFetch_CacheHit(t *testing.T) {
	ResetCache()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		resp := response{Data: []entry{
			{ID: "model-a", ContextLength: 8192},
			{ID: "model-b", ContextLength: 32768},
		}}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	restore := SetModelsURLForTest(srv.URL)
	defer restore()

	ctx := context.Background()
	m1, err := Fetch(ctx)
	if err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	if len(m1) != 2 {
		t.Fatalf("first Fetch returned %d entries, want 2", len(m1))
	}
	if m1["model-a"] != 8192 {
		t.Errorf("model-a context = %d, want 8192", m1["model-a"])
	}

	// Second call should hit cache.
	m2, err := Fetch(ctx)
	if err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 HTTP call (cache hit), got %d", calls)
	}
	if m2["model-b"] != 32768 {
		t.Errorf("model-b context = %d, want 32768", m2["model-b"])
	}
}

// TestFetch_MalformedResponse verifies that a non-JSON response returns an error.
func TestFetch_MalformedResponse(t *testing.T) {
	ResetCache()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("this is not json"))
	}))
	defer srv.Close()

	restore := SetModelsURLForTest(srv.URL)
	defer restore()

	_, err := Fetch(context.Background())
	if err == nil {
		t.Fatal("expected error for malformed response, got nil")
	}
}

// TestFetch_NonOKStatus verifies that a non-200 response returns an error.
func TestFetch_NonOKStatus(t *testing.T) {
	ResetCache()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	restore := SetModelsURLForTest(srv.URL)
	defer restore()

	_, err := Fetch(context.Background())
	if err == nil {
		t.Fatal("expected error for 500 response, got nil")
	}
}

// TestLookup_WarmCache verifies Lookup returns data from a warm cache without I/O.
func TestLookup_WarmCache(t *testing.T) {
	ResetCache()
	SetCacheForTest(map[string]int{"gpt-4": 128000, "claude-3": 200000})

	cl, ok := Lookup("gpt-4")
	if !ok {
		t.Fatal("Lookup(gpt-4) should hit warm cache")
	}
	if cl != 128000 {
		t.Errorf("Lookup(gpt-4) = %d, want 128000", cl)
	}
}

// TestLookup_ColdCache verifies Lookup returns ok=false on a cold cache.
func TestLookup_ColdCache(t *testing.T) {
	ResetCache()
	_, ok := Lookup("anything")
	if ok {
		t.Error("Lookup should return ok=false on cold cache")
	}
}

// TestFetch_ReturnsCopy verifies that the returned map is a copy — mutating it
// must not affect the internal cache.
func TestFetch_ReturnsCopy(t *testing.T) {
	ResetCache()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := response{Data: []entry{{ID: "m1", ContextLength: 4096}}}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	restore := SetModelsURLForTest(srv.URL)
	defer restore()

	m, err := Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	// Mutate the returned map.
	m["m1"] = 999
	delete(m, "m1")

	// Internal cache must be unchanged.
	cl, ok := Lookup("m1")
	if !ok {
		t.Fatal("internal cache was mutated: m1 missing")
	}
	if cl != 4096 {
		t.Errorf("internal cache m1 = %d, want 4096 (mutation leaked)", cl)
	}
}

// TestFetch_LookupAfterSuccess verifies that Lookup returns data from a
// warm cache after a successful Fetch. This is NOT a stale-on-failure test —
// the stale fallback path requires forcing cache TTL expiry, which needs a
// time-injection seam not currently available in the orregistry package.
// A proper stale test would: (1) Fetch successfully, (2) force TTL expiry,
// (3) Fetch against a dead server, (4) assert stale data returned with nil err.
func TestFetch_LookupAfterSuccess(t *testing.T) {
	ResetCache()
	goodSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := response{Data: []entry{{ID: "stale-model", ContextLength: 8192}}}
		json.NewEncoder(w).Encode(resp)
	}))

	restore := SetModelsURLForTest(goodSrv.URL)
	m, err := Fetch(context.Background())
	if err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	if m["stale-model"] != 8192 {
		t.Fatalf("first Fetch: stale-model = %d, want 8192", m["stale-model"])
	}
	goodSrv.Close()
	restore()

	// Lookup should still return the cached data.
	cl, ok := Lookup("stale-model")
	if !ok || cl != 8192 {
		t.Errorf("Lookup after first fetch: ok=%v, cl=%d, want ok=true, cl=8192", ok, cl)
	}
}
