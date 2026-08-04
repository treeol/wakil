package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	agent "github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
)

// appWithOutputMode returns a test app (like newTestApp) but with a specific
// OutputMode so NewTUIModel snapshots it as the model's startup mode.
func appWithOutputMode(mode config.OutputMode) *agent.App {
	app := newTestApp("http://unused", newFakeExecutor(), nil)
	app.Cfg.OutputMode = mode
	return app
}

func TestOutputModeDefaultIsDebug(t *testing.T) {
	m := NewTUIModel(newTestApp("http://unused", newFakeExecutor(), nil))
	if m.outputMode != config.OutputModeDebug {
		t.Fatalf("default outputMode = %q, want %q", m.outputMode, config.OutputModeDebug)
	}
}

func TestOutputModeDebugShowsDiagnostics(t *testing.T) {
	m := NewTUIModel(appWithOutputMode(config.OutputModeDebug))
	m = step(m, tea.WindowSizeMsg{Width: 200, Height: 50})
	m = step(m, agent.CompactedMsg{})
	m.refreshViewport()
	view := plain(m.View())
	if !strings.Contains(view, "compacted earlier turns") {
		t.Errorf("debug mode should show diagnostic note; view lacked 'compacted earlier turns'")
	}
}

func TestOutputModeSimpleHidesDiagnostics(t *testing.T) {
	m := NewTUIModel(appWithOutputMode(config.OutputModeSimple))
	m = step(m, tea.WindowSizeMsg{Width: 200, Height: 50})
	// Add an actionable iSys note first (must stay), then a diagnostic iDiag.
	m = step(m, agent.CompactedMsg{})                 // iDiag
	m.addItem(iSys, "error: something went wrong")    // actionable — must stay
	m.addItem(iSys, "▶ user prompt")                  // iUser-ish content stays
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
	m := NewTUIModel(appWithOutputMode(config.OutputModeSimple))
	m = step(m, agent.CompactedMsg{})
	// Item is retained in the transcript even though the view hides it.
	if len(*m.items) != 1 {
		t.Fatalf("simple mode hid an item from storage: items len = %d, want 1 (retained)", len(*m.items))
	}
	if (*m.items)[0].kind != iDiag {
		t.Errorf("retained item kind = %v, want iDiag", (*m.items)[0].kind)
	}
}

func TestOutputModeSimpleHidesLiveReasoningBody(t *testing.T) {
	m := NewTUIModel(appWithOutputMode(config.OutputModeSimple))
	m = step(m, tea.WindowSizeMsg{Width: 200, Height: 50})
	m = step(m, agent.ReasoningChunkMsg{Text: "thinking hard about the problem"})
	m.refreshViewport()
	view := plain(m.View())
	if strings.Contains(view, "thinking hard about the problem") {
		t.Errorf("simple mode should hide the live reasoning BODY; view contained reasoning text")
	}
}

func TestOutputModeDebugShowsLiveReasoningBody(t *testing.T) {
	m := NewTUIModel(appWithOutputMode(config.OutputModeDebug))
	m = step(m, tea.WindowSizeMsg{Width: 200, Height: 50})
	m = step(m, agent.ReasoningChunkMsg{Text: "thinking hard about the problem"})
	m.refreshViewport()
	view := plain(m.View())
	if !strings.Contains(view, "thinking hard about the problem") {
		t.Errorf("debug mode should show the live reasoning BODY; view lacked reasoning text")
	}
}

// TestOutputModeSimpleNoLeadingSeparator guards against a hidden diagnostic
// item leaving a dangling user-separator: when the first visible item is a user
// item, simple mode must not render a separator above it.
func TestOutputModeSimpleNoLeadingSeparator(t *testing.T) {
	m := NewTUIModel(appWithOutputMode(config.OutputModeSimple))
	m = step(m, tea.WindowSizeMsg{Width: 200, Height: 50})
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

// TestOutputModeSimpleHidesCommittedThought guards the reasoning-collapse path:
// after reasoning is committed as an iDiag "· thought" line, simple mode must
// hide it, while the answer content streams.
func TestOutputModeSimpleHidesCommittedThought(t *testing.T) {
	m := NewTUIModel(appWithOutputMode(config.OutputModeSimple))
	m = step(m, tea.WindowSizeMsg{Width: 200, Height: 50})
	// Reasoning, then answer content — collapses reasoning to a committed iDiag.
	m = step(m, agent.ReasoningChunkMsg{Text: "internal thinking"})
	m = step(m, agent.StreamChunkMsg{Text: "the final answer here"})
	m.refreshViewport()
	view := plain(m.View())
	if strings.Contains(view, "thought") || strings.Contains(view, "tokens") {
		t.Errorf("simple mode must hide the committed thought collapse line; view contained 'thought'/'tokens'")
	}
	if !strings.Contains(view, "final answer") {
		t.Errorf("simple mode must keep the answer streaming tail; view lacked 'final answer'")
	}
}

// TestOutputModeStartupSnapshot verifies the mode is snapshotted at construction
// and a later mutation of app.Cfg does NOT change rendering (startup-only).
func TestOutputModeStartupSnapshot(t *testing.T) {
	app := appWithOutputMode(config.OutputModeSimple)
	m := NewTUIModel(app)
	m = step(m, tea.WindowSizeMsg{Width: 200, Height: 50})
	// Flip the config after construction — must have no effect on the model.
	app.Cfg.OutputMode = config.OutputModeDebug
	m = step(m, agent.CompactedMsg{})
	m.refreshViewport()
	if strings.Contains(plain(m.View()), "compacted") {
		t.Errorf("startup snapshot violated: changing app.Cfg.OutputMode mid-session changed rendering")
	}
}
