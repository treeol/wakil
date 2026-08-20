package wiring

import (
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/proxy"
)

// TestDispatchHandoffDefersPipeline verifies /handoff classification does NOT
// execute the summarizer pipeline: with a handoff-able conversation,
// DispatchCommand returns Rotate{Type:"handoff"} without calling
// RunHandoffPipeline (the pipeline runs once, inside HandoffConversation).
func TestDispatchHandoffDefersPipeline(t *testing.T) {
	f := newTestFacade(t)
	t.Setenv("WAKIL_SESSIONS_DIR", t.TempDir())
	f.app.Conv = []proxy.Message{
		{Role: "user", Content: agent.StrPtr("do the thing")},
	}
	f.app.Session = &agent.Session{ChatID: f.app.Client.ChatID}

	cr := f.DispatchCommand("/handoff")
	if !cr.Handled {
		t.Fatal("/handoff not handled")
	}
	if cr.Rotate == nil || cr.Rotate.Type != "handoff" {
		t.Fatalf("Rotate = %+v, want {Type:handoff}", cr.Rotate)
	}
	if cr.Rotate.Proceed {
		t.Error("bare /handoff should default to proceed=false")
	}
	// The old session must NOT have been touched by dispatch (no save).
	if f.app.Session.Updated.IsZero() == false && f.app.Session.Conv != nil {
		t.Error("dispatch executed pipeline work — Session.Conv already folded")
	}

	// proceed variant.
	cr = f.DispatchCommand("/handoff proceed")
	if cr.Rotate == nil || !cr.Rotate.Proceed {
		t.Fatalf("/handoff proceed: Rotate = %+v, want Proceed=true", cr.Rotate)
	}
	// usage error.
	cr = f.DispatchCommand("/handoff bogus")
	if cr.Rotate != nil || !strings.Contains(cr.Notice, "usage") {
		t.Fatalf("bad arg: Rotate=%v Notice=%q", cr.Rotate, cr.Notice)
	}
}

// TestDispatchHandoffEmptyQuickFail verifies the emptiness guards fire at
// dispatch time (fast) instead of after a rotation attempt.
func TestDispatchHandoffEmptyQuickFail(t *testing.T) {
	f := newTestFacade(t)

	cr := f.DispatchCommand("/handoff")
	if !cr.Handled || cr.Rotate != nil {
		t.Fatalf("empty conv: %+v — want Notice, no Rotate", cr)
	}
	if !strings.Contains(cr.Notice, "nothing to hand off") {
		t.Errorf("Notice = %q, want the emptiness message", cr.Notice)
	}
}

// TestDispatchLearnSubmitsLiteral verifies /learn maps to the literal turn
// text the old TUI submitted — NOT "/learn", which would re-dispatch and loop.
func TestDispatchLearnSubmitsLiteral(t *testing.T) {
	// Build the interpretAgentMsg result directly: LearnTurnMsg is what the
	// agent's /learn Cmd returns.
	f := newTestFacade(t)
	cr := f.interpretAgentMsg(agent.LearnTurnMsg{}, false)
	if cr.Submit != "learn this for next time" {
		t.Fatalf("Submit = %q, want the literal learn text", cr.Submit)
	}
	if cr.Submit == "/learn" {
		t.Fatal("Submit must not be /learn (infinite redispatch)")
	}
}

// TestDispatchNewIntercepted verifies /new & /reset are intercepted BEFORE
// agent.HandleTUICommand can mutate the old App (m4b): dispatch returns
// Rotate{new} + Rotating without calling app.NewConversation.
func TestDispatchNewIntercepted(t *testing.T) {
	f := newTestFacade(t)
	oldChat := f.app.Client.ChatID
	f.app.Conv = []proxy.Message{{Role: "user", Content: agent.StrPtr("x")}}
	f.app.Session = &agent.Session{ChatID: oldChat}

	cr := f.DispatchCommand("/new")
	if !cr.Handled || cr.Rotate == nil || cr.Rotate.Type != "new" {
		t.Fatalf("/new: %+v — want Rotate{new}", cr)
	}
	if !cr.Rotating {
		t.Error("/new must set Rotating (async finalize on the manager path)")
	}
	if f.app.Client.ChatID != oldChat {
		t.Error("dispatch mutated the old App's ChatID — /new must not touch it")
	}
	if len(f.app.Conv) != 1 {
		t.Error("dispatch cleared the old App's Conv — /new must not touch it")
	}

	cr = f.DispatchCommand("/reset")
	if cr.Rotate == nil || cr.Rotate.Type != "new" {
		t.Fatalf("/reset: %+v — want Rotate{new}", cr)
	}
	if f.app.Client.ChatID != oldChat {
		t.Error("/reset mutated the old App")
	}
}

// TestDispatchResumeClassifications verifies /resume classification (m4b):
// bare /resume and /resume all open the picker; /resume <id> rotates; the
// agent's resume Cmds never run (they would mutate the old App).
func TestDispatchResumeClassifications(t *testing.T) {
	f := newTestFacade(t)
	oldChat := f.app.Client.ChatID

	for _, line := range []string{"/resume", "/resume all"} {
		cr := f.DispatchCommand(line)
		if !cr.Handled || !cr.ResumePicker {
			t.Fatalf("%s: %+v — want ResumePicker=true", line, cr)
		}
		if cr.Rotate != nil {
			t.Errorf("%s: Rotate must be nil (picker is not a rotation)", line)
		}
	}

	cr := f.DispatchCommand("/resume someid")
	if cr.Rotate == nil || cr.Rotate.Type != "resume" {
		t.Fatalf("/resume <id>: %+v — want Rotate{resume}", cr)
	}
	if !cr.Rotating {
		t.Error("/resume <id> must set Rotating")
	}
	if f.app.Client.ChatID != oldChat {
		t.Error("dispatch mutated the old App's ChatID — /resume must not touch it")
	}
}

// TestInterpretBatchMsgAppliesState verifies /backend's BatchMsg (note +
// ctx-limit probe) is fully interpreted: the notice surfaces AND the
// BackendCtxLimitMsg side effect is applied to the App facade-side (D24:
// query-state, no event carries it — the old wiring path dropped both).
func TestInterpretBatchMsgAppliesState(t *testing.T) {
	f := newTestFacade(t)
	before := f.Snapshot().Version

	lim := agent.ContextLimit{NCtx: 123456, Source: "backend"}
	cr := f.interpretAgentMsg(agent.BatchMsg{Cmds: []agent.Cmd{
		func() agent.Msg { return agent.SysNoteMsg{Text: "backend: set to b"} },
		func() agent.Msg { return agent.BackendCtxLimitMsg{Limit: lim, Note: "ctx: 123k from backend"} },
	}}, false)

	if !strings.Contains(cr.Notice, "backend: set to b") {
		t.Errorf("Notice = %q — batch's note lost", cr.Notice)
	}
	if !strings.Contains(cr.Notice, "ctx: 123k") {
		t.Errorf("Notice = %q — ctx-limit note lost", cr.Notice)
	}
	if got := f.app.CtxLimit.NCtx; got != 123456 {
		t.Errorf("app.CtxLimit.NCtx = %d, want 123456 (side effect not applied)", got)
	}
	if f.Snapshot().Version <= before {
		t.Error("version not bumped — snapshot staleness check would miss the mutation")
	}
}

// TestInterpretMCPReconnectAppliesTools verifies MCPReconnectedMsg applies the
// rebuilt tool list facade-side (the old mapping produced a notice only; the
// TUI's snapshot would show stale Tools forever).
func TestInterpretMCPReconnectAppliesTools(t *testing.T) {
	f := newTestFacade(t)
	tools := []proxy.Tool{{Function: proxy.ToolFunction{Name: "x_test_tool"}}}
	cr := f.interpretAgentMsg(agent.MCPReconnectedMsg{Name: "srv", Tools: tools}, false)

	if !strings.Contains(cr.Notice, "mcp reconnected: srv") {
		t.Errorf("Notice = %q", cr.Notice)
	}
	if len(f.app.Tools) != 1 || f.app.Tools[0].Function.Name != "x_test_tool" {
		t.Errorf("app.Tools = %+v — rebuilt list not applied", f.app.Tools)
	}
}

// TestInterpretClipboardImageSentinel verifies the /image clipboard sentinel
// maps to CommandResult.ClipboardImage (the TUI substitutes its own clipboard
// reader; the old mapping dropped the sentinel entirely).
func TestInterpretClipboardImageSentinel(t *testing.T) {
	f := newTestFacade(t)
	cmd := func() agent.Msg { return agent.ClipboardImageRequest }
	cr := f.interpretAgentMsg(cmd(), false)
	if !cr.ClipboardImage {
		t.Fatalf("ClipboardImage not set: %+v", cr)
	}
	if cr.Notice != "" || cr.Rotate != nil || cr.Submit != "" {
		t.Errorf("sentinel should carry no other action: %+v", cr)
	}
}
