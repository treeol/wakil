package tui

import (
	"strings"
	"testing"
	"time"

	agent "github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"

	tea "github.com/charmbracelet/bubbletea"
)

func testSessions() []agent.Session {
	return []agent.Session{
		{ChatID: "newest01", Updated: time.Now(), Conv: []proxy.Message{{Role: "user", Content: strPtr("newest task")}}},
		{ChatID: "middle02", Updated: time.Now().Add(-time.Hour), Conv: []proxy.Message{{Role: "user", Content: strPtr("middle task")}}},
		{ChatID: "oldest03", Updated: time.Now().Add(-2 * time.Hour), Conv: []proxy.Message{{Role: "user", Content: strPtr("oldest task")}}},
	}
}

func newPickerModel() tuiModel {
	m := tuiModel{
		app:   &agent.App{Cfg: config.DefaultConfig(), Client: &proxy.Client{ChatID: "current"}},
		ta:    newTA(""),
		width: 80, height: 24, ready: true,
	}
	m = m.openResumePicker(agent.OpenResumePickerMsg{Sessions: testSessions(), Scope: agent.SessionScope{Workspace: "/work"}})
	return m
}

func TestOpenResumePicker_ActivatesAndClosesCompletion(t *testing.T) {
	m := tuiModel{app: &agent.App{Cfg: config.DefaultConfig()}, ta: newTA("")}
	m.comp = completionState{active: true}
	m = m.openResumePicker(agent.OpenResumePickerMsg{Sessions: testSessions()})
	if !m.resumePicker.active {
		t.Fatal("expected picker to be active")
	}
	if m.comp.active {
		t.Fatal("opening the resume picker should close the completion picker")
	}
	if len(m.resumePicker.sessions) != 3 {
		t.Fatalf("expected 3 sessions, got %d", len(m.resumePicker.sessions))
	}
	if m.resumePicker.sel != 0 {
		t.Fatalf("initial selection should be 0 (newest); got %d", m.resumePicker.sel)
	}
}

func TestResumePickerNavigation(t *testing.T) {
	m := newPickerModel()

	m, _, consumed := m.handleResumePickerKey(tea.KeyMsg{Type: tea.KeyDown})
	if !consumed {
		t.Fatal("down should be consumed")
	}
	if m.resumePicker.sel != 1 {
		t.Fatalf("sel after down = %d, want 1", m.resumePicker.sel)
	}

	m, _, _ = m.handleResumePickerKey(tea.KeyMsg{Type: tea.KeyUp})
	if m.resumePicker.sel != 0 {
		t.Fatalf("sel after up = %d, want 0", m.resumePicker.sel)
	}

	// Wraps at the top going up.
	m, _, _ = m.handleResumePickerKey(tea.KeyMsg{Type: tea.KeyUp})
	if m.resumePicker.sel != 2 {
		t.Fatalf("sel should wrap to last index (2); got %d", m.resumePicker.sel)
	}
}

func TestResumePickerEsc_ClosesWithoutMutating(t *testing.T) {
	m := newPickerModel()
	origChatID := m.app.Client.ChatID

	m, cmd, consumed := m.handleResumePickerKey(tea.KeyMsg{Type: tea.KeyEsc})
	if !consumed {
		t.Fatal("esc should be consumed")
	}
	if m.resumePicker.active {
		t.Fatal("esc should close the picker")
	}
	if cmd != nil {
		t.Fatal("esc should not produce a resume command")
	}
	if m.app.Client.ChatID != origChatID {
		t.Fatal("esc must not mutate app state")
	}
}

func TestResumePickerEnter_ResumesSelected(t *testing.T) {
	m := newPickerModel()
	// Select the middle session.
	m, _, _ = m.handleResumePickerKey(tea.KeyMsg{Type: tea.KeyDown})
	if m.resumePicker.sel != 1 {
		t.Fatalf("setup: sel = %d, want 1", m.resumePicker.sel)
	}

	m, cmd, consumed := m.handleResumePickerKey(tea.KeyMsg{Type: tea.KeyEnter})
	if !consumed {
		t.Fatal("enter should be consumed")
	}
	if m.resumePicker.active {
		t.Fatal("enter should close the picker")
	}
	if cmd == nil {
		t.Fatal("enter should produce a resume command")
	}
	msg := cmd()
	nc, ok := msg.(agent.NewConvMsg)
	if !ok {
		t.Fatalf("expected NewConvMsg from resume, got %T", msg)
	}
	if m.app.Client.ChatID != "middle02" {
		t.Fatalf("app.Client.ChatID = %q, want middle02", m.app.Client.ChatID)
	}
	if nc.Note == "" || !nc.RebuildConv {
		t.Fatalf("unexpected NewConvMsg: %+v", nc)
	}
}

func TestResumePickerCtrlC_NotConsumed(t *testing.T) {
	m := newPickerModel()
	_, _, consumed := m.handleResumePickerKey(tea.KeyMsg{Type: tea.KeyCtrlC})
	if consumed {
		t.Fatal("ctrl+c must not be consumed by the picker — quitting must still work")
	}
}

func TestResumePickerEmptyState(t *testing.T) {
	m := tuiModel{app: &agent.App{Cfg: config.DefaultConfig()}, ta: newTA(""), width: 80, height: 24, ready: true}
	m = m.openResumePicker(agent.OpenResumePickerMsg{Sessions: nil, Hidden: 2})
	out := plain(m.renderResumePicker())
	if !strings.Contains(out, "no sessions") {
		t.Fatalf("expected 'no sessions' hint; got %q", out)
	}
}
