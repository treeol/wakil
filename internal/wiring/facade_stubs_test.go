package wiring

import (
	"context"
	"testing"

	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionclient"
	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/workflow"
)

// newTestFacade builds a wiringFacade over a fake App without a real backend —
// for facade-surface tests (side questions, SetWorkflow, snapshot versioning)
// that don't drive turns.
func newTestFacade(t *testing.T) *wiringFacade {
	t.Helper()
	app := fakeApp("http://127.0.0.1:1/v1/chat/completions")
	handle, err := NewHostTurnHandle(app)
	if err != nil {
		t.Fatalf("NewHostTurnHandle: %v", err)
	}
	f := newWiringFacade(app, handle, nil, nil, core.Principal{
		TenantID: event.EmbeddedTenantID,
		UserID:   event.EmbeddedUserID,
		Role:     core.RoleOwner,
	})
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// TestStartSideQuestionUniqueOpIDs verifies the facade mints a distinct OpID
// per side question (the old stub returned a constant "op_sq") and that
// CancelSideQuestion removes the entry (a second cancel is a no-op).
func TestStartSideQuestionUniqueOpIDs(t *testing.T) {
	f := newTestFacade(t)

	id1, cancel1 := f.StartSideQuestion(context.Background(), "first question")
	id2, _ := f.StartSideQuestion(context.Background(), "second question")

	if id1 == "" {
		t.Fatal("OpID is empty")
	}
	if id1 == id2 {
		t.Fatalf("two side questions share OpID %q — must be unique", id1)
	}
	if err := event.OpID(id1).Validate(); err != nil {
		t.Errorf("OpID %q does not validate: %v", id1, err)
	}

	f.mu.Lock()
	registered := len(f.sideQuestions)
	f.mu.Unlock()
	if registered != 2 {
		t.Errorf("registry holds %d entries, want 2", registered)
	}

	// Cancel the first — it must leave the registry; the second stays.
	f.CancelSideQuestion(id1)
	f.mu.Lock()
	registered = len(f.sideQuestions)
	f.mu.Unlock()
	if registered != 1 {
		t.Errorf("after cancel, registry holds %d entries, want 1", registered)
	}

	// Cancelling an unknown ID is a no-op (no panic, no state change).
	f.CancelSideQuestion(sessionclient.OpID("op_unknown"))

	cancel1() // silence unused-var
}

// TestSetWorkflowRoundTrip verifies SetWorkflow converts a WorkflowSnapshot to
// a real workflow.WorkflowState (the old stub nulled it unconditionally) and
// that the phase name round-trips through Snapshot().
func TestSetWorkflowRoundTrip(t *testing.T) {
	f := newTestFacade(t)

	f.SetWorkflow(&sessionclient.WorkflowSnapshot{
		Task:      "test task",
		Phase:     "implement",
		StepCount: 3,
		StepIdx:   2,
		PlanPath:  "/work/.wakil/plan.md",
	})

	wf := f.app.Workflow
	if wf == nil {
		t.Fatal("SetWorkflow(snapshot) left app.Workflow nil — conversion missing")
	}
	if wf.Phase != workflow.WFImplement {
		t.Errorf("Phase = %v, want WFImplement", wf.Phase)
	}
	if wf.Task != "test task" || wf.StepCount != 3 || wf.StepIdx != 2 || wf.PlanPath != "/work/.wakil/plan.md" {
		t.Errorf("workflow fields = %+v", wf)
	}

	// Snapshot round-trips the phase name.
	snap := f.Snapshot()
	if snap.Workflow == nil {
		t.Fatal("Snapshot().Workflow is nil")
	}
	if snap.Workflow.Phase != "implement" {
		t.Errorf("snapshot phase = %q, want implement", snap.Workflow.Phase)
	}
	if snap.Workflow.StepIdx != 2 || snap.Workflow.StepCount != 3 {
		t.Errorf("snapshot steps = %d/%d, want 2/3", snap.Workflow.StepIdx, snap.Workflow.StepCount)
	}

	// nil clears.
	f.SetWorkflow(nil)
	if f.app.Workflow != nil {
		t.Error("SetWorkflow(nil) did not clear the workflow")
	}
}

// TestSnapshotVersionIncrements verifies every facade mutation bumps the
// snapshot version so clients can detect staleness (the old stub was always 0).
func TestSnapshotVersionIncrements(t *testing.T) {
	f := newTestFacade(t)

	v0 := f.Snapshot().Version
	f.SetAutoApprove(true)
	v1 := f.Snapshot().Version
	if v1 <= v0 {
		t.Errorf("SetAutoApprove: version %d not > %d", v1, v0)
	}
	f.SetModelList([]string{"m1"})
	v2 := f.Snapshot().Version
	if v2 <= v1 {
		t.Errorf("SetModelList: version %d not > %d", v2, v1)
	}
	f.AddPendingImage(proxy.ImagePart{})
	v3 := f.Snapshot().Version
	if v3 <= v2 {
		t.Errorf("AddPendingImage: version %d not > %d", v3, v2)
	}
}
