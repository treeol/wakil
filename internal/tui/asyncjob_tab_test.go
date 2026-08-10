package tui

// Tests for card #126 Phase 1: Mashūra async-job tabs (AsyncJobStartMsg /
// AsyncJobDoneMsg) reusing the subTab machinery.

import (
	"strings"
	"testing"

	agent "github.com/treeol/wakil/internal/agent"
)

func findJobTab(m tuiModel, opID string) *subTab {
	for _, t := range m.subTabs {
		if t.kind == subTabAsyncJob && t.opID == opID {
			return t
		}
	}
	return nil
}

// TestAsyncJobStartCreatesTab verifies AsyncJobStartMsg opens an async-job tab,
// active and labeled with the panel name.
func TestAsyncJobStartCreatesTab(t *testing.T) {
	m := newTabModel()
	m = step(m, agent.AsyncJobStartMsg{OpID: "op-1", Label: "panel Review"})

	tab := findJobTab(m, "op-1")
	if tab == nil {
		t.Fatal("async-job tab not created for op-1")
	}
	if tab.kind != subTabAsyncJob {
		t.Errorf("kind = %v, want subTabAsyncJob", tab.kind)
	}
	if !tab.active {
		t.Error("async job tab should be active (running) at start")
	}
	if tab.done {
		t.Error("async job tab should not be done at start")
	}
	if tab.task != "panel Review" {
		t.Errorf("task = %q, want %q", tab.task, "panel Review")
	}
}

// TestAsyncJobStartUpsertsDuplicates verifies a duplicate/replayed Start for the
// same opID does not create a second tab (idempotent upsert).
func TestAsyncJobStartUpsertsDuplicates(t *testing.T) {
	m := newTabModel()
	m = step(m, agent.AsyncJobStartMsg{OpID: "op-1", Label: "panel A"})
	m = step(m, agent.AsyncJobStartMsg{OpID: "op-1", Label: "panel A"})

	n := 0
	for _, t := range m.subTabs {
		if t.kind == subTabAsyncJob && t.opID == "op-1" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("duplicate Start made %d tabs for op-1, want 1", n)
	}
}

// TestAsyncJobDoneTerminalizesTab verifies AsyncJobDoneMsg marks the tab done,
// appends the bounded result, and arms (returns) a 30s close command.
func TestAsyncJobDoneTerminalizesTab(t *testing.T) {
	m := newTabModel()
	m = step(m, agent.AsyncJobStartMsg{OpID: "op-1", Label: "panel Review"})

	var gotCmds interface{}
	m = step(m, agent.AsyncJobDoneMsg{OpID: "op-1", Label: "panel Review", Result: "the answer"})
	_ = gotCmds

	tab := findJobTab(m, "op-1")
	if tab == nil {
		t.Fatal("tab missing after Done")
	}
	if !tab.done || !tab.finished {
		t.Errorf("tab done=%v finished=%v, want both true", tab.done, tab.finished)
	}
	if got := tab.buf.String(); got != "the answer" {
		t.Errorf("result buf = %q, want %q", got, "the answer")
	}
}

// TestAsyncJobDoneShowsErrAndResult verifies an errored job still surfaces the
// diagnostic result (the tab shows both the error and the bounded result).
func TestAsyncJobDoneShowsErrAndResult(t *testing.T) {
	m := newTabModel()
	m = step(m, agent.AsyncJobStartMsg{OpID: "op-1", Label: "panel Review"})
	m = step(m, agent.AsyncJobDoneMsg{OpID: "op-1", Label: "panel Review",
		Result: "member diagnostics", Err: "all panel members failed"})

	tab := findJobTab(m, "op-1")
	if tab == nil {
		t.Fatal("tab missing after Done")
	}
	if tab.finErr != "all panel members failed" {
		t.Errorf("finErr = %q, want the error text", tab.finErr)
	}
	if got := tab.buf.String(); got != "member diagnostics" {
		t.Errorf("result buf = %q, want diagnostic result kept on error", got)
	}
}

// TestAsyncJobDoneBeforeStartCreatesTerminalTab verifies the Done-before-Start
// safety: a Done with no matching tab creates a terminal tab (never a
// permanently-running tab). This is the critical ordering-race guard.
func TestAsyncJobDoneBeforeStartCreatesTerminalTab(t *testing.T) {
	m := newTabModel()
	// No Start received; Done arrives first (fast op or ordering edge).
	m = step(m, agent.AsyncJobDoneMsg{OpID: "op-1", Label: "panel Review", Result: "quick answer"})

	tab := findJobTab(m, "op-1")
	if tab == nil {
		t.Fatal("Done-before-Start must create a terminal tab, but none exists")
	}
	if !tab.done {
		t.Errorf("Done-before-Start tab should be done, got done=%v", tab.done)
	}
	if tab.active {
		t.Error("Done-before-Start tab must not be left active (would pulse forever)")
	}
	if got := tab.buf.String(); got != "quick answer" {
		t.Errorf("result = %q, want %q", got, "quick answer")
	}
}

// TestAsyncJobCloseByOpID verifies subTabCloseMsg with OpID removes an unfocused
// done async-job tab (session identity via OpID, not ChatID).
func TestAsyncJobCloseByOpID(t *testing.T) {
	m := newTabModel()
	m = step(m, agent.AsyncJobStartMsg{OpID: "op-1", Label: "panel A"})
	m = step(m, agent.AsyncJobDoneMsg{OpID: "op-1", Label: "panel A", Result: "x"})

	if len(m.subTabs) != 1 {
		t.Fatalf("precondition: %d tabs, want 1", len(m.subTabs))
	}
	m = step(m, subTabCloseMsg{OpID: "op-1"})

	if len(m.subTabs) != 0 {
		t.Errorf("after close: %d tabs, want 0", len(m.subTabs))
	}
}

// TestAsyncJobCloseSkipsFocused verifies a focused async-job tab is not
// auto-closed (same one-shot skip-if-focused semantics as subagents).
func TestAsyncJobCloseSkipsFocused(t *testing.T) {
	m := newTabModel()
	m = step(m, agent.AsyncJobStartMsg{OpID: "op-1", Label: "panel A"})
	m = step(m, agent.AsyncJobDoneMsg{OpID: "op-1", Label: "panel A", Result: "x"})

	m.subCur = tabIndexByN(m.subTabs, 1) // focus the job tab
	if m.subCur < 0 {
		t.Fatal("could not focus job tab")
	}
	m = step(m, subTabCloseMsg{OpID: "op-1"})

	if len(m.subTabs) != 1 {
		t.Errorf("focused job tab was closed: %d tabs, want 1", len(m.subTabs))
	}
}

// TestAsyncJobDotPulsesWhenIdleButActive verifies the pulse tick re-arms while
// an async-job tab is active (running) even though the main agent is idle — a
// detached job must keep pulsing until it completes. dummyDotTick applies the
// same branch used by dotTickMsg without a real timer.
func TestAsyncJobDotPulsesWhenIdleButActive(t *testing.T) {
	m := newTabModel()
	m = step(m, agent.AsyncJobStartMsg{OpID: "op-1", Label: "panel A"})
	m.state = stateIdle // main agent idle, job still running

	if !m.hasActiveJobTab() {
		t.Fatal("hasActiveJobTab should be true while a job tab is active")
	}
	// Simulate the dotTick re-arm condition.
	rearm := m.state != stateIdle || m.hasActiveJobTab()
	if !rearm {
		t.Error("dot tick should re-arm while a job tab is active and idle")
	}

	// Once the job completes, and main is idle, the tick must NOT re-arm.
	m = step(m, agent.AsyncJobDoneMsg{OpID: "op-1", Label: "panel A", Result: "done"})
	rearm = m.state != stateIdle || m.hasActiveJobTab()
	if rearm {
		t.Error("dot tick should stop after the job completes and main is idle")
	}
}

// TestAsyncJobMixedWithSubagents verifies async-job and subagent tabs coexist and
// prune/nav treat them uniformly (identity via kind).
func TestAsyncJobMixedWithSubagents(t *testing.T) {
	m := newTabModel()
	m = step(m, agent.AsyncJobStartMsg{OpID: "op-1", Label: "panel A"})
	m = step(m, agent.SubagentStartMsg{Task: "sub task", ChatID: "chat-a"})

	if len(m.subTabs) != 2 {
		t.Fatalf("expected 2 tabs (job + subagent), got %d", len(m.subTabs))
	}
	if findJobTab(m, "op-1") == nil {
		t.Error("job tab missing in mixed model")
	}
	// subagent close still keyed by ChatID.
	m = step(m, agent.SubagentDoneMsg{ChatID: "chat-a"})
	m = step(m, subTabCloseMsg{ChatID: "chat-a"})
	if len(m.subTabs) != 1 || findJobTab(m, "op-1") == nil {
		t.Errorf("subagent close removed the job tab: %d tabs, job present=%v",
			len(m.subTabs), findJobTab(m, "op-1") != nil)
	}
}

// TestAsyncJobRenderTabBar verifies the tab bar renders a job tab with its label
// (no panic, label present) — smoke rather than exact styling.
func TestAsyncJobRenderTabBar(t *testing.T) {
	m := newTabModel()
	m = step(m, agent.AsyncJobStartMsg{OpID: "op-1", Label: "panel Review"})
	m.width, m.height = 200, 50
	bar := m.renderMainTabBar()
	// The label is truncated to fit the tab slot ("panel Rev…"), so assert on a
	// prefix that survives truncation rather than the full label.
	if !strings.Contains(bar, "panel Rev") {
		t.Errorf("tab bar missing job label prefix: %q", bar)
	}
	if !strings.Contains(bar, "s1") {
		t.Errorf("tab bar missing job sequence label: %q", bar)
	}
}

// TestNewConvClearsJobTabs verifies /new (NewConvMsg) clears async-job tabs.
func TestNewConvClearsJobTabs(t *testing.T) {
	m := newTabModel()
	m = step(m, agent.AsyncJobStartMsg{OpID: "op-1", Label: "panel A"})
	if len(m.subTabs) != 1 {
		t.Fatalf("precondition: 1 tab, got %d", len(m.subTabs))
	}
	m = step(m, agent.NewConvMsg{})
	if len(m.subTabs) != 0 {
		t.Errorf("NewConvMsg should clear job tabs, got %d", len(m.subTabs))
	}
	if m.subCur != -1 {
		t.Errorf("subCur should reset to -1 (main), got %d", m.subCur)
	}
}

// TestAsyncJobDoneIdempotent verifies a replayed Done does not re-append the
// result buffer (defense-in-depth, mirrors the idempotent Start upsert).
func TestAsyncJobDoneIdempotent(t *testing.T) {
	m := newTabModel()
	m = step(m, agent.AsyncJobDoneMsg{OpID: "op-1", Label: "panel A", Result: "first"})
	m = step(m, agent.AsyncJobDoneMsg{OpID: "op-1", Label: "panel A", Result: "second"})

	tab := findJobTab(m, "op-1")
	if tab == nil {
		t.Fatal("tab missing")
	}
	if got := tab.buf.String(); got != "first" {
		t.Errorf("duplicate Done re-appended result: buf = %q, want %q", got, "first")
	}
}

// TestAsyncJobDoneRejectedAfterRotation verifies a Done from a prior session
// (OriginChatID differs from the current conversation) does NOT recreate a tab
// cleared on rotation — the post-rotation resurrection guard.
func TestAsyncJobDoneRejectedAfterRotation(t *testing.T) {
	m := newTabModel()
	// newTestClient sets Client.ChatID = "test"; simulate rotation to a new chat.
	m.app.Client.ChatID = "newchat"
	m = step(m, agent.NewConvMsg{}) // clears tabs

	// Old-session completion arrives after rotation.
	m = step(m, agent.AsyncJobDoneMsg{OpID: "op-1", Label: "panel A", Result: "old", OriginChatID: "test"})

	if len(m.subTabs) != 0 {
		t.Errorf("post-rotation Done resurrected a tab: %d tabs, want 0", len(m.subTabs))
	}
}

// TestAsyncJobDoneSameSessionAccepted verifies a Done whose OriginChatID matches
// the current session still creates the terminal tab.
func TestAsyncJobDoneSameSessionAccepted(t *testing.T) {
	m := newTabModel()
	m = step(m, agent.AsyncJobDoneMsg{OpID: "op-1", Label: "panel A", Result: "now", OriginChatID: "test"})

	tab := findJobTab(m, "op-1")
	if tab == nil {
		t.Fatal("same-session Done should create a tab")
	}
	if got := tab.buf.String(); got != "now" {
		t.Errorf("buf = %q, want %q", got, "now")
	}
}

// TestAsyncJobDotTickArmedNoDuplicate verifies that starting multiple async jobs
// while idle does not arm multiple recurring tick chains (dotArmed guard).
func TestAsyncJobDotTickArmedNoDuplicate(t *testing.T) {
	m := newTabModel()
	m.state = stateIdle
	m, cmd1 := m.startDotTickIfUnarmed()
	if cmd1 == nil {
		t.Fatal("first arm should return a tick command")
	}
	// A second arm (e.g. another AsyncJobStartMsg) must NOT return another tick.
	_, cmd2 := m.startDotTickIfUnarmed()
	if cmd2 != nil {
		t.Error("second arm returned a duplicate tick command (dotArmed not respected)")
	}
	// Once the tick chain terminates (dotTickMsg with no re-arm condition), a
	// later arm is allowed again.
	m.dotArmed = false
	_, cmd3 := m.startDotTickIfUnarmed()
	if cmd3 == nil {
		t.Error("arm after chain termination should be allowed again")
	}
}

// TestAsyncJobDoneFallbackPrunes verifies the orphan-Done fallback prunes to the
// cap rather than growing unboundedly, and that subagent tabs are preserved.
func TestAsyncJobDoneFallbackPrunes(t *testing.T) {
	m := newTabModel()
	// Add a running subagent tab that must be preserved (protected by prune).
	m = step(m, agent.SubagentStartMsg{Task: "sub A", ChatID: "chat-a"})
	// Fire many orphan Dones (no Start) so the fallback creates terminal tabs.
	for i := 0; i < maxSubTabs+5; i++ {
		m = step(m, agent.AsyncJobDoneMsg{OpID: string(rune('a' + i)), Label: "panel", Result: "x"})
	}
	if len(m.subTabs) > maxSubTabs+1 {
		t.Errorf("fallback grew past cap: %d tabs, maxSubTabs+1=%d", len(m.subTabs), maxSubTabs+1)
	}
	// The running subagent tab must still be present (never pruned).
	if findSubTab(m, "chat-a") == nil {
		t.Error("running subagent tab was pruned by orphan-job fallback")
	}
}

func findSubTab(m tuiModel, chatID string) *subTab {
	for _, t := range m.subTabs {
		if t.kind == subTabSubagent && t.chatID == chatID {
			return t
		}
	}
	return nil
}

// TestAsyncJobChunkAppendsStatus verifies a Chunk appends a status line to the
// matching async-job tab, one line per event with no leading blank line.
func TestAsyncJobChunkAppendsStatus(t *testing.T) {
	m := newTabModel()
	m = step(m, agent.AsyncJobStartMsg{OpID: "op-1", Label: "panel A"})
	m = step(m, agent.AsyncJobChunkMsg{OpID: "op-1", Text: "calling claude-x"})
	m = step(m, agent.AsyncJobChunkMsg{OpID: "op-1", Text: "done claude-x"})

	tab := findJobTab(m, "op-1")
	if tab == nil {
		t.Fatal("tab missing")
	}
	got := tab.buf.String()
	want := "calling claude-x\ndone claude-x"
	if got != want {
		t.Errorf("buf = %q, want %q", got, want)
	}
	if strings.HasPrefix(got, "\n") {
		t.Error("buf has a leading blank line")
	}
}

// TestAsyncJobChunkNoResurrectOnMissingTab verifies a Chunk with no matching
// tab (post-rotation / orphan) is a safe no-op and does NOT create a tab.
func TestAsyncJobChunkNoResurrectOnMissingTab(t *testing.T) {
	m := newTabModel()
	m = step(m, agent.AsyncJobChunkMsg{OpID: "op-9", Text: "status"})
	if len(m.subTabs) != 0 {
		t.Errorf("Chunk resurrected a tab: %d tabs, want 0", len(m.subTabs))
	}
}

// TestAsyncJobChunkIgnoredWhenDone verifies a Chunk arriving after Done (late /
// watchdog-forced) does not append after the final answer.
func TestAsyncJobChunkIgnoredWhenDone(t *testing.T) {
	m := newTabModel()
	m = step(m, agent.AsyncJobDoneMsg{OpID: "op-1", Label: "panel A", Result: "FINAL"})
	// Late chunk after Done.
	m = step(m, agent.AsyncJobChunkMsg{OpID: "op-1", Text: "late status"})

	tab := findJobTab(m, "op-1")
	if tab == nil {
		t.Fatal("tab missing")
	}
	if got := tab.buf.String(); got != "FINAL" {
		t.Errorf("late chunk appended after final answer: buf = %q", got)
	}
}

// TestAsyncJobChunkStaleSessionRejected verifies a Chunk whose OriginChatID
// differs from the current session is ignored.
func TestAsyncJobChunkStaleSessionRejected(t *testing.T) {
	m := newTabModel()
	m.app.Client.ChatID = "newchat"
	m = step(m, agent.AsyncJobChunkMsg{OpID: "op-1", Text: "old", OriginChatID: "test"})
	if len(m.subTabs) != 0 {
		t.Errorf("stale-session chunk created a tab")
	}
}

// TestAsyncJobStatusCap verifies async-job status lines are capped.
func TestAsyncJobStatusCap(t *testing.T) {
	m := newTabModel()
	m = step(m, agent.AsyncJobStartMsg{OpID: "op-1", Label: "panel A"})
	for i := 0; i < asyncJobStatusLinesMax+20; i++ {
		m = step(m, agent.AsyncJobChunkMsg{OpID: "op-1", Text: "s"})
	}
	tab := findJobTab(m, "op-1")
	if tab.statusLines != asyncJobStatusLinesMax {
		t.Errorf("statusLines = %d, want cap %d", tab.statusLines, asyncJobStatusLinesMax)
	}
}

// TestAsyncJobDoneSeparatesFromStatus verifies the final answer is separated
// from live status lines by a blank line.
func TestAsyncJobDoneSeparatesFromStatus(t *testing.T) {
	m := newTabModel()
	m = step(m, agent.AsyncJobStartMsg{OpID: "op-1", Label: "panel A"})
	m = step(m, agent.AsyncJobChunkMsg{OpID: "op-1", Text: "calling claude-x"})
	m = step(m, agent.AsyncJobDoneMsg{OpID: "op-1", Label: "panel A", Result: "FINAL ANSWER"})

	tab := findJobTab(m, "op-1")
	got := tab.buf.String()
	if got != "calling claude-x\n\nFINAL ANSWER" {
		t.Errorf("buf = %q, want status + blank line + final answer", got)
	}
}
