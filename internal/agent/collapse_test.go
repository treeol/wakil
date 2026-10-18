package agent

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/proxy"
)

// --- test helpers for long-session collapse tests ---

// bigContent is large enough to push W over ContextCollapseMinBytes (default 2000).
var bigContent = strings.Repeat("x", 2500)

// tcMsg creates an assistant message with a single tool call.
func tcMsg(id, name, args string) proxy.Message {
	return proxy.Message{
		Role: "assistant",
		ToolCalls: []proxy.ToolCall{{
			ID:       id,
			Type:     "function",
			Function: proxy.FunctionCall{Name: name, Arguments: args},
		}},
	}
}

// trMsg creates a tool result message.
func trMsg(id, name, content string) proxy.Message {
	return proxy.Message{
		Role:       "tool",
		ToolCallID: id,
		Name:       name,
		Content:    StrPtr(content),
	}
}

// oneCollapsedTurn builds a turn (U + W + A) where W is large enough to be
// collapsible. Returns the messages and the indices of U and A.
func oneCollapsedTurn(turn int) []proxy.Message {
	return []proxy.Message{
		{Role: "user", Content: StrPtr("do something"), Turn: turn},
		tcMsg("tc1", "read_file", `{"path":"foo.go"}`),
		trMsg("tc1", "read_file", bigContent),
		tcMsg("tc2", "search_files", `{"pattern":"bar","path":"src"}`),
		trMsg("tc2", "search_files", bigContent),
		{Role: "assistant", Content: StrPtr("Here is the answer."), Turn: turn},
	}
}

// twoCollapsedTurns builds two turns, both with large W and clean A.
func twoCollapsedTurns() []proxy.Message {
	t1 := oneCollapsedTurn(1)
	t2 := oneCollapsedTurn(2)
	// Set Turn on W messages (tcMsg/trMsg don't set it).
	for i := range t1 {
		t1[i].Turn = 1
	}
	for i := range t2 {
		t2[i].Turn = 2
	}
	return append(append([]proxy.Message{}, t1...), t2...)
}

// assertNoOrphanedToolCalls verifies that every assistant tool_call ID has a
// following tool result in the same turn, and every tool result has a parent.
func assertNoOrphanedToolCalls(t *testing.T, msgs []proxy.Message) {
	t.Helper()
	// Track tool_call IDs that have been emitted but not yet matched by a result.
	pending := map[string]bool{}
	for _, m := range msgs {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				pending[tc.ID] = true
			}
		}
		if m.Role == "tool" {
			if !pending[m.ToolCallID] {
				t.Errorf("tool result %q has no preceding tool_call", m.ToolCallID)
			}
			delete(pending, m.ToolCallID)
		}
	}
	if len(pending) > 0 {
		t.Errorf("orphaned tool_calls without results: %v", pending)
	}
}

// --- 1. Standard mode: contextProjection returns a.Conv (identity) ---

func TestCollapseStandardModeIdentity(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("standard")
	app.Conv = []proxy.Message{
		{Role: "user", Content: StrPtr("hello"), Turn: 1},
		{Role: "assistant", Content: StrPtr("hi"), Turn: 1},
	}
	proj := app.contextProjection()
	if len(proj) != len(app.Conv) {
		t.Fatalf("standard mode: want %d msgs, got %d", len(app.Conv), len(proj))
	}
	if &proj[0] != &app.Conv[0] {
		t.Error("standard mode: projection should be the same slice (identity)")
	}
}

// --- 2. Long-session mode with no records: projection is a.Conv ---

func TestCollapseLongSessionNoRecords(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Conv = oneCollapsedTurn(1)
	for i := range app.Conv {
		app.Conv[i].Turn = 1
	}
	proj := app.contextProjection()
	if len(proj) != len(app.Conv) {
		t.Errorf("no records: want %d msgs, got %d", len(app.Conv), len(proj))
	}
}

// --- 3. Basic collapse: large W turn is collapsed, projection has U+A+note ---

func TestCollapseBasic(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Conv = oneCollapsedTurn(1)
	for i := range app.Conv {
		app.Conv[i].Turn = 1
	}
	app.turnSeq = 1

	app.collapseFinishedTurns()

	n, isLong := app.CollapsedCount()
	if !isLong || n != 1 {
		t.Fatalf("CollapsedCount = (%d, %v), want (1, true)", n, isLong)
	}

	proj := app.contextProjection()
	// U + A = 2 messages (W is collapsed).
	if len(proj) != 2 {
		t.Fatalf("projection: want 2 msgs (U+A), got %d", len(proj))
	}
	if proj[0].Role != "user" {
		t.Errorf("proj[0] role = %q, want user", proj[0].Role)
	}
	if proj[1].Role != "assistant" {
		t.Errorf("proj[1] role = %q, want assistant", proj[1].Role)
	}
	// A should contain the note.
	if !strings.Contains(DerefStr(proj[1].Content), "collapsed") {
		t.Error("projected A should contain collapse note")
	}

	// Cleanup archive.
	app.stateMu.RLock()
	rec := app.collapsed[1]
	app.stateMu.RUnlock()
	if rec.Archive != "" {
		os.Remove(rec.Archive)
	}
}

// --- 4. Rule 2: last message not a clean answer → not collapsed ---

func TestCollapseRuleNotFinished(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	// Turn ends with an assistant message that has tool_calls (not a clean A).
	app.Conv = []proxy.Message{
		{Role: "user", Content: StrPtr("do something"), Turn: 1},
		tcMsg("tc1", "read_file", `{"path":"foo.go"}`),
		trMsg("tc1", "read_file", bigContent),
		tcMsg("tc2", "search_files", `{"pattern":"bar"}`), // last msg has tool_calls
	}
	for i := range app.Conv {
		app.Conv[i].Turn = 1
	}
	app.turnSeq = 1

	app.collapseFinishedTurns()

	n, _ := app.CollapsedCount()
	if n != 0 {
		t.Errorf("not-finished turn should not collapse: CollapsedCount = %d, want 0", n)
	}
}

// --- 5. Rule 3: |W| < min_bytes → not collapsed ---

func TestCollapseRuleSmallW(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	// Small W — well under the 2000 byte threshold.
	app.Conv = []proxy.Message{
		{Role: "user", Content: StrPtr("hi"), Turn: 1},
		tcMsg("tc1", "read_file", `{"path":"f.go"}`),
		trMsg("tc1", "read_file", "small"),
		{Role: "assistant", Content: StrPtr("ok"), Turn: 1},
	}
	for i := range app.Conv {
		app.Conv[i].Turn = 1
	}
	app.turnSeq = 1

	app.collapseFinishedTurns()

	n, _ := app.CollapsedCount()
	if n != 0 {
		t.Errorf("small W turn should not collapse: CollapsedCount = %d, want 0", n)
	}
}

// --- 6. Rule 4: in keep set → not collapsed ---

func TestCollapseRuleKeepSet(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Conv = oneCollapsedTurn(1)
	for i := range app.Conv {
		app.Conv[i].Turn = 1
	}
	app.turnSeq = 1
	app.AddKeep(1)

	app.collapseFinishedTurns()

	n, _ := app.CollapsedCount()
	if n != 0 {
		t.Errorf("kept turn should not collapse: CollapsedCount = %d, want 0", n)
	}
}

// --- 7. Rule 5: subagent → not collapsed ---

func TestCollapseRuleSubagent(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.IsSubagent = true
	app.Conv = oneCollapsedTurn(1)
	for i := range app.Conv {
		app.Conv[i].Turn = 1
	}
	app.turnSeq = 1

	app.collapseFinishedTurns()

	n, _ := app.CollapsedCount()
	if n != 0 {
		t.Errorf("subagent turn should not collapse: CollapsedCount = %d, want 0", n)
	}
}

// --- 8. Rule 6: async pending → most recent not collapsed, older is ---

func TestCollapseRuleAsyncPending(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Conv = twoCollapsedTurns()
	app.turnSeq = 2

	// Simulate an active async op (most recent turn blocked).
	app.asyncActive = 1

	app.collapseFinishedTurns()

	n, _ := app.CollapsedCount()
	if n != 1 {
		t.Fatalf("async pending: CollapsedCount = %d, want 1 (older turn only)", n)
	}
	// Verify turn 1 is collapsed, turn 2 is not.
	app.stateMu.RLock()
	_, t1Collapsed := app.collapsed[1]
	_, t2Collapsed := app.collapsed[2]
	app.stateMu.RUnlock()
	if !t1Collapsed {
		t.Error("turn 1 (older) should be collapsed")
	}
	if t2Collapsed {
		t.Error("turn 2 (most recent) should NOT be collapsed when async pending")
	}

	// Cleanup.
	app.stateMu.RLock()
	rec := app.collapsed[1]
	app.stateMu.RUnlock()
	if rec.Archive != "" {
		os.Remove(rec.Archive)
	}
}

// --- 9. Rule 6: suspended → most recent not collapsed ---

func TestCollapseRuleSuspended(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Conv = twoCollapsedTurns()
	app.turnSeq = 2
	app.lastOutcomeSuspended = true

	app.collapseFinishedTurns()

	n, _ := app.CollapsedCount()
	if n != 1 {
		t.Fatalf("suspended: CollapsedCount = %d, want 1 (older turn only)", n)
	}
	app.stateMu.RLock()
	_, t2Collapsed := app.collapsed[2]
	app.stateMu.RUnlock()
	if t2Collapsed {
		t.Error("turn 2 (most recent) should NOT be collapsed when suspended")
	}

	// Cleanup.
	app.stateMu.RLock()
	rec := app.collapsed[1]
	app.stateMu.RUnlock()
	if rec.Archive != "" {
		os.Remove(rec.Archive)
	}
}

// --- 10. Rule 7: archive failure (empty chatID) → not collapsed ---

func TestCollapseRuleArchiveFailure(t *testing.T) {
	// Create an app with no chatID (Session nil, Client.ChatID empty).
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Client.ChatID = "" // forces SpillToCache to return ""
	app.Conv = oneCollapsedTurn(1)
	for i := range app.Conv {
		app.Conv[i].Turn = 1
	}
	app.turnSeq = 1

	app.collapseFinishedTurns()

	n, _ := app.CollapsedCount()
	if n != 0 {
		t.Errorf("archive failure should not collapse: CollapsedCount = %d, want 0", n)
	}
	// The archive failure warning flag should be set.
	if !app.collapseArchiveFailed {
		t.Error("collapseArchiveFailed should be true after archive failure")
	}
}

// --- 11. Projection invariant: no orphaned tool calls, U/A alternation ---

func TestCollapseProjectionInvariant(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Conv = twoCollapsedTurns()
	app.turnSeq = 2

	app.collapseFinishedTurns()

	proj := app.contextProjection()
	assertNoOrphanedToolCalls(t, proj)

	// Check U/A alternation for collapsed turns: each collapsed turn should
	// contribute exactly U then A (user then assistant), no W in between.
	collapsedTurns := map[int]bool{}
	app.stateMu.RLock()
	for t := range app.collapsed {
		collapsedTurns[t] = true
	}
	app.stateMu.RUnlock()

	for _, m := range proj {
		if collapsedTurns[m.Turn] {
			// Collapsed turn messages in the projection should only be U or A,
			// never tool calls or tool results.
			if m.Role == "tool" {
				t.Errorf("collapsed turn %d: tool result leaked into projection", m.Turn)
			}
			if m.Role == "assistant" && len(m.ToolCalls) > 0 {
				t.Errorf("collapsed turn %d: assistant with tool_calls leaked into projection", m.Turn)
			}
		}
	}

	// Cleanup.
	app.stateMu.RLock()
	for _, rec := range app.collapsed {
		if rec.Archive != "" {
			os.Remove(rec.Archive)
		}
	}
	app.stateMu.RUnlock()
}

// --- 12. Archive file contains full tool arguments and results ---

func TestCollapseArchiveContent(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Conv = oneCollapsedTurn(1)
	for i := range app.Conv {
		app.Conv[i].Turn = 1
	}
	app.turnSeq = 1

	app.collapseFinishedTurns()

	app.stateMu.RLock()
	rec := app.collapsed[1]
	app.stateMu.RUnlock()

	if rec.Archive == "" {
		t.Fatal("archive path should be non-empty")
	}
	defer os.Remove(rec.Archive)

	data, err := os.ReadFile(rec.Archive)
	if err != nil {
		t.Fatalf("reading archive: %v", err)
	}
	content := string(data)
	// The archive should contain the tool call arguments and tool results.
	if !strings.Contains(content, "read_file") {
		t.Error("archive should contain tool name 'read_file'")
	}
	if !strings.Contains(content, "search_files") {
		t.Error("archive should contain tool name 'search_files'")
	}
	if !strings.Contains(content, "foo.go") {
		t.Error("archive should contain tool call argument 'foo.go'")
	}
	// The archive should contain the big content from tool results.
	if !strings.Contains(content, bigContent[:100]) {
		t.Error("archive should contain the tool result content")
	}
}

// --- 13. Note properties: ≤600 bytes, paths extracted, counts correct ---

func TestCollapseNoteProperties(t *testing.T) {
	w := []proxy.Message{
		tcMsg("tc1", "read_file", `{"path":"internal/agent/collapse.go"}`),
		trMsg("tc1", "read_file", bigContent),
		tcMsg("tc2", "search_files", `{"pattern":"func","path":"internal/agent"}`),
		trMsg("tc2", "search_files", bigContent),
	}
	note := buildCollapseNote(1, w, "/fake/archive/turn-1-abc.txt")

	if len(note) > 600 {
		t.Errorf("note length = %d, want ≤ 600", len(note))
	}
	if !strings.Contains(note, "read_file") {
		t.Error("note should mention read_file")
	}
	if !strings.Contains(note, "search_files") {
		t.Error("note should mention search_files")
	}
	// Tool counts: "read_file×1" and "search_files×1".
	if !strings.Contains(note, "read_file×1") {
		t.Error("note should contain 'read_file×1'")
	}
	if !strings.Contains(note, "search_files×1") {
		t.Error("note should contain 'search_files×1'")
	}
	// Total tool calls: 2.
	if !strings.Contains(note, "2 tool calls") {
		t.Errorf("note should contain '2 tool calls': %q", note)
	}
	// Paths extracted from arguments.
	if !strings.Contains(note, "internal/agent/collapse.go") {
		t.Error("note should contain touched path 'internal/agent/collapse.go'")
	}
	if !strings.Contains(note, "internal/agent") {
		t.Error("note should contain touched path 'internal/agent'")
	}
	// Archive path.
	if !strings.Contains(note, "/fake/archive/turn-1-abc.txt") {
		t.Error("note should contain archive path")
	}
}

// --- 14. Mode switch round-trip: long → standard → long ---

func TestCollapseModeSwitchRoundTrip(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Conv = oneCollapsedTurn(1)
	for i := range app.Conv {
		app.Conv[i].Turn = 1
	}
	app.turnSeq = 1

	app.collapseFinishedTurns()
	n, _ := app.CollapsedCount()
	if n != 1 {
		t.Fatalf("after first collapse: CollapsedCount = %d, want 1", n)
	}

	// Switch to standard — projection should be full Conv.
	app.SetContextMode("standard")
	projStd := app.contextProjection()
	if len(projStd) != len(app.Conv) {
		t.Errorf("standard mode: want %d msgs, got %d", len(app.Conv), len(projStd))
	}

	// Switch back to long — records retained, no re-archiving.
	app.SetContextMode("long-session")
	n, _ = app.CollapsedCount()
	if n != 1 {
		t.Errorf("after switch back: CollapsedCount = %d, want 1 (records retained)", n)
	}
	projLong := app.contextProjection()
	if len(projLong) != 2 {
		t.Errorf("long mode after switch: want 2 msgs (U+A), got %d", len(projLong))
	}

	// Cleanup.
	app.stateMu.RLock()
	rec := app.collapsed[1]
	app.stateMu.RUnlock()
	if rec.Archive != "" {
		os.Remove(rec.Archive)
	}
}

// --- 15. Shared Content pointer not mutated ---

func TestCollapseSharedContentNotMutated(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Conv = oneCollapsedTurn(1)
	for i := range app.Conv {
		app.Conv[i].Turn = 1
	}
	app.turnSeq = 1

	app.collapseFinishedTurns()

	// Save the original A content.
	origAContent := app.Conv[5].Content
	origAText := *origAContent

	proj := app.contextProjection()

	// Original should be unchanged.
	if *app.Conv[5].Content != origAText {
		t.Error("original A content was mutated by contextProjection")
	}
	if app.Conv[5].Content != origAContent {
		t.Error("original A Content pointer was changed by contextProjection")
	}

	// Projected A should have the note appended.
	projAText := DerefStr(proj[1].Content)
	if !strings.Contains(projAText, origAText) {
		t.Error("projected A should contain original text")
	}
	if !strings.Contains(projAText, "collapsed") {
		t.Error("projected A should contain the collapse note")
	}

	// Cleanup.
	app.stateMu.RLock()
	rec := app.collapsed[1]
	app.stateMu.RUnlock()
	if rec.Archive != "" {
		os.Remove(rec.Archive)
	}
}

// --- 16. Rewind prunes collapse records for removed turns ---

func TestCollapseRewindPrune(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Conv = twoCollapsedTurns()
	app.turnSeq = 2

	app.collapseFinishedTurns()
	n, _ := app.CollapsedCount()
	if n != 2 {
		t.Fatalf("before rewind: CollapsedCount = %d, want 2", n)
	}

	// Simulate rewind: truncate Conv to remove turn 2 (keep only turn 1).
	app.convMu.Lock()
	app.Conv = app.Conv[:6] // turn 1 has 6 messages
	app.convMu.Unlock()

	app.pruneCollapseOnRewind()

	n, _ = app.CollapsedCount()
	if n != 1 {
		t.Errorf("after rewind: CollapsedCount = %d, want 1", n)
	}
	app.stateMu.RLock()
	_, t2Still := app.collapsed[2]
	app.stateMu.RUnlock()
	if t2Still {
		t.Error("turn 2 record should be pruned after rewind")
	}
	if app.turnSeq != 1 {
		t.Errorf("turnSeq = %d, want 1 after rewind", app.turnSeq)
	}

	// Cleanup.
	app.stateMu.RLock()
	rec := app.collapsed[1]
	app.stateMu.RUnlock()
	if rec.Archive != "" {
		os.Remove(rec.Archive)
	}
}

// --- 16b. Rewind prunes records when last message is no longer a clean answer ---

func TestCollapseRewindPruneNoCleanAnswer(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Conv = oneCollapsedTurn(1)
	for i := range app.Conv {
		app.Conv[i].Turn = 1
	}
	app.turnSeq = 1

	app.collapseFinishedTurns()
	n, _ := app.CollapsedCount()
	if n != 1 {
		t.Fatalf("before rewind: CollapsedCount = %d, want 1", n)
	}

	// Simulate partial rewind: remove only the last message (A), leaving
	// the user message and W but no clean answer.
	app.convMu.Lock()
	app.Conv = app.Conv[:5] // drop A (index 5)
	app.convMu.Unlock()

	app.pruneCollapseOnRewind()

	n, _ = app.CollapsedCount()
	if n != 0 {
		t.Errorf("after partial rewind (no clean A): CollapsedCount = %d, want 0", n)
	}
}

// --- 17. Session restore: RestoreTurnState rebuilds projection ---

func TestCollapseSessionRestore(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Conv = oneCollapsedTurn(1)
	for i := range app.Conv {
		app.Conv[i].Turn = 1
	}
	app.turnSeq = 1

	app.collapseFinishedTurns()
	projBefore := app.contextProjection()

	// Build a Session from the current state.
	app.stateMu.RLock()
	rec := app.collapsed[1]
	app.stateMu.RUnlock()
	s := &Session{
		ChatID:    "test-restore",
		Conv:      append([]proxy.Message(nil), app.Conv...),
		Collapsed: []CollapseRecord{rec},
	}

	// Simulate a fresh app loading the session.
	app2 := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app2.SetContextMode("long-session")
	app2.Conv = append([]proxy.Message(nil), s.Conv...)
	app2.RestoreTurnState(s)

	projAfter := app2.contextProjection()
	if len(projAfter) != len(projBefore) {
		t.Errorf("after restore: projection len = %d, want %d", len(projAfter), len(projBefore))
	}

	// Cleanup.
	if rec.Archive != "" {
		os.Remove(rec.Archive)
	}
}

// --- 18. Compact on projection: sent size under compactAt → no compaction ---

func TestCollapseCompactSentSize(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Conv = twoCollapsedTurns()
	app.turnSeq = 2

	app.collapseFinishedTurns()

	// After collapse, the sent (projected) size should be small (just U+A+note).
	// Set compactAt very high so compaction does not fire.
	app.Cfg.CompactAt = 1000000
	app.Cfg.KeepBytes = 500000
	app.Cfg.HardMaxBytes = 2000000

	did, err := app.Compact(context.Background(), app.summarizeFn(), false)
	if err != nil {
		t.Fatalf("Compact error: %v", err)
	}
	if did {
		t.Error("Compact should not fire when sent size is under compactAt")
	}
	n, _ := app.CollapsedCount()
	if n != 2 {
		t.Errorf("records should be retained: CollapsedCount = %d, want 2", n)
	}

	// Cleanup.
	app.stateMu.RLock()
	for _, rec := range app.collapsed {
		if rec.Archive != "" {
			os.Remove(rec.Archive)
		}
	}
	app.stateMu.RUnlock()
}

// --- 19. Compact soft path is a no-op in long-session mode ---
//
// v2 (soft-budget middle cut): in long-session mode the deterministic tail
// degrade in contextProjection owns old-turn overhead. A soft Compact pass
// would materializeCollapsed() — baking the projection into a.Conv and
// destroying the collapse records that anchor archive-pointer recovery — then
// re-summarize what Layer 2 already degrades. So the soft path returns
// (false, nil) without touching anything; enforceHardMax keeps its own
// materialize call as the last-resort fallback (and /compact forces=true).

func TestCollapseCompactOverThreshold(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Conv = twoCollapsedTurns()
	app.turnSeq = 2

	app.collapseFinishedTurns()

	// Set compactAt very low so the sent projection would exceed it.
	app.Cfg.CompactAt = 10
	app.Cfg.KeepBytes = 5
	app.Cfg.HardMaxBytes = 100000
	// Inject a fake summarizer.
	app.Summarize = func(_ context.Context, _ string) (string, error) {
		return "[summary of older turns]", nil
	}

	did, err := app.Compact(context.Background(), app.summarizeFn(), false)
	if err != nil {
		t.Fatalf("Compact error: %v", err)
	}
	if did {
		t.Error("soft Compact should be a no-op in long-session mode (Layer 2 owns the middle cut)")
	}
	n, _ := app.CollapsedCount()
	if n != 2 {
		t.Errorf("after soft Compact: CollapsedCount = %d, want 2 (records preserved)", n)
	}

	// Cleanup: archives were written to disk by collapseFinishedTurns.
	app.stateMu.RLock()
	for _, rec := range app.collapsed {
		if rec.Archive != "" {
			os.Remove(rec.Archive)
		}
	}
	app.stateMu.RUnlock()
}

// --- 20. AddKeep removes existing collapse record ---

func TestCollapseAddKeepRemovesRecord(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Conv = oneCollapsedTurn(1)
	for i := range app.Conv {
		app.Conv[i].Turn = 1
	}
	app.turnSeq = 1

	app.collapseFinishedTurns()
	n, _ := app.CollapsedCount()
	if n != 1 {
		t.Fatalf("after collapse: CollapsedCount = %d, want 1", n)
	}

	// Save archive path for cleanup before AddKeep deletes the record.
	app.stateMu.RLock()
	rec := app.collapsed[1]
	app.stateMu.RUnlock()

	// AddKeep on an already-collapsed turn should delete the record.
	if err := app.AddKeep(1); err != nil {
		t.Fatalf("AddKeep error: %v", err)
	}
	n, _ = app.CollapsedCount()
	if n != 0 {
		t.Errorf("after AddKeep on collapsed turn: CollapsedCount = %d, want 0", n)
	}

	// Projection should now include full turn (not collapsed).
	proj := app.contextProjection()
	if len(proj) != len(app.Conv) {
		t.Errorf("after AddKeep: projection = %d msgs, want %d (full)", len(proj), len(app.Conv))
	}

	// Cleanup.
	if rec.Archive != "" {
		os.Remove(rec.Archive)
	}
}

// --- 21. RemoveKeep allows re-collapse ---

func TestCollapseRemoveKeepAllowsCollapse(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Conv = oneCollapsedTurn(1)
	for i := range app.Conv {
		app.Conv[i].Turn = 1
	}
	app.turnSeq = 1
	app.AddKeep(1)

	app.collapseFinishedTurns()
	n, _ := app.CollapsedCount()
	if n != 0 {
		t.Errorf("kept turn should not collapse: CollapsedCount = %d, want 0", n)
	}

	// Remove keep, then collapse again.
	app.RemoveKeep(1)
	app.collapseFinishedTurns()
	n, _ = app.CollapsedCount()
	if n != 1 {
		t.Errorf("after RemoveKeep and re-collapse: CollapsedCount = %d, want 1", n)
	}

	// Cleanup.
	app.stateMu.RLock()
	rec := app.collapsed[1]
	app.stateMu.RUnlock()
	if rec.Archive != "" {
		os.Remove(rec.Archive)
	}
}

// --- 22. Mid-turn collapse_context: basic partial collapse & projection ---

func TestCollapseContextMidTurnBasic(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.turnSeq = 1
	app.curTurn = 1

	// Simulate an active turn on iteration 2:
	// idx 0: U
	// idx 1: assistant tool_call read_file (iter 0)
	// idx 2: tool result read_file (iter 0)
	// idx 3: assistant tool_call collapse_context (iter 1 — currently executing)
	collapseCall := proxy.ToolCall{
		ID:   "cc-1",
		Type: "function",
		Function: proxy.FunctionCall{
			Name:      "collapse_context",
			Arguments: `{"summary":"Inspected foo.go and confirmed handler layout","next_steps":"Edit foo.go and run tests"}`,
		},
	}
	app.Conv = []proxy.Message{
		{Role: "user", Content: StrPtr("refactor foo.go"), Turn: 1},
		{Role: "assistant", ToolCalls: []proxy.ToolCall{{ID: "tc1", Type: "function", Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"foo.go"}`}}}, Turn: 1},
		{Role: "tool", ToolCallID: "tc1", Name: "read_file", Content: StrPtr(bigContent), Turn: 1},
		{Role: "assistant", ToolCalls: []proxy.ToolCall{collapseCall}, Turn: 1},
	}

	res := app.handletoolCallForTest(collapseCall)
	if !strings.Contains(res, "collapsed request 1 working set") {
		t.Fatalf("unexpected handleCollapseContext result: %q", res)
	}

	// Simulate finalizeToolResult appending the collapse_context tool result.
	app.appendConv(proxy.Message{
		Role:       "tool",
		ToolCallID: "cc-1",
		Name:       "collapse_context",
		Content:    StrPtr(res),
	})

	app.stateMu.RLock()
	rec, ok := app.collapsed[1]
	app.stateMu.RUnlock()
	if !ok || !rec.Partial || rec.BoundaryCallID != "cc-1" {
		t.Fatalf("expected partial CollapseRecord with BoundaryCallID=cc-1, got %+v (ok=%v)", rec, ok)
	}
	defer func() {
		if rec.Archive != "" {
			os.Remove(rec.Archive)
		}
	}()

	proj := app.contextProjection()
	assertNoOrphanedToolCalls(t, proj)

	// Projection should have:
	// proj[0]: U + partial checkpoint block
	// proj[1]: assistant tool_call collapse_context (idx 3)
	// proj[2]: tool result collapse_context (idx 4)
	// (idx 1 and idx 2 — the read_file call+result — are elided!)
	if len(proj) != 3 {
		t.Fatalf("projection len = %d, want 3 (U + collapse_context call + result)", len(proj))
	}
	uText := DerefStr(proj[0].Content)
	if !strings.Contains(uText, "[assistant checkpoint") {
		t.Errorf("projected U should contain checkpoint header: %q", uText)
	}
	if !strings.Contains(uText, "Inspected foo.go and confirmed handler layout") {
		t.Errorf("projected U should contain summary: %q", uText)
	}
	if !strings.Contains(uText, "Edit foo.go and run tests") {
		t.Errorf("projected U should contain next_steps: %q", uText)
	}
	if !strings.Contains(uText, rec.Archive) {
		t.Errorf("projected U should contain archive path %q: %q", rec.Archive, uText)
	}
	// Original U in a.Conv must not be mutated.
	if *app.Conv[0].Content != "refactor foo.go" {
		t.Errorf("original U in a.Conv was mutated: %q", *app.Conv[0].Content)
	}
}

// handletoolCallForTest dispatches a tool call through handleToolCall.
func (a *App) handletoolCallForTest(tc proxy.ToolCall) string {
	return a.handleToolCall(context.Background(), tc).text
}

// --- 23. Mid-turn collapse upgrades to full collapse at finalizeTurn ---

func TestCollapseContextUpgradesToFullAtFinalize(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.turnSeq = 1
	app.curTurn = 1

	collapseCall := proxy.ToolCall{
		ID:   "cc-1",
		Type: "function",
		Function: proxy.FunctionCall{
			Name:      "collapse_context",
			Arguments: `{"summary":"Phase 1 investigation done","next_steps":"Write final answer"}`,
		},
	}
	app.Conv = []proxy.Message{
		{Role: "user", Content: StrPtr("investigate and summarize"), Turn: 1},
		{Role: "assistant", ToolCalls: []proxy.ToolCall{{ID: "tc1", Type: "function", Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"a.go"}`}}}, Turn: 1},
		{Role: "tool", ToolCallID: "tc1", Name: "read_file", Content: StrPtr(bigContent), Turn: 1},
		{Role: "assistant", ToolCalls: []proxy.ToolCall{collapseCall}, Turn: 1},
	}

	res := app.handleCollapseContext(collapseCall)
	app.appendConv(proxy.Message{
		Role:       "tool",
		ToolCallID: "cc-1",
		Name:       "collapse_context",
		Content:    StrPtr(res),
	})

	app.stateMu.RLock()
	partArchive := app.collapsed[1].Archive
	app.stateMu.RUnlock()
	defer func() {
		if partArchive != "" {
			os.Remove(partArchive)
		}
	}()

	// Now the turn finishes with a clean assistant answer A.
	app.appendConv(proxy.Message{
		Role:    "assistant",
		Content: StrPtr("All done — here is the final answer."),
	})

	// Run collapseFinishedTurns (as finalizeTurn does).
	app.collapseFinishedTurns()

	app.stateMu.RLock()
	fullRec, ok := app.collapsed[1]
	app.stateMu.RUnlock()
	if !ok {
		t.Fatal("expected collapse record after finalize")
	}
	defer func() {
		if fullRec.Archive != "" {
			os.Remove(fullRec.Archive)
		}
	}()
	if fullRec.Partial {
		t.Error("expected Partial=false after turn finished and upgraded")
	}
	if fullRec.BoundaryCallID != "" {
		t.Errorf("expected empty BoundaryCallID after upgrade, got %q", fullRec.BoundaryCallID)
	}
	if fullRec.Summary != "Phase 1 investigation done" {
		t.Errorf("expected Summary preserved across upgrade, got %q", fullRec.Summary)
	}

	// Projection should now be just U + A (2 messages).
	proj := app.contextProjection()
	assertNoOrphanedToolCalls(t, proj)
	if len(proj) != 2 {
		t.Fatalf("upgraded projection: want 2 msgs (U+A), got %d", len(proj))
	}
	if !strings.Contains(DerefStr(proj[1].Content), "Phase 1 investigation done") {
		t.Errorf("projected A should preserve checkpoint summary: %q", DerefStr(proj[1].Content))
	}
}

// --- 24. Second mid-turn collapse in the same turn advances boundary ---

func TestCollapseContextRepeatedInSameTurn(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.turnSeq = 1
	app.curTurn = 1

	cc1 := proxy.ToolCall{
		ID:   "cc-1",
		Type: "function",
		Function: proxy.FunctionCall{
			Name:      "collapse_context",
			Arguments: `{"summary":"Round 1 summary","next_steps":"Do round 2"}`,
		},
	}
	app.Conv = []proxy.Message{
		{Role: "user", Content: StrPtr("multi-phase task"), Turn: 1},
		{Role: "assistant", ToolCalls: []proxy.ToolCall{{ID: "tc1", Type: "function", Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"p1.go"}`}}}, Turn: 1},
		{Role: "tool", ToolCallID: "tc1", Name: "read_file", Content: StrPtr(bigContent), Turn: 1},
		{Role: "assistant", ToolCalls: []proxy.ToolCall{cc1}, Turn: 1},
	}
	res1 := app.handleCollapseContext(cc1)
	app.appendConv(proxy.Message{Role: "tool", ToolCallID: "cc-1", Name: "collapse_context", Content: StrPtr(res1)})

	app.stateMu.RLock()
	arch1 := app.collapsed[1].Archive
	app.stateMu.RUnlock()
	defer func() {
		if arch1 != "" {
			os.Remove(arch1)
		}
	}()

	// Now round 2 happens, followed by a second collapse_context call cc2.
	cc2 := proxy.ToolCall{
		ID:   "cc-2",
		Type: "function",
		Function: proxy.FunctionCall{
			Name:      "collapse_context",
			Arguments: `{"summary":"Cumulative round 1 + round 2 summary","next_steps":"Finish up"}`,
		},
	}
	app.appendConv(
		proxy.Message{Role: "assistant", ToolCalls: []proxy.ToolCall{{ID: "tc2", Type: "function", Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"p2.go"}`}}}},
		proxy.Message{Role: "tool", ToolCallID: "tc2", Name: "read_file", Content: StrPtr(bigContent)},
		proxy.Message{Role: "assistant", ToolCalls: []proxy.ToolCall{cc2}},
	)
	res2 := app.handleCollapseContext(cc2)
	app.appendConv(proxy.Message{Role: "tool", ToolCallID: "cc-2", Name: "collapse_context", Content: StrPtr(res2)})

	app.stateMu.RLock()
	rec2 := app.collapsed[1]
	app.stateMu.RUnlock()
	defer func() {
		if rec2.Archive != "" {
			os.Remove(rec2.Archive)
		}
	}()

	if rec2.BoundaryCallID != "cc-2" {
		t.Errorf("BoundaryCallID = %q, want cc-2", rec2.BoundaryCallID)
	}

	proj := app.contextProjection()
	assertNoOrphanedToolCalls(t, proj)
	// Only U + cc2 assistant + cc2 tool result should remain in projection (3 msgs).
	if len(proj) != 3 {
		t.Fatalf("after 2nd collapse: projection len = %d, want 3", len(proj))
	}
	if !strings.Contains(DerefStr(proj[0].Content), "Cumulative round 1 + round 2 summary") {
		t.Errorf("U checkpoint should carry updated cumulative summary: %q", DerefStr(proj[0].Content))
	}
}

// --- 25. Multi-tool batch safety: sibling tools in same batch stay intact ---

func TestCollapseContextMultiToolBatchSafety(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.turnSeq = 1
	app.curTurn = 1

	ccCall := proxy.ToolCall{
		ID:   "cc-batch",
		Type: "function",
		Function: proxy.FunctionCall{
			Name:      "collapse_context",
			Arguments: `{"summary":"Collapsed prior round","next_steps":"Read bar.go in same batch"}`,
		},
	}
	siblingCall := proxy.ToolCall{
		ID:   "tc-sibling",
		Type: "function",
		Function: proxy.FunctionCall{
			Name:      "read_file",
			Arguments: `{"path":"bar.go"}`,
		},
	}
	app.Conv = []proxy.Message{
		{Role: "user", Content: StrPtr("check files"), Turn: 1},
		{Role: "assistant", ToolCalls: []proxy.ToolCall{{ID: "tc1", Type: "function", Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"foo.go"}`}}}, Turn: 1},
		{Role: "tool", ToolCallID: "tc1", Name: "read_file", Content: StrPtr(bigContent), Turn: 1},
		// Current batch has both collapse_context AND a sibling tool call:
		{Role: "assistant", ToolCalls: []proxy.ToolCall{ccCall, siblingCall}, Turn: 1},
	}

	res := app.handleCollapseContext(ccCall)
	app.appendConv(
		proxy.Message{Role: "tool", ToolCallID: "cc-batch", Name: "collapse_context", Content: StrPtr(res)},
		proxy.Message{Role: "tool", ToolCallID: "tc-sibling", Name: "read_file", Content: StrPtr("package bar")},
	)

	app.stateMu.RLock()
	rec := app.collapsed[1]
	app.stateMu.RUnlock()
	defer func() {
		if rec.Archive != "" {
			os.Remove(rec.Archive)
		}
	}()

	proj := app.contextProjection()
	assertNoOrphanedToolCalls(t, proj)
	// U + assistant(2 tool calls) + tool(cc-batch) + tool(tc-sibling) = 4 messages.
	if len(proj) != 4 {
		t.Fatalf("multi-tool batch projection: want 4 msgs, got %d", len(proj))
	}
}

// --- 26. Edge cases: standard mode no-op, iter-0 no-op + prior turn sweep, async guard ---

func TestCollapseContextEdgeCases(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("standard")

	cc := proxy.ToolCall{
		ID:   "cc-std",
		Type: "function",
		Function: proxy.FunctionCall{
			Name:      "collapse_context",
			Arguments: `{"summary":"test"}`,
		},
	}
	// 1. Standard mode is a clean no-op.
	if out := app.handleCollapseContext(cc); !strings.Contains(out, "no-op in standard mode") {
		t.Errorf("standard mode: got %q", out)
	}

	// 2. Switch to long-session. Set up Turn 1 (finished, uncollapsed) and Turn 2 on iter 0
	// (user switches task and model calls collapse_context on the first iteration of Turn 2).
	app.SetContextMode("long-session")
	t1 := oneCollapsedTurn(1)
	for i := range t1 {
		t1[i].Turn = 1
	}
	app.Conv = append([]proxy.Message{}, t1...)
	app.turnSeq = 2
	app.curTurn = 2
	app.appendConv(
		proxy.Message{Role: "user", Content: StrPtr("new task now")},
		proxy.Message{Role: "assistant", ToolCalls: []proxy.ToolCall{cc}},
	)

	out := app.handleCollapseContext(cc)
	if !strings.Contains(out, "collapsed 1 prior request(s)") {
		t.Errorf("iter-0 with uncollapsed prior turn: got %q", out)
	}
	app.stateMu.RLock()
	rec1, ok1 := app.collapsed[1]
	_, ok2 := app.collapsed[2]
	app.stateMu.RUnlock()
	defer func() {
		if rec1.Archive != "" {
			os.Remove(rec1.Archive)
		}
	}()
	if !ok1 {
		t.Error("turn 1 should have been collapsed by prior-turn sweep")
	}
	if ok2 {
		t.Error("turn 2 (iter 0) has no prior tool rounds and should not have a record")
	}

	// 3. Async pending guard blocks current-turn collapse.
	app.asyncActive = 1
	outAsync := app.handleCollapseContext(cc)
	if !strings.Contains(outAsync, "deferred") {
		t.Errorf("async pending: expected deferred message, got %q", outAsync)
	}
	app.asyncActive = 0
}
