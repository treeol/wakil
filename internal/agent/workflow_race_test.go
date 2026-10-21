package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/workflow"
)

// TestWorkflowWritesSynchronizedWithReaders is a -race regression test for the
// workflow ownership model: the facade reads the workflow under stateMu.RLock
// on the TUI goroutine (Snapshot/Info every frame) while the turn goroutine
// drives transitions and idle-time /plan commands mutate it. Every write must
// go through UpdateWorkflow/SetWorkflow; a bare field write here trips -race.
func TestWorkflowWritesSynchronizedWithReaders(t *testing.T) {
	app := &App{
		Cfg:  config.DefaultConfig(),
		Exec: newFakeExecutor(),
		Out:  io.Discard,
		Conv: []proxy.Message{
			{Role: "user", Content: strPtr("investigate")},
			{Role: "assistant", Content: strPtr("done %%PHASE_DONE%%")},
		},
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // facade-style reader
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			app.StateRLock()
			if wf := app.Workflow; wf != nil {
				_ = wf.SidebarLabel()
				_ = wf.StepIdx
			}
			app.StateRUnlock()
			if cp := app.WorkflowCopy(); cp != nil {
				_ = cp.PhaseName()
			}
		}
	}()

	for i := 0; i < 50; i++ {
		app.SetWorkflow(&workflow.WorkflowState{Task: "t", Phase: workflow.WFGather, PlanPath: ".wakil/plan.md"})
		HandleWorkflowTransition(context.Background(), app) // gather → plan (turn-side write)
		HandleTUICommand("/plan review", app)               // phase write (WFPlan: rejected, no write)
		app.UpdateWorkflow(func(w *workflow.WorkflowState) { w.Phase = workflow.WFPresent })
		HandleTUICommand("/plan review", app)  // WFPresent → WFReview
		HandleTUICommand("/plan approve", app) // WFReview → WFPresent (force-skip)
		HandleTUICommand("/plan abort", app)
	}
	close(stop)
	wg.Wait()
}

// TestWorkflowCopyIsDetached verifies that the persisted SavedWorkflow
// is a value copy: later mutation of the live workflow must not leak into a
// snapshot that WriteSession marshals outside the lock.
func TestWorkflowCopyIsDetached(t *testing.T) {
	app := &App{}
	app.SetWorkflow(&workflow.WorkflowState{Task: "t", Phase: workflow.WFGather})
	cp := app.WorkflowCopy()
	app.UpdateWorkflow(func(w *workflow.WorkflowState) { w.Phase = workflow.WFImplement; w.StepIdx = 3 })
	if cp.Phase != workflow.WFGather || cp.StepIdx != 0 {
		t.Fatalf("WorkflowCopy shares state with the live workflow: %+v", *cp)
	}
	if app.UpdateWorkflow(func(*workflow.WorkflowState) {}) != true {
		t.Fatal("UpdateWorkflow should report an active workflow")
	}
	app.SetWorkflow(nil)
	if app.UpdateWorkflow(func(*workflow.WorkflowState) { t.Fatal("fn must not run without a workflow") }) {
		t.Fatal("UpdateWorkflow should report no workflow")
	}
}

// TestBackendConsentConcurrentAccess is a -race regression test: detached
// async subagent workers snapshot the consent set while the turn goroutine of
// a later turn grants consent; both sides must go through consentMu.
func TestBackendConsentConcurrentAccess(t *testing.T) {
	app := &App{}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = app.snapshotBackendConsent()
				_ = app.hasBackendConsent("openrouter")
			}
		}()
	}
	for j := 0; j < 200; j++ {
		app.grantBackendConsent("openrouter")
		if j%50 == 0 {
			app.resetBackendConsent()
		}
	}
	wg.Wait()
	app.grantBackendConsent("x")
	snap := app.snapshotBackendConsent()
	snap["y"] = true // must not leak into the live map
	if app.hasBackendConsent("y") {
		t.Fatal("snapshot shares the live consent map")
	}
}

// TestTouchedExternalParallelLatch is a -race regression test for the taint
// latch written by parallel subagent workers.
func TestTouchedExternalParallelLatch(t *testing.T) {
	app := &App{}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); app.touchedExternal.Store(true) }()
	}
	wg.Wait()
	if app.computeTainted() == 0 || !app.touchedExternal.Load() {
		t.Fatal("taint latch lost")
	}
}

// reviewOracleApp builds an auto-mode App in WFReview whose oracle endpoint
// runs onCall (e.g. to edit the plan) while the review request is in flight.
func reviewOracleApp(t *testing.T, onCall func()) (*App, *fakeExecutor) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if onCall != nil {
			onCall()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"looks fine"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("MASHURA_TEST_KEY", "x")

	fe := newFakeExecutor()
	fe.files[".wakil/plan.md"] = "## Task\n\nt\n\n## Plan\n\n1. one\n2. two\n"
	app := &App{
		Cfg: config.Config{
			OracleAPIKeyEnv:      "MASHURA_TEST_KEY",
			OracleModel:          "anthropic:test-model",
			OracleMaxTokens:      256,
			OracleEnabled:        true,
			OracleEndpoint:       srv.URL + "/v1/messages",
			OracleTimeoutSeconds: 5,
		},
		Client:   &proxy.Client{},
		Exec:     fe,
		Out:      io.Discard,
		Confirm:  func(_, _, _ string, _ bool) bool { return true },
		Workflow: &workflow.WorkflowState{Task: "t", Phase: workflow.WFReview, PlanPath: ".wakil/plan.md", StepCount: 2},
	}
	app.SetAutoApprove(true)
	return app, fe
}

// TestReviewOracleHashBindsReviewedContent: the stored ReviewPlanHash is the
// hash of the content the oracle reviewed, and an unchanged plan auto-advances.
func TestReviewOracleHashBindsReviewedContent(t *testing.T) {
	app, fe := reviewOracleApp(t, nil)
	reviewed := fe.files[".wakil/plan.md"]
	if msg := HandleReviewOracle(context.Background(), app); msg == nil {
		t.Fatal("unchanged plan in auto mode should auto-advance")
	}
	wf := app.WorkflowCopy()
	if wf.Phase != workflow.WFImplement || wf.StepIdx != 1 {
		t.Fatalf("want WFImplement step 1, got %v step %d", wf.Phase, wf.StepIdx)
	}
	if wf.ReviewPlanHash != workflow.HashPlanSection(reviewed) {
		t.Fatal("ReviewPlanHash must be the hash of the reviewed content")
	}
}

// TestReviewOracleNoAutoAdvanceWhenPlanEditedDuringReview: an edit landing
// while the oracle call is in flight must park the workflow at WFPresent with
// the hash of the REVIEWED content, so /plan approve's stale check fires.
func TestReviewOracleNoAutoAdvanceWhenPlanEditedDuringReview(t *testing.T) {
	var fe *fakeExecutor
	var app *App
	app, fe = reviewOracleApp(t, func() {
		fe.mu.Lock()
		fe.files[".wakil/plan.md"] = "## Task\n\nt\n\n## Plan\n\n1. one\n2. two\n3. sneaked in\n"
		fe.mu.Unlock()
	})
	reviewed := fe.files[".wakil/plan.md"]
	if msg := HandleReviewOracle(context.Background(), app); msg != nil {
		t.Fatal("plan edited during review must not auto-advance")
	}
	wf := app.WorkflowCopy()
	if wf.Phase != workflow.WFPresent {
		t.Fatalf("want WFPresent, got %v", wf.Phase)
	}
	if wf.ReviewPlanHash != workflow.HashPlanSection(reviewed) {
		t.Fatal("ReviewPlanHash must describe the reviewed (pre-edit) content")
	}
}

// TestPlanApproveRejectsReplacedWorkflow: a /plan approve closure whose
// workflow was replaced before it commits must not advance the new workflow.
func TestPlanApproveRejectsReplacedWorkflow(t *testing.T) {
	fe := newFakeExecutor()
	fe.files[".wakil/plan.md"] = "## Plan\n\n1. one\n"
	app := &App{Cfg: config.DefaultConfig(), Exec: fe, Out: io.Discard}
	app.SetWorkflow(&workflow.WorkflowState{Task: "a", Phase: workflow.WFPresent, PlanPath: ".wakil/plan.md", StepCount: 1})

	handled, _, cmd := HandleTUICommand("/plan approve", app)
	if !handled || cmd == nil {
		t.Fatal("expected a deferred approve command")
	}
	// Before the closure runs: abort and start a different workflow.
	HandleTUICommand("/plan abort", app)
	replacement := &workflow.WorkflowState{Task: "b", Phase: workflow.WFPresent, PlanPath: ".wakil/plan.md", StepCount: 1}
	app.SetWorkflow(replacement)

	msg := cmd()
	if _, ok := msg.(WFStartTurnMsg); ok {
		t.Fatal("stale approve must not start an implementation turn")
	}
	if wf := app.WorkflowCopy(); wf.Phase != workflow.WFPresent || wf.Task != "b" {
		t.Fatalf("replacement workflow was mutated by a stale approve: %+v", *wf)
	}
}
