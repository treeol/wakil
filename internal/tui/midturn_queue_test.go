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
	if m.queuedPrompts[0].text != "follow up question" {
		t.Errorf("queued text mismatch: %q", m.queuedPrompts[0].text)
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

func TestQueuePrompt_AutoCommand_DeferGrantMidTurn(t *testing.T) {
	m := newTestTUI(t)
	m = midTurnEnter(m, "/auto", stateStreaming)
	if len(m.queuedPrompts) != 0 {
		t.Fatalf("/auto should NOT be queued, got %d", len(m.queuedPrompts))
	}
	// /auto mid-turn (OFF→ON) defers the grant — it does not apply immediately.
	if !m.pendingAutoGrant {
		t.Error("OFF→ON /auto mid-turn should set pendingAutoGrant")
	}
	if m.app.Consent().AutoApprove {
		t.Error("deferred grant must NOT apply immediately — auto should still be OFF")
	}
	last := lastItemText(m)
	if !strings.Contains(last, "pending grant") {
		t.Errorf("expected pending-grant notice, got: %q", last)
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

func TestQueueCommand_MidTurnClearViaHandleKey(t *testing.T) {
	m := newTestTUI(t)
	m.queuedPrompts = []queuedPrompt{{text: "first"}, {text: "second"}}
	// Drive through the real handleKey path mid-turn (stateStreaming).
	m = midTurnEnter(m, "/queue clear", stateStreaming)
	if len(m.queuedPrompts) != 0 {
		t.Fatalf("queue should be empty after /queue clear via handleKey, got %d", len(m.queuedPrompts))
	}
	last := lastItemText(m)
	if !strings.Contains(last, "cleared") {
		t.Errorf("expected 'cleared' notice, got: %q", last)
	}
}

func TestQueueCommand_MidTurnListViaHandleKey(t *testing.T) {
	m := newTestTUI(t)
	m.queuedPrompts = []queuedPrompt{{text: "first question"}}
	m = midTurnEnter(m, "/queue", stateStreaming)
	last := lastItemText(m)
	if !strings.Contains(last, "queue (1):") {
		t.Errorf("expected 'queue (1):' via handleKey, got: %q", last)
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
	m.queuedPrompts = []queuedPrompt{{text: "follow up"}}
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
	m.queuedPrompts = []queuedPrompt{{text: "follow up"}}
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
	m.queuedPrompts = []queuedPrompt{{text: "follow up"}}
	m.state = stateStreaming
	m = step(m, agent.AgentDoneMsg{Err: context.Canceled})
	if len(m.queuedPrompts) != 1 {
		t.Fatalf("queue should hold on cancel, got %d", len(m.queuedPrompts))
	}
}

func TestQueuePrompt_HoldOnError(t *testing.T) {
	m := newTestTUI(t)
	m.queuedPrompts = []queuedPrompt{{text: "follow up"}}
	m.state = stateStreaming
	m = step(m, agent.AgentDoneMsg{Err: errors.New("backend failed")})
	if len(m.queuedPrompts) != 1 {
		t.Fatalf("queue should hold on error, got %d", len(m.queuedPrompts))
	}
}

func TestQueuePrompt_MultipleQueued_FlushesOneAtATime(t *testing.T) {
	m := newTestTUI(t)
	m.queuedPrompts = []queuedPrompt{{text: "first"}, {text: "second"}}
	m.state = stateStreaming
	// First flush.
	m = step(m, agent.AgentDoneMsg{})
	if len(m.queuedPrompts) != 1 {
		t.Fatalf("after first flush, 1 should remain, got %d", len(m.queuedPrompts))
	}
	if m.queuedPrompts[0].text != "second" {
		t.Errorf("remaining should be 'second', got %q", m.queuedPrompts[0].text)
	}
}

func TestQueuePrompt_StatusLine_ShowsQueueCount(t *testing.T) {
	m := newTestTUI(t)
	m.queuedPrompts = []queuedPrompt{{text: "a"}, {text: "b"}, {text: "c"}}
	in := m.headerStatusInput()
	if in.queueLen != 3 {
		t.Errorf("headerStatusInput queueLen: expected 3, got %d", in.queueLen)
	}
}

func TestQueuePrompt_HoldOnBackendWarning(t *testing.T) {
	m := newTestTUI(t)
	m.queuedPrompts = []queuedPrompt{{text: "follow up"}}
	m.state = stateStreaming
	m = step(m, agent.AgentDoneMsg{Warn: "⚠ backend unreachable"})
	if len(m.queuedPrompts) != 1 {
		t.Fatalf("queue should hold on backend warning, got %d", len(m.queuedPrompts))
	}
}

func TestQueuePrompt_ClearedOnNewConv(t *testing.T) {
	m := newTestTUI(t)
	m.queuedPrompts = []queuedPrompt{{text: "stale prompt 1"}, {text: "stale prompt 2"}}
	m = step(m, agent.NewConvMsg{Note: "fresh conversation"})
	if len(m.queuedPrompts) != 0 {
		t.Fatalf("queue should be cleared on NewConvMsg, got %d", len(m.queuedPrompts))
	}
	// The clear notice is added before the NewConvMsg note — check all items.
	found := false
	for _, item := range *m.items {
		if strings.Contains(item.text, "queue cleared") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'queue cleared' notice in conversation items")
	}
}

func TestQueueCommand_ListEmpty(t *testing.T) {
	m := newTestTUI(t)
	m, _ = m.handleQueueCommand("/queue")
	last := lastItemText(m)
	if !strings.Contains(last, "empty") {
		t.Errorf("expected 'empty' for empty queue, got: %q", last)
	}
}

func TestQueueCommand_ListWithPrompts(t *testing.T) {
	m := newTestTUI(t)
	m.queuedPrompts = []queuedPrompt{{text: "first question"}, {text: "second question"}}
	m, _ = m.handleQueueCommand("/queue")
	last := lastItemText(m)
	if !strings.Contains(last, "queue (2):") {
		t.Errorf("expected 'queue (2):' header, got: %q", last)
	}
	if !strings.Contains(last, "first question") {
		t.Errorf("expected 'first question' in list, got: %q", last)
	}
}

func TestQueueCommand_Clear(t *testing.T) {
	m := newTestTUI(t)
	m.queuedPrompts = []queuedPrompt{{text: "a"}, {text: "b"}}
	m, _ = m.handleQueueCommand("/queue clear")
	if len(m.queuedPrompts) != 0 {
		t.Fatalf("queue should be empty after clear, got %d", len(m.queuedPrompts))
	}
	last := lastItemText(m)
	if !strings.Contains(last, "cleared") {
		t.Errorf("expected 'cleared' notice, got: %q", last)
	}
}

func TestQueueCommand_Drop(t *testing.T) {
	m := newTestTUI(t)
	m.queuedPrompts = []queuedPrompt{{text: "first"}, {text: "second"}, {text: "third"}}
	m, _ = m.handleQueueCommand("/queue drop 2")
	if len(m.queuedPrompts) != 2 {
		t.Fatalf("expected 2 remaining after drop, got %d", len(m.queuedPrompts))
	}
	if m.queuedPrompts[1].text != "third" {
		t.Errorf("expected 'third' at position 2, got %q", m.queuedPrompts[1].text)
	}
}

func TestQueueCommand_DropInvalid(t *testing.T) {
	m := newTestTUI(t)
	m.queuedPrompts = []queuedPrompt{{text: "only"}}
	m, _ = m.handleQueueCommand("/queue drop 5")
	if len(m.queuedPrompts) != 1 {
		t.Fatalf("invalid drop should not modify queue, got %d", len(m.queuedPrompts))
	}
	last := lastItemText(m)
	if !strings.Contains(last, "invalid") {
		t.Errorf("expected 'invalid' error, got: %q", last)
	}
}

func TestToolStartMsg_SetsRunningTool(t *testing.T) {
	m := newTestTUI(t)
	m.state = stateStreaming
	m = step(m, agent.ToolStartMsg{ToolCallID: "tc1", Name: "run_shell", Command: "ls -la"})
	if m.runningTool == nil {
		t.Fatal("runningTool should be set after ToolStartMsg")
	}
	if m.runningTool.name != "run_shell" {
		t.Errorf("expected name 'run_shell', got %q", m.runningTool.name)
	}
	if m.runningTool.command != "ls -la" {
		t.Errorf("expected command 'ls -la', got %q", m.runningTool.command)
	}
	// Status line should show the tool segment.
	in := m.headerStatusInput()
	if !strings.Contains(in.runningTool, "tool: run_shell") {
		t.Errorf("status line should show 'tool: run_shell', got %q", in.runningTool)
	}
}

func TestToolResultMsg_ClearsRunningTool(t *testing.T) {
	m := newTestTUI(t)
	m.state = stateStreaming
	m = step(m, agent.ToolStartMsg{ToolCallID: "tc1", Name: "run_shell", Command: "ls"})
	m = step(m, agent.ToolResultMsg{ToolCallID: "tc1", Name: "run_shell", Result: "output"})
	if m.runningTool != nil {
		t.Fatal("runningTool should be nil after ToolResultMsg")
	}
}

func TestToolResultMsg_DoesNotClearMismatchedID(t *testing.T) {
	m := newTestTUI(t)
	m.state = stateStreaming
	m = step(m, agent.ToolStartMsg{ToolCallID: "tc1", Name: "run_shell", Command: "ls"})
	m = step(m, agent.ToolResultMsg{ToolCallID: "tc2", Name: "run_shell", Result: "output"})
	if m.runningTool == nil {
		t.Fatal("runningTool should NOT be cleared by mismatched ToolCallID")
	}
}

func TestAgentDoneMsg_ClearsRunningTool(t *testing.T) {
	m := newTestTUI(t)
	m.state = stateStreaming
	m = step(m, agent.ToolStartMsg{ToolCallID: "tc1", Name: "run_shell", Command: "ls"})
	m = step(m, agent.AgentDoneMsg{})
	if m.runningTool != nil {
		t.Fatal("runningTool should be cleared on AgentDoneMsg")
	}
}
