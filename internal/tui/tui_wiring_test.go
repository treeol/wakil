package tui

// tui_wiring_test.go: wiring-path TUI tests (m4b stage 3). A fake facade
// drives the event switch through a full turn lifecycle — submit → delta →
// tool → approval → completion — asserting the same view mutations the old
// agent-message path produced.

import (
	"context"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionclient"
	"github.com/treeol/wakil/internal/proxy"
)

// fakeFacade implements sessionclient.Facade for the event-switch tests. Only
// the members the wiring path touches in Update are real; the rest panic via
// the embed (compile-time completeness) — tests that need more fill them in.
type fakeFacade struct {
	sessionclient.Facade // embed for interface completeness; panics if used

	sid        event.SessionID
	chatID     string
	snap       sessionclient.ClientSnapshot
	info       sessionclient.InfoSnapshot
	consent    sessionclient.Consent
	submitted  []core.SubmitInputRequest
	responded  []core.ApprovalDecision
	interrupts int
	notes      []string
}

func (f *fakeFacade) Snapshot() sessionclient.ClientSnapshot { return f.snap }
func (f *fakeFacade) Info() sessionclient.InfoSnapshot       { return f.info }
func (f *fakeFacade) Consent() sessionclient.Consent         { return f.consent }
func (f *fakeFacade) ConsumeStartupNote() string {
	if len(f.notes) == 0 {
		return ""
	}
	n := f.notes[0]
	f.notes = f.notes[1:]
	return n
}
func (f *fakeFacade) SubmitInput(ctx context.Context, p core.Principal, req core.SubmitInputRequest) (core.TurnAck, error) {
	f.submitted = append(f.submitted, req)
	return core.TurnAck{SessionID: req.SessionID, TurnID: "trn_fake"}, nil
}
func (f *fakeFacade) RespondToApproval(ctx context.Context, p core.Principal, d core.ApprovalDecision) error {
	f.responded = append(f.responded, d)
	return nil
}
func (f *fakeFacade) Interrupt(ctx context.Context, p core.Principal, sid event.SessionID) error {
	f.interrupts++
	return nil
}
func (f *fakeFacade) AddPendingImage(img proxy.ImagePart) {
	f.snap.PendingImages = append(f.snap.PendingImages, img)
}
func (f *fakeFacade) ReplacePendingImages(imgs []proxy.ImagePart) {
	f.snap.PendingImages = append([]proxy.ImagePart(nil), imgs...)
}
func (f *fakeFacade) SetAutoApprove(v bool)          { f.consent.AutoApprove = v }
func (f *fakeFacade) SetAllowDestructive(v bool)     { f.consent.AllowDestructive = v }
func (f *fakeFacade) RevokeAuto()                    { f.consent.AutoApprove, f.consent.AllowDestructive = false, false }
func (f *fakeFacade) SetInfoPanelOpen(open bool)     { f.info.InfoPanelOpen = open }
func (f *fakeFacade) SaveRepoState(mutate func(*sessionclient.RepoStateMutator)) {}
func (f *fakeFacade) ListSessions(scope sessionclient.SessionScope) ([]sessionclient.SessionSummary, int, error) {
	return nil, 0, nil
}
func (f *fakeFacade) StartSideQuestion(ctx context.Context, question string) (sessionclient.OpID, context.CancelFunc) {
	return "op_sq_fake", func() {}
}

// newWiringModel builds a facade-backed model for event-switch tests.
func newWiringModel(f *fakeFacade) tuiModel {
	f.snap.SessionID = f.sid
	f.snap.ChatID = f.chatID
	m := NewTUIModelWithFacade(f, nil, core.Principal{
		TenantID: event.EmbeddedTenantID,
		UserID:   event.EmbeddedUserID,
		Role:     core.RoleOwner,
	})
	m.width, m.height, m.ready = 100, 30, true
	return m
}
// evt builds a committed-style event for direct Update feeding. Ephemeral
// kinds carry Seq 0; durable kinds get an incrementing Seq (the guard only
// checks SessionID, so Seq is cosmetic here).
var evtSeq = event.Seq(0)

func evt(kind event.Kind, payload any, sid event.SessionID) event.Event {
	evtSeq++
	return event.Event{
		TenantID:  event.EmbeddedTenantID,
		SessionID: sid,
		Seq:       evtSeq,
		Ts:        time.Now(),
		Kind:      kind,
		Payload:   payload,
	}
}

// keyMsg builds a control-key tea.KeyMsg by its string name.
func wiringKeyMsg(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

// wiringTestInfo builds an InfoSnapshot with a backend context limit — the
// common shape render tests need.
func wiringTestInfo(ncTx int) sessionclient.InfoSnapshot {
	return sessionclient.InfoSnapshot{
		ContextLimit: sessionclient.ContextLimit{
			NCtx: ncTx, Source: "backend", ReasoningBudget: 4096, AnswerMargin: 4096,
		},
		ContextExact: true,
	}
}

// rotatedFake builds a fresh prepared facade for rotation tests (its snapshot
// carries the new session ID, matching what the real wiring facade returns).
func rotatedFake() *fakeFacade {
	f := &fakeFacade{sid: "sess_rotated", chatID: "chat_new"}
	f.snap.SessionID = f.sid
	f.snap.ChatID = f.chatID
	return f
}

// TestWiringTurnLifecycle drives a full turn through the event switch:
// turn_started → reasoning → deltas → tool → completion. Asserts the state
// machine (idle→streaming→idle), streaming accumulation, tool indicators,
// and reasoning collapse.
func TestWiringTurnLifecycle(t *testing.T) {
	f := &fakeFacade{sid: "sess_tui_test", chatID: "chat123"}
	f.snap.ChatID = f.chatID
	m := newWiringModel(f)

	// TurnStarted (e.g. an optimistic send already set streaming; a
	// workflow-continuation turn arrives at idle).
	m.state = stateIdle
	m = step(m, evt(event.KindTurnStarted, event.TurnStarted{TurnID: "trn_1", TurnIndex: 1}, f.sid))
	if m.state != stateStreaming {
		t.Fatalf("after TurnStarted: state = %v, want streaming", m.state)
	}

	// Reasoning then content: the reasoning collapses on first delta.
	m = step(m, evt(event.KindReasoningDelta, event.ReasoningDelta{Text: "thinking…"}, f.sid))
	if m.reasoning.Len() == 0 {
		t.Fatal("ReasoningDelta not accumulated")
	}
	m = step(m, evt(event.KindMessageDelta, event.MessageDelta{Text: "hello "}, f.sid))
	m = step(m, evt(event.KindMessageDelta, event.MessageDelta{Text: "world"}, f.sid))
	if !m.reasoningDone {
		t.Fatal("first MessageDelta should collapse the reasoning block")
	}
	if got := m.streaming.String(); got != "hello world" {
		t.Fatalf("streaming = %q, want %q", got, "hello world")
	}

	// Tool start/result drive the status-line indicators.
	m = step(m, evt(event.KindToolCallStarted, event.ToolCallStarted{
		TurnID: "trn_1", ToolCallID: "tcl_1", Name: "run_shell", ArgDigest: "ls -la",
	}, f.sid))
	if m.runningTool == nil || m.runningTool.name != "run_shell" || m.runningTool.command != "ls -la" {
		t.Fatalf("runningTool = %+v", m.runningTool)
	}
	m = step(m, evt(event.KindToolCallCompleted, event.ToolCallCompleted{
		ToolCallID: "tcl_1", Name: "run_shell", Status: "ok",
	}, f.sid))
	if m.runningTool != nil {
		t.Fatal("ToolCallCompleted should clear runningTool")
	}
	if m.lastTool == nil {
		t.Fatal("lastTool should persist after completion")
	}

	// TurnCompleted flushes the stream and returns to idle.
	m = step(m, evt(event.KindTurnCompleted, event.TurnCompleted{TurnID: "trn_1", Outcome: "complete"}, f.sid))
	if m.state != stateIdle {
		t.Fatalf("after TurnCompleted: state = %v, want idle", m.state)
	}
	if m.streaming.Len() != 0 {
		t.Fatal("streaming not flushed at turn end")
	}
	if !m.hadTurn {
		t.Fatal("hadTurn not set")
	}
}

// TestWiringApprovalGate verifies the async confirm gate: ApprovalRequested
// parks the model in stateConfirm with the approval ID; 'y' answers through
// facade.RespondToApproval; ApprovalResolved resumes streaming.
func TestWiringApprovalGate(t *testing.T) {
	f := &fakeFacade{sid: "sess_tui_test", chatID: "chat123"}
	m := newWiringModel(f)
	m.state = stateStreaming

	m = step(m, evt(event.KindApprovalRequested, event.ApprovalRequested{
		ApprovalID: "apr_1", ToolName: "write_file", Headline: "write a.txt",
		Detail: "$ echo hi", ReadAction: true,
	}, f.sid))
	if m.state != stateConfirm {
		t.Fatalf("after ApprovalRequested: state = %v, want confirm", m.state)
	}
	if m.pendApproval == nil || m.pendApproval.approvalID != "apr_1" {
		t.Fatalf("pendApproval = %+v", m.pendApproval)
	}

	// 'a' (allow reads — readAction tool) answers with allowed_reads_once.
	m = step(m, wiringKeyMsg("a"))
	if len(f.responded) != 1 {
		t.Fatalf("RespondToApproval calls = %d, want 1", len(f.responded))
	}
	d := f.responded[0]
	if d.ApprovalID != event.ApprovalID("apr_1") {
		t.Fatalf("ApprovalID = %q", d.ApprovalID)
	}
	if d.Outcome != core.ApprovalAllowReadsOnce {
		t.Fatalf("Outcome = %v, want allow_reads_once", d.Outcome)
	}
	if m.state != stateStreaming {
		t.Fatalf("after answer: state = %v, want streaming (optimistic)", m.state)
	}

	// A second ApprovalRequested + decline path via 'n'.
	m = step(m, evt(event.KindApprovalRequested, event.ApprovalRequested{
		ApprovalID: "apr_2", ToolName: "run_shell", Headline: "rm file",
	}, f.sid))
	if m.state != stateConfirm {
		t.Fatal("second approval did not park")
	}
	m = step(m, wiringKeyMsg("n"))
	if f.responded[len(f.responded)-1].Outcome != core.ApprovalDeny {
		t.Fatal("decline not sent")
	}

	// ctrl+c during confirm: decline + interrupt.
	m = step(m, evt(event.KindApprovalRequested, event.ApprovalRequested{
		ApprovalID: "apr_3", ToolName: "run_shell", Headline: "x",
	}, f.sid))
	m = step(m, wiringKeyMsg("ctrl+c"))
	if f.interrupts != 1 {
		t.Fatalf("Interrupt calls = %d, want 1", f.interrupts)
	}
	if !m.cancelling {
		t.Fatal("cancelling flag not set on ctrl+c-in-confirm")
	}
}

// TestWiringSessionGuard drops events from a foreign session (stale pump
// deliveries racing a rotation).
func TestWiringSessionGuard(t *testing.T) {
	f := &fakeFacade{sid: "sess_current", chatID: "chat123"}
	m := newWiringModel(f)
	m.state = stateIdle

	m = step(m, evt(event.KindTurnStarted, event.TurnStarted{TurnID: "trn_x", TurnIndex: 1}, "sess_stale"))
	if m.state != stateIdle {
		t.Fatal("stale-session event mutated state — guard failed")
	}
	m = step(m, evt(event.KindMessageDelta, event.MessageDelta{Text: "junk"}, "sess_stale"))
	if m.streaming.Len() != 0 {
		t.Fatal("stale-session delta accumulated — guard failed")
	}
}

// TestWiringSendPath submits through the facade: Enter in idle produces
// SubmitInput with the mention-resolved text, and the view shows the user
// item.
func TestWiringSendPath(t *testing.T) {
	f := &fakeFacade{sid: "sess_tui_test", chatID: "chat123"}
	m := newWiringModel(f)
	m.ta.SetValue("hello agent")
	m = step(m, wiringKeyMsg("enter"))

	if len(f.submitted) != 1 {
		t.Fatalf("SubmitInput calls = %d, want 1", len(f.submitted))
	}
	if f.submitted[0].Text != "hello agent" {
		t.Fatalf("submitted text = %q", f.submitted[0].Text)
	}
	if f.submitted[0].SessionID != f.sid {
		t.Fatalf("submitted session = %q, want %q", f.submitted[0].SessionID, f.sid)
	}
	if m.state != stateStreaming {
		t.Fatal("send did not set streaming (optimistic)")
	}
	// The user item landed in the view.
	found := false
	for _, it := range *m.items {
		if it.kind == iUser && it.text == "hello agent" {
			found = true
		}
	}
	if !found {
		t.Fatal("user item not added to the view")
	}
}

// TestWiringQueueFlushAfterTurn verifies the queue-flush gate: a queued
// prompt (midturn Enter) flushes as a SubmitInput on a clean TurnCompleted,
// and is retained on a workflow-continuation turn.
func TestWiringQueueFlushAfterTurn(t *testing.T) {
	f := &fakeFacade{sid: "sess_tui_test", chatID: "chat123"}
	m := newWiringModel(f)

	// Midturn queue: state streaming + Enter.
	m.state = stateStreaming
	m.ta.SetValue("queued prompt")
	m = step(m, wiringKeyMsg("enter"))
	if len(m.queuedPrompts) != 1 {
		t.Fatalf("queuedPrompts = %d, want 1", len(m.queuedPrompts))
	}
	if len(f.submitted) != 0 {
		t.Fatal("midturn Enter must NOT submit directly")
	}

	// Workflow continuation: queue survives.
	m = step(m, evt(event.KindTurnCompleted, event.TurnCompleted{
		TurnID: "trn_1", Outcome: "complete", WorkflowWillContinue: true,
	}, f.sid))
	if len(m.queuedPrompts) != 1 {
		t.Fatal("queue should survive a workflow continuation")
	}

	// Clean completion: queue flushes as a submit.
	m.state = stateStreaming
	m = step(m, evt(event.KindTurnCompleted, event.TurnCompleted{
		TurnID: "trn_2", Outcome: "complete",
	}, f.sid))
	if len(f.submitted) != 1 {
		t.Fatalf("flush submits = %d, want 1", len(f.submitted))
	}
	if f.submitted[0].Text != "queued prompt" {
		t.Fatalf("flushed text = %q", f.submitted[0].Text)
	}
}
