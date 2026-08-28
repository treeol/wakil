package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/core/event"
)

// modelWithOutputMode builds a wiring model and sets the (startup-only)
// output mode directly — the mode is snapshotted at construction.
func modelWithOutputMode(mode config.OutputMode) (tuiModel, *fakeFacade) {
	f := &fakeFacade{sid: "sess_tui_test", chatID: "chat123"}
	m := newWiringModel(f)
	m.outputMode = mode
	return step(m, tea.WindowSizeMsg{Width: 200, Height: 50}), f
}

func TestOutputModeDefaultIsDebug(t *testing.T) {
	m := newWiringModel(&fakeFacade{sid: "sess_tui_test", chatID: "chat123"})
	if m.outputMode != config.OutputModeDebug {
		t.Fatalf("default outputMode = %q, want debug", m.outputMode)
	}
}

func TestOutputModeDebugShowsDiagnostics(t *testing.T) {
	m, f := modelWithOutputMode(config.OutputModeDebug)
	m = step(m, evt(event.KindConversationCompacted, event.ConversationCompacted{TurnID: "trn_1"}, f.sid))
	m.refreshViewport()
	view := plain(m.View())
	if !strings.Contains(view, "compacted earlier turns") {
		t.Errorf("debug mode should show diagnostic note; view lacked 'compacted earlier turns'")
	}
}

func TestOutputModeSimpleHidesDiagnostics(t *testing.T) {
	m, f := modelWithOutputMode(config.OutputModeSimple)
	// Add an actionable iSys note first (must stay), then a diagnostic iDiag.
	m = step(m, evt(event.KindConversationCompacted, event.ConversationCompacted{TurnID: "trn_1"}, f.sid)) // iDiag
	m.addItem(iSys, "error: something went wrong")                                                         // actionable — must stay
	m.addItem(iSys, "▶ user prompt")                                                                       // iUser-ish content stays
	m.refreshViewport()
	view := plain(m.View())
	if strings.Contains(view, "compacted earlier turns") {
		t.Errorf("simple mode should hide diagnostic note; view contained 'compacted earlier turns'")
	}
	if !strings.Contains(view, "something went wrong") {
		t.Errorf("simple mode must keep actionable error; view lost 'error: something went wrong'")
	}
	if !strings.Contains(view, "user prompt") {
		t.Errorf("simple mode must keep user prompt; view lost 'user prompt'")
	}
}

func TestOutputModeSimpleRetainsDiagnostics(t *testing.T) {
	m, f := modelWithOutputMode(config.OutputModeSimple)
	m = step(m, evt(event.KindConversationCompacted, event.ConversationCompacted{TurnID: "trn_1"}, f.sid))
	// Item is retained in the transcript even though the view hides it.
	if len(*m.items) != 1 {
		t.Fatalf("simple mode hid an item from storage: items len = %d, want 1 (retained)", len(*m.items))
	}
	if (*m.items)[0].kind != iDiag {
		t.Errorf("retained item kind = %v, want iDiag", (*m.items)[0].kind)
	}
}

func TestOutputModeSimpleHidesLiveReasoningBody(t *testing.T) {
	m, f := modelWithOutputMode(config.OutputModeSimple)
	m = step(m, evt(event.KindReasoningDelta, event.ReasoningDelta{Text: "thinking hard about the problem"}, f.sid))
	m.refreshViewport()
	view := plain(m.View())
	if strings.Contains(view, "thinking hard about the problem") {
		t.Errorf("simple mode should hide the live reasoning BODY; view contained reasoning text")
	}
}

func TestOutputModeDebugShowsLiveReasoningBody(t *testing.T) {
	m, f := modelWithOutputMode(config.OutputModeDebug)
	m = step(m, evt(event.KindReasoningDelta, event.ReasoningDelta{Text: "thinking hard about the problem"}, f.sid))
	m.refreshViewport()
	view := plain(m.View())
	if !strings.Contains(view, "thinking hard about the problem") {
		t.Errorf("debug mode should show the live reasoning BODY; view lacked reasoning text")
	}
}

// TestOutputModeSimpleNoLeadingSeparator guards against a hidden diagnostic
// item leaving a dangling user-separator.
func TestOutputModeSimpleNoLeadingSeparator(t *testing.T) {
	m, _ := modelWithOutputMode(config.OutputModeSimple)
	// Diagnostic first, then a user item.
	m.addItem(iDiag, dim2("· compacted earlier turns"))
	m.addItem(iUser, "my prompt")
	m.refreshViewport()
	view := plain(m.View())
	if strings.Contains(view, "compacted") {
		t.Errorf("simple mode should hide diagnostic; view contained 'compacted'")
	}
	if !strings.Contains(view, "my prompt") {
		t.Errorf("simple mode must keep the user prompt; view lacked 'my prompt'")
	}
	// A separator is a line of only box-drawing dashes. With no earlier visible
	// item, no separator may precede the user prompt.
	for _, ln := range strings.Split(view, "\n") {
		trimmed := strings.TrimSpace(ln)
		if trimmed != "" && strings.Trim(trimmed, "─") == "" {
			t.Errorf("simple mode must not render a leading separator above the first user prompt; found line %q", ln)
		}
	}
}

// TestOutputModeSimpleHidesCommittedThought guards the reasoning-collapse path.
func TestOutputModeSimpleHidesCommittedThought(t *testing.T) {
	m, f := modelWithOutputMode(config.OutputModeSimple)
	// Reasoning, then answer content — collapses reasoning to a committed iDiag.
	m = step(m, evt(event.KindReasoningDelta, event.ReasoningDelta{Text: "internal thinking"}, f.sid))
	m = step(m, evt(event.KindMessageDelta, event.MessageDelta{Text: "the final answer here"}, f.sid))
	m.refreshViewport()
	view := plain(m.View())
	if strings.Contains(view, "thought") || strings.Contains(view, "tokens") {
		t.Errorf("simple mode must hide the committed thought collapse line; view contained 'thought'/'tokens'")
	}
	if !strings.Contains(view, "final answer") {
		t.Errorf("simple mode must keep the answer streaming tail; view lacked 'final answer'")
	}
}
