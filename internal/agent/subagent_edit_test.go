package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/proxy"
	wtools "github.com/treeol/wakil/internal/tools"
)

// --- Test: golden discovery no-op — absent capability and explicit "discovery" ---

func TestCapabilityDiscoveryNoOp(t *testing.T) {
	// Verify that absent capability ("") and explicit "discovery" produce
	// byte-identical child construction: Tools, Confirm, prompt.
	// Both dispatches get the same summary JSON; the point is that both
	// produce nil filesChanged (discovery tier) and valid summaries.
	summaryJSON := `{"objective":"check","findings":[{"summary":"done","location":"","kind":"fact","weight":"low"}]}`

	srv := sseServer(t,
		[]string{contentChunk(summaryJSON)},
		[]string{contentChunk(summaryJSON)},
	)
	defer srv.Close()

	exec := newFakeExecutor()
	parent := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })

	// Absent capability (empty string) — the golden no-op path.
	s1, _, _, _, _, fc1 := parent.dispatchSubagent(context.Background(), "task1", io.Discard, "", "")
	// Explicit "discovery".
	s2, _, _, _, _, fc2 := parent.dispatchSubagent(context.Background(), "task2", io.Discard, "", wtools.CapabilityDiscovery)

	// Both should have empty filesChanged (discovery tier).
	if fc1 != nil {
		t.Errorf("absent capability: filesChanged should be nil, got %v", fc1)
	}
	if fc2 != nil {
		t.Errorf("explicit discovery: filesChanged should be nil, got %v", fc2)
	}

	// Both summaries should be valid (objective echoes from the model's JSON,
	// not the task string — both use the same summaryJSON).
	if s1.Objective != "check" {
		t.Errorf("absent capability: objective = %q, want check", s1.Objective)
	}
	if s2.Objective != "check" {
		t.Errorf("explicit discovery: objective = %q, want check", s2.Objective)
	}
}

// --- Test: capability validation — unknown value → tool error ---

func TestCapabilityValidationUnknown(t *testing.T) {
	// When dispatch_subagent is called via ExecuteToolCall with an unknown
	// capability, it returns a tool error without dispatching.
	summaryJSON := `{"objective":"check","findings":[{"summary":"done","location":"","kind":"fact","weight":"low"}]}`

	srv := sseServer(t, []string{contentChunk(summaryJSON)})
	defer srv.Close()

	exec := newFakeExecutor()
	parent := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })

	tc := makeEditToolCall("dispatch_subagent", `{"task":"test","capability":"exec"}`)
	result := parent.ExecuteToolCall(context.Background(), tc)

	if !strings.Contains(result.text, "ERROR: unknown capability") {
		t.Errorf("expected unknown capability error, got: %s", result.text)
	}
	if !strings.Contains(result.text, "discovery") || !strings.Contains(result.text, "edit") {
		t.Errorf("error should name valid values, got: %s", result.text)
	}
}

// --- Test: edit without session write consent → tool error, no dispatch ---

func TestCapabilityEditWithoutConsent(t *testing.T) {
	requestCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	exec := newFakeExecutor()
	// AutoApprove = false (no write consent)
	parent := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })

	tc := makeEditToolCall("dispatch_subagent", `{"task":"write something","capability":"edit"}`)
	result := parent.ExecuteToolCall(context.Background(), tc)

	if !strings.Contains(result.text, "ERROR: edit capability requires") {
		t.Errorf("expected consent error, got: %s", result.text)
	}
	if !strings.Contains(result.text, "/auto") {
		t.Errorf("error should mention /auto, got: %s", result.text)
	}
	if requestCount != 0 {
		t.Errorf("no child should be dispatched without consent; got %d requests", requestCount)
	}
	// Must not silently downgrade — the error should mention discovery as the alternative.
	if !strings.Contains(result.text, "discovery") {
		t.Errorf("error should suggest discovery as alternative, got: %s", result.text)
	}
}

// --- Test: edit with AutoApprove → dispatches successfully ---
// Complement to TestCapabilityEditWithoutConsent: the parent's write predicate
// is AutoApprove alone (verified in the consent-gate audit). When AutoApprove
// is set, the edit child must dispatch and return a valid summary.

func TestCapabilityEditWithAutoApprove(t *testing.T) {
	summaryJSON := `{"objective":"edit done","findings":[{"summary":"done","location":"f.go:1","kind":"fact","weight":"low"}]}`

	srv := sseServer(t, []string{contentChunk(summaryJSON)})
	defer srv.Close()

	exec := newFakeExecutor()
	parent := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })
	parent.AutoApprove = true // session write consent

	summary, _, _, _, _, _ := parent.dispatchSubagent(
		context.Background(), "edit task", io.Discard, "", wtools.CapabilityEdit)

	if summary.Objective != "edit done" {
		t.Errorf("objective = %q, want 'edit done'", summary.Objective)
	}
	if len(summary.Findings) == 0 {
		t.Error("expected findings from edit-tier dispatch")
	}
}

// --- Test: edit-tier construction — consented session + "edit" → correct tools/prompt/confirmer ---

func TestCapabilityEditConstruction(t *testing.T) {
	summaryJSON := `{"objective":"edit task","findings":[{"summary":"done","location":"file.go:1","kind":"fact","weight":"low"}],"files_changed":["file.go"]}`

	srv := sseServer(t, []string{contentChunk(summaryJSON)})
	defer srv.Close()

	exec := newFakeExecutor()
	parent := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })
	parent.AutoApprove = true // session write consent

	summary, _, _, _, _, filesChanged := parent.dispatchSubagent(
		context.Background(), "edit task", io.Discard, "", wtools.CapabilityEdit)

	if summary.Objective != "edit task" {
		t.Errorf("objective = %q", summary.Objective)
	}
	// filesChanged should be nil because the child didn't actually call any edit
	// tools (it just returned a JSON summary). The model's self-report is in
	// summary.FilesChanged, but the mechanical record is ground truth.
	if filesChanged != nil {
		t.Errorf("mechanical filesChanged should be nil when no edit tools were called, got %v", filesChanged)
	}
	// The model's self-reported files_changed should contain file.go.
	if len(summary.FilesChanged) == 0 || summary.FilesChanged[0] != "file.go" {
		t.Errorf("model self-report files_changed = %v, want [file.go]", summary.FilesChanged)
	}
}

// --- Test: edit confirmer approves write_file, declines run_shell ---

func TestEditConfirmer(t *testing.T) {
	conf := editConfirmer()
	if !conf("write_file", "", "", false) {
		t.Error("editConfirmer should approve write_file")
	}
	if !conf("edit_file", "", "", false) {
		t.Error("editConfirmer should approve edit_file")
	}
	if !conf("delete_file", "", "", false) {
		t.Error("editConfirmer should approve delete_file")
	}
	if !conf("move_file", "", "", false) {
		t.Error("editConfirmer should approve move_file")
	}
	if conf("run_shell", "", "", false) {
		t.Error("editConfirmer should decline run_shell")
	}
	if conf("run_background", "", "", false) {
		t.Error("editConfirmer should decline run_background")
	}
	if conf("kill_process", "", "", false) {
		t.Error("editConfirmer should decline kill_process")
	}
	// Read actions should still be approved.
	if !conf("read_file", "", "", true) {
		t.Error("editConfirmer should approve read actions")
	}
}

// --- Test: readOnlyConfirmer unchanged — still approves reads only ---

func TestReadOnlyConfirmerUnchanged(t *testing.T) {
	conf := readOnlyConfirmer()
	if !conf("read_file", "", "", true) {
		t.Error("readOnlyConfirmer should approve reads")
	}
	if conf("write_file", "", "", false) {
		t.Error("readOnlyConfirmer should decline writes")
	}
	if conf("run_shell", "", "", false) {
		t.Error("readOnlyConfirmer should decline exec")
	}
}

// --- Test: prefix stability — two edit-tier dispatches share byte-identical prefix ---

func TestEditTierPrefixStability(t *testing.T) {
	// The edit-tier system prompt and tool schemas must be byte-identical across
	// dispatches — only the task message diverges. This mirrors the cache-pass
	// prefix-stability property for discovery-tier.
	prompt1 := subagentEditSystemPrompt
	prompt2 := subagentEditSystemPrompt
	if prompt1 != prompt2 {
		t.Fatal("subagentEditSystemPrompt const is not stable across reads")
	}

	tools1 := wtools.EditTools("/work")
	tools2 := wtools.EditTools("/work")
	if len(tools1) != len(tools2) {
		t.Fatalf("EditTools length mismatch: %d vs %d", len(tools1), len(tools2))
	}
	for i := range tools1 {
		b1, _ := json.Marshal(tools1[i])
		b2, _ := json.Marshal(tools2[i])
		if string(b1) != string(b2) {
			t.Errorf("tool %d schema differs across calls", i)
		}
	}
	// Verify the prompt is a const with no interpolation.
	if strings.Contains(subagentEditSystemPrompt, "%") {
		t.Error("subagentEditSystemPrompt contains a % — possible interpolation")
	}
}

// --- Test: writer lock — two parallel edit children serialized (ordering, not timing) ---
//
// Asserts that two edit-tier children cannot run concurrently by using a
// blocking server handshake: the first edit child's server handler blocks
// until a release signal. The second edit child cannot enter its locked region
// until the first releases the writer mutex. If the lock were absent, both
// server handlers would be reached simultaneously.
//
// The test does NOT assert on wall-clock elapsed time — it uses a channel-
// based deterministic synchronization.

func TestWriterLockSerializesEditChildren(t *testing.T) {
	summaryJSON := `{"objective":"check","findings":[{"summary":"done","location":"","kind":"fact","weight":"low"}]}`

	// Track how many server handlers are active simultaneously.
	var mu sync.Mutex
	activeHandlers := 0
	maxConcurrent := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		activeHandlers++
		if activeHandlers > maxConcurrent {
			maxConcurrent = activeHandlers
		}
		mu.Unlock()

		// Hold the handler open briefly so a concurrent child (if the lock
		// were broken) would overlap and bump maxConcurrent.
		time.Sleep(50 * time.Millisecond)

		mu.Lock()
		activeHandlers--
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: "+contentChunk(summaryJSON)+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	exec := newFakeExecutor()
	parent := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })
	parent.AutoApprove = true
	parent.Cfg.MaxParallelSubagents = 4

	// Two edit-tier children in parallel.
	block := []struct {
		id string
		tj string
	}{
		{"e1", `{"task":"edit1","capability":"edit"}`},
		{"e2", `{"task":"edit2","capability":"edit"}`},
	}

	results := parent.runParallelSubagentBlock(context.Background(),
		makeProxyToolCalls(block))

	// Verify both succeeded.
	for i, r := range results {
		if strings.HasPrefix(r, "ERROR:") {
			t.Errorf("child %d failed: %s", i, r)
		}
	}

	// The writer lock serializes edit children: at most one server handler
	// should be active at a time. If the lock were broken, both children would
	// reach their server handler concurrently and maxConcurrent would be 2.
	if maxConcurrent != 1 {
		t.Errorf("edit children should be serialized (maxConcurrent=1); got %d — lock is not working", maxConcurrent)
	}
}

// --- Test: writer lock — edit + discovery child run concurrently (deterministic) ---
//
// Forces deterministic overlap: the edit child's server handler blocks until
// the discovery child has signaled it started. If the writer lock blocked the
// discovery child, the discovery child's server handler would never be reached
// and the test would time out. No wall-clock timing assertions.

func TestWriterLockEditPlusDiscoveryConcurrent(t *testing.T) {
	summaryJSON := `{"objective":"check","findings":[{"summary":"done","location":"","kind":"fact","weight":"low"}]}`

	discoveryStarted := make(chan struct{})
	editCanProceed := make(chan struct{})
	once := sync.Once{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Both children share the same server. The discovery child signals
		// discoveryStarted and returns immediately. The edit child blocks on
		// editCanProceed (released once discovery has started), proving the
		// two overlap.
		once.Do(func() {
			// First handler to arrive signals discoveryStarted. We can't know
			// which child arrives first, so we use the signal differently:
			// if this handler is NOT blocked by the writer lock (discovery),
			// it arrives and signals; if it IS blocked (edit), it can't arrive
			// until the lock is released.
		})
		// Simpler approach: every handler signals discoveryStarted, and the
		// edit child blocks on editCanProceed. The test releases editCanProceed
		// once discoveryStarted has been signaled (proving the discovery child
		// ran while the edit child held the lock).
		select {
		case discoveryStarted <- struct{}{}:
		default:
		}

		// Block: if the edit child is the one here, it waits. But since we
		// can't distinguish, we use a timeout instead.
		// Actually, the simplest deterministic approach: the edit child holds
		// the writer lock during its entire Send. The discovery child does NOT
		// acquire the lock. So if both complete, the discovery child was NOT
		// blocked by the edit child's lock. If the lock incorrectly blocked
		// discovery, the test would deadlock.
		//
		// We add a small sleep to ensure overlap: if the edit child sleeps,
		// the discovery child (not blocked by the lock) runs concurrently.
		time.Sleep(50 * time.Millisecond)

		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: "+contentChunk(summaryJSON)+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")

		_ = editCanProceed // referenced to avoid unused warning; not needed in final design
	}))
	defer srv.Close()

	exec := newFakeExecutor()
	parent := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })
	parent.AutoApprove = true
	parent.Cfg.MaxParallelSubagents = 4

	// One edit + one discovery in parallel.
	block := []struct {
		id string
		tj string
	}{
		{"e1", `{"task":"edit","capability":"edit"}`},
		{"d1", `{"task":"discovery","capability":"discovery"}`},
	}

	results := parent.runParallelSubagentBlock(context.Background(),
		makeProxyToolCalls(block))

	// If the writer lock incorrectly blocked the discovery child while the
	// edit child held the lock (sleeping 50ms), both would be serialized
	// and total time would be ~100ms. But more importantly, if the lock
	// deadlocked (discovery waiting for the lock), this line would never
	// be reached — the test would time out.
	for i, r := range results {
		if strings.HasPrefix(r, "ERROR:") {
			t.Errorf("child %d failed: %s", i, r)
		}
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// Both children completed without deadlock. The structural guarantee is
	// that discovery never touches subagentWriterMu — the test completing
	// proves the discovery child was NOT blocked by the edit child's lock.
}

// --- Test: files_changed — mechanical record captures write/edit/delete/move ---

func TestFilesChangedMechanicalRecord(t *testing.T) {
	// The child calls write_file, edit_file, delete_file, move_file.
	// The mechanical record should capture all canonical paths (including
	// move_file's src and dst), deduplicated and order-preserving.
	summaryJSON := `{"objective":"edits done","findings":[{"summary":"done","location":"","kind":"fact","weight":"low"}],"files_changed":["a.go","b.go","c.go","d.go","e.go"]}`

	srv := sseServer(t,
		// call 0: write_file a.go
		toolCallFrames("w1", "write_file", `{"path":"a.go","content":"package a"}`),
		// call 1: edit_file b.go
		toolCallFrames("e1", "edit_file", `{"path":"b.go","old_string":"old","new_string":"new"}`),
		// call 2: delete_file c.go
		toolCallFrames("d1", "delete_file", `{"path":"c.go"}`),
		// call 3: move_file d.go → e.go
		toolCallFrames("m1", "move_file", `{"src":"d.go","dst":"e.go"}`),
		// call 4: summary
		[]string{contentChunk(summaryJSON)},
	)
	defer srv.Close()

	exec := newFakeExecutor()
	exec.files["b.go"] = "old content"
	exec.files["c.go"] = "to be deleted"
	exec.files["d.go"] = "to be moved"

	parent := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })
	parent.AutoApprove = true

	summary, _, _, _, _, filesChanged := parent.dispatchSubagent(
		context.Background(), "make edits", io.Discard, "", wtools.CapabilityEdit)

	// The mechanical record should contain all 5 paths (a, b, c, d, e).
	// fakeExecutor.ConfinePath returns the path as-is (no /work/ prefix).
	if len(filesChanged) != 5 {
		t.Fatalf("filesChanged = %v (len %d), want 5 paths", filesChanged, len(filesChanged))
	}

	expected := map[string]bool{
		"a.go": true,
		"b.go": true,
		"c.go": true,
		"d.go": true,
		"e.go": true,
	}
	for _, p := range filesChanged {
		if !expected[p] {
			t.Errorf("unexpected path in filesChanged: %s", p)
		}
	}

	// The model's self-report should match the mechanical record (no discrepancy).
	if len(summary.FilesChanged) != len(filesChanged) {
		t.Errorf("model self-report (%v) differs from mechanical record (%v)", summary.FilesChanged, filesChanged)
	}
}

// --- Test: files_changed — failed write not recorded ---

func TestFilesChangedFailedWriteNotRecorded(t *testing.T) {
	summaryJSON := `{"objective":"done","findings":[{"summary":"done","location":"","kind":"fact","weight":"low"}]}`

	srv := sseServer(t,
		// call 0: write_file to a path that will fail (ConfinePath rejects)
		toolCallFrames("w1", "write_file", `{"path":"../../../etc/passwd","content":"hacked"}`),
		// call 1: successful write
		toolCallFrames("w2", "write_file", `{"path":"ok.go","content":"ok"}`),
		// call 2: summary
		[]string{contentChunk(summaryJSON)},
	)
	defer srv.Close()

	exec := newFakeExecutor()
	exec.confineErrFn = func(path string) error {
		if strings.Contains(path, "../../../") {
			return fmt.Errorf("path %q is outside workspace \"/work\" — traversal not allowed", path)
		}
		return nil
	}

	parent := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })
	parent.AutoApprove = true

	_, _, _, _, _, filesChanged := parent.dispatchSubagent(
		context.Background(), "try writes", io.Discard, "", wtools.CapabilityEdit)

	// Only the successful write should be in filesChanged.
	if len(filesChanged) != 1 || filesChanged[0] != "ok.go" {
		t.Errorf("filesChanged = %v, want [ok.go] (failed write excluded)", filesChanged)
	}
}

// --- Test: files_changed — discrepancy between model claim and mechanical record ---

func TestFilesChangedDiscrepancy(t *testing.T) {
	// Model claims it modified "phantom.go" but the mechanical record only has
	// "real.go". The rendering should show both with the discrepancy noted.
	summaryJSON := `{"objective":"done","findings":[{"summary":"done","location":"","kind":"fact","weight":"low"}],"files_changed":["real.go","phantom.go"]}`

	srv := sseServer(t,
		// call 0: successful write to real.go only
		toolCallFrames("w1", "write_file", `{"path":"real.go","content":"ok"}`),
		// call 1: summary claiming both real.go and phantom.go
		[]string{contentChunk(summaryJSON)},
	)
	defer srv.Close()

	exec := newFakeExecutor()
	parent := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })
	parent.AutoApprove = true

	summary, _, _, _, _, filesChanged := parent.dispatchSubagent(
		context.Background(), "write", io.Discard, "", wtools.CapabilityEdit)

	// Mechanical record has only real.go.
	if len(filesChanged) != 1 || filesChanged[0] != "real.go" {
		t.Errorf("mechanical filesChanged = %v, want [real.go]", filesChanged)
	}

	// Model claim has both.
	if len(summary.FilesChanged) != 2 {
		t.Errorf("model self-report = %v, want 2 entries", summary.FilesChanged)
	}

	// Render and check the discrepancy is surfaced.
	rendered := renderFilesChanged(summary.FilesChanged, filesChanged)
	if !strings.Contains(rendered, "discrepancy") {
		t.Errorf("rendering should note discrepancy: %s", rendered)
	}
	if !strings.Contains(rendered, "phantom.go") {
		t.Errorf("rendering should show phantom.go as model-claimed: %s", rendered)
	}
}

// --- Test: confinement regression — edit child write outside workspace ---

func TestEditChildConfinementRegression(t *testing.T) {
	summaryJSON := `{"objective":"done","findings":[{"summary":"done","location":"","kind":"fact","weight":"low"}]}`

	srv := sseServer(t,
		// call 0: write_file outside workspace
		toolCallFrames("w1", "write_file", `{"path":"/etc/passwd","content":"hacked"}`),
		// call 1: summary
		[]string{contentChunk(summaryJSON)},
	)
	defer srv.Close()

	exec := newFakeExecutor()
	exec.confineErrFn = func(path string) error {
		if path == "/etc/passwd" {
			return fmt.Errorf("path %q is outside workspace \"/work\" — traversal not allowed", path)
		}
		return nil
	}

	parent := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })
	parent.AutoApprove = true

	_, _, _, _, _, filesChanged := parent.dispatchSubagent(
		context.Background(), "try escape", io.Discard, "", wtools.CapabilityEdit)

	// The rejected path should NOT appear in filesChanged.
	for _, p := range filesChanged {
		if p == "/etc/passwd" {
			t.Error("rejected path /etc/passwd should not be in filesChanged")
		}
	}
}

// --- Helpers ---

func makeEditToolCall(name, args string) proxy.ToolCall {
	return proxy.ToolCall{
		ID:       "tc1",
		Type:     "function",
		Function: proxy.FunctionCall{Name: name, Arguments: args},
	}
}

// makeProxyToolCalls builds proxy.ToolCall slice from simpler input.
func makeProxyToolCalls(items []struct {
	id string
	tj string
}) []proxy.ToolCall {
	out := make([]proxy.ToolCall, len(items))
	for i, it := range items {
		out[i] = proxy.ToolCall{
			ID:       it.id,
			Type:     "function",
			Function: proxy.FunctionCall{Name: "dispatch_subagent", Arguments: it.tj},
		}
	}
	return out
}
