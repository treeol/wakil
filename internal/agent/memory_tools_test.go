package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/memory"
	"github.com/treeol/wakil/internal/proxy"
)

// memoryTestApp creates an App with a real memory store for testing.
func memoryTestApp(t *testing.T, isSubagent bool) (*App, func()) {
	t.Helper()
	dir := t.TempDir()
	wsRoot := filepath.Join(dir, "workspace")
	dbPath := filepath.Join(dir, "memory", "test.db")

	store, err := memory.Open(dbPath, wsRoot)
	if err != nil {
		t.Fatal(err)
	}

	prefix := "main"
	if isSubagent {
		prefix = "sub-abc12345"
	}

	app := &App{
		MemoryStore: store,
		AgentPrefix: prefix,
		IsSubagent:  isSubagent,
	}

	cleanup := func() { store.Close() }
	t.Cleanup(cleanup)
	return app, cleanup
}

func memCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10e9)
	t.Cleanup(cancel)
	return ctx
}

func memToolCall(name, args string) proxy.ToolCall {
	return proxy.ToolCall{
		ID:       "test-tc",
		Function: proxy.FunctionCall{Name: name, Arguments: args},
	}
}

// ─── Tier-gating tests ─────────────────────────────────────────────────────

func TestSubagentMemoryPromoteRejected(t *testing.T) {
	app, _ := memoryTestApp(t, true)
	ctx := memCtx(t)

	// First, put a proposed entry as the subagent.
	putResult := app.ExecuteToolCall(ctx, memToolCall("memory_put",
		`{"key":"test/promote","value":"data","kind":"note"}`))
	if !strings.Contains(putResult.text, "proposed") {
		t.Fatalf("subagent put should produce proposed, got: %s", putResult.text)
	}

	// Try to promote — should be rejected.
	result := app.ExecuteToolCall(ctx, memToolCall("memory_promote",
		`{"id":1}`))
	if !strings.Contains(result.text, "main-agent only") {
		t.Fatalf("subagent promote should be rejected, got: %s", result.text)
	}
}

func TestSubagentMemoryRejectRejected(t *testing.T) {
	app, _ := memoryTestApp(t, true)
	ctx := memCtx(t)

	app.ExecuteToolCall(ctx, memToolCall("memory_put",
		`{"key":"test/reject","value":"data","kind":"note"}`))

	result := app.ExecuteToolCall(ctx, memToolCall("memory_reject",
		`{"id":1}`))
	if !strings.Contains(result.text, "main-agent only") {
		t.Fatalf("subagent reject should be rejected, got: %s", result.text)
	}
}

func TestSubagentMemoryForgetRejected(t *testing.T) {
	app, _ := memoryTestApp(t, true)
	ctx := memCtx(t)

	result := app.ExecuteToolCall(ctx, memToolCall("memory_forget",
		`{"key":"test/forget"}`))
	if !strings.Contains(result.text, "main-agent only") {
		t.Fatalf("subagent forget should be rejected, got: %s", result.text)
	}
}

func TestSubagentMemoryPromoteFromStagingRejected(t *testing.T) {
	app, _ := memoryTestApp(t, true)
	ctx := memCtx(t)

	result := app.ExecuteToolCall(ctx, memToolCall("memory_promote_from_staging",
		`{"staging_key":"sub-abc/data","key":"test/bridge","kind":"note"}`))
	if !strings.Contains(result.text, "main-agent only") {
		t.Fatalf("subagent promote_from_staging should be rejected, got: %s", result.text)
	}
}

func TestSubagentMemoryPutWithoutTTLIsProposed(t *testing.T) {
	app, _ := memoryTestApp(t, true)
	ctx := memCtx(t)

	result := app.ExecuteToolCall(ctx, memToolCall("memory_put",
		`{"key":"test/sub-proposed","value":"subagent data","kind":"note"}`))
	if !strings.Contains(result.text, "proposed") {
		t.Fatalf("subagent put without TTL should be proposed, got: %s", result.text)
	}
	if !strings.Contains(result.text, "durable") {
		t.Fatalf("subagent put without TTL should be durable tier, got: %s", result.text)
	}
}

func TestSubagentMemoryPutWithTTLIsActive(t *testing.T) {
	app, _ := memoryTestApp(t, true)
	ctx := memCtx(t)

	result := app.ExecuteToolCall(ctx, memToolCall("memory_put",
		`{"key":"test/sub-ttl","value":"subagent data","kind":"note","ttl_seconds":3600}`))
	if !strings.Contains(result.text, "mid-tier") {
		t.Fatalf("subagent put with TTL should be mid-tier, got: %s", result.text)
	}
	if !strings.Contains(result.text, "expires") {
		t.Fatalf("subagent put with TTL should show expiry, got: %s", result.text)
	}
}

// ─── TTL bounds ────────────────────────────────────────────────────────────

func TestTTLBoundsEnforced(t *testing.T) {
	app, _ := memoryTestApp(t, false)
	ctx := memCtx(t)

	// Below minimum (3600).
	result := app.ExecuteToolCall(ctx, memToolCall("memory_put",
		`{"key":"test/ttl-low","value":"data","kind":"note","ttl_seconds":1800}`))
	if !strings.Contains(result.text, "ERROR") || !strings.Contains(result.text, "3600") {
		t.Fatalf("TTL below minimum should error, got: %s", result.text)
	}

	// Above maximum (604800).
	result = app.ExecuteToolCall(ctx, memToolCall("memory_put",
		`{"key":"test/ttl-high","value":"data","kind":"note","ttl_seconds":700000}`))
	if !strings.Contains(result.text, "ERROR") || !strings.Contains(result.text, "604800") {
		t.Fatalf("TTL above maximum should error, got: %s", result.text)
	}

	// Valid TTL.
	result = app.ExecuteToolCall(ctx, memToolCall("memory_put",
		`{"key":"test/ttl-ok","value":"data","kind":"note","ttl_seconds":86400}`))
	if strings.Contains(result.text, "ERROR") {
		t.Fatalf("valid TTL should succeed, got: %s", result.text)
	}
}

// ─── Main agent promote of tainted entry ───────────────────────────────────

func TestMainAgentPromoteTaintedEntry(t *testing.T) {
	app, _ := memoryTestApp(t, false)
	ctx := memCtx(t)

	// Simulate that the agent touched external content.
	app.touchedExternal = true

	// Put a proposed entry — should be tainted.
	putResult := app.ExecuteToolCall(ctx, memToolCall("memory_put",
		`{"key":"test/tainted","value":"web-derived conclusion","kind":"decision"}`))
	if !strings.Contains(putResult.text, "tainted") {
		t.Fatalf("proposed entry from tainted agent should show taint, got: %s", putResult.text)
	}

	// Promote it — should succeed (main agent can promote tainted entries).
	promoteResult := app.ExecuteToolCall(ctx, memToolCall("memory_promote",
		`{"id":1}`))
	if strings.Contains(promoteResult.text, "ERROR") {
		t.Fatalf("main agent promote of tainted entry should succeed, got: %s", promoteResult.text)
	}
	// The promoted entry should still carry the taint flag.
	if !strings.Contains(promoteResult.text, "tainted") {
		t.Fatalf("promoted tainted entry should still show taint, got: %s", promoteResult.text)
	}
}

// ─── Nil-store behavior ────────────────────────────────────────────────────

func TestNilStoreBehavior(t *testing.T) {
	app := &App{
		MemoryStore: nil,
		AgentPrefix: "main",
	}
	ctx := memCtx(t)

	for _, tool := range []string{"memory_put", "memory_get", "memory_search", "memory_list"} {
		result := app.ExecuteToolCall(ctx, memToolCall(tool, `{"key":"x","query":"x","value":"v","kind":"n"}`))
		if !strings.Contains(result.text, "memory unavailable") {
			t.Fatalf("%s with nil store should return 'memory unavailable', got: %s", tool, result.text)
		}
	}
}

// ─── Rendering golden tests ────────────────────────────────────────────────

func TestRenderProvenance(t *testing.T) {
	tests := []struct {
		name     string
		entry    *memory.Entry
		contains []string
	}{
		{
			name: "mid-tier active with expiry and taint",
			entry: &memory.Entry{
				Tier:      "mid",
				Status:    "active",
				Writer:    "sub-4f2a91c3",
				CreatedAt: 1720915200000,           // 2024-07-14
				ExpiresAt: int64Ptr(1721088000000), // 2024-07-16
				Tainted:   memory.TaintTrue,
			},
			contains: []string{"mid-tier", "sub-4f2a91c3", "expires", "tainted"},
		},
		{
			name: "durable active promoted",
			entry: &memory.Entry{
				Tier:       "durable",
				Status:     "active",
				Writer:     "main",
				CreatedAt:  1720915200000,
				PromotedBy: "main",
				Tainted:    memory.TaintUnknown,
			},
			contains: []string{"durable-tier", "main", "promoted", "taint-unknown"},
		},
		{
			name: "durable proposed",
			entry: &memory.Entry{
				Tier:      "durable",
				Status:    "proposed",
				Writer:    "sub-abc12345",
				CreatedAt: 1720915200000,
				Tainted:   memory.TaintUnknown,
			},
			contains: []string{"durable-tier", "sub-abc12345", "proposed", "taint-unknown"},
		},
		{
			name: "stale anchors",
			entry: &memory.Entry{
				Tier:         "durable",
				Status:       "active",
				Writer:       "main",
				CreatedAt:    1720915200000,
				Tainted:      memory.TaintUnknown,
				StaleAnchors: 1,
				TotalAnchors: 2,
			},
			contains: []string{"anchors: 1 stale of 2"},
		},
		{
			name: "no taint flag for false",
			entry: &memory.Entry{
				Tier:      "durable",
				Status:    "active",
				Writer:    "main",
				CreatedAt: 1720915200000,
				Tainted:   memory.TaintFalse,
			},
			contains: []string{"durable-tier", "main"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := renderProvenance(tt.entry)
			for _, s := range tt.contains {
				if !strings.Contains(result, s) {
					t.Errorf("expected %q in provenance %q", s, result)
				}
			}
			// Must always be bracketed.
			if !strings.HasPrefix(result, "[") || !strings.HasSuffix(result, "]") {
				t.Errorf("provenance must be bracketed, got: %s", result)
			}
		})
	}
}

// ─── End-to-end put → get ──────────────────────────────────────────────────

func TestMemoryPutGetEndToEnd(t *testing.T) {
	app, _ := memoryTestApp(t, false)
	ctx := memCtx(t)

	// Put with TTL.
	putResult := app.ExecuteToolCall(ctx, memToolCall("memory_put",
		`{"key":"arch/flow","value":"auth uses JWT","kind":"note","ttl_seconds":86400}`))
	if strings.Contains(putResult.text, "ERROR") {
		t.Fatalf("put failed: %s", putResult.text)
	}

	// Get it back.
	getResult := app.ExecuteToolCall(ctx, memToolCall("memory_get",
		`{"key":"arch/flow"}`))
	if !strings.Contains(getResult.text, "auth uses JWT") {
		t.Fatalf("get should return value, got: %s", getResult.text)
	}
	if !strings.Contains(getResult.text, "mid-tier") {
		t.Fatalf("get should show mid-tier in provenance, got: %s", getResult.text)
	}
	if !strings.Contains(getResult.text, "main") {
		t.Fatalf("get should show writer 'main' in provenance, got: %s", getResult.text)
	}
}

func TestMemoryPutProposedPromoteEndToEnd(t *testing.T) {
	app, _ := memoryTestApp(t, false)
	ctx := memCtx(t)

	// Put without TTL → proposed.
	putResult := app.ExecuteToolCall(ctx, memToolCall("memory_put",
		`{"key":"decision/db","value":"use sqlite","kind":"decision"}`))
	if !strings.Contains(putResult.text, "proposed") {
		t.Fatalf("put without TTL should be proposed, got: %s", putResult.text)
	}

	// Promote.
	promoteResult := app.ExecuteToolCall(ctx, memToolCall("memory_promote",
		`{"id":1}`))
	if strings.Contains(promoteResult.text, "ERROR") {
		t.Fatalf("promote failed: %s", promoteResult.text)
	}
	if !strings.Contains(promoteResult.text, "active") {
		t.Fatalf("promote should show active, got: %s", promoteResult.text)
	}

	// Get should return the promoted entry.
	getResult := app.ExecuteToolCall(ctx, memToolCall("memory_get",
		`{"key":"decision/db"}`))
	if !strings.Contains(getResult.text, "use sqlite") {
		t.Fatalf("get should return promoted value, got: %s", getResult.text)
	}
	if !strings.Contains(getResult.text, "durable-tier") {
		t.Fatalf("get should show durable-tier, got: %s", getResult.text)
	}
}

// ─── Search ────────────────────────────────────────────────────────────────

func TestMemorySearch(t *testing.T) {
	app, _ := memoryTestApp(t, false)
	ctx := memCtx(t)

	app.ExecuteToolCall(ctx, memToolCall("memory_put",
		`{"key":"arch/auth","value":"auth uses JWT tokens","kind":"note","ttl_seconds":86400}`))
	app.ExecuteToolCall(ctx, memToolCall("memory_put",
		`{"key":"arch/session","value":"session stored in redis","kind":"note","ttl_seconds":86400}`))

	// Search for "auth".
	result := app.ExecuteToolCall(ctx, memToolCall("memory_search",
		`{"query":"auth"}`))
	if !strings.Contains(result.text, "auth uses JWT") {
		t.Fatalf("search should find auth entry, got: %s", result.text)
	}

	// Search for "redis".
	result = app.ExecuteToolCall(ctx, memToolCall("memory_search",
		`{"query":"redis"}`))
	if !strings.Contains(result.text, "session stored in redis") {
		t.Fatalf("search should find redis entry, got: %s", result.text)
	}

	// Search for nonexistent.
	result = app.ExecuteToolCall(ctx, memToolCall("memory_search",
		`{"query":"nonexistent"}`))
	if !strings.Contains(result.text, "no matches") {
		t.Fatalf("search for nonexistent should return no matches, got: %s", result.text)
	}
}

// ─── Taint signal: session-cumulative (A1) ─────────────────────────────────

func TestTaintSessionCumulative(t *testing.T) {
	app, _ := memoryTestApp(t, false)
	ctx := memCtx(t)

	// First put: no external exposure → taint-unknown.
	result1 := app.ExecuteToolCall(ctx, memToolCall("memory_put",
		`{"key":"test/clean","value":"clean note","kind":"note"}`))
	if !strings.Contains(result1.text, "taint-unknown") {
		t.Fatalf("first put should be taint-unknown, got: %s", result1.text)
	}

	// Simulate the agent touching external content (e.g. web search in a
	// previous tool call this session).
	app.touchedExternal = true

	// Second put: now tainted (sticky — once set, never cleared).
	result2 := app.ExecuteToolCall(ctx, memToolCall("memory_put",
		`{"key":"test/tainted","value":"after web search","kind":"note"}`))
	if !strings.Contains(result2.text, "tainted") {
		t.Fatalf("second put after external exposure should be tainted, got: %s", result2.text)
	}
	if strings.Contains(result2.text, "taint-unknown") {
		t.Fatalf("second put should NOT be taint-unknown, got: %s", result2.text)
	}
}

func int64Ptr(v int64) *int64 { return &v }

// ── Mid-tier write gate tests ──────────────────────────────────────────────

// TestMemoryPutSubagentMidTierQuarantined verifies that a subagent's mid-tier
// write is quarantined (proposed, not active) and requires promotion.
func TestMemoryPutSubagentMidTierQuarantined(t *testing.T) {
	app, _ := memoryTestApp(t, true) // isSubagent=true
	ctx := memCtx(t)

	result := app.ExecuteToolCall(ctx, memToolCall("memory_put",
		`{"key":"test/sub-quarantine","value":"secret","kind":"note","ttl_seconds":3600}`))

	if strings.Contains(result.text, "ERROR") {
		t.Fatalf("unexpected error: %s", result.text)
	}
	if !strings.Contains(result.text, "quarantined") {
		t.Errorf("subagent mid-tier write should be quarantined, got: %s", result.text)
	}
	if !strings.Contains(result.text, "subagent write") {
		t.Errorf("should mention 'subagent write' reason, got: %s", result.text)
	}

	// The entry should NOT be retrievable via memory_get (only active entries).
	getResult := app.ExecuteToolCall(ctx, memToolCall("memory_get",
		`{"key":"test/sub-quarantine"}`))
	if !strings.Contains(getResult.text, "not found") {
		t.Errorf("quarantined entry should not be retrievable via memory_get, got: %s", getResult.text)
	}

	// The entry SHOULD appear in memory_list status=quarantined.
	listResult := app.ExecuteToolCall(ctx, memToolCall("memory_list",
		`{"status":"quarantined"}`))
	if !strings.Contains(listResult.text, "test/sub-quarantine") {
		t.Errorf("quarantined entry should appear in list status=quarantined, got: %s", listResult.text)
	}
}

// TestMemoryPutTaintedMidTierQuarantined verifies that a tainted main-agent
// mid-tier write is quarantined.
func TestMemoryPutTaintedMidTierQuarantined(t *testing.T) {
	app, _ := memoryTestApp(t, false) // main agent
	ctx := memCtx(t)

	// Simulate external content exposure (sets touchedExternal → TaintTrue).
	app.addExternalGrounding(proxy.GroundingEntry{Type: "web", Label: "https://example.com"})

	result := app.ExecuteToolCall(ctx, memToolCall("memory_put",
		`{"key":"test/tainted-quarantine","value":"data","kind":"note","ttl_seconds":3600}`))

	if strings.Contains(result.text, "ERROR") {
		t.Fatalf("unexpected error: %s", result.text)
	}
	if !strings.Contains(result.text, "quarantined") {
		t.Errorf("tainted mid-tier write should be quarantined, got: %s", result.text)
	}
	if !strings.Contains(result.text, "tainted session") {
		t.Errorf("should mention 'tainted session' reason, got: %s", result.text)
	}
}

// TestMemoryPutUntaintedMainMidTierActive verifies that an untainted main-agent
// mid-tier write still goes active (unchanged behavior).
func TestMemoryPutUntaintedMainMidTierActive(t *testing.T) {
	app, _ := memoryTestApp(t, false) // main agent, no external exposure
	ctx := memCtx(t)

	result := app.ExecuteToolCall(ctx, memToolCall("memory_put",
		`{"key":"test/normal-mid","value":"data","kind":"note","ttl_seconds":3600}`))

	if strings.Contains(result.text, "ERROR") {
		t.Fatalf("unexpected error: %s", result.text)
	}
	if !strings.Contains(result.text, "stored (mid-tier") {
		t.Errorf("untainted main mid-tier write should be stored active, got: %s", result.text)
	}
	if strings.Contains(result.text, "quarantined") {
		t.Errorf("should NOT be quarantined, got: %s", result.text)
	}

	// Should be retrievable via memory_get.
	getResult := app.ExecuteToolCall(ctx, memToolCall("memory_get",
		`{"key":"test/normal-mid"}`))
	if strings.Contains(getResult.text, "not found") {
		t.Errorf("active mid-tier entry should be retrievable, got: %s", getResult.text)
	}
}

// TestMemoryPutDurableUnchanged verifies that durable writes (no TTL) are
// unaffected by the quarantine gate — they always go to proposed.
func TestMemoryPutDurableUnchanged(t *testing.T) {
	app, _ := memoryTestApp(t, true) // subagent
	ctx := memCtx(t)

	result := app.ExecuteToolCall(ctx, memToolCall("memory_put",
		`{"key":"test/sub-durable","value":"data","kind":"note"}`))

	if strings.Contains(result.text, "ERROR") {
		t.Fatalf("unexpected error: %s", result.text)
	}
	if !strings.Contains(result.text, "proposed (durable") {
		t.Errorf("durable write should still be proposed, got: %s", result.text)
	}
	if strings.Contains(result.text, "quarantined") {
		t.Errorf("durable write should NOT say 'quarantined', got: %s", result.text)
	}
}
