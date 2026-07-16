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
	if strings.Contains(stagingPutResult.text, "ERROR") {
		t.Fatalf("subagent staging_put failed: %s", stagingPutResult.text)
	}
	if !strings.Contains(stagingPutResult.text, "sub-abc12345/research-findings") {
		t.Fatalf("staging put should show prefixed key, got: %s", stagingPutResult.text)
	}

	// Phase 2: Main agent bridges from staging to durable (proposed).
	stagingApp.AgentPrefix = "main"
	stagingApp.IsSubagent = false
	stagingApp.MemoryStore = store

	bridgeResult := stagingApp.ExecuteToolCall(ctx, memToolCall("memory_promote_from_staging",
		`{"staging_key":"sub-abc12345/research-findings","key":"arch/auth-flow","kind":"summary"}`))
	if strings.Contains(bridgeResult.text, "ERROR") {
		t.Fatalf("memory_promote_from_staging failed: %s", bridgeResult.text)
	}

	// Verify writer=sub-abc12345 (provenance flows through from staging prefix).
	if !strings.Contains(bridgeResult.text, "writer=sub-abc12345") {
		t.Fatalf("bridge should show writer=sub-abc12345, got: %s", bridgeResult.text)
	}

	// Verify promoted_by=main.
	if !strings.Contains(bridgeResult.text, "promoted_by=main") {
		t.Fatalf("bridge should show promoted_by=main, got: %s", bridgeResult.text)
	}

	// Verify taint is unknown (staging values carry no taint metadata).
	if !strings.Contains(bridgeResult.text, "taint-unknown") {
		t.Fatalf("bridge should show taint-unknown, got: %s", bridgeResult.text)
	}

	// Verify the value is present.
	if !strings.Contains(bridgeResult.text, "JWT with refresh tokens") {
		t.Fatalf("bridge should show staging value, got: %s", bridgeResult.text)
	}

	// Phase 3: The entry should be proposed (not active) — memory_get won't find it.
	getResult := stagingApp.ExecuteToolCall(ctx, memToolCall("memory_get",
		`{"key":"arch/auth-flow"}`))
	if !strings.Contains(getResult.text, "not found") {
		t.Fatalf("proposed entry should not be found by memory_get, got: %s", getResult.text)
	}

	// Phase 4: List proposed entries to find the ID.
	listResult := stagingApp.ExecuteToolCall(ctx, memToolCall("memory_list",
		`{"status":"proposed"}`))
	if !strings.Contains(listResult.text, "arch/auth-flow") {
		t.Fatalf("proposed entry should be listable, got: %s", listResult.text)
	}

	// Phase 5: Promote to active.
	// The entry ID should be 1 (first entry in a fresh store).
	promoteResult := stagingApp.ExecuteToolCall(ctx, memToolCall("memory_promote",
		`{"id":1}`))
	if strings.Contains(promoteResult.text, "ERROR") {
		t.Fatalf("memory_promote failed: %s", promoteResult.text)
	}
	if !strings.Contains(promoteResult.text, "active") {
		t.Fatalf("promoted entry should be active, got: %s", promoteResult.text)
	}

	// Phase 6: memory_get should now return the active entry with full provenance.
	finalGet := stagingApp.ExecuteToolCall(ctx, memToolCall("memory_get",
		`{"key":"arch/auth-flow"}`))
	if !strings.Contains(finalGet.text, "JWT with refresh tokens") {
		t.Fatalf("get should return the value, got: %s", finalGet.text)
	}
	if !strings.Contains(finalGet.text, "durable-tier") {
		t.Fatalf("get should show durable-tier, got: %s", finalGet.text)
	}
	if !strings.Contains(finalGet.text, "sub-abc12345") {
		t.Fatalf("get should show original writer sub-abc12345, got: %s", finalGet.text)
	}
	if !strings.Contains(finalGet.text, "promoted by main") {
		t.Fatalf("get should show promoted by main, got: %s", finalGet.text)
	}

	// Phase 7: Staging key should NOT have been deleted (bridge does not delete).
	stagingGetResult := stagingApp.ExecuteToolCall(ctx, memToolCall("staging_get",
		`{"key":"sub-abc12345/research-findings"}`))
	if strings.Contains(stagingGetResult.text, "not found") {
		t.Fatalf("staging key should still exist after bridge, got: %s", stagingGetResult.text)
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

	// Simulate web exposure (eagerly sets sticky flag via addExternalGrounding).
	app.addExternalGrounding(proxy.GroundingEntry{Type: "web", Label: "example.com"})

	// Multiple puts — all should be tainted.
	for i := 0; i < 3; i++ {
		result := app.ExecuteToolCall(ctx, memToolCall("memory_put",
			`{"key":"test/sticky-`+string(rune('a'+i))+`","value":"v","kind":"note"}`))
		if !strings.Contains(result.text, "tainted") {
			t.Fatalf("put %d should be tainted (sticky), got: %s", i, result.text)
		}
		if strings.Contains(result.text, "taint-unknown") {
			t.Fatalf("put %d should not be taint-unknown, got: %s", i, result.text)
		}
	}
}

// TestTaintSurvivesGroundingResetAcrossTurns is the genuine A1 cross-turn test.
//
// Scenario: agent fetches web content in turn 1 (AddGrounding sets the sticky
// flag eagerly). Turn 2 starts: ResetGrounding clears the Client's per-turn
// grounding slice. Agent calls memory_put in turn 2 — the sticky flag must
// still be set, so the entry must be tainted=true, NOT taint-unknown.
//
// This test would FAIL under the old lazy-scan-at-put implementation because
// touchFromGrounding() would see an empty grounding slice after the reset.
func TestTaintSurvivesGroundingResetAcrossTurns(t *testing.T) {
	app, _ := memoryTestApp(t, false)
	app.Client = &proxy.Client{}
	ctx := memCtx(t)

	// Turn 1: agent fetches a web page. addExternalGrounding eagerly sets
	// a.touchedExternal = true AND adds the grounding entry.
	app.addExternalGrounding(proxy.GroundingEntry{Type: "web", Label: "hostile-page.com"})

	// Verify the flag was set eagerly (before any memory_put).
	if !app.touchedExternal {
		t.Fatal("touchedExternal should be set eagerly by addExternalGrounding")
	}

	// Turn 2 starts: ResetGrounding clears the per-turn grounding slice.
	// This is what happens at agent_async.go:42 at the start of each turn.
	app.Client.ResetGrounding()

	// Verify grounding is now empty (the reset worked).
	if len(app.Client.Grounding()) > 0 {
		t.Fatalf("grounding should be empty after reset, got %d entries", len(app.Client.Grounding()))
	}

	// The sticky flag must survive the reset.
	if !app.touchedExternal {
		t.Fatal("touchedExternal should survive ResetGrounding (sticky, A1)")
	}

	// Turn 2: agent calls memory_put. The entry must be tainted=true,
	// NOT taint-unknown — even though grounding is empty.
	result := app.ExecuteToolCall(ctx, memToolCall("memory_put",
		`{"key":"test/cross-turn","value":"conclusion from web research","kind":"decision"}`))
	if !strings.Contains(result.text, "tainted") {
		t.Fatalf("entry after cross-turn exposure should be tainted, got: %s", result.text)
	}
	if strings.Contains(result.text, "taint-unknown") {
		t.Fatalf("entry should NOT be taint-unknown after cross-turn exposure, got: %s", result.text)
	}
}

// TestTaintCleanAgentStaysUnknown verifies that an agent that never touches
// external content writes taint-unknown (not false — absence of signal is
// not proof of cleanliness).
func TestTaintCleanAgentStaysUnknown(t *testing.T) {
	app, _ := memoryTestApp(t, false)
	app.Client = &proxy.Client{}
	ctx := memCtx(t)

	// No external exposure at all.
	result := app.ExecuteToolCall(ctx, memToolCall("memory_put",
		`{"key":"test/clean","value":"local only","kind":"note"}`))
	if !strings.Contains(result.text, "taint-unknown") {
		t.Fatalf("clean agent should produce taint-unknown, got: %s", result.text)
	}
}
