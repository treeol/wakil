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
	cr := interpretAgentMsg(agent.LearnTurnMsg{}, false)
	if cr.Submit != "learn this for next time" {
		t.Fatalf("Submit = %q, want the literal learn text", cr.Submit)
	}
	if cr.Submit == "/learn" {
		t.Fatal("Submit must not be /learn (infinite redispatch)")
	}
}
