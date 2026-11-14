package agent

// Check A (egress consent for automatic Mashūra rounds): the session's main
// backend is a local host (not OpenRouter), no consent for OpenRouter was
// given in this session, auto mode is on, level high, a struggle is detected.
// Expected: no request reaches the Mashūra host, and the user sees the
// suggest note instead of a silent send.

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/workflow"
)

func checkAApp(t *testing.T, confirm func(_, _, _ string, _ bool) bool) (*App, *int) {
	t.Helper()
	os.Setenv("CHECK_A_OR_KEY", "x")
	t.Cleanup(func() { os.Unsetenv("CHECK_A_OR_KEY") })
	_, count := counselServer(t)

	app := &App{
		Cfg: config.Config{
			OpenRouterAPIKeyEnv: "CHECK_A_OR_KEY",
			OracleAPIKeyEnv:     "CHECK_A_OR_KEY",
			OracleModel:         "anthropic:test-model",
			OracleMaxTokens:     4096,
			OracleEnabled:       true,
			OracleTimeoutSeconds: 5,
			Mashura:             &config.MashuraConfig{Level: "high", Members: []string{"openrouter:test-model"}},
		},
		Exec:   newFakeExecutor(),
		Client: &proxy.Client{},
		Out:    &strings.Builder{},
		Confirm: confirm,
		// Local main backend; never consented to OpenRouter this session.
		SelectedBackend: "local",
		CounselMode:     "auto",
		MaxCounsel:      1,
	}
	app.SetAutoApprove(true) // /auto mode
	return app, count
}

// TestCheckA_AutoCounselBlockedWithoutEgressConsent drives the struggle path
// in auto mode with a local main backend and no OpenRouter consent: the
// Mashūra host must see zero requests, one consent prompt is asked, and the
// suggest note names the missing consent.
func TestCheckA_AutoCounselBlockedWithoutEgressConsent(t *testing.T) {
	asks := 0
	app, count := checkAApp(t, func(toolName, _, _ string, _ bool) bool {
		asks++
		if toolName != "external_backend" {
			t.Errorf("consent ask must use the external_backend surface, got %q", toolName)
		}
		return false // decline
	})
	app.recentTraces = buildStruggleTraces()
	app.maybeSuggestDebug(context.Background())
	if *count != 0 {
		t.Fatalf("egress without consent: %d requests reached the Mashūra host", *count)
	}
	if asks != 1 {
		t.Fatalf("expected exactly one external_backend consent ask, got %d", asks)
	}
	if !strings.Contains(app.Out.(*strings.Builder).String(), "consent") {
		t.Fatalf("user should see the suggest note naming missing consent, got out=%q", app.Out.(*strings.Builder).String())
	}
	// And nothing was injected (blocked, not a diagnosis).
	for _, m := range app.Conv {
		if m.Role == "tool" && strings.Contains(DerefStr(m.Content), "fix the loop exit condition") {
			t.Fatal("blocked auto-counsel still injected a diagnosis")
		}
	}
}

// TestCheckA_ModelCalledToolBlockedWithoutEgressConsent: the model-called
// tool asks the egress consent once and, when declined, returns the blocked
// message instead of queueing (zero requests to the OpenRouter host).
func TestCheckA_ModelCalledToolBlockedWithoutEgressConsent(t *testing.T) {
	asks := 0
	app, count := checkAApp(t, func(toolName, _, _ string, _ bool) bool {
		asks++
		if toolName != "external_backend" {
			t.Errorf("consent ask must use the external_backend surface, got %q", toolName)
		}
		return false // decline
	})
	got := app.handleMashura(context.Background(), "mashura__review", tcArgs("mashura__review", `{"focus":"x"}`))
	if *count != 0 {
		t.Fatalf("model-called tool sent without consent: %d requests", *count)
	}
	app.drainAsyncInbox()
	if *count != 0 {
		t.Fatalf("model-called tool sent without consent: %d requests", *count)
	}
	if asks != 1 {
		t.Fatalf("expected exactly one egress consent ask, got %d", asks)
	}
	if !strings.Contains(got, "consent") {
		t.Fatalf("declined model-called tool must report missing consent, got %q", got)
	}
	if strings.Contains(got, "queued as op-") {
		t.Fatalf("tool was queued without a consent check: %q", got)
	}
}

// TestCheckA_WorkflowPanelBlockedWithoutEgressConsent: the workflow-triggered
// review panel must also stop at the same gate, asking once and recording
// the missing-consent reason.
func TestCheckA_WorkflowPanelBlockedWithoutEgressConsent(t *testing.T) {
	asks := 0
	app, count := checkAApp(t, func(toolName, _, _ string, _ bool) bool {
		asks++
		if toolName != "external_backend" {
			t.Errorf("consent ask must use the external_backend surface, got %q", toolName)
		}
		return false // decline
	})
	app.Workflow = &workflow.WorkflowState{Task: "t", Phase: workflow.WFReview, PlanPath: ".wakil/plan.md", StepCount: 2}
	plan := "## Findings\n\nf\n\n## Plan\n\n1. do it\n\n## Step log\n\n[step 1] done\n"
	_ = app.Exec.(*fakeExecutor)
	app.Exec.(*fakeExecutor).files[".wakil/plan.md"] = plan
	out := HandleReviewOracle(context.Background(), app)
	_ = out
	if *count != 0 {
		t.Fatalf("workflow panel sent without consent: %d requests", *count)
	}
	if app.Workflow.ReviewSkipReason == "" || !strings.Contains(app.Workflow.ReviewSkipReason, "consent") {
		t.Fatalf("workflow should record the missing-consent reason, got %q", app.Workflow.ReviewSkipReason)
	}
	if asks != 1 {
		t.Fatalf("expected exactly one egress consent ask, got %d", asks)
	}
}

// TestCheckA_ConsentGrantRecordsForSession: when the user approves the egress
// prompt once, later Mashūra calls in the same session do not ask again.
func TestCheckA_ConsentGrantRecordsForSession(t *testing.T) {
	asks := 0
	app, _ := checkAApp(t, func(_, _, _ string, _ bool) bool { asks++; return true })
	if !app.mashuraEgressConsent() {
		t.Fatal("approved consent must allow the send")
	}
	if !app.mashuraEgressConsent() {
		t.Fatal("recorded consent must allow subsequent sends without asking")
	}
	if asks != 1 {
		t.Fatalf("expected one ask total, got %d", asks)
	}
}
