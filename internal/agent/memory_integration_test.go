package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/memory"
	"github.com/treeol/wakil/internal/proxy"
)

// TestStagingToDurableBridgeIntegration tests the full handoff:
// subagent staging_put → main memory_promote_from_staging → proposed entry
// has writer=sub-prefix, promoted_by=main, tainted=unknown → memory_promote
// → active, provenance rendered correctly end-to-end.
func TestStagingToDurableBridgeIntegration(t *testing.T) {
	// Start a real kvr-server for staging.
	stagingApp, stagingCleanup := stagingTestServer(t)
	defer stagingCleanup()

	// Create a memory store alongside.
	dir := t.TempDir()
	dbPath := dir + "/memory/test.db"
	store, err := memory.Open(dbPath, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Phase 1: Subagent writes to staging.
	stagingApp.AgentPrefix = "sub-abc12345"
	stagingApp.IsSubagent = true
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	stagingPutResult := stagingApp.ExecuteToolCall(ctx, memToolCall("staging_put",
		`{"key":"research-findings","value":"The auth module uses JWT with refresh tokens. Token expiry is 15min, refresh is 7d."}`))
	if strings.Contains(stagingPutResult, "ERROR") {
		t.Fatalf("subagent staging_put failed: %s", stagingPutResult)
	}
	if !strings.Contains(stagingPutResult, "sub-abc12345/research-findings") {
		t.Fatalf("staging put should show prefixed key, got: %s", stagingPutResult)
	}

	// Phase 2: Main agent bridges from staging to durable (proposed).
	stagingApp.AgentPrefix = "main"
	stagingApp.IsSubagent = false
	stagingApp.MemoryStore = store

	bridgeResult := stagingApp.ExecuteToolCall(ctx, memToolCall("memory_promote_from_staging",
		`{"staging_key":"sub-abc12345/research-findings","key":"arch/auth-flow","kind":"summary"}`))
	if strings.Contains(bridgeResult, "ERROR") {
		t.Fatalf("memory_promote_from_staging failed: %s", bridgeResult)
	}

	// Verify writer=sub-abc12345 (provenance flows through from staging prefix).
	if !strings.Contains(bridgeResult, "writer=sub-abc12345") {
		t.Fatalf("bridge should show writer=sub-abc12345, got: %s", bridgeResult)
	}

	// Verify promoted_by=main.
	if !strings.Contains(bridgeResult, "promoted_by=main") {
		t.Fatalf("bridge should show promoted_by=main, got: %s", bridgeResult)
	}

	// Verify taint is unknown (staging values carry no taint metadata).
	if !strings.Contains(bridgeResult, "taint-unknown") {
		t.Fatalf("bridge should show taint-unknown, got: %s", bridgeResult)
	}

	// Verify the value is present.
	if !strings.Contains(bridgeResult, "JWT with refresh tokens") {
		t.Fatalf("bridge should show staging value, got: %s", bridgeResult)
	}

	// Phase 3: The entry should be proposed (not active) — memory_get won't find it.
	getResult := stagingApp.ExecuteToolCall(ctx, memToolCall("memory_get",
		`{"key":"arch/auth-flow"}`))
	if !strings.Contains(getResult, "not found") {
		t.Fatalf("proposed entry should not be found by memory_get, got: %s", getResult)
	}

	// Phase 4: List proposed entries to find the ID.
	listResult := stagingApp.ExecuteToolCall(ctx, memToolCall("memory_list",
		`{"status":"proposed"}`))
	if !strings.Contains(listResult, "arch/auth-flow") {
		t.Fatalf("proposed entry should be listable, got: %s", listResult)
	}

	// Phase 5: Promote to active.
	// The entry ID should be 1 (first entry in a fresh store).
	promoteResult := stagingApp.ExecuteToolCall(ctx, memToolCall("memory_promote",
		`{"id":1}`))
	if strings.Contains(promoteResult, "ERROR") {
		t.Fatalf("memory_promote failed: %s", promoteResult)
	}
	if !strings.Contains(promoteResult, "active") {
		t.Fatalf("promoted entry should be active, got: %s", promoteResult)
	}

	// Phase 6: memory_get should now return the active entry with full provenance.
	finalGet := stagingApp.ExecuteToolCall(ctx, memToolCall("memory_get",
		`{"key":"arch/auth-flow"}`))
	if !strings.Contains(finalGet, "JWT with refresh tokens") {
		t.Fatalf("get should return the value, got: %s", finalGet)
	}
	if !strings.Contains(finalGet, "durable-tier") {
		t.Fatalf("get should show durable-tier, got: %s", finalGet)
	}
	if !strings.Contains(finalGet, "sub-abc12345") {
		t.Fatalf("get should show original writer sub-abc12345, got: %s", finalGet)
	}
	if !strings.Contains(finalGet, "promoted by main") {
		t.Fatalf("get should show promoted by main, got: %s", finalGet)
	}

	// Phase 7: Staging key should NOT have been deleted (bridge does not delete).
	stagingGetResult := stagingApp.ExecuteToolCall(ctx, memToolCall("staging_get",
		`{"key":"sub-abc12345/research-findings"}`))
	if strings.Contains(stagingGetResult, "not found") {
		t.Fatalf("staging key should still exist after bridge, got: %s", stagingGetResult)
	}
}

// TestMemoryDigestInPreamble tests that the memory digest appears in the
// preamble when the store has entries.
func TestMemoryDigestInPreamble(t *testing.T) {
	app, _ := memoryTestApp(t, false)
	ctx := memCtx(t)

	// Write some durable entries.
	app.ExecuteToolCall(ctx, memToolCall("memory_put",
		`{"key":"arch/test","value":"data","kind":"note"}`))
	app.ExecuteToolCall(ctx, memToolCall("memory_put",
		`{"key":"decision/test","value":"data","kind":"decision"}`))

	// Promote one.
	app.ExecuteToolCall(ctx, memToolCall("memory_promote",
		`{"id":1}`))

	// Build preamble — should include memory digest.
	preamble := app.buildPreamble("Monday, 14 July 2026")
	if !strings.Contains(preamble, "Memory:") {
		t.Fatalf("preamble should contain memory digest, got: %s", preamble)
	}
	if !strings.Contains(preamble, "active durable") {
		t.Fatalf("preamble should show active durable count, got: %s", preamble)
	}
	if !strings.Contains(preamble, "pending proposals") {
		t.Fatalf("preamble should show pending proposals, got: %s", preamble)
	}
}

// TestMemoryDigestEmptyStore tests that the digest is omitted when the store
// is empty or nil.
func TestMemoryDigestEmptyStore(t *testing.T) {
	app := &App{
		MemoryStore: nil,
	}
	preamble := app.buildPreamble("Monday, 14 July 2026")
	if strings.Contains(preamble, "Memory:") {
		t.Fatalf("preamble should not contain memory digest for nil store, got: %s", preamble)
	}
}

// TestTaintDoesNotResetAcrossPuts verifies that once the taint flag is set,
// it persists across multiple memory_put calls (session-cumulative, A1).
func TestTaintDoesNotResetAcrossPuts(t *testing.T) {
	app, _ := memoryTestApp(t, false)
	app.Client = &proxy.Client{}
	ctx := memCtx(t)

	// Simulate web exposure.
	app.Client.AddGrounding(proxy.GroundingEntry{Type: "web", Label: "example.com"})

	// Multiple puts — all should be tainted.
	for i := 0; i < 3; i++ {
		result := app.ExecuteToolCall(ctx, memToolCall("memory_put",
			`{"key":"test/sticky-`+string(rune('a'+i))+`","value":"v","kind":"note"}`))
		if !strings.Contains(result, "tainted") {
			t.Fatalf("put %d should be tainted (sticky), got: %s", i, result)
		}
		if strings.Contains(result, "taint-unknown") {
			t.Fatalf("put %d should not be taint-unknown, got: %s", i, result)
		}
	}
}
