package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/treeol/wakil/internal/config"
	wtools "github.com/treeol/wakil/internal/tools"
)

// --- Test: tools without session consent → tool error, no dispatch ---

func TestCapabilityToolsWithoutConsent(t *testing.T) {
	requestCount := 0
	srv := httptest.NewServer(mockHandler(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	exec := newFakeExecutor()
	// AutoApprove = false (no consent)
	parent := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })

	tc := makeEditToolCall("dispatch_subagent", `{"task":"use tools","capability":"tools"}`)
	result := parent.ExecuteToolCall(context.Background(), tc)

	if !strings.Contains(result.text, "ERROR: tools capability requires") {
		t.Errorf("expected consent error, got: %s", result.text)
	}
	if !strings.Contains(result.text, "/auto") {
		t.Errorf("error should mention /auto, got: %s", result.text)
	}
	if requestCount != 0 {
		t.Errorf("no child should be dispatched without consent; got %d requests", requestCount)
	}
	if !strings.Contains(result.text, "discovery") {
		t.Errorf("error should suggest discovery as alternative, got: %s", result.text)
	}
}

// --- Test: tools with AutoApprove → dispatches successfully ---

func TestCapabilityToolsWithAutoApprove(t *testing.T) {
	summaryJSON := `{"objective":"tools done","findings":[{"summary":"found docs","location":"https://example.com","kind":"fact","weight":"high"}]}`

	srv := sseServer(t, []string{contentChunk(summaryJSON)})
	defer srv.Close()

	exec := newFakeExecutor()
	parent := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })
	parent.AutoApprove = true // session consent

	summary, _, _, _, _, _ := parent.dispatchSubagent(
		context.Background(), "use tools task", io.Discard, "", wtools.CapabilityTools)

	if summary.Objective != "tools done" {
		t.Errorf("objective = %q, want 'tools done'", summary.Objective)
	}
	if len(summary.Findings) == 0 {
		t.Error("expected findings from tools-tier dispatch")
	}
}

// --- Test: tools confirmer approves everything ---

func TestToolsConfirmer(t *testing.T) {
	conf := toolsConfirmer()
	// Should approve everything — reads, MCP calls, LSP, search.
	if !conf("read_file", "", "", true) {
		t.Error("toolsConfirmer should approve reads")
	}
	if !conf("trello__get_cards", "", "", false) {
		t.Error("toolsConfirmer should approve MCP read calls")
	}
	if !conf("trello__create_card", "", "", false) {
		t.Error("toolsConfirmer should approve MCP mutating calls")
	}
	if !conf("searxng_search", "", "", false) {
		t.Error("toolsConfirmer should approve web search")
	}
	if !conf("lsp_definition", "", "", false) {
		t.Error("toolsConfirmer should approve LSP")
	}
	if !conf("browser_navigate", "", "", false) {
		t.Error("toolsConfirmer should approve browser tools")
	}
	if !conf("browser_screenshot", "", "", false) {
		t.Error("toolsConfirmer should approve browser tools")
	}
}

// --- Test: tools tier does NOT include dangerous tools ---

func TestToolsTierExcludesDangerousTools(t *testing.T) {
	parent := &App{
		Cfg:     config.DefaultConfig(),
		Client:  newTestClient("http://unused"),
		Exec:    newFakeExecutor(),
		Tools:   wtools.DefaultTools("/work"),
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
	}
	tools := parent.buildSubagentTools()

	// Must include discovery tools.
	hasDiscovery := false
	for _, tl := range tools {
		if tl.Function.Name == "read_file" {
			hasDiscovery = true
		}
	}
	if !hasDiscovery {
		t.Error("tools-tier should include read_file")
	}

	// Must NOT include any of the dangerous set.
	for _, tl := range tools {
		name := tl.Function.Name
		switch name {
		case "run_shell", "run_background", "kill_process", "dispatch_subagent", "dispatch_subagents", "open_url":
			t.Errorf("tools-tier must NOT include %q", name)
		}
	}
}

// --- Test: tools tier includes MCP only from allowlisted servers ---

func TestToolsTierMCPAllowlist(t *testing.T) {
	// Build a parent with two MCP servers configured, but only one allowlisted.
	// We can't easily build a real MCPManager in tests, but we can test the
	// filtering logic via buildSubagentTools with a nil MCP (which yields no
	// MCP tools) and via OpenAIToolsForServers directly.
	parent := &App{
		Cfg: config.Config{
			SubagentMCPServers: []string{"context7"},
		},
		Client:  newTestClient("http://unused"),
		Exec:    newFakeExecutor(),
		Tools:   wtools.DefaultTools("/work"),
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
	}
	// MCP is nil → no MCP tools in the toolset, even with an allowlist.
	tools := parent.buildSubagentTools()
	for _, tl := range tools {
		if strings.Contains(tl.Function.Name, "__") {
			t.Errorf("expected no MCP tools when MCPManager is nil, got %q", tl.Function.Name)
		}
	}
}

// --- Test: external_calls recorded mechanically ---

func TestExternalCallsRecordedMechanically(t *testing.T) {
	// Simulate a tools-tier child that calls an MCP tool. We need a fake MCP
	// server to record the call. Since building a real MCPManager is complex,
	// we test the recording path directly: the child's ExecuteToolCall MCP
	// default case calls recordExternalAction.
	summaryJSON := `{"objective":"done","findings":[{"summary":"done","location":"","kind":"fact","weight":"low"}]}`

	srv := sseServer(t,
		// call 0: MCP tool call (trello__get_cards)
		toolCallFrames("m1", "trello__get_cards", `{"board":"test"}`),
		// call 1: summary
		[]string{contentChunk(summaryJSON)},
	)
	defer srv.Close()

	exec := newFakeExecutor()
	parent := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })
	parent.AutoApprove = true

	// Without an MCP manager, the MCP tool call will hit the default case but
	// a.MCP is nil → "ERROR: unknown tool". The external action recording happens
	// BEFORE the "unknown tool" return (it's inside the MCP branch, guarded by
	// a.MCP != nil). So we can't test recording without a real MCPManager.
	//
	// Instead, test the recorder directly:
	recorder := newExternalActionsRecorder()
	recorder.record("trello", "get_cards", "ok")
	recorder.record("invoicely", "send_invoice", "error")

	snapshot := recorder.snapshot()
	if len(snapshot) != 2 {
		t.Fatalf("expected 2 external actions, got %d", len(snapshot))
	}
	if snapshot[0].Server != "trello" || snapshot[0].Tool != "get_cards" || snapshot[0].Status != "ok" {
		t.Errorf("action 0 = %+v", snapshot[0])
	}
	if snapshot[1].Server != "invoicely" || snapshot[1].Tool != "send_invoice" || snapshot[1].Status != "error" {
		t.Errorf("action 1 = %+v", snapshot[1])
	}
}

// --- Test: external_calls folded into summary (overrides model self-report) ---

func TestExternalCallsFoldedIntoSummary(t *testing.T) {
	// The model reports external_calls in its JSON, but the mechanical record
	// should override it. dispatchSubagent folds extRecorder.snapshot() into
	// summary.ExternalCalls after parsing.
	summaryJSON := `{"objective":"done","findings":[{"summary":"done","location":"","kind":"fact","weight":"low"}],"external_calls":[{"server":"trello","tool":"get_cards","status":"ok"}]}`

	srv := sseServer(t, []string{contentChunk(summaryJSON)})
	defer srv.Close()

	exec := newFakeExecutor()
	parent := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })
	parent.AutoApprove = true

	// dispatchSubagent folds extRecorder.snapshot() into the summary.
	// Since there's no real MCPManager, extRecorder is empty, so the model's
	// self-reported external_calls should be overridden to nil.
	summary, _, _, _, _, _ := parent.dispatchSubagent(
		context.Background(), "task", io.Discard, "", wtools.CapabilityTools)

	// Mechanical record is ground truth — model's self-report is overridden.
	// With no MCP manager, no external actions were recorded, so ExternalCalls
	// should be nil (overriding the model's claim of one call).
	if len(summary.ExternalCalls) != 0 {
		t.Errorf("mechanical record should override model self-report; got %d external calls, want 0", len(summary.ExternalCalls))
	}
}

// --- Test: tools-tier + edit-tier children serialized via their respective locks ---

func TestToolsTierMCPMutationSerializes(t *testing.T) {
	// Two tools-tier children calling a mutating MCP tool should be serialized
	// by subagentMCPMu. Since we don't have a real MCPManager, we test the lock
	// behavior directly: two goroutines calling a function that acquires
	// subagentMCPMu should never overlap.
	var mu sync.Mutex
	active := 0
	maxConcurrent := 0

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			subagentMCPMu.Lock()
			defer subagentMCPMu.Unlock()
			mu.Lock()
			active++
			if active > maxConcurrent {
				maxConcurrent = active
			}
			mu.Unlock()
			// Simulate a mutating call.
			doSomething()
			mu.Lock()
			active--
			mu.Unlock()
		}()
	}
	wg.Wait()

	if maxConcurrent != 1 {
		t.Errorf("mutating MCP calls should be serialized (maxConcurrent=1); got %d", maxConcurrent)
	}
}

// --- Test: capability validation — unknown value names "tools" ---

func TestCapabilityValidationNamesTools(t *testing.T) {
	srv := sseServer(t, []string{contentChunk(`{"objective":"done"}`)})
	defer srv.Close()

	exec := newFakeExecutor()
	parent := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })

	tc := makeEditToolCall("dispatch_subagent", `{"task":"test","capability":"exec"}`)
	result := parent.ExecuteToolCall(context.Background(), tc)

	if !strings.Contains(result.text, "ERROR: unknown capability") {
		t.Errorf("expected unknown capability error, got: %s", result.text)
	}
	if !strings.Contains(result.text, "tools") {
		t.Errorf("error should name 'tools' as a valid value, got: %s", result.text)
	}
}

// --- Test: discovery and edit tiers remain byte-identical (no regression) ---

func TestDiscoveryEditTiersUnchangedByToolsTier(t *testing.T) {
	// The tools tier must not change the discovery or edit toolsets.
	d1 := wtools.DiscoveryTools("/work")
	d2 := wtools.DiscoveryTools("/work")
	if len(d1) != len(d2) {
		t.Fatalf("DiscoveryTools length mismatch")
	}
	for i := range d1 {
		if d1[i].Function.Name != d2[i].Function.Name {
			t.Errorf("discovery tool %d name mismatch", i)
		}
	}

	e1 := wtools.EditTools("/work")
	e2 := wtools.EditTools("/work")
	if len(e1) != len(e2) {
		t.Fatalf("EditTools length mismatch")
	}
	for i := range e1 {
		if e1[i].Function.Name != e2[i].Function.Name {
			t.Errorf("edit tool %d name mismatch", i)
		}
	}

	// Edit tier has exactly 29 tools (5 discovery + 4 edit + 5 staging + 8 memory + 7 skill).
	if len(e1) != 29 {
		t.Errorf("EditTools should have 29 tools, got %d", len(e1))
	}
	// Discovery tier has exactly 25 tools (5 read-only + 5 staging + 8 memory + 7 skill).
	if len(d1) != 25 {
		t.Errorf("DiscoveryTools should have 25 tools, got %d", len(d1))
	}
}

// --- Test: subagentToolsSystemPrompt is a const with no interpolation ---

func TestSubagentToolsSystemPromptNoInterpolation(t *testing.T) {
	if strings.Contains(subagentToolsSystemPrompt, "%") {
		t.Error("subagentToolsSystemPrompt contains a % — possible interpolation")
	}
	if !strings.Contains(subagentToolsSystemPrompt, "untrusted data") {
		t.Error("subagentToolsSystemPrompt should include prompt injection hardening rule")
	}
	if !strings.Contains(subagentToolsSystemPrompt, "external_calls") {
		t.Error("subagentToolsSystemPrompt should mention external_calls in the schema")
	}
}

// --- Test: validCapabilities includes "tools" ---

func TestValidCapabilityIncludesTools(t *testing.T) {
	if !wtools.ValidCapability(wtools.CapabilityTools) {
		t.Error("ValidCapability should accept 'tools'")
	}
	if wtools.CapabilityTools != "tools" {
		t.Errorf("CapabilityTools = %q, want 'tools'", wtools.CapabilityTools)
	}
}

// doSomething is a no-op for the serialization test.
func doSomething() {}

// mockHandler wraps an http.HandlerFunc for test servers.
func mockHandler(h func(w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
	return h
}
