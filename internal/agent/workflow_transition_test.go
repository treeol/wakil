package agent

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/workflow"
)

// TestHandleWorkflowTransition_GatherToPlan verifies that when a gather-phase
// turn emits %%PHASE_DONE%%, the workflow transitions to WFPlan.
func TestHandleWorkflowTransition_GatherToPlan(t *testing.T) {
	app := &App{
		Cfg: config.DefaultConfig(),
		Conv: []proxy.Message{
			{Role: "user", Content: strPtr("investigate")},
			{Role: "assistant", Content: strPtr("done investigating %%PHASE_DONE%%")},
		},
		Workflow: &workflow.WorkflowState{
			Task:     "test task",
			Phase:    workflow.WFGather,
			PlanPath: ".wakil/plan.md",
		},
		Exec: newFakeExecutor(),
		Out:  io.Discard,
	}

	msg := HandleWorkflowTransition(context.Background(), app)
	if msg == nil {
		t.Fatal("expected WFStartTurnMsg for gather→plan transition, got nil")
	}
	if app.Workflow.Phase != workflow.WFPlan {
		t.Errorf("phase = %v, want WFPlan", app.Workflow.Phase)
	}
}

// TestHandleWorkflowTransition_GatherNoPhaseDone verifies that a gather turn
// without %%PHASE_DONE%% does not transition.
func TestHandleWorkflowTransition_GatherNoPhaseDone(t *testing.T) {
	app := &App{
		Cfg: config.DefaultConfig(),
		Conv: []proxy.Message{
			{Role: "user", Content: strPtr("investigate")},
			{Role: "assistant", Content: strPtr("still investigating")},
		},
		Workflow: &workflow.WorkflowState{
			Task:     "test task",
			Phase:    workflow.WFGather,
			PlanPath: ".wakil/plan.md",
		},
		Exec: newFakeExecutor(),
		Out:  io.Discard,
	}

	msg := HandleWorkflowTransition(context.Background(), app)
	if msg != nil {
		t.Error("expected nil when gather phase not done")
	}
	if app.Workflow.Phase != workflow.WFGather {
		t.Errorf("phase should not have changed, got %v", app.Workflow.Phase)
	}
}

// TestHandleWorkflowTransition_PlanComplete verifies that a plan with numbered
// steps transitions to WFReview.
func TestHandleWorkflowTransition_PlanComplete(t *testing.T) {
	// Set up a fake executor with a plan file that has numbered steps.
	fe := newFakeExecutor()
	fe.files[".wakil/plan.md"] = "## Task\n\ntest task\n\n## Plan\n\n1. First\n2. Second\n3. Third\n"

	app := &App{
		Cfg: config.DefaultConfig(),
		Conv: []proxy.Message{
			{Role: "user", Content: strPtr("write plan")},
			{Role: "assistant", Content: strPtr("plan written %%PHASE_DONE%%")},
		},
		Workflow: &workflow.WorkflowState{
			Task:     "test task",
			Phase:    workflow.WFPlan,
			PlanPath: ".wakil/plan.md",
		},
		Exec: fe,
		Out:  io.Discard,
	}

	// HandleWorkflowTransition will call HandleReviewOracle which calls doWFOracle.
	// Since there's no oracle configured, it should skip the review gracefully.
	// The important thing is that Phase transitions to WFReview.
	_ = HandleWorkflowTransition(context.Background(), app)

	if app.Workflow.Phase != workflow.WFReview {
		t.Errorf("phase = %v, want WFReview", app.Workflow.Phase)
	}
	if app.Workflow.StepCount != 3 {
		t.Errorf("StepCount = %d, want 3", app.Workflow.StepCount)
	}
}

// TestHandleWorkflowTransition_PlanFormatInvalid verifies that a plan with
// content but no numbered steps sets PlanFormatInvalid and stays in WFPlan.
func TestHandleWorkflowTransition_PlanFormatInvalid(t *testing.T) {
	fe := newFakeExecutor()
	fe.files[".wakil/plan.md"] = "## Task\n\ntest task\n\n## Plan\n\n### Step One\n### Step Two\n"

	app := &App{
		Cfg: config.DefaultConfig(),
		Conv: []proxy.Message{
			{Role: "user", Content: strPtr("write plan")},
			{Role: "assistant", Content: strPtr("plan written %%PHASE_DONE%%")},
		},
		Workflow: &workflow.WorkflowState{
			Task:     "test task",
			Phase:    workflow.WFPlan,
			PlanPath: ".wakil/plan.md",
		},
		Exec: fe,
		Out:  io.Discard,
	}

	_ = HandleWorkflowTransition(context.Background(), app)

	if app.Workflow.Phase != workflow.WFPlan {
		t.Errorf("phase = %v, want WFPlan (should not advance with invalid format)", app.Workflow.Phase)
	}
	if !app.Workflow.PlanFormatInvalid {
		t.Error("PlanFormatInvalid should be true when plan has no numbered steps")
	}
}

// TestHandleWorkflowTransition_ImplementStepDone verifies that a %%STEP_DONE%%
// marker in implement phase advances the step index from 1 to 2.
// When StepIdx is 1 and StepCount is 2, the step completes and the workflow
// should advance to step 2, returning a WFStartTurnMsg to kick off the next
// step turn.
func TestHandleWorkflowTransition_ImplementStepDone(t *testing.T) {
	fe := newFakeExecutor()
	fe.files[".wakil/plan.md"] = "## Task\n\ntest\n\n## Plan\n\n1. First\n2. Second\n\n## Step log\n\nStep 1: done"

	app := &App{
		Cfg: config.DefaultConfig(),
		Conv: []proxy.Message{
			{Role: "user", Content: strPtr("do step 1")},
			{Role: "assistant", Content: strPtr("%%STEP_LOG: Step 1: implemented | outcome: ok%%\n%%STEP_DONE%%")},
		},
		Workflow: &workflow.WorkflowState{
			Task:      "test",
			Phase:     workflow.WFImplement,
			PlanPath:  ".wakil/plan.md",
			StepCount: 2,
			StepIdx:   1,
		},
		Exec: fe,
		Out:  io.Discard,
	}

	HandleWorkflowTransition(context.Background(), app)

	// StepIdx must have advanced from 1 to 2.
	if app.Workflow.StepIdx != 2 {
		t.Errorf("StepIdx = %d, want 2 (should advance after %%STEP_DONE%%)", app.Workflow.StepIdx)
	}
}

// TestRetryBackoff_Override verifies that the override function is used when set.
func TestRetryBackoff_Override(t *testing.T) {
	override := func(n int) time.Duration {
		return time.Duration(n) * 10 * time.Millisecond
	}
	got := retryBackoff(3, override)
	if got != 30*time.Millisecond {
		t.Errorf("retryBackoff(3, override) = %v, want 30ms", got)
	}
}

// TestRetryBackoff_Default verifies the default exponential schedule.
func TestRetryBackoff_Default(t *testing.T) {
	// attempt 0: 1s base + jitter (0 to 500ms)
	got := retryBackoff(0, nil)
	if got < 1*time.Second || got > 1500*time.Millisecond {
		t.Errorf("retryBackoff(0, nil) = %v, want 1s-1.5s", got)
	}
	// attempt 1: 2s base + jitter (0 to 1s)
	got = retryBackoff(1, nil)
	if got < 2*time.Second || got > 3*time.Second {
		t.Errorf("retryBackoff(1, nil) = %v, want 2s-3s", got)
	}
}

// strPtr creates a *string from s.
func strPtr(s string) *string { return &s }
