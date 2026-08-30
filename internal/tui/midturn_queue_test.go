package tui

import (
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/core/event"

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

func queueModel(t *testing.T) (tuiModel, *fakeFacade) {
	t.Helper()
	f := &fakeFacade{sid: "sess_tui_test", chatID: "chat123"}
	return newWiringModel(f), f
}

func turnDone(f *fakeFacade) event.Event {
	return evt(event.KindTurnCompleted, event.TurnCompleted{TurnID: "trn_1", Outcome: "complete"}, f.sid)
}

func TestQueuePrompt_MidTurn_QueuesVisibly(t *testing.T) {
	m, _ := queueModel(t)
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
	m, _ := queueModel(t)
	m = midTurnEnter(m, "queued during compaction", stateCompacting)
	if len(m.queuedPrompts) != 1 {
		t.Fatalf("expected 1 queued prompt during compacting, got %d", len(m.queuedPrompts))
	}
}

func TestQueuePrompt_EmptyInput_MidTurn_DoesNothing(t *testing.T) {
	m, _ := queueModel(t)
	m = midTurnEnter(m, "   ", stateStreaming)
	if len(m.queuedPrompts) != 0 {
		t.Fatalf("empty input should not queue, got %d", len(m.queuedPrompts))
	}
}

func TestQueuePrompt_SlashCommand_RejectedWithNotice(t *testing.T) {
	m, _ := queueModel(t)
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
	m, _ := queueModel(t)
	m = midTurnEnter(m, "/auto", stateStreaming)
	if len(m.queuedPrompts) != 0 {
		t.Fatalf("/auto should NOT be queued, got %d queued prompts", len(m.queuedPrompts))
	}
	// /auto mid-turn (OFF→ON) defers the grant — it does not apply immediately.
	if !m.pendingAutoGrant {
		t.Error("OFF→ON /auto mid-turn should set pendingAutoGrant")
	}
	if m.facade.Consent().AutoApprove {
		t.Error("deferred grant must NOT apply immediately — auto should still be OFF")
	}
	last := lastItemText(m)
	if !strings.Contains(last, "pending grant") {
		t.Errorf("expected pending-grant notice, got: %q", last)
	}
}

func TestQueuePrompt_InfoCommand_ExecutesImmediately(t *testing.T) {
	m, _ := queueModel(t)
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
	m, _ := queueModel(t)
	m.queuedPrompts = []queuedPrompt{{text: "first"}, {text: "second"}}
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
	m, _ := queueModel(t)
	m.queuedPrompts = []queuedPrompt{{text: "first question"}}
	m = midTurnEnter(m, "/queue", stateStreaming)
	last := lastItemText(m)
	if !strings.Contains(last, "queue (1):") {
		t.Errorf("expected 'queue (1):' via handleKey, got: %q", last)
	}
}

func TestQueuePrompt_ConfirmGate_OwnsInput(t *testing.T) {
	m, _ := queueModel(t)
	m.state = stateConfirm
	m.pendApproval = &pendingApprovalState{approvalID: "apr_1", toolName: "run_shell"}
	m.ta.SetValue("some text")
	// Enter in confirm state should go to the confirm gate, not the queue.
	m, _, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.queuedPrompts) != 0 {
		t.Fatalf("confirm gate should consume Enter; queue should be empty, got %d", len(m.queuedPrompts))
	}
}

func TestQueuePrompt_FlushOnIdle(t *testing.T) {
	m, f := queueModel(t)
	m.queuedPrompts = []queuedPrompt{{text: "follow up"}}
	m.state = stateStreaming
	// Clean TurnCompleted → flush.
	m = step(m, turnDone(f))
	if len(m.queuedPrompts) != 0 {
		t.Fatalf("queue should be flushed on idle, got %d remaining", len(m.queuedPrompts))
	}
	if m.state != stateStreaming {
		t.Errorf("flush should start a new turn (stateStreaming), got %v", m.state)
	}
}

func TestQueuePrompt_HoldDuringWorkflowAutoContinue(t *testing.T) {
	m, f := queueModel(t)
	m.queuedPrompts = []queuedPrompt{{text: "follow up"}}
	m.state = stateStreaming
	m = step(m, evt(event.KindTurnCompleted, event.TurnCompleted{TurnID: "trn_1", Outcome: "complete", WorkflowWillContinue: true}, f.sid))
	if len(m.queuedPrompts) != 1 {
		t.Fatalf("queue should hold during workflow auto-continuation, got %d", len(m.queuedPrompts))
	}
	if m.state != stateIdle {
		t.Errorf("state should be idle (workflow turn not started yet), got %v", m.state)
	}
}

func TestQueuePrompt_HoldOnCancel(t *testing.T) {
	m, f := queueModel(t)
	m.queuedPrompts = []queuedPrompt{{text: "follow up"}}
	m.state = stateStreaming
	m = step(m, evt(event.KindTurnCompleted, event.TurnCompleted{TurnID: "trn_1", Outcome: "cancelled"}, f.sid))
	if len(m.queuedPrompts) != 1 {
		t.Fatalf("queue should hold on cancel, got %d", len(m.queuedPrompts))
	}
}

func TestQueuePrompt_HoldOnError(t *testing.T) {
	m, f := queueModel(t)
	m.queuedPrompts = []queuedPrompt{{text: "follow up"}}
	m.state = stateStreaming
	m = step(m, evt(event.KindTurnCompleted, event.TurnCompleted{TurnID: "trn_1", Outcome: "stream_error"}, f.sid))
	if len(m.queuedPrompts) != 1 {
		t.Fatalf("queue should hold on error, got %d", len(m.queuedPrompts))
	}
}

func TestQueuePrompt_MultipleQueued_FlushesOneAtATime(t *testing.T) {
	m, f := queueModel(t)
	m.queuedPrompts = []queuedPrompt{{text: "first"}, {text: "second"}}
	m.state = stateStreaming
	// First flush.
	m = step(m, turnDone(f))
	if len(m.queuedPrompts) != 1 {
		t.Fatalf("after first flush, 1 should remain, got %d", len(m.queuedPrompts))
	}
	if m.queuedPrompts[0].text != "second" {
		t.Errorf("remaining should be 'second', got %q", m.queuedPrompts[0].text)
	}
}

func TestQueuePrompt_StatusLine_ShowsQueueCount(t *testing.T) {
	m, _ := queueModel(t)
	m.queuedPrompts = []queuedPrompt{{text: "a"}, {text: "b"}, {text: "c"}}
	in := m.headerStatusInput()
	if in.queueLen != 3 {
		t.Errorf("headerStatusInput queueLen: expected 3, got %d", in.queueLen)
	}
}

func TestQueuePrompt_ClearedOnRotation(t *testing.T) {
	m, _ := queueModel(t)
	m.queuedPrompts = []queuedPrompt{{text: "stale prompt 1"}, {text: "stale prompt 2"}}
	m = step(m, rotationMsg{facade: rotatedFake()})
	if len(m.queuedPrompts) != 0 {
		t.Fatalf("queue should be cleared on rotation, got %d", len(m.queuedPrompts))
	}
	// The clear notice is added — check all items.
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
	m, _ := queueModel(t)
	m, _ = m.handleQueueCommand("/queue")
	last := lastItemText(m)
	if !strings.Contains(last, "empty") {
		t.Errorf("expected 'empty' for empty queue, got: %q", last)
	}
}

func TestQueueCommand_ListWithPrompts(t *testing.T) {
	m, _ := queueModel(t)
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
	m, _ := queueModel(t)
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
	m, _ := queueModel(t)
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
	m, _ := queueModel(t)
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

// --- Tests for card #170: TurnResumed race during waiting-input cancel ---

// TestQueuePrompt_WaitingInput_CancelsAndFlushes: the happy path — send a
// prompt while stateWaiting, verify the turn is cancelled and the prompt
// flushes when the cancelled TurnCompleted arrives.
func TestQueuePrompt_WaitingInput_CancelsAndFlushes(t *testing.T) {
	m, f := queueModel(t)
	m.state = stateWaiting

	// Simulate typing and pressing Enter in stateWaiting.
	m.ta.SetValue("hello from waiting")
	m, _, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})

	if len(m.queuedPrompts) != 1 {
		t.Fatalf("expected 1 queued prompt, got %d", len(m.queuedPrompts))
	}
	if m.queuedPrompts[0].text != "hello from waiting" {
		t.Errorf("expected 'hello from waiting', got %q", m.queuedPrompts[0].text)
	}
	if !m.cancelling {
		t.Error("cancelling should be true after sending prompt while waiting")
	}
	if !m.flushOnCancel {
		t.Error("flushOnCancel should be true after sending prompt while waiting")
	}
	if f.interrupts != 1 {
		t.Errorf("expected 1 interrupt call, got %d", f.interrupts)
	}

	// TurnCompleted with cancelled outcome should flush the queued prompt.
	m = step(m, evt(event.KindTurnCompleted, event.TurnCompleted{
		TurnID: "trn_1", Outcome: "cancelled",
	}, f.sid))

	if len(m.queuedPrompts) != 0 {
		t.Fatalf("queue should be flushed on cancel completion, got %d remaining", len(m.queuedPrompts))
	}
	if m.state != stateStreaming {
		t.Errorf("flush should start a new turn (stateStreaming), got %v", m.state)
	}
	if len(f.submitted) != 1 {
		t.Errorf("expected 1 submit call for flushed prompt, got %d", len(f.submitted))
	}
	if f.submitted[0].Text != "hello from waiting" {
		t.Errorf("expected flushed text 'hello from waiting', got %q", f.submitted[0].Text)
	}
}

// TestQueuePrompt_TurnResumedRace_DoesNotTransitionToStreaming: if TurnResumed
// arrives after the user sent a prompt from stateWaiting (cancelling=true),
// the TUI must NOT transition back to stateStreaming. The in-flight cancel
// takes precedence.
func TestQueuePrompt_TurnResumedRace_DoesNotTransitionToStreaming(t *testing.T) {
	m, f := queueModel(t)
	m.state = stateWaiting

	// User sends a prompt while waiting → triggers cancel + flushOnCancel.
	m.ta.SetValue("prompt 1")
	m, _, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})

	if !m.cancelling {
		t.Fatal("cancelling should be true after sending prompt while waiting")
	}

	// TurnResumed arrives while cancel is in flight — must NOT transition.
	m = step(m, evt(event.KindTurnResumed, nil, f.sid))

	if m.state != stateWaiting {
		t.Errorf("state should remain stateWaiting during cancel, got %v", m.state)
	}
	if !m.cancelling {
		t.Error("cancelling should still be true after TurnResumed race")
	}
	if !m.flushOnCancel {
		t.Error("flushOnCancel should still be true after TurnResumed race")
	}
}

// TestQueuePrompt_SecondPromptWhileCancelling_NoDoubleInterrupt: if the user
// sends a second prompt while still in stateWaiting and a cancel is already
// pending, the second prompt should be queued without issuing another
// interrupt, and flushOnCancel should stay true.
func TestQueuePrompt_SecondPromptWhileCancelling_NoDoubleInterrupt(t *testing.T) {
	m, f := queueModel(t)
	m.state = stateWaiting

	// First prompt from stateWaiting — triggers cancel + flushOnCancel.
	m.ta.SetValue("first prompt")
	m, _, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})

	if f.interrupts != 1 {
		t.Fatalf("expected 1 interrupt after first prompt, got %d", f.interrupts)
	}

	// TurnResumed races in — state should remain stateWaiting.
	m = step(m, evt(event.KindTurnResumed, nil, f.sid))
	if m.state != stateWaiting {
		t.Fatalf("state should still be stateWaiting, got %v", m.state)
	}

	// Second prompt — should be queued without another interrupt.
	m.ta.SetValue("second prompt")
	m, _, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})

	if len(m.queuedPrompts) != 2 {
		t.Fatalf("expected 2 queued prompts, got %d", len(m.queuedPrompts))
	}
	if m.queuedPrompts[1].text != "second prompt" {
		t.Errorf("expected 'second prompt' at index 1, got %q", m.queuedPrompts[1].text)
	}
	if !m.flushOnCancel {
		t.Error("flushOnCancel should still be true after second prompt while cancelling")
	}
	if f.interrupts != 1 {
		t.Errorf("should NOT issue a second interrupt, got %d", f.interrupts)
	}

	// Cancel completes — should flush ONE prompt (the first).
	m = step(m, evt(event.KindTurnCompleted, event.TurnCompleted{
		TurnID: "trn_1", Outcome: "cancelled",
	}, f.sid))

	if len(m.queuedPrompts) != 1 {
		t.Fatalf("one prompt should remain after cancel flush, got %d", len(m.queuedPrompts))
	}
	if m.queuedPrompts[0].text != "second prompt" {
		t.Errorf("remaining should be 'second prompt', got %q", m.queuedPrompts[0].text)
	}
}

// TestQueuePrompt_StreamingWhileCancelling_SetsFlushOnCancel: directly tests
// the streaming-branch change. When state is stateStreaming and cancelling is
// already true (e.g. from a manual Esc cancel), a queued plain-text prompt
// should set flushOnCancel so it flushes on cancel completion.
func TestQueuePrompt_StreamingWhileCancelling_SetsFlushOnCancel(t *testing.T) {
	m, f := queueModel(t)
	m.state = stateStreaming
	m.cancelling = true
	m.flushOnCancel = false

	// Queue a prompt while streaming + cancelling.
	m.ta.SetValue("queued during cancel")
	m, _, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})

	if len(m.queuedPrompts) != 1 {
		t.Fatalf("expected 1 queued prompt, got %d", len(m.queuedPrompts))
	}
	if m.queuedPrompts[0].text != "queued during cancel" {
		t.Errorf("expected 'queued during cancel', got %q", m.queuedPrompts[0].text)
	}
	if !m.flushOnCancel {
		t.Error("flushOnCancel should be true after queueing while cancelling in streaming state")
	}
	// No additional interrupt — the cancel is already in flight.
	if f.interrupts != 0 {
		t.Errorf("streaming-branch queue should not call interrupt, got %d", f.interrupts)
	}

	// Cancel completes — should flush the queued prompt.
	m = step(m, evt(event.KindTurnCompleted, event.TurnCompleted{
		TurnID: "trn_1", Outcome: "cancelled",
	}, f.sid))

	if len(m.queuedPrompts) != 0 {
		t.Fatalf("queue should be flushed on cancel completion, got %d remaining", len(m.queuedPrompts))
	}
	if m.state != stateStreaming {
		t.Errorf("flush should start a new turn (stateStreaming), got %v", m.state)
	}
}

// TestQueuePrompt_TurnResumed_NormalCase_StillTransitions: when not cancelling,
// TurnResumed should still transition stateWaiting → stateStreaming (the
// normal, non-race path).
func TestQueuePrompt_TurnResumed_NormalCase_StillTransitions(t *testing.T) {
	m, f := queueModel(t)
	m.state = stateWaiting

	// No cancel in flight — TurnResumed should transition normally.
	m = step(m, evt(event.KindTurnResumed, nil, f.sid))

	if m.state != stateStreaming {
		t.Errorf("state should transition to stateStreaming on normal TurnResumed, got %v", m.state)
	}
}

func toolStart(tcID, name, cmd string, f *fakeFacade) event.Event {
	return evt(event.KindToolCallStarted, event.ToolCallStarted{
		TurnID: "trn_1", ToolCallID: event.ToolCallID(tcID), Name: name, ArgDigest: cmd,
	}, f.sid)
}

func toolDone(tcID, name string, f *fakeFacade) event.Event {
	return evt(event.KindToolCallCompleted, event.ToolCallCompleted{
		ToolCallID: event.ToolCallID(tcID), Name: name, Status: "ok",
	}, f.sid)
}

func TestToolStartEvent_SetsRunningTool(t *testing.T) {
	m, f := queueModel(t)
	m.state = stateStreaming
	m = step(m, toolStart("tcl_1", "run_shell", "ls -la", f))
	if m.runningTool == nil {
		t.Fatal("runningTool should be set after ToolCallStarted")
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

func TestToolCompletedEvent_ClearsRunningTool(t *testing.T) {
	m, f := queueModel(t)
	m.state = stateStreaming
	m = step(m, toolStart("tcl_1", "run_shell", "ls", f))
	m = step(m, toolDone("tcl_1", "run_shell", f))
	if m.runningTool != nil {
		t.Fatal("runningTool should be nil after ToolCallCompleted")
	}
}

func TestToolCompletedEvent_DoesNotClearMismatchedID(t *testing.T) {
	m, f := queueModel(t)
	m.state = stateStreaming
	m = step(m, toolStart("tcl_1", "run_shell", "ls", f))
	m = step(m, toolDone("tcl_2", "run_shell", f))
	if m.runningTool == nil {
		t.Fatal("runningTool should NOT be cleared by mismatched ToolCallID")
	}
}

func TestTurnCompleted_ClearsRunningTool(t *testing.T) {
	m, f := queueModel(t)
	m.state = stateStreaming
	m = step(m, toolStart("tcl_1", "run_shell", "ls", f))
	m = step(m, turnDone(f))
	if m.runningTool != nil {
		t.Fatal("runningTool should be cleared on TurnCompleted")
	}
}
