package wiring

import (
	"context"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionhost"
	"github.com/treeol/wakil/internal/workflow"
)

// TestWorkflowContinuationEnqueued verifies the 7b3 m4 workflow-continuation
// mechanism (plan decision "host enqueues"): a turn whose workflow transitions
// (WFGather → WFPlan, signalled by %%PHASE_DONE%% in the assistant reply)
// causes the adapter to (a) run HandleWorkflowTransition, (b) enqueue the
// continuation ("continue") through the host's EnqueueInput hook, and (c) emit
// a durable workflow_turn_started audit marker. The host must run BOTH turns
// (two TurnStarted/TurnCompleted pairs) and set WorkflowWillContinue=true on
// the first turn's TurnCompleted — all without the TUI doing anything.
func TestWorkflowContinuationEnqueued(t *testing.T) {
	// Call 1: the gather-phase reply containing the phase-done sentinel.
	// Call 2: the continuation turn's reply (plan phase).
	srv := sseServer(t,
		[]string{contentChunk("gathered\n%%PHASE_DONE%%")},
		[]string{contentChunk("plan written")},
	)
	defer srv.Close()

	app := fakeApp(srv.URL)
	app.Workflow = &workflow.WorkflowState{
		Task:     "test task",
		Phase:    workflow.WFGather,
		PlanPath: "/work/.wakil/plan.md",
	}

	turnFn, err := NewHostTurnHandle(app)
	if err != nil {
		t.Fatalf("NewHostTurnHandle: %v", err)
	}
	h := sessionhost.New(turnFn.Turn)
	p := core.Principal{TenantID: "tnt_test", UserID: "usr_owner", Role: core.RoleOwner}
	defer h.Close(context.Background())

	s, err := h.CreateSession(context.Background(), p, core.CreateSessionRequest{Workspace: "wsp_test", Title: "t"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Subscribe to the live stream to observe ephemeral + durable events.
	sub, err := h.Subscribe(context.Background(), p, s.ID, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	if _, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: s.ID, Text: "gather"}); err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}

	// Drain events until the session returns to idle (both turns done).
	waitUntil(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s.ID)
		return g.State == core.SessionIdle
	})

	events, err := h.ListEvents(context.Background(), p, s.ID, 0, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}

	var turnStarts, turnCompletes int
	var wfTurnStarted int
	var firstWillContinue bool
	turnsSeen := 0
	for _, e := range events {
		switch e.Kind {
		case event.KindTurnStarted:
			turnStarts++
		case event.KindTurnCompleted:
			turnCompletes++
			tc := e.Payload.(event.TurnCompleted)
			if turnsSeen == 0 {
				firstWillContinue = tc.WorkflowWillContinue
			}
			turnsSeen++
		case event.KindWorkflowTurnStarted:
			wfTurnStarted++
		}
	}

	if turnStarts != 2 {
		t.Errorf("TurnStarted count = %d, want 2 (original + enqueued continuation)", turnStarts)
	}
	if turnCompletes != 2 {
		t.Errorf("TurnCompleted count = %d, want 2", turnCompletes)
	}
	if wfTurnStarted != 1 {
		t.Errorf("workflow_turn_started count = %d, want 1", wfTurnStarted)
	}
	if !firstWillContinue {
		t.Error("first TurnCompleted.WorkflowWillContinue = false, want true (queued continuation followed)")
	}

	// The workflow state advanced: gather → plan.
	if app.Workflow == nil || app.Workflow.Phase != workflow.WFPlan {
		got := "nil"
		if app.Workflow != nil {
			got = app.Workflow.PhaseName()
		}
		t.Errorf("workflow phase after continuation = %s, want plan", got)
	}
}

// TestWorkflowContinuationNotEnqueuedWithoutWorkflow verifies a plain turn
// (no active workflow) never enqueues a continuation: exactly one
// TurnStarted/TurnCompleted pair and no workflow events.
func TestWorkflowContinuationNotEnqueuedWithoutWorkflow(t *testing.T) {
	srv := sseServer(t, []string{contentChunk("plain reply")})
	defer srv.Close()

	app := fakeApp(srv.URL)

	turnFn, err := NewHostTurnHandle(app)
	if err != nil {
		t.Fatalf("NewHostTurnHandle: %v", err)
	}
	h := sessionhost.New(turnFn.Turn)
	p := core.Principal{TenantID: "tnt_test", UserID: "usr_owner", Role: core.RoleOwner}
	defer h.Close(context.Background())

	s, err := h.CreateSession(context.Background(), p, core.CreateSessionRequest{Workspace: "wsp_test", Title: "t"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if _, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: s.ID, Text: "hi"}); err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}

	waitUntil(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s.ID)
		return g.State == core.SessionIdle
	})

	events, err := h.ListEvents(context.Background(), p, s.ID, 0, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}

	var turnStarts, turnCompletes, wfEvents int
	for _, e := range events {
		switch e.Kind {
		case event.KindTurnStarted:
			turnStarts++
		case event.KindTurnCompleted:
			turnCompletes++
		case event.KindWorkflowTurnStarted, event.KindWorkflowFinalReview:
			wfEvents++
		}
	}
	if turnStarts != 1 || turnCompletes != 1 {
		t.Errorf("turns = %d/%d, want 1/1 without workflow", turnStarts, turnCompletes)
	}
	if wfEvents != 0 {
		t.Errorf("workflow events = %d, want 0 without workflow", wfEvents)
	}
}

// TestWorkflowNotesProjected verifies the in-turn workflow progress notes
// (wfProgNote → SysNoteMsg via EventSink) reach subscribers as ephemeral
// session_note events — the display parity for phase transitions.
func TestWorkflowNotesProjected(t *testing.T) {
	srv := sseServer(t, []string{contentChunk("gathered\n%%PHASE_DONE%%")})
	defer srv.Close()

	app := fakeApp(srv.URL)
	app.Workflow = &workflow.WorkflowState{
		Task:     "note test",
		Phase:    workflow.WFGather,
		PlanPath: "/work/.wakil/plan.md",
	}

	turnFn, err := NewHostTurnHandle(app)
	if err != nil {
		t.Fatalf("NewHostTurnHandle: %v", err)
	}
	h := sessionhost.New(turnFn.Turn)
	p := core.Principal{TenantID: "tnt_test", UserID: "usr_owner", Role: core.RoleOwner}
	defer h.Close(context.Background())

	s, err := h.CreateSession(context.Background(), p, core.CreateSessionRequest{Workspace: "wsp_test", Title: "t"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Live subscription to capture ephemeral events (they are not durable).
	sub, err := h.Subscribe(context.Background(), p, s.ID, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	if _, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: s.ID, Text: "gather"}); err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}

	// Drain the live stream until idle; collect ephemeral session notes.
	var notes []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for {
			ev, err := sub.Next(ctx)
			if err != nil {
				return // stream ended (session closed) or timeout
			}
			if ev.Kind == event.KindSessionNote {
				notes = append(notes, ev.Payload.(event.SessionNote).Text)
			}
			if ev.Kind == event.KindTurnCompleted {
				return
			}
		}
	}()

	waitUntil(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s.ID)
		return g.State == core.SessionIdle
	})
	select {
	case <-done:
	case <-waitForTimeout(5):
	}

	if len(notes) == 0 {
		t.Error("no session_note events observed — workflow progress notes lost on wiring path")
	}
	foundTransition := false
	for _, n := range notes {
		if n == "· gather complete → plan phase" {
			foundTransition = true
		}
	}
	if !foundTransition {
		t.Errorf("gather→plan transition note not found in %v", notes)
	}
}

// waitForTimeout is a small helper for select deadlines in tests.
func waitForTimeout(sec int) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		time.Sleep(time.Duration(sec) * time.Second)
		close(ch)
	}()
	return ch
}
