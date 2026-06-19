package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	agent "wakil/internal/agent"

	"wakil/internal/config"
	"wakil/internal/proxy"
)

// newTestTUI builds a driven-ready model over a minimal app.
func newTestTUI(t *testing.T) tuiModel {
	t.Helper()
	app := &agent.App{Cfg: config.DefaultConfig(), Client: newTestClient(""), Exec: newFakeExecutor()}
	m := NewTUIModel(app)
	return m
}


// lastItemText returns the ANSI-stripped text of the most recent conv item.
func lastItemText(m tuiModel) string {
	items := *m.items
	if len(items) == 0 {
		return ""
	}
	return plain(items[len(items)-1].text)
}

func TestUpdateSysNoteAppendsItem(t *testing.T) {
	m := newTestTUI(t)
	before := len(*m.items)
	m = step(m, agent.SysNoteMsg{Text: "hello note"})
	if len(*m.items) != before+1 {
		t.Fatalf("agent.SysNoteMsg should append exactly one item")
	}
	if !strings.Contains(lastItemText(m), "hello note") {
		t.Errorf("note text missing; got %q", lastItemText(m))
	}
}

func TestUpdateAgentDonePlainError(t *testing.T) {
	m := newTestTUI(t)
	m.state = stateStreaming
	m = step(m, agent.AgentDoneMsg{Err: errors.New("boom")})
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

func TestUpdateAgentDoneBackendStreamErrorIsTidy(t *testing.T) {
	m := newTestTUI(t)
	m.state = stateStreaming
	m = step(m, agent.AgentDoneMsg{Err: fmt.Errorf("%w: connection reset by peer", proxy.ErrBackendStream)})
	got := lastItemText(m)
	if !strings.Contains(got, "backend stream error") {
		t.Errorf("stream error should be surfaced; got %q", got)
	}
	// The raw low-level cause must NOT leak into the rendered line.
	if strings.Contains(got, "connection reset by peer") {
		t.Errorf("raw cause should not be shown to the user; got %q", got)
	}
	if strings.Contains(got, "error:") {
		t.Errorf("stream error should not render as a raw red 'error:' trace; got %q", got)
	}
}

func TestUpdateAgentDoneWarnLine(t *testing.T) {
	m := newTestTUI(t)
	m.state = stateStreaming
	m = step(m, agent.AgentDoneMsg{Warn: "⚠ backend stream error (near the context limit)"})
	if got := lastItemText(m); !strings.Contains(got, "backend stream error") || !strings.Contains(got, "context limit") {
		t.Errorf("warn line should render verbatim; got %q", got)
	}
}

func TestUpdateAgentDoneCancelled(t *testing.T) {
	m := newTestTUI(t)
	m.state = stateStreaming
	m = step(m, agent.AgentDoneMsg{Err: context.Canceled})
	if got := lastItemText(m); !strings.Contains(got, "cancelled") {
		t.Errorf("cancellation should render '[turn cancelled]'; got %q", got)
	}
	if m.state != stateIdle {
		t.Errorf("cancelled turn should return to idle")
	}
}

func TestUpdateNewConvClearsViewport(t *testing.T) {
	m := newTestTUI(t)
	m = step(m, agent.SysNoteMsg{Text: "a"})
	m = step(m, agent.SysNoteMsg{Text: "b"})
	m = step(m, agent.NewConvMsg{Note: "fresh conversation: abc"})
	items := *m.items
	if len(items) != 1 {
		t.Fatalf("agent.NewConvMsg should clear items and leave only the note; got %d", len(items))
	}
	if !strings.Contains(plain(items[0].text), "fresh conversation") {
		t.Errorf("note should remain after clear; got %q", plain(items[0].text))
	}
}

func TestUpdateStreamingAndTokRate(t *testing.T) {
	m := newTestTUI(t)
	m = step(m, agent.StreamChunkMsg{Text: "partial answer"})
	if m.streaming.String() != "partial answer" {
		t.Errorf("stream chunk should accumulate; got %q", m.streaming.String())
	}
	m = step(m, agent.TokRateMsg{Tps: 42.5})
	if m.tps != 42.5 {
		t.Errorf("agent.TokRateMsg should set tps; got %v", m.tps)
	}
}

func TestUpdateReasoningCollapsesOnFirstContent(t *testing.T) {
	m := newTestTUI(t)
	m = step(m, agent.ReasoningChunkMsg{Text: "thinking hard about it"})
	if m.reasoning.Len() == 0 {
		t.Fatal("reasoning should accumulate")
	}
	before := len(*m.items)
	m = step(m, agent.StreamChunkMsg{Text: "the answer"})
	// First content delta collapses the reasoning buffer into one committed line.
	if m.reasoning.Len() != 0 || !m.reasoningDone {
		t.Error("reasoning should be collapsed on first content")
	}
	if len(*m.items) != before+1 || !strings.Contains(lastItemText(m), "thought") {
		t.Errorf("a collapsed 'thought' line should be committed; last=%q", lastItemText(m))
	}
}

func TestUpdateCompactedAndCopied(t *testing.T) {
	m := newTestTUI(t)
	m = step(m, agent.CompactedMsg{})
	if !strings.Contains(lastItemText(m), "compacted") {
		t.Errorf("agent.CompactedMsg should note a compaction; got %q", lastItemText(m))
	}
	m = step(m, copiedMsg{n: 128})
	if !strings.Contains(m.flash, "128") {
		t.Errorf("copiedMsg should set the flash with the count; got %q", m.flash)
	}
}
