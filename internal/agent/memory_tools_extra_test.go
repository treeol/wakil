package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/memory"
	"github.com/treeol/wakil/internal/proxy"
)

// ─── TTL exact boundaries ──────────────────────────────────────────────────

func TestTTLExactBoundaries(t *testing.T) {
	app, _ := memoryTestApp(t, false)
	ctx := memCtx(t)

	// Minimum boundary (3600) — should succeed.
	result := app.ExecuteToolCall(ctx, memToolCall("memory_put",
		`{"key":"test/ttl-min","value":"data","kind":"note","ttl_seconds":3600}`))
	if strings.Contains(result.text, "ERROR") {
		t.Fatalf("TTL at minimum (3600) should succeed, got: %s", result.text)
	}

	// Maximum boundary (604800) — should succeed.
	result = app.ExecuteToolCall(ctx, memToolCall("memory_put",
		`{"key":"test/ttl-max","value":"data","kind":"note","ttl_seconds":604800}`))
	if strings.Contains(result.text, "ERROR") {
		t.Fatalf("TTL at maximum (604800) should succeed, got: %s", result.text)
	}
}

// ─── Taint via grounding (A1 detection mechanism) ──────────────────────────

func TestTaintViaGrounding(t *testing.T) {
	app, _ := memoryTestApp(t, false)
	ctx := memCtx(t)

	// Wire a proxy.Client with grounding entries.
	app.Client = &proxy.Client{}

	// Before any grounding: taint-unknown.
	result := app.ExecuteToolCall(ctx, memToolCall("memory_put",
		`{"key":"test/before","value":"clean","kind":"note"}`))
	if !strings.Contains(result.text, "taint-unknown") {
		t.Fatalf("before grounding should be taint-unknown, got: %s", result.text)
	}

	// Simulate a web search tool call adding grounding (eagerly sets sticky flag).
	app.addExternalGrounding(proxy.GroundingEntry{Type: "web", Label: "example.com"})

	// After grounding: tainted (sticky).
	result = app.ExecuteToolCall(ctx, memToolCall("memory_put",
		`{"key":"test/after","value":"web-derived","kind":"note"}`))
	if !strings.Contains(result.text, "tainted") {
		t.Fatalf("after web grounding should be tainted, got: %s", result.text)
	}

	// Simulate oracle grounding.
	app2, _ := memoryTestApp(t, false)
	app2.Client = &proxy.Client{}
	app2.addExternalGrounding(proxy.GroundingEntry{Type: "oracle", Label: "gpt-5"})
	result = app2.ExecuteToolCall(ctx, memToolCall("memory_put",
		`{"key":"test/oracle","value":"oracle-derived","kind":"note"}`))
	if !strings.Contains(result.text, "tainted") {
		t.Fatalf("after oracle grounding should be tainted, got: %s", result.text)
	}
}

// ─── Taint sticky across multiple puts (never reset) ───────────────────────

func TestTaintStickyNeverReset(t *testing.T) {
	app, _ := memoryTestApp(t, false)
	ctx := memCtx(t)
	app.Client = &proxy.Client{}

	// Touch external content.
	app.addExternalGrounding(proxy.GroundingEntry{Type: "web", Label: "x"})

	// First put: tainted.
	result := app.ExecuteToolCall(ctx, memToolCall("memory_put",
		`{"key":"test/a","value":"v","kind":"note"}`))
	if !strings.Contains(result.text, "tainted") {
		t.Fatalf("first put after grounding should be tainted, got: %s", result.text)
	}

	// Second put much later: still tainted (sticky, never reset).
	result = app.ExecuteToolCall(ctx, memToolCall("memory_put",
		`{"key":"test/b","value":"v","kind":"note"}`))
	if !strings.Contains(result.text, "tainted") {
		t.Fatalf("second put should still be tainted (sticky), got: %s", result.text)
	}

	// Even if we call computeTainted again, it stays tainted.
	result = app.ExecuteToolCall(ctx, memToolCall("memory_put",
		`{"key":"test/c","value":"v","kind":"note"}`))
	if !strings.Contains(result.text, "tainted") {
		t.Fatalf("third put should still be tainted (sticky never reset), got: %s", result.text)
	}
}

// ─── Nil-store for main-only tools ─────────────────────────────────────────

func TestNilStoreMainOnlyTools(t *testing.T) {
	app := &App{
		MemoryStore: nil,
		AgentPrefix: "main",
		IsSubagent:  false,
	}
	ctx := memCtx(t)

	// All main-only tools should return memory unavailable.
	for _, tool := range []string{"memory_promote", "memory_reject", "memory_forget"} {
		result := app.ExecuteToolCall(ctx, memToolCall(tool, `{"id":1,"key":"x"}`))
		if !strings.Contains(result.text, "memory unavailable") {
			t.Fatalf("%s with nil store should return 'memory unavailable', got: %s", tool, result.text)
		}
	}
}

// ─── Main-agent reject and forget happy paths ──────────────────────────────

func TestMainAgentRejectAndForget(t *testing.T) {
	app, _ := memoryTestApp(t, false)
	ctx := memCtx(t)

	// Put a proposed entry.
	app.ExecuteToolCall(ctx, memToolCall("memory_put",
		`{"key":"test/reject-me","value":"bad idea","kind":"decision"}`))

	// Reject it.
	result := app.ExecuteToolCall(ctx, memToolCall("memory_reject",
		`{"id":1,"reason":"not viable"}`))
	if strings.Contains(result.text, "ERROR") {
		t.Fatalf("reject failed: %s", result.text)
	}

	// Put an active entry with TTL.
	app.ExecuteToolCall(ctx, memToolCall("memory_put",
		`{"key":"test/forget-me","value":"data","kind":"note","ttl_seconds":3600}`))

	// Forget it.
	result = app.ExecuteToolCall(ctx, memToolCall("memory_forget",
		`{"key":"test/forget-me"}`))
	if strings.Contains(result.text, "ERROR") {
		t.Fatalf("forget failed: %s", result.text)
	}

	// Get should return not found.
	result = app.ExecuteToolCall(ctx, memToolCall("memory_get",
		`{"key":"test/forget-me"}`))
	if !strings.Contains(result.text, "not found") {
		t.Fatalf("get after forget should be not found, got: %s", result.text)
	}
}

// ─── Promote with edited value ─────────────────────────────────────────────

func TestPromoteWithEditedValue(t *testing.T) {
	app, _ := memoryTestApp(t, false)
	ctx := memCtx(t)

	// Put a proposed entry.
	app.ExecuteToolCall(ctx, memToolCall("memory_put",
		`{"key":"test/edit-promote","value":"original","kind":"note"}`))

	// Promote with an edited value.
	result := app.ExecuteToolCall(ctx, memToolCall("memory_promote",
		`{"id":1,"edited_value":"edited by main"}`))
	if strings.Contains(result.text, "ERROR") {
		t.Fatalf("promote with edit failed: %s", result.text)
	}
	if !strings.Contains(result.text, "edited by main") {
		t.Fatalf("promoted entry should show edited value, got: %s", result.text)
	}

	// Get should return the edited value.
	result = app.ExecuteToolCall(ctx, memToolCall("memory_get",
		`{"key":"test/edit-promote"}`))
	if !strings.Contains(result.text, "edited by main") {
		t.Fatalf("get should return edited value, got: %s", result.text)
	}
}

// ─── Staging bridge end-to-end (requires kvr-server) ───────────────────────

func TestStagingBridgeEndToEnd(t *testing.T) {
	stagingApp, stagingCleanup := stagingTestServer(t)
	defer stagingCleanup()

	// Create a memory store alongside the staging server.
	dir := t.TempDir()
	dbPath := dir + "/memory/test.db"
	store, err := memory.Open(dbPath, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	stagingApp.MemoryStore = store
	stagingApp.AgentPrefix = "main"
	stagingApp.IsSubagent = false

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Put a value in staging under a subagent prefix.
	stagingApp.AgentPrefix = "sub-abc12345"
	putResult := stagingApp.ExecuteToolCall(ctx, memToolCall("staging_put",
		`{"key":"research","value":"subagent findings data"}`))
	if strings.Contains(putResult.text, "ERROR") {
		t.Fatalf("staging put failed: %s", putResult.text)
	}

	// Switch to main agent for the bridge.
	stagingApp.AgentPrefix = "main"
	stagingApp.IsSubagent = false

	// Promote from staging.
	result := stagingApp.ExecuteToolCall(ctx, memToolCall("memory_promote_from_staging",
		`{"staging_key":"sub-abc12345/research","key":"arch/research","kind":"summary"}`))
	if strings.Contains(result.text, "ERROR") {
		t.Fatalf("promote from staging failed: %s", result.text)
	}

	// Verify the proposed entry has writer=sub-abc12345.
	if !strings.Contains(result.text, "writer=sub-abc12345") {
		t.Fatalf("bridge should show writer=sub-abc12345, got: %s", result.text)
	}
	// Verify taint is unknown.
	if !strings.Contains(result.text, "taint-unknown") {
		t.Fatalf("bridge should show taint-unknown, got: %s", result.text)
	}
	// Verify the value is present.
	if !strings.Contains(result.text, "subagent findings data") {
		t.Fatalf("bridge should show staging value, got: %s", result.text)
	}

	// Verify the staging key was NOT deleted.
	getResult := stagingApp.ExecuteToolCall(ctx, memToolCall("staging_get",
		`{"key":"sub-abc12345/research"}`))
	if strings.Contains(getResult.text, "not found") {
		t.Fatalf("staging key should still exist after bridge, got: %s", getResult.text)
	}

	// Verify the entry is proposed (not active) — memory_get should not find it.
	memGetResult := stagingApp.ExecuteToolCall(ctx, memToolCall("memory_get",
		`{"key":"arch/research"}`))
	if !strings.Contains(memGetResult.text, "not found") {
		t.Fatalf("bridge entry should be proposed (not found by get), got: %s", memGetResult.text)
	}

	// Promote it to active.
	listResult := stagingApp.ExecuteToolCall(ctx, memToolCall("memory_list",
		`{"status":"proposed"}`))
	if !strings.Contains(listResult.text, "arch/research") {
		t.Fatalf("proposed entry should be listable, got: %s", listResult.text)
	}
}

// ─── memory_list basic ─────────────────────────────────────────────────────

func TestMemoryList(t *testing.T) {
	app, _ := memoryTestApp(t, false)
	ctx := memCtx(t)

	// Put some entries.
	app.ExecuteToolCall(ctx, memToolCall("memory_put",
		`{"key":"arch/a","value":"v1","kind":"note","ttl_seconds":3600}`))
	app.ExecuteToolCall(ctx, memToolCall("memory_put",
		`{"key":"arch/b","value":"v2","kind":"note","ttl_seconds":3600}`))
	app.ExecuteToolCall(ctx, memToolCall("memory_put",
		`{"key":"decision/c","value":"v3","kind":"decision","ttl_seconds":3600}`))

	// List all active.
	result := app.ExecuteToolCall(ctx, memToolCall("memory_list",
		`{}`))
	if !strings.Contains(result.text, "arch/a") || !strings.Contains(result.text, "arch/b") || !strings.Contains(result.text, "decision/c") {
		t.Fatalf("list should show all entries, got: %s", result.text)
	}

	// List by prefix.
	result = app.ExecuteToolCall(ctx, memToolCall("memory_list",
		`{"prefix":"arch/"}`))
	if !strings.Contains(result.text, "arch/a") || !strings.Contains(result.text, "arch/b") {
		t.Fatalf("list with prefix should show arch entries, got: %s", result.text)
	}
	if strings.Contains(result.text, "decision/c") {
		t.Fatalf("list with prefix should not show decision entries, got: %s", result.text)
	}
}

// ─── include_proposed as boolean ───────────────────────────────────────────

func TestSearchIncludeProposedBoolean(t *testing.T) {
	app, _ := memoryTestApp(t, false)
	ctx := memCtx(t)

	// Put a proposed entry (no TTL).
	app.ExecuteToolCall(ctx, memToolCall("memory_put",
		`{"key":"test/bool-proposed","value":"proposed data","kind":"note"}`))

	// Search active only — should not find it.
	result := app.ExecuteToolCall(ctx, memToolCall("memory_search",
		`{"query":"proposed"}`))
	if strings.Contains(result.text, "proposed data") {
		t.Fatalf("search without include_proposed should not find proposed, got: %s", result.text)
	}

	// Search with include_proposed=true (boolean).
	result = app.ExecuteToolCall(ctx, memToolCall("memory_search",
		`{"query":"proposed","include_proposed":true}`))
	if !strings.Contains(result.text, "proposed data") {
		t.Fatalf("search with include_proposed=true should find proposed, got: %s", result.text)
	}
}
