package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	agent "github.com/treeol/wakil/internal/agent"

	tea "github.com/charmbracelet/bubbletea"
)

// midTurnEnter simulates typing text into the textarea and pressing Enter
// while the model is in the given state. Returns the updated model.
func midTurnEnter(m tuiModel, text string, state agentState) tuiModel {
	m.state = state
	m.ta.SetValue(text)
	m, _, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	return m
}

func TestQueuePrompt_MidTurn_QueuesVisibly(t *testing.T) {
	m := newTestTUI(t)
	m = midTurnEnter(m, "follow up question", stateStreaming)
	if len(m.queuedPrompts) != 1 {
		t.Fatalf("expected 1 queued prompt, got %d", len(m.queuedPrompts))
	}
	if m.queuedPrompts[0] != "follow up question" {
		t.Errorf("queued text mismatch: %q", m.queuedPrompts[0])
	}
	// Textarea should be cleared.
	if m.ta.Value() != "" {
		t.Errorf("textarea should be cleared after queueing, got %q", m.ta.Value())
	}
	// A visible notice should be added to the conversation.
	last := lastItemText(m)
	if !strings.Contains(last, "queue") {
		t.Errorf("expected queue notice in conversation, last item: %q", last)
	}
}

func TestQueuePrompt_MidTurnCompacting_AlsoQueues(t *testing.T) {
	m := newTestTUI(t)
	m = midTurnEnter(m, "queued during compaction", stateCompacting)
	if len(m.queuedPrompts) != 1 {
		t.Fatalf("expected 1 queued prompt during compacting, got %d", len(m.queuedPrompts))
	}
}

func TestQueuePrompt_EmptyInput_MidTurn_DoesNothing(t *testing.T) {
	m := newTestTUI(t)
	m = midTurnEnter(m, "   ", stateStreaming)
	if len(m.queuedPrompts) != 0 {
		t.Fatalf("empty input should not queue, got %d", len(m.queuedPrompts))
	}
}

func TestQueuePrompt_SlashCommand_RejectedWithNotice(t *testing.T) {
	m := newTestTUI(t)
	m = midTurnEnter(m, "/backend openrouter", stateStreaming)
	if len(m.queuedPrompts) != 0 {
		t.Fatalf("/backend should NOT be queued, got %d queued prompts", len(m.queuedPrompts))
	}
	last := lastItemText(m)
	if !strings.Contains(last, "not available mid-turn") {
		t.Errorf("expected reject notice, got: %q", last)
	}
}

func TestQueuePrompt_AutoCommand_RejectedWithNotice(t *testing.T) {
	m := newTestTUI(t)
	m = midTurnEnter(m, "/auto", stateStreaming)
	if len(m.queuedPrompts) != 0 {
		t.Fatalf("/auto should NOT be queued in Phase A, got %d", len(m.queuedPrompts))
	}
	last := lastItemText(m)
	if !strings.Contains(last, "not available") {
		t.Errorf("expected /auto reject notice, got: %q", last)
	}
}

func TestQueuePrompt_InfoCommand_ExecutesImmediately(t *testing.T) {
	m := newTestTUI(t)
	infoActiveBefore := m.infoPanel.active
	m = midTurnEnter(m, "/info", stateStreaming)
	if m.infoPanel.active == infoActiveBefore {
		t.Error("/info should toggle the info panel even mid-turn")
	}
	if len(m.queuedPrompts) != 0 {
		t.Fatalf("/info should not queue, got %d", len(m.queuedPrompts))
	}
}

func TestQueuePrompt_ConfirmGate_OwnsInput(t *testing.T) {
	m := newTestTUI(t)
	m.state = stateConfirm
	m.pendConf = &agent.ConfirmReqMsg{
		ToolName: "run_shell",
		RespCh:   make(chan agent.ConfirmChoice, 1),
	}
	m.ta.SetValue("some text")
	// Enter in confirm state should go to the confirm gate, not the queue.
	m, _, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.queuedPrompts) != 0 {
		t.Fatalf("confirm gate should consume Enter; queue should be empty, got %d", len(m.queuedPrompts))
	}
}

func TestQueuePrompt_FlushOnIdle(t *testing.T) {
	m := newTestTUI(t)
	m.queuedPrompts = []string{"follow up"}
	m.state = stateStreaming
	// AgentDoneMsg with no error, no workflow continuation → flush.
	m = step(m, agent.AgentDoneMsg{})
	if len(m.queuedPrompts) != 0 {
		t.Fatalf("queue should be flushed on idle, got %d remaining", len(m.queuedPrompts))
	}
	if m.state != stateStreaming {
		t.Errorf("flush should start a new turn (stateStreaming), got %v", m.state)
	}
}

func TestQueuePrompt_HoldDuringWorkflowAutoContinue(t *testing.T) {
	m := newTestTUI(t)
	m.queuedPrompts = []string{"follow up"}
	m.state = stateStreaming
	m = step(m, agent.AgentDoneMsg{WorkflowWillContinue: true})
	if len(m.queuedPrompts) != 1 {
		t.Fatalf("queue should hold during workflow auto-continuation, got %d", len(m.queuedPrompts))
	}
	if m.state != stateIdle {
		t.Errorf("state should be idle (workflow turn not started yet), got %v", m.state)
	}
}

func TestQueuePrompt_HoldOnCancel(t *testing.T) {
	m := newTestTUI(t)
	m.queuedPrompts = []string{"follow up"}
	m.state = stateStreaming
	m = step(m, agent.AgentDoneMsg{Err: context.Canceled})
	if len(m.queuedPrompts) != 1 {
		t.Fatalf("queue should hold on cancel, got %d", len(m.queuedPrompts))
	}
}

func TestQueuePrompt_HoldOnError(t *testing.T) {
	m := newTestTUI(t)
	m.queuedPrompts = []string{"follow up"}
	m.state = stateStreaming
	m = step(m, agent.AgentDoneMsg{Err: errors.New("backend failed")})
	if len(m.queuedPrompts) != 1 {
		t.Fatalf("queue should hold on error, got %d", len(m.queuedPrompts))
	}
}

func TestQueuePrompt_MultipleQueued_FlushesOneAtATime(t *testing.T) {
	m := newTestTUI(t)
	m.queuedPrompts = []string{"first", "second"}
	m.state = stateStreaming
	// First flush.
	m = step(m, agent.AgentDoneMsg{})
	if len(m.queuedPrompts) != 1 {
		t.Fatalf("after first flush, 1 should remain, got %d", len(m.queuedPrompts))
	}
	if m.queuedPrompts[0] != "second" {
		t.Errorf("remaining should be 'second', got %q", m.queuedPrompts[0])
	}
}

func TestQueuePrompt_StatusLine_ShowsQueueCount(t *testing.T) {
	m := newTestTUI(t)
	m.queuedPrompts = []string{"a", "b", "c"}
	in := m.headerStatusInput()
	if in.queueLen != 3 {
		t.Errorf("headerStatusInput queueLen: expected 3, got %d", in.queueLen)
	}
}

func TestQueuePrompt_HoldOnBackendWarning(t *testing.T) {
	m := newTestTUI(t)
	m.queuedPrompts = []string{"follow up"}
	m.state = stateStreaming
	m = step(m, agent.AgentDoneMsg{Warn: "⚠ backend unreachable"})
	if len(m.queuedPrompts) != 1 {
		t.Fatalf("queue should hold on backend warning, got %d", len(m.queuedPrompts))
	}
}

func TestQueuePrompt_ClearedOnNewConv(t *testing.T) {
	m := newTestTUI(t)
	m.queuedPrompts = []string{"stale prompt 1", "stale prompt 2"}
	m = step(m, agent.NewConvMsg{Note: "fresh conversation"})
	if len(m.queuedPrompts) != 0 {
		t.Fatalf("queue should be cleared on NewConvMsg, got %d", len(m.queuedPrompts))
	}
}
