package tui

import (

	"strings"
	"testing"

	"github.com/treeol/wakil/internal/core/event"

	"github.com/charmbracelet/x/ansi"
)

// newTestTUI builds a driven-ready wiring model.
func newTestTUI(t *testing.T) (tuiModel, *fakeFacade) {
	t.Helper()
	f := &fakeFacade{sid: "sess_tui_test", chatID: "chat123"}
	return newWiringModel(f), f
}

// lastItemText returns the ANSI-stripped text of the most recent conv item.
func lastItemText(m tuiModel) string {
	items := *m.items
	if len(items) == 0 {
		return ""
	}
	return plain(items[len(items)-1].text)
}

func TestUpdateSessionNoteAppendsItem(t *testing.T) {
	m, f := newTestTUI(t)
	before := len(*m.items)
	m = step(m, evt(event.KindSessionNote, event.SessionNote{Text: "hello note"}, f.sid))
	if len(*m.items) != before+1 {
		t.Fatalf("SessionNote should append exactly one item")
	}
	if !strings.Contains(lastItemText(m), "hello note") {
		t.Errorf("note text missing; got %q", lastItemText(m))
	}
}

func TestUpdateTurnCompletedPlainError(t *testing.T) {
	m, f := newTestTUI(t)
	m.state = stateStreaming
	m = step(m, evt(event.KindTurnCompleted, event.TurnCompleted{TurnID: "trn_1", Outcome: "stream_error"}, f.sid))
	m = step(m, evt(event.KindSessionError, event.SessionError{Reason: "backend_failure", Err: "boom"}, f.sid))
	if m.state != stateIdle {
		t.Errorf("turn end should return to idle; got %v", m.state)
	}
	if got := lastItemText(m); !strings.Contains(got, "error:") || !strings.Contains(got, "boom") {
		t.Errorf("plain error should render as 'error: boom'; got %q", got)
	}
	if !m.hadTurn {
		t.Error("hadTurn should be set after a completed turn")
	}
}

func TestUpdateCancelledRendersTidy(t *testing.T) {
	m, f := newTestTUI(t)
	m.state = stateStreaming
	m = step(m, evt(event.KindTurnCompleted, event.TurnCompleted{TurnID: "trn_1", Outcome: "cancelled"}, f.sid))
	if got := lastItemText(m); !strings.Contains(got, "cancelled") {
		t.Errorf("cancellation should render '[turn cancelled]'; got %q", got)
	}
	if m.state != stateIdle {
		t.Errorf("cancelled turn should return to idle")
	}
}

func TestUpdateRotationClearsViewport(t *testing.T) {
	m, f := newTestTUI(t)
	m = step(m, evt(event.KindSessionNote, event.SessionNote{Text: "a"}, f.sid))
	m = step(m, evt(event.KindSessionNote, event.SessionNote{Text: "b"}, f.sid))
	m = step(m, rotationMsg{facade: rotatedFake()})
	items := *m.items
	if len(items) == 0 {
		t.Fatal("rotation should clear items (and may add its own notes)")
	}
	for _, it := range items {
		if strings.Contains(plain(it.text), "> a<") {
			t.Fatal("old item survived rotation")
		}
	}
}

func TestUpdateStreamingAndTokRate(t *testing.T) {
	m, f := newTestTUI(t)
	m = step(m, evt(event.KindMessageDelta, event.MessageDelta{Text: "partial answer"}, f.sid))
	if m.streaming.String() != "partial answer" {
		t.Errorf("stream chunk should accumulate; got %q", m.streaming.String())
	}
	m = step(m, evt(event.KindTokRate, event.TokRate{Rate: 42.5}, f.sid))
	if m.tps != 42.5 {
		t.Errorf("TokRate should set tps; got %v", m.tps)
	}
}

func TestUpdateReasoningCollapsesOnFirstContent(t *testing.T) {
	m, f := newTestTUI(t)
	m = step(m, evt(event.KindReasoningDelta, event.ReasoningDelta{Text: "thinking hard about it"}, f.sid))
	if m.reasoning.Len() == 0 {
		t.Fatal("reasoning should accumulate")
	}
	before := len(*m.items)
	m = step(m, evt(event.KindMessageDelta, event.MessageDelta{Text: "the answer"}, f.sid))
	// First content delta collapses the reasoning buffer into one committed line.
	if m.reasoning.Len() != 0 || !m.reasoningDone {
		t.Error("reasoning should be collapsed on first content")
	}
	if m.reasoningExpanded {
		t.Error("reasoningExpanded should be reset on collapse")
	}
	if len(*m.items) != before+1 || !strings.Contains(lastItemText(m), "thought") {
		t.Errorf("a collapsed 'thought' line should be committed; last=%q", lastItemText(m))
	}
}

func TestUpdateReasoningExpandedResetsOnDone(t *testing.T) {
	m, f := newTestTUI(t)
	m.reasoningExpanded = true
	m = step(m, evt(event.KindTurnCompleted, event.TurnCompleted{TurnID: "trn_1", Outcome: "complete"}, f.sid))
	if m.reasoningExpanded {
		t.Error("reasoningExpanded should be reset on TurnCompleted")
	}
}

func TestUpdateReasoningExpandedResetsOnRotation(t *testing.T) {
	m, _ := newTestTUI(t)
	m.reasoningExpanded = true
	m = step(m, rotationMsg{facade: rotatedFake()})
	if m.reasoningExpanded {
		t.Error("reasoningExpanded should be reset on rotation")
	}
}

func TestRenderReasoningCollapsed(t *testing.T) {
	// Generate enough text to exceed maxReasoningCollapsedLines when wrapped.
	long := strings.Repeat("thinking about stuff ", 50)
	out := renderReasoning(long, 40, false)
	plainOut := ansi.Strip(out)
	lines := strings.Split(plainOut, "\n")
	// Collapsed: indicator + last N lines = maxReasoningCollapsedLines+1.
	if len(lines) > maxReasoningCollapsedLines+1 {
		t.Errorf("collapsed reasoning should be capped at %d lines; got %d",
			maxReasoningCollapsedLines+1, len(lines))
	}
	if !strings.Contains(plainOut, "ctrl+e to expand") {
		t.Error("collapsed reasoning should show the expand indicator")
	}
}

func TestRenderReasoningExpanded(t *testing.T) {
	long := strings.Repeat("thinking about stuff ", 50)
	out := renderReasoning(long, 40, true)
	plainOut := ansi.Strip(out)
	lines := strings.Split(plainOut, "\n")
	// Expanded: all lines, no indicator.
	if len(lines) <= maxReasoningCollapsedLines {
		t.Error("expanded reasoning should show all lines")
	}
	if strings.Contains(plainOut, "ctrl+e to expand") {
		t.Error("expanded reasoning should not show the collapse indicator")
	}
}

func TestRenderReasoningShortShowsAll(t *testing.T) {
	short := "just a brief thought"
	out := renderReasoning(short, 40, false)
	plainOut := ansi.Strip(out)
	if strings.Contains(plainOut, "ctrl+e to expand") {
		t.Error("short reasoning should not show the expand indicator")
	}
}

func TestUpdateCompactedAndCopied(t *testing.T) {
	m, f := newTestTUI(t)
	m = step(m, evt(event.KindConversationCompacted, event.ConversationCompacted{TurnID: "trn_1"}, f.sid))
	if !strings.Contains(lastItemText(m), "compacted") {
		t.Errorf("ConversationCompacted should note a compaction; got %q", lastItemText(m))
	}
	m = step(m, copiedMsg{n: 128})
	if !strings.Contains(m.flash, "128") {
		t.Errorf("copiedMsg should set the flash with the count; got %q", m.flash)
	}
}
