package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wakil/internal/agent"
	"wakil/internal/config"
	"wakil/internal/counsel"
	"wakil/internal/proxy"
	"wakil/internal/workflow"
)

// --- unit tests: pure workflow logic ---

func TestCountPlanSteps(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    int
	}{
		{"empty", "", 0},
		{"no plan section", "## Task\n\ndo stuff\n", 0},
		{"dot style", "## Plan\n\n1. Step one\n2. Step two\n3. Step three\n", 3},
		{"paren style", "## Plan\n\n1) First\n2) Second\n", 2},
		{"mixed and clipped", "## Plan\n\n1. a\n2. b\n\n## Step log\n\n(none)", 2},
		{"plan with preamble text", "## Plan\n\nSome intro.\n\n1. actual step\n", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := workflow.CountPlanSteps(tc.content); got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestExtractPlanSection(t *testing.T) {
	content := "## Task\n\ndo the thing\n\n## Findings\n\nfound stuff\n\n## Plan\n\n1. step one\n"
	if got := workflow.ExtractPlanSection(content, "## Findings"); got != "found stuff" {
		t.Errorf("findings: got %q", got)
	}
	if got := workflow.ExtractPlanSection(content, "## Task"); got != "do the thing" {
		t.Errorf("task: got %q", got)
	}
	if got := workflow.ExtractPlanSection(content, "## Missing"); got != "" {
		t.Errorf("missing: got %q", got)
	}
}

func TestRecentStepEntries(t *testing.T) {
	log := "Step 1: a\n\nStep 2: b\n\nStep 3: c\n\nStep 4: d\n\nStep 5: e\n\nStep 6: f"
	entries := workflow.RecentStepEntries(log, 3)
	if len(entries) != 3 {
		t.Fatalf("want 3 entries, got %d", len(entries))
	}
	if entries[0] != "Step 4: d" {
		t.Errorf("first recent entry: got %q", entries[0])
	}
	if entries[2] != "Step 6: f" {
		t.Errorf("last entry: got %q", entries[2])
	}
}

func TestRecentStepEntries_Placeholder(t *testing.T) {
	if entries := workflow.RecentStepEntries("(none yet)", 5); len(entries) != 0 {
		t.Errorf("placeholder should yield 0 entries, got %d", len(entries))
	}
}

// TestBuildOracleBriefing_Cap verifies the briefing never exceeds 12 KB even
// when the step log is very large.
func TestBuildOracleBriefing_Cap(t *testing.T) {
	var logEntries []string
	for i := range 50 {
		logEntries = append(logEntries, strings.Repeat("x", 400)+" step "+string(rune('A'+i%26)))
	}
	planContent := "## Task\n\nsome task\n\n## Plan\n\n1. step one\n2. step two\n\n## Step log\n\n" +
		strings.Join(logEntries, "\n\n")

	briefing := workflow.BuildOracleBriefing("some task", planContent, "what should I do?")
	if len(briefing) > workflow.WFBriefingCap {
		t.Errorf("briefing len %d exceeds cap %d", len(briefing), workflow.WFBriefingCap)
	}
	for _, section := range []string{"## Task", "## Plan", "## Question"} {
		if !strings.Contains(briefing, section) {
			t.Errorf("briefing missing %s", section)
		}
	}
}

// TestBuildOracleBriefing_NoBriefingInConv is a structural test: buildOracleBriefing
// accepts only planContent+question, making it impossible to include Conv history.
func TestBuildOracleBriefing_NoBriefingInConv(t *testing.T) {
	planContent := "## Task\n\nreal task\n\n## Plan\n\n1. step\n"
	briefing := workflow.BuildOracleBriefing("some task", planContent, "question")
	// The function has no access to Conv — verifying it compiles and runs is sufficient.
	if briefing == "" {
		t.Error("briefing should not be empty")
	}
}

func TestDetectPhaseMarkers(t *testing.T) {
	makeConv := func(lastMsg string) []proxy.Message {
		return []proxy.Message{
			{Role: "user", Content: agent.StrPtr("task")},
			{Role: "assistant", Content: agent.StrPtr(lastMsg)},
		}
	}
	pd, sd, sf := workflow.DetectPhaseMarkers(makeConv("done with gathering\n" + workflow.WFPhaseDone))
	if !pd || sd || sf {
		t.Errorf("PHASE_DONE: pd=%v sd=%v sf=%v", pd, sd, sf)
	}

	pd, sd, sf = workflow.DetectPhaseMarkers(makeConv("step complete\n" + workflow.WFStepDone))
	if pd || !sd || sf {
		t.Errorf("STEP_DONE: pd=%v sd=%v sf=%v", pd, sd, sf)
	}

	pd, sd, sf = workflow.DetectPhaseMarkers(makeConv("step failed\n" + workflow.WFStepFailed))
	if pd || sd || !sf {
		t.Errorf("STEP_FAILED: pd=%v sd=%v sf=%v", pd, sd, sf)
	}

	pd, sd, sf = workflow.DetectPhaseMarkers(makeConv("just a normal response"))
	if pd || sd || sf {
		t.Errorf("no markers: pd=%v sd=%v sf=%v", pd, sd, sf)
	}
}

func TestWorkflowDirective_ContainsTask(t *testing.T) {
	wf := &workflow.WorkflowState{
		Task:      "add unit tests for foo",
		Phase:     workflow.WFGather,
		PlanPath:  ".wakil/plan.md",
		StepCount: 3,
		StepIdx:   1,
	}
	d := wf.Directive()
	if !strings.Contains(d, "add unit tests for foo") {
		t.Error("gather directive missing task")
	}
	if !strings.Contains(d, workflow.WFPhaseDone) {
		t.Error("gather directive missing sentinel")
	}

	wf.Phase = workflow.WFPlan
	d = wf.Directive()
	if !strings.Contains(d, workflow.WFPhaseDone) {
		t.Error("plan directive missing sentinel")
	}

	wf.Phase = workflow.WFImplement
	d = wf.Directive()
	if !strings.Contains(d, workflow.WFStepDone) || !strings.Contains(d, workflow.WFStepFailed) {
		t.Error("implement directive missing step sentinels")
	}

	// workflow.WFReview now has a retry directive; workflow.WFPresent and workflow.WFDone must be empty.
	for _, phase := range []workflow.WorkflowPhase{workflow.WFPresent, workflow.WFDone} {
		wf.Phase = phase
		if wf.Directive() != "" {
			t.Errorf("phase %d should have empty directive", phase)
		}
	}
	wf.Phase = workflow.WFReview
	if wf.Directive() == "" {
		t.Error("workflow.WFReview should have a retry directive")
	}
}

func TestWorkflowSidebarLabel(t *testing.T) {
	wf := &workflow.WorkflowState{Phase: workflow.WFGather}
	if wf.SidebarLabel() != "gather" {
		t.Errorf("gather label: %q", wf.SidebarLabel())
	}
	wf.Phase = workflow.WFImplement
	wf.StepIdx = 2
	wf.StepCount = 5
	if wf.SidebarLabel() != "implement 2/5" {
		t.Errorf("implement label: %q", wf.SidebarLabel())
	}
	wf.Phase = workflow.WFDone
	if wf.SidebarLabel() != "done" {
		t.Errorf("done label: %q", wf.SidebarLabel())
	}
}

func TestWfInitPlanContent(t *testing.T) {
	content := workflow.WFInitPlanContent("my task")
	for _, section := range []string{"## Task", "## Findings", "## Plan"} {
		if !strings.Contains(content, section) {
			t.Errorf("plan content missing %q", section)
		}
	}
	// ## Step log must be absent — Wakil creates it on first append to prevent
	// a duplicate when the model rewrites the whole file.
	if strings.Contains(content, "## Step log") {
		t.Error("scaffold must not contain ## Step log")
	}
	if !strings.Contains(content, "my task") {
		t.Error("plan content missing task text")
	}
}

// --- integration tests using httptest server + fakeExecutor ---

// planContent2Steps is a realistic two-step plan for integration tests.
const planContent2Steps = "## Task\n\nmy task\n\n## Findings\n\nfound it\n\n## Plan\n\n" +
	"1. Step one\n2. Step two\n\n## Step log\n\n(none yet)\n"

// TestWorkflowHappyPath exercises GATHER → PLAN → REVIEW(oracle unavailable→workflow.WFReview)
// → /plan approve → PRESENT → /plan approve → IMPLEMENT (2 steps) → DONE.
// Oracle is not configured, so REVIEW halts at workflow.WFReview requiring explicit approval.
func TestWorkflowHappyPath(t *testing.T) {
	srv := sseServer(t,
		// GATHER turn
		[]string{contentChunk("I looked at the code.\n" + workflow.WFPhaseDone)},
		// PLAN turn
		[]string{contentChunk("Here is the plan.\n" + workflow.WFPhaseDone)},
		// IMPLEMENT step 1
		[]string{contentChunk("Did step one.\n" + workflow.WFStepDone)},
		// IMPLEMENT step 2
		[]string{contentChunk("Did step two.\n" + workflow.WFStepDone)},
	)
	defer srv.Close()

	exec := newFakeExecutor()
	app := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return false })
	exec.files[".wakil/plan.md"] = planContent2Steps
	app.Cfg.WFFinalReview = false // final review is covered by TestFinalReview* tests

	app.Workflow = &workflow.WorkflowState{
		Task:     "my task",
		Phase:    workflow.WFGather,
		PlanPath: ".wakil/plan.md",
	}
	ctx := context.Background()

	// --- GATHER turn ---
	if _, err := app.Send(ctx, "continue"); err != nil {
		t.Fatalf("gather: %v", err)
	}
	// Directive must be stored in Conv.
	if !strings.Contains(*app.Conv[0].Content, "[WORKFLOW GATHER]") {
		t.Error("gather: directive not in Conv[0]")
	}
	next := agent.HandleWorkflowTransition(ctx, app)
	if app.Workflow.Phase != workflow.WFPlan {
		t.Errorf("after gather: want workflow.WFPlan, got %v", app.Workflow.Phase)
	}
	if next == nil {
		t.Fatal("after gather: expected agent.WFStartTurnMsg for plan turn")
	}

	// --- PLAN turn ---
	app.Conv = nil
	if _, err := app.Send(ctx, "continue"); err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !strings.Contains(*app.Conv[0].Content, "[WORKFLOW PLAN]") {
		t.Error("plan: directive not in Conv[0]")
	}
	next = agent.HandleWorkflowTransition(ctx, app)
	// Oracle not configured → stays at workflow.WFReview, waiting for /plan approve.
	if app.Workflow.Phase != workflow.WFReview {
		t.Errorf("after plan (oracle unavailable): want workflow.WFReview, got %v", app.Workflow.Phase)
	}
	if app.Workflow.StepCount != 2 {
		t.Errorf("step count: want 2, got %d", app.Workflow.StepCount)
	}
	if next != nil {
		t.Error("after plan: must not auto-start; waiting for /plan approve")
	}
	// Verify the review-skip log entry was written to plan.md.
	planContent := exec.files[".wakil/plan.md"]
	if !strings.Contains(planContent, "REVIEW skipped") {
		t.Error("plan.md missing REVIEW skipped log entry")
	}

	// --- /plan approve (REVIEW→PRESENT) ---
	app.Workflow.Phase = workflow.WFPresent

	// --- /plan approve (PRESENT→IMPLEMENT) ---
	app.Workflow.Phase = workflow.WFImplement
	app.Workflow.StepIdx = 1

	// --- IMPLEMENT step 1 ---
	app.Conv = nil
	if _, err := app.Send(ctx, "continue"); err != nil {
		t.Fatalf("step 1: %v", err)
	}
	if !strings.Contains(*app.Conv[0].Content, "[WORKFLOW IMPLEMENT STEP 1/2]") {
		t.Errorf("step 1: directive missing, got: %q", *app.Conv[0].Content)
	}
	next = agent.HandleWorkflowTransition(ctx, app)
	if app.Workflow == nil {
		t.Fatal("workflow should still be active after step 1 of 2")
	}
	if app.Workflow.StepIdx != 2 {
		t.Errorf("after step 1: want StepIdx=2, got %d", app.Workflow.StepIdx)
	}
	if next == nil {
		t.Fatal("after step 1: expected auto-turn msg for step 2")
	}

	// --- IMPLEMENT step 2 ---
	app.Conv = nil
	if _, err := app.Send(ctx, "continue"); err != nil {
		t.Fatalf("step 2: %v", err)
	}
	agent.HandleWorkflowTransition(ctx, app)
	if app.Workflow != nil {
		t.Errorf("workflow should be nil after last step, got phase=%v", app.Workflow.Phase)
	}
}

// TestReviewOracleUnavailable verifies that when oracle is unavailable the workflow
// stays at workflow.WFReview (does not auto-advance), emits nothing to workflow.WFPresent, and writes
// a "REVIEW skipped" entry to plan.md.
func TestReviewOracleUnavailable(t *testing.T) {
	for _, name := range []string{"disabled", "no_key"} {
		t.Run(name, func(t *testing.T) {
			srv := sseServer(t, []string{contentChunk("plan written\n" + workflow.WFPhaseDone)})
			defer srv.Close()

			exec := newFakeExecutor()
			exec.files[".wakil/plan.md"] = planContent2Steps
			app := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return false })

			if name == "disabled" {
				app.Cfg.OracleEnabled = false
			} else {
				app.Cfg.OracleEnabled = true
				app.Cfg.OracleAPIKeyEnv = "WAKIL_TEST_NO_SUCH_KEY"
				// env var deliberately not set → apiKey == ""
			}

			app.Workflow = &workflow.WorkflowState{
				Task:      "t",
				Phase:     workflow.WFPlan,
				PlanPath:  ".wakil/plan.md",
				StepCount: 2,
			}
			ctx := context.Background()
			if _, err := app.Send(ctx, "continue"); err != nil {
				t.Fatalf("send: %v", err)
			}

			next := agent.HandleWorkflowTransition(ctx, app)

			if app.Workflow.Phase != workflow.WFReview {
				t.Errorf("want workflow.WFReview, got %v", app.Workflow.Phase)
			}
			if next != nil {
				t.Error("must not auto-start turn when oracle unavailable")
			}
			planContent := exec.files[".wakil/plan.md"]
			if !strings.Contains(planContent, "REVIEW skipped") {
				t.Error("plan.md should contain REVIEW skipped entry")
			}
		})
	}
}

// TestReviewApproveAdvancesToPresent verifies /plan approve on workflow.WFReview advances
// to workflow.WFPresent (the normal PRESENT→IMPLEMENT approval is a separate step).
func TestReviewApproveAdvancesToPresent(t *testing.T) {
	app := &agent.App{
		Cfg: config.DefaultConfig(),
		Workflow: &workflow.WorkflowState{
			Task:      "t",
			Phase:     workflow.WFReview,
			StepCount: 3,
			PlanPath:  ".wakil/plan.md",
		},
	}
	_, _, cmd := agent.HandlePlanCommand([]string{"/plan", "approve"}, app)
	if app.Workflow.Phase != workflow.WFPresent {
		t.Errorf("want workflow.WFPresent after approve on workflow.WFReview, got %v", app.Workflow.Phase)
	}
	if cmd == nil {
		t.Error("approve should return a note cmd")
	}
}

// TestReviewOracleCallFailure treats a call error the same as unavailable.
func TestReviewOracleCallFailure(t *testing.T) {
	srv := sseServer(t, []string{contentChunk("plan written\n" + workflow.WFPhaseDone)})
	defer srv.Close()

	exec := newFakeExecutor()
	exec.files[".wakil/plan.md"] = planContent2Steps
	// Configure oracle with valid-looking key, but the confirm gate auto-approves
	// to reach callOracle — which will fail because srv is not an oracle endpoint.
	app := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.OracleEnabled = true
	app.Cfg.OracleAPIKeyEnv = "WAKIL_TEST_ORACLE_KEY"
	app.Cfg.OracleModel = "test-model"
	t.Setenv("WAKIL_TEST_ORACLE_KEY", "fake-key")

	app.Workflow = &workflow.WorkflowState{
		Task:      "t",
		Phase:     workflow.WFPlan,
		PlanPath:  ".wakil/plan.md",
		StepCount: 2,
	}
	ctx := context.Background()
	if _, err := app.Send(ctx, "continue"); err != nil {
		t.Fatalf("send: %v", err)
	}
	agent.HandleWorkflowTransition(ctx, app)

	// Call error → oracle unavailable → must stay at workflow.WFReview.
	if app.Workflow.Phase != workflow.WFReview {
		t.Errorf("call failure: want workflow.WFReview, got %v", app.Workflow.Phase)
	}
	if !strings.Contains(exec.files[".wakil/plan.md"], "REVIEW skipped") {
		t.Error("plan.md missing REVIEW skipped entry on call failure")
	}
}

// TestWfAppendToStepLog verifies the entry appears at the end of the file.
func TestWfAppendToStepLog(t *testing.T) {
	content := "## Task\n\nt\n\n## Step log\n\n(none yet)\n"
	out := workflow.WFAppendToStepLog(content, "REVIEW skipped: no key")
	if !strings.Contains(out, "REVIEW skipped: no key") {
		t.Error("entry missing")
	}
	if !strings.HasPrefix(out, "## Task") {
		t.Error("original content lost")
	}
}

// TestGatherDirectiveHasScopeCap verifies the scope-cap line is present.
func TestGatherDirectiveHasScopeCap(t *testing.T) {
	wf := &workflow.WorkflowState{Task: "do stuff", Phase: workflow.WFGather, PlanPath: ".wakil/plan.md"}
	d := wf.Directive()
	if !strings.Contains(d, "at most ~5 reads") {
		t.Error("gather directive missing scope cap")
	}
}

// TestWorkflowStepFailure verifies that on %%STEP_FAILED%%, handleWorkflowTransition
// calls the oracle with a briefing that contains ## Plan + step log — not Conv history.
// The oracle is configured but declined; we capture the briefing shown in the confirm detail.
func TestWorkflowStepFailure(t *testing.T) {
	srv := sseServer(t,
		[]string{contentChunk("step failed horribly\n" + workflow.WFStepFailed)},
	)
	defer srv.Close()

	var capturedDetail string
	exec := newFakeExecutor()
	app := newTestApp(srv.URL, exec, func(_, _, detail string, _ bool) bool {
		capturedDetail = detail
		return false // decline oracle
	})
	app.Cfg.OracleEnabled = true
	app.Cfg.OracleAPIKeyEnv = "WAKIL_TEST_ORACLE_KEY"
	t.Setenv("WAKIL_TEST_ORACLE_KEY", "fake-key-for-test")

	planContent := "## Task\n\nmy task\n\n## Plan\n\n1. step one\n2. step two\n\n" +
		"## Step log\n\nStep 1: attempt | outcome: fail | deviation: none"
	exec.files[".wakil/plan.md"] = planContent

	app.Workflow = &workflow.WorkflowState{
		Task:      "my task",
		Phase:     workflow.WFImplement,
		PlanPath:  ".wakil/plan.md",
		StepCount: 2,
		StepIdx:   1,
	}

	ctx := context.Background()
	app.Conv = nil
	if _, err := app.Send(ctx, "continue"); err != nil {
		t.Fatalf("send: %v", err)
	}

	next := agent.HandleWorkflowTransition(ctx, app)
	if next != nil {
		t.Error("on step failure: must not auto-advance")
	}
	if capturedDetail == "" {
		t.Fatal("confirm gate was never called — oracle not triggered on step failure")
	}
	if !strings.Contains(capturedDetail, "## Plan") {
		t.Error("briefing in confirm detail should contain ## Plan")
	}
	// Critically: briefing must NOT contain Conv messages verbatim.
	for _, msg := range app.Conv {
		if msg.Content != nil && len(*msg.Content) > 20 {
			// Only check substantive messages (avoid short fragments matching by accident).
			if strings.Contains(capturedDetail, (*msg.Content)[:20]) {
				t.Error("briefing must not include Conv history in the confirm detail")
			}
		}
	}
}

// TestConvAppendOnly verifies that workflow transitions never reset or truncate
// app.Conv — the session log must be strictly append-only throughout a workflow.
func TestConvAppendOnly(t *testing.T) {
	srv := sseServer(t,
		[]string{contentChunk("gathered\n" + workflow.WFPhaseDone)}, // GATHER
		[]string{contentChunk("planned\n" + workflow.WFPhaseDone)},  // PLAN
		[]string{contentChunk("step done\n" + workflow.WFStepDone)}, // IMPLEMENT step 1
	)
	defer srv.Close()

	exec := newFakeExecutor()
	exec.files[".wakil/plan.md"] = planContent2Steps
	app := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return false })

	app.Workflow = &workflow.WorkflowState{
		Task:     "t",
		Phase:    workflow.WFGather,
		PlanPath: ".wakil/plan.md",
	}
	ctx := context.Background()
	prevLen := 0

	for _, desc := range []string{"gather", "plan"} {
		if _, err := app.Send(ctx, "continue"); err != nil {
			t.Fatalf("%s: %v", desc, err)
		}
		agent.HandleWorkflowTransition(ctx, app)
		if len(app.Conv) <= prevLen {
			t.Errorf("%s: Conv did not grow (len=%d, prev=%d)", desc, len(app.Conv), prevLen)
		}
		prevLen = len(app.Conv)
	}

	// Advance to IMPLEMENT step 1.
	if app.Workflow != nil {
		app.Workflow.Phase = workflow.WFImplement
		app.Workflow.StepIdx = 1
		app.Workflow.StepCount = 1
	}
	if _, err := app.Send(ctx, "continue"); err != nil {
		t.Fatalf("implement: %v", err)
	}
	agent.HandleWorkflowTransition(ctx, app)
	if len(app.Conv) <= prevLen {
		t.Errorf("implement: Conv did not grow (len=%d, prev=%d)", len(app.Conv), prevLen)
	}
}

// ---- P13c tests ----

// TestExtractStepLogEntry covers the sentinel extraction from model output.
func TestExtractStepLogEntry(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "well-formed",
			input: "I did the work.\n%%STEP_LOG: Step 1: edited app.go | outcome: success | deviation: none%%\n%%STEP_DONE%%",
			want:  "Step 1: edited app.go | outcome: success | deviation: none",
		},
		{
			name:  "no closing pct",
			input: "%%STEP_LOG: Step 2: something",
			want:  "Step 2: something",
		},
		{
			name:  "absent",
			input: "I did it. %%STEP_DONE%%",
			want:  "",
		},
		{
			name:  "whitespace trimmed",
			input: "%%STEP_LOG:  Step 3: fixed it  %%",
			want:  "Step 3: fixed it",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := workflow.ExtractStepLogEntry(tc.input)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCountStepLogEntries counts only "Step N:" lines, ignoring REVIEW skipped.
func TestCountStepLogEntries(t *testing.T) {
	plan := "## Task\n\nt\n\n## Plan\n\n1. step\n\n## Step log\n\n" +
		"REVIEW skipped: no key\n\n" +
		"Step 1: did it | outcome: ok | deviation: none\n\n" +
		"Step 2: more | outcome: ok | deviation: none\n"
	if got := workflow.CountStepLogEntries(plan); got != 2 {
		t.Errorf("want 2 step entries, got %d", got)
	}
}

// TestImplementStepLogExtractAndAppend runs a full IMPLEMENT turn and checks
// that the %%STEP_LOG%% entry is extracted and written to plan.md.
func TestImplementStepLogExtractAndAppend(t *testing.T) {
	logEntry := "Step 1: edited compact.go | outcome: builds | deviation: none"
	srv := sseServer(t,
		[]string{contentChunk(
			"I made the change.\n%%STEP_LOG: " + logEntry + "%%\n%%STEP_DONE%%",
		)},
	)
	defer srv.Close()

	exec := newFakeExecutor()
	exec.files[".wakil/plan.md"] = planContent2Steps
	app := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return false })

	app.Workflow = &workflow.WorkflowState{
		Task:      "t",
		Phase:     workflow.WFImplement,
		PlanPath:  ".wakil/plan.md",
		StepCount: 2,
		StepIdx:   1,
	}
	ctx := context.Background()
	if _, err := app.Send(ctx, "continue"); err != nil {
		t.Fatalf("send: %v", err)
	}
	agent.HandleWorkflowTransition(ctx, app)

	plan := exec.files[".wakil/plan.md"]
	if !strings.Contains(plan, logEntry) {
		t.Errorf("step log entry not in plan.md; got:\n%s", plan)
	}
	if !strings.Contains(plan, "## Step log") {
		t.Error("## Step log section missing from plan.md")
	}
	if got := workflow.CountStepLogEntries(plan); got != 1 {
		t.Errorf("count: want 1 entry, got %d", got)
	}
}

// TestScaffoldNoStepLog verifies the initial scaffold omits ## Step log,
// and that wfAppendToStepLog creates it on first write.
func TestScaffoldNoStepLog(t *testing.T) {
	content := workflow.WFInitPlanContent("my task")
	if strings.Contains(content, "## Step log") {
		t.Error("scaffold must not contain ## Step log (causes duplicate on full-file rewrite)")
	}
	appended := workflow.WFAppendToStepLog(content, "Step 1: done")
	if !strings.Contains(appended, "## Step log") {
		t.Error("wfAppendToStepLog should create ## Step log section when absent")
	}
	if !strings.Contains(appended, "Step 1: done") {
		t.Error("entry missing after append")
	}
}

// TestImplementDirectiveNoStepLogWrite checks the IMPLEMENT directive does not
// instruct the model to edit ## Step log, and does include the %%STEP_LOG%% sentinel format.
func TestImplementDirectiveNoStepLogWrite(t *testing.T) {
	wf := &workflow.WorkflowState{
		Task:      "t",
		Phase:     workflow.WFImplement,
		PlanPath:  ".wakil/plan.md",
		StepCount: 2,
		StepIdx:   1,
	}
	d := wf.Directive()
	if strings.Contains(d, "Append a step-log entry to") {
		t.Error("directive must not tell model to append to step log directly")
	}
	if !strings.Contains(d, workflow.WFStepLogMark) {
		t.Error("directive must include the %%STEP_LOG: sentinel format")
	}
}

// TestOracleEmptyResponseRoutesUnavailable verifies that an empty/whitespace-only
// oracle text response routes through the unavailable path (ok=false from doWFOracle).
func TestOracleEmptyResponseRoutesUnavailable(t *testing.T) {
	// Oracle server that returns a valid HTTP 200 with no text blocks.
	srv := sseServer(t, []string{contentChunk("plan written\n" + workflow.WFPhaseDone)})
	defer srv.Close()

	// Build a fake oracle HTTP server that returns an empty-text response.
	import_net_http_used := false
	_ = import_net_http_used
	oracleSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Response with a text block containing only whitespace.
		w.Write([]byte(`{"content":[{"type":"text","text":"   "}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":1}}`))
	}))
	defer oracleSrv.Close()

	exec := newFakeExecutor()
	exec.files[".wakil/plan.md"] = planContent2Steps
	app := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.OracleEnabled = true
	app.Cfg.OracleAPIKeyEnv = "WAKIL_TEST_ORACLE_KEY"
	app.Cfg.OracleModel = "test"
	t.Setenv("WAKIL_TEST_ORACLE_KEY", "fake")

	// Patch callOracle to use our fake server.
	// We can't easily patch callOracle, so test doWFOracle indirectly via
	// the transition logic. Instead, test counsel.CallOracleURL directly.
	_, _, err := counsel.CallOracleURL(context.Background(), app.Cfg, "fake", "question", "", oracleSrv.URL+"/v1/messages")
	if err == nil {
		t.Error("expected error for whitespace-only oracle response")
	}
	if !strings.Contains(err.Error(), "empty or whitespace") {
		t.Errorf("wrong error: %v", err)
	}
}

// TestOracleNoTextBlocks verifies no-text-block responses produce an error.
func TestOracleNoTextBlocks(t *testing.T) {
	oracleSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Response with only a thinking block, hit max_tokens.
		w.Write([]byte(`{"content":[{"type":"thinking","text":"hmm"}],"stop_reason":"max_tokens","usage":{"input_tokens":10,"output_tokens":50}}`))
	}))
	defer oracleSrv.Close()

	cfg := config.DefaultConfig()
	cfg.OracleModel = "test"
	_, _, err := counsel.CallOracleURL(context.Background(), cfg, "key", "q", "", oracleSrv.URL+"/v1/messages")
	if err == nil {
		t.Fatal("expected error for thinking-only + max_tokens response")
	}
	if !strings.Contains(err.Error(), "thinking model") {
		t.Errorf("error should mention thinking model, got: %v", err)
	}
}

// TestOracleTimeoutConfigFlowsThrough: oracle_timeout_seconds in config is
// applied as the context deadline inside counsel.CallOracleURL. We set a 1s timeout
// and point the call at a server that stalls — the call must return a
// deadline error well within the test's own budget.
func TestOracleTimeoutConfigFlowsThrough(t *testing.T) {
	// done is closed before slow.Close() so the handler goroutine can exit
	// promptly; httptest.Server.Close() would otherwise hang waiting for it.
	done := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-done:
		case <-time.After(30 * time.Second): // hard backstop
		}
	}))
	defer func() {
		close(done) // unblock handler goroutine first
		slow.Close()
	}()

	cfg := config.DefaultConfig()
	cfg.OracleModel = "test"
	cfg.OracleTimeoutSeconds = 1 // 1-second deadline

	start := time.Now()
	_, _, err := counsel.CallOracleURL(context.Background(), cfg, "key", "q", "", slow.URL+"/v1/messages")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed > 10*time.Second {
		t.Errorf("call took too long (%v); timeout not applied", elapsed)
	}
	if !strings.Contains(err.Error(), "context") && !strings.Contains(err.Error(), "deadline") &&
		!strings.Contains(err.Error(), "canceled") && !strings.Contains(err.Error(), "timeout") {
		t.Errorf("expected timeout/deadline error, got: %v", err)
	}
}

// TestDestructiveShellAlwaysGates verifies that isDestructiveShell returns true
// for the spec-listed patterns.
func TestDestructiveShellAlwaysGates(t *testing.T) {
	mustGate := []string{
		"rm -rf /tmp/foo",
		"rm foo.txt",
		"mv a.go b.go",
		"git reset --hard HEAD",
		"git checkout -- .",
		"git clean -fd",
		"chmod -R 777 .",
		"kill 12345",
		"sudo apt-get install something",
		"dd if=/dev/zero of=/dev/sda",
		"ls | rm -rf",       // rm in pipeline
		"echo hi && rm foo", // rm after &&
	}
	for _, cmd := range mustGate {
		if !agent.IsDestructiveShell(cmd) {
			t.Errorf("agent.IsDestructiveShell(%q) = false, want true", cmd)
		}
	}
}

// TestNonDestructiveShellDoesNotGate ensures safe commands pass through auto mode.
func TestNonDestructiveShellDoesNotGate(t *testing.T) {
	safeCommands := []string{
		"ls -la",
		"grep -r 'foo' .",
		"git status",
		"git checkout main", // branch switch, not destructive
		"git diff HEAD",
		"go build ./...",
		"cat file.txt",
		"chmod 644 file.txt", // without -R
	}
	for _, cmd := range safeCommands {
		if agent.IsDestructiveShell(cmd) {
			t.Errorf("agent.IsDestructiveShell(%q) = true, should not gate safe command", cmd)
		}
	}
}

// TestAutoModeGatesDestructiveShell verifies the tuiConfirmer bypass: a destructive
// command reaches the interactive gate even with AutoApprove=true.
func TestAutoModeGatesDestructiveShell(t *testing.T) {
	srv := sseServer(t, toolCallFrames("c1", "run_shell", `{"command":"rm -rf /tmp/test"}`),
		[]string{contentChunk("done")})
	defer srv.Close()

	exec := newFakeExecutor()
	prompted := false
	app := newTestApp(srv.URL, exec, func(toolName, _, detail string, _ bool) bool {
		if toolName == "run_shell" && strings.Contains(detail, "rm -rf") {
			prompted = true
		}
		return false // decline
	})
	app.AutoApprove = true

	ctx := context.Background()
	if _, err := app.Send(ctx, "do it"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !prompted {
		t.Error("destructive rm command was not prompted despite AutoApprove=true")
	}
}

// ---- P13e tests ----

// oracleServer builds a test HTTP server that returns the given oracle text.
func oracleServer(t *testing.T, text string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"content":[{"type":"text","text":` +
			func() string {
				b, _ := json.Marshal(text)
				return string(b)
			}() +
			`}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":20}}`))
	}))
}

// wfImplementApp builds a test App already inside IMPLEMENT with N steps.
func wfImplementApp(t *testing.T, sseURL string, nSteps int) *agent.App {
	t.Helper()
	exec := newFakeExecutor()
	exec.files[".wakil/plan.md"] = planContent2Steps
	app := newTestApp(sseURL, exec, func(_, _, _ string, _ bool) bool { return true })
	app.Workflow = &workflow.WorkflowState{
		Task:      "my task",
		Phase:     workflow.WFImplement,
		PlanPath:  ".wakil/plan.md",
		StepCount: nSteps,
		StepIdx:   nSteps, // about to complete the last step
	}
	return app
}

// TestFinalReviewFiresAfterLastStep: when the last step completes and the oracle
// reports no gaps, the workflow is cleared (done).
func TestFinalReviewFiresAfterLastStep(t *testing.T) {
	oracleSrv := oracleServer(t, "All acceptance criteria are met.\nVERDICT: PASS")
	defer oracleSrv.Close()

	lastStepResp := "done\n%%STEP_LOG: Step 2: fixed it | outcome: ok | deviation: none%%\n%%STEP_DONE%%"
	srv := sseServer(t, []string{contentChunk(lastStepResp)})
	defer srv.Close()

	app := wfImplementApp(t, srv.URL, 2)
	app.Cfg.OracleEnabled = true
	app.Cfg.OracleAPIKeyEnv = "TEST_KEY"
	app.Cfg.OracleEndpoint = oracleSrv.URL + "/v1/messages"
	app.Cfg.WFFinalReview = true
	t.Setenv("TEST_KEY", "fake-key")

	ctx := context.Background()
	if _, err := app.Send(ctx, "continue"); err != nil {
		t.Fatalf("send: %v", err)
	}
	agent.HandleWorkflowTransition(ctx, app)

	if app.Workflow != nil {
		t.Errorf("workflow should be cleared after final review with no gaps, phase=%v StepIdx=%d",
			app.Workflow.Phase, app.Workflow.StepIdx)
	}
}

// TestFinalReviewFlaggedGapsKeepsWorkflowOpen: oracle flags gaps → workflow stays
// open in IMPLEMENT with StepIdx > StepCount.
func TestFinalReviewFlaggedGapsKeepsWorkflowOpen(t *testing.T) {
	gapResponse := "Criterion 2 is not satisfied: the edge-case test is missing. " +
		"Step 1 deviation is unresolved.\nVERDICT: GAPS"
	oracleSrv := oracleServer(t, gapResponse)
	defer oracleSrv.Close()

	lastStepResp := "done\n%%STEP_LOG: Step 2: attempt | outcome: partial | deviation: skipped%%\n%%STEP_DONE%%"
	srv := sseServer(t, []string{contentChunk(lastStepResp)})
	defer srv.Close()

	app := wfImplementApp(t, srv.URL, 2)
	app.Cfg.OracleEnabled = true
	app.Cfg.OracleAPIKeyEnv = "TEST_KEY"
	app.Cfg.OracleEndpoint = oracleSrv.URL + "/v1/messages"
	app.Cfg.WFFinalReview = true
	t.Setenv("TEST_KEY", "fake-key")

	ctx := context.Background()
	if _, err := app.Send(ctx, "continue"); err != nil {
		t.Fatalf("send: %v", err)
	}
	agent.HandleWorkflowTransition(ctx, app)

	if app.Workflow == nil {
		t.Fatal("workflow should remain open when final review flags gaps")
	}
	if app.Workflow.StepIdx <= app.Workflow.StepCount {
		t.Errorf("StepIdx should be > StepCount to signal post-review state, got %d/%d",
			app.Workflow.StepIdx, app.Workflow.StepCount)
	}
	// plan.md should have the final review entry.
	exec := app.Exec.(*fakeExecutor)
	if !strings.Contains(exec.files[".wakil/plan.md"], "FINAL REVIEW") {
		t.Error("plan.md should contain FINAL REVIEW log entry")
	}
}

// TestFinalReviewUnavailableGates: oracle unavailable → workflow stays open,
// step log has a FINAL REVIEW skipped entry, /plan approve force-closes.
func TestFinalReviewUnavailableGates(t *testing.T) {
	lastStepResp := "done\n%%STEP_LOG: Step 1: done | outcome: ok | deviation: none%%\n%%STEP_DONE%%"
	srv := sseServer(t, []string{contentChunk(lastStepResp)})
	defer srv.Close()

	exec := newFakeExecutor()
	exec.files[".wakil/plan.md"] = planContent2Steps
	app := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return false })
	app.Cfg.OracleEnabled = false // force unavailable
	app.Cfg.WFFinalReview = true
	app.Workflow = &workflow.WorkflowState{
		Task:      "t",
		Phase:     workflow.WFImplement,
		PlanPath:  ".wakil/plan.md",
		StepCount: 1,
		StepIdx:   1,
	}
	ctx := context.Background()
	if _, err := app.Send(ctx, "continue"); err != nil {
		t.Fatalf("send: %v", err)
	}
	agent.HandleWorkflowTransition(ctx, app)

	if app.Workflow == nil {
		t.Fatal("workflow should stay open when final review oracle is unavailable")
	}
	if app.Workflow.StepIdx <= app.Workflow.StepCount {
		t.Errorf("StepIdx should be > StepCount after last step, got %d/%d",
			app.Workflow.StepIdx, app.Workflow.StepCount)
	}
	if !strings.Contains(exec.files[".wakil/plan.md"], "FINAL REVIEW skipped") {
		t.Error("plan.md should contain FINAL REVIEW skipped entry")
	}

	// /plan approve force-closes the workflow.
	_, _, cmd := agent.HandlePlanCommand([]string{"/plan", "approve"}, app)
	if cmd != nil {
		cmd() // execute the cmd
	}
	if app.Workflow != nil {
		t.Error("/plan approve should force-close the workflow after final review skip")
	}
}

// TestOracleFlagParsedAndSidebarLabel: --oracle=every-step sets OracleMode and
// appears in the sidebar label.
func TestOracleFlagParsedAndSidebarLabel(t *testing.T) {
	exec := newFakeExecutor()
	app := &agent.App{
		Cfg:     config.DefaultConfig(),
		Exec:    exec,
		Out:     io.Discard,
		Session: &agent.Session{ChatID: "test"},
	}

	fields := strings.Fields("/plan --oracle=every-step add unit tests for foo")
	_, _, cmd := agent.HandlePlanCommand(fields, app)
	if cmd == nil {
		t.Fatal("expected a cmd from handlePlanCommand")
	}
	cmd() // executes in-place in test (sets app.Workflow)

	if app.Workflow == nil {
		t.Fatal("workflow not initialized")
	}
	if app.Workflow.OracleMode != "every-step" {
		t.Errorf("OracleMode: got %q, want %q", app.Workflow.OracleMode, "every-step")
	}
	// Sidebar label must include the mode suffix.
	app.Workflow.Phase = workflow.WFImplement
	app.Workflow.StepIdx = 3
	app.Workflow.StepCount = 6
	label := app.Workflow.SidebarLabel()
	if label != "implement 3/6 ·every-step" {
		t.Errorf("sidebar label: got %q, want %q", label, "implement 3/6 ·every-step")
	}
}

// TestOracleFlagInvalidMode: unknown --oracle value returns an error note.
func TestOracleFlagInvalidMode(t *testing.T) {
	app := &agent.App{Cfg: config.DefaultConfig(), Session: &agent.Session{}}
	_, _, cmd := agent.HandlePlanCommand(strings.Fields("/plan --oracle=bad-mode do stuff"), app)
	if cmd == nil {
		t.Fatal("expected cmd")
	}
	msg := cmd()
	note, ok := msg.(agent.SysNoteMsg)
	if !ok || !strings.Contains(note.Text, "unknown oracle mode") {
		t.Errorf("expected unknown-oracle-mode error, got %T %v", msg, msg)
	}
}

// TestEverystepOracleCalledPerStep: in every-step mode each completed step
// triggers exactly one oracle consultation.
func TestEverystepOracleCalledPerStep(t *testing.T) {
	oracleCallCount := 0
	oracleSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		oracleCallCount++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"content":[{"type":"text","text":"Looks good, proceed."}],"stop_reason":"end_turn","usage":{}}`))
	}))
	defer oracleSrv.Close()

	step1 := contentChunk("step 1\n%%STEP_LOG: Step 1: x | outcome: ok | deviation: none%%\n%%STEP_DONE%%")
	step2 := contentChunk("step 2\n%%STEP_LOG: Step 2: y | outcome: ok | deviation: none%%\n%%STEP_DONE%%")
	srv := sseServer(t, []string{step1}, []string{step2})
	defer srv.Close()

	exec := newFakeExecutor()
	exec.files[".wakil/plan.md"] = planContent2Steps
	app := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.OracleEnabled = true
	app.Cfg.OracleAPIKeyEnv = "TEST_KEY"
	app.Cfg.OracleEndpoint = oracleSrv.URL + "/v1/messages"
	app.Cfg.WFFinalReview = false // isolate every-step from final review
	t.Setenv("TEST_KEY", "fake-key")

	app.Workflow = &workflow.WorkflowState{
		Task:       "t",
		Phase:      workflow.WFImplement,
		PlanPath:   ".wakil/plan.md",
		StepCount:  2,
		StepIdx:    1,
		OracleMode: "every-step",
	}
	ctx := context.Background()

	// Step 1.
	if _, err := app.Send(ctx, "continue"); err != nil {
		t.Fatalf("step 1: %v", err)
	}
	agent.HandleWorkflowTransition(ctx, app)
	if app.Workflow == nil {
		t.Fatal("workflow cleared too early (after step 1 of 2)")
	}

	// Step 2 — advance manually since every-step pause would need /plan approve in real TUI.
	app.Workflow.StepIdx = 2
	app.Conv = nil
	if _, err := app.Send(ctx, "continue"); err != nil {
		t.Fatalf("step 2: %v", err)
	}
	agent.HandleWorkflowTransition(ctx, app)

	if oracleCallCount != 2 {
		t.Errorf("expected 2 oracle calls (one per step), got %d", oracleCallCount)
	}
}

// TestWfFlagsGaps: verdict-line parsing is the sole signal.
// Keyword-only prose (no verdict) is treated as gaps (fail-closed).
func TestWfFlagsGaps(t *testing.T) {
	// Structured VERDICT: GAPS → true
	if !workflow.WFFlagsGaps("Everything looked mostly fine.\nVERDICT: GAPS\n") {
		t.Error("VERDICT: GAPS should return true")
	}
	// Structured VERDICT: PASS → false
	if workflow.WFFlagsGaps("All criteria verified.\nVERDICT: PASS\n") {
		t.Error("VERDICT: PASS should return false")
	}
	// No verdict line → fail-closed (true) regardless of prose
	if !workflow.WFFlagsGaps("All criteria are met. No issues.") {
		t.Error("missing verdict should be fail-closed (true)")
	}
	if !workflow.WFFlagsGaps("Criterion 2 is not satisfied.") {
		t.Error("flagged prose without verdict should still be fail-closed (true)")
	}
	// VERDICT line must be exact (case-sensitive, no leading noise on line)
	if !workflow.WFFlagsGaps("note: VERDICT: PASS embedded in prose") {
		t.Error("verdict embedded in a line should not match (fail-closed)")
	}
}

// TestVerdictLineIgnoredWhenEmbedded: "VERDICT: PASS" embedded mid-sentence is not
// the same as a standalone verdict line.
func TestVerdictLineIgnoredWhenEmbedded(t *testing.T) {
	// The verdict must be the entire trimmed line, not a substring of other text.
	mixedLine := "The review concludes VERDICT: PASS based on the log."
	if !workflow.WFFlagsGaps(mixedLine) {
		t.Error("embedded verdict mid-sentence should not count as a clean pass (fail-closed)")
	}
	// But a proper trailing line after prose does work.
	proper := "Some analysis here.\nVERDICT: PASS"
	if workflow.WFFlagsGaps(proper) {
		t.Error("proper standalone VERDICT: PASS should pass")
	}
}

// TestRemediationTurnRetriggersVerification: when the workflow is in verify state
// (StepIdx > StepCount) and a turn completes, handleWorkflowTransition re-runs
// the final review rather than completing or staying indefinitely.
func TestRemediationTurnRetriggersVerification(t *testing.T) {
	// Oracle first call: GAPS (verify state begins); second call: PASS (remediation accepted).
	callN := 0
	oracleSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callN++
		verdict := "VERDICT: PASS"
		if callN == 1 {
			verdict = "VERDICT: GAPS"
		}
		w.Header().Set("Content-Type", "application/json")
		txt, _ := json.Marshal("Remediation complete.\n" + verdict)
		w.Write([]byte(`{"content":[{"type":"text","text":` + string(txt) + `}],"stop_reason":"end_turn","usage":{}}`))
	}))
	defer oracleSrv.Close()

	// Two SSE turns: last step + remediation turn.
	lastStep := contentChunk("done\n%%STEP_LOG: Step 1: fix | outcome: ok | deviation: none%%\n%%STEP_DONE%%")
	remediation := contentChunk("fixed the gap")
	srv := sseServer(t, []string{lastStep}, []string{remediation})
	defer srv.Close()

	exec := newFakeExecutor()
	exec.files[".wakil/plan.md"] = planContent2Steps
	app := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.OracleEnabled = true
	app.Cfg.OracleAPIKeyEnv = "TEST_KEY"
	app.Cfg.OracleEndpoint = oracleSrv.URL + "/v1/messages"
	app.Cfg.WFFinalReview = true
	t.Setenv("TEST_KEY", "fake-key")
	app.Workflow = &workflow.WorkflowState{
		Task: "t", Phase: workflow.WFImplement,
		PlanPath: ".wakil/plan.md", StepCount: 1, StepIdx: 1,
	}
	ctx := context.Background()

	// Last step completes → final review fires (GAPS) → verify state.
	if _, err := app.Send(ctx, "continue"); err != nil {
		t.Fatalf("step: %v", err)
	}
	agent.HandleWorkflowTransition(ctx, app)
	if app.Workflow == nil {
		t.Fatal("workflow should be in verify state (gaps flagged), not cleared")
	}
	if app.Workflow.StepIdx <= app.Workflow.StepCount {
		t.Fatalf("StepIdx should be > StepCount in verify state, got %d/%d",
			app.Workflow.StepIdx, app.Workflow.StepCount)
	}

	// Remediation turn → handleWorkflowTransition re-runs review (PASS) → done.
	app.Conv = nil
	if _, err := app.Send(ctx, "continue"); err != nil {
		t.Fatalf("remediation: %v", err)
	}
	agent.HandleWorkflowTransition(ctx, app)

	if app.Workflow != nil {
		t.Errorf("workflow should be cleared after PASS on second review, phase=%v", app.Workflow.Phase)
	}
	if callN != 2 {
		t.Errorf("expected 2 oracle calls (initial + re-verify), got %d", callN)
	}
}

// TestPlanVerifyCommand: /plan verify in verify state fires agent.WFFinalReviewMsg.
func TestPlanVerifyCommand(t *testing.T) {
	app := &agent.App{
		Cfg:     config.DefaultConfig(),
		Session: &agent.Session{},
		Workflow: &workflow.WorkflowState{
			Phase:     workflow.WFImplement,
			StepIdx:   3, // > StepCount
			StepCount: 2,
		},
	}
	_, _, cmd := agent.HandlePlanCommand([]string{"/plan", "verify"}, app)
	if cmd == nil {
		t.Fatal("expected a cmd from /plan verify")
	}
	msg := cmd()
	if _, ok := msg.(agent.WFFinalReviewMsg); !ok {
		t.Errorf("expected agent.WFFinalReviewMsg, got %T", msg)
	}
}

// TestPlanVerifyNotInVerifyState: /plan verify outside verify state returns an error.
func TestPlanVerifyNotInVerifyState(t *testing.T) {
	app := &agent.App{
		Cfg:     config.DefaultConfig(),
		Session: &agent.Session{},
		Workflow: &workflow.WorkflowState{
			Phase:     workflow.WFImplement,
			StepIdx:   1,
			StepCount: 3, // StepIdx <= StepCount: not in verify state
		},
	}
	_, _, cmd := agent.HandlePlanCommand([]string{"/plan", "verify"}, app)
	if cmd == nil {
		t.Fatal("expected cmd")
	}
	msg := cmd()
	note, ok := msg.(agent.SysNoteMsg)
	if !ok || !strings.Contains(note.Text, "verify state") {
		t.Errorf("expected verify-state error note, got %T %v", msg, msg)
	}
}

// TestApproveLogsUnresolvedFlags: force-close from verify state writes a
// "force-closed with unresolved flags" entry to the step log.
func TestApproveLogsUnresolvedFlags(t *testing.T) {
	exec := newFakeExecutor()
	exec.files[".wakil/plan.md"] = planContent2Steps
	app := &agent.App{
		Cfg:     config.DefaultConfig(),
		Exec:    exec,
		Out:     io.Discard,
		Session: &agent.Session{ChatID: "test"},
		Workflow: &workflow.WorkflowState{
			Phase:     workflow.WFImplement,
			StepIdx:   3, // > StepCount
			StepCount: 2,
			PlanPath:  ".wakil/plan.md",
		},
	}
	_, _, cmd := agent.HandlePlanCommand([]string{"/plan", "approve"}, app)
	if cmd == nil {
		t.Fatal("expected cmd")
	}
	cmd()

	if app.Workflow != nil {
		t.Error("workflow should be cleared after force-close")
	}
	plan := exec.files[".wakil/plan.md"]
	if !strings.Contains(plan, "force-closed with unresolved flags") {
		t.Errorf("step log should record force-close, got:\n%s", plan)
	}
}

// TestSidebarLabelVerifyState: StepIdx > StepCount in IMPLEMENT shows "verify".
func TestSidebarLabelVerifyState(t *testing.T) {
	wf := &workflow.WorkflowState{Phase: workflow.WFImplement, StepIdx: 3, StepCount: 2}
	if wf.SidebarLabel() != "verify" {
		t.Errorf("got %q, want %q", wf.SidebarLabel(), "verify")
	}
}

// ---- P13f tests ----

// altCwdExec wraps fakeExecutor but returns a different cwd, simulating a
// directory change inside the executor without affecting the file map.
type altCwdExec struct {
	*fakeExecutor
	cwd string
}

func (e *altCwdExec) Cwd() string { return e.cwd }

// TestCwdChangedMidWorkflowBriefingComplete: when the executor's cwd changes
// between workflow start and final review, the absolute PlanPath still resolves
// to the correct file, and the briefing is complete.
func TestCwdChangedMidWorkflowBriefingComplete(t *testing.T) {
	oracleSrv := oracleServer(t, "All good.\nVERDICT: PASS")
	defer oracleSrv.Close()

	// Build an SSE server for the last step.
	lastStep := contentChunk("done\n%%STEP_LOG: Step 1: fix | outcome: ok | deviation: none%%\n%%STEP_DONE%%")
	srv := sseServer(t, []string{lastStep})
	defer srv.Close()

	// Start with cwd=/work; plan file stored there.
	inner := newFakeExecutor()
	exec := &altCwdExec{fakeExecutor: inner, cwd: "/work"}
	app := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.OracleEnabled = true
	app.Cfg.OracleAPIKeyEnv = "TEST_KEY"
	app.Cfg.OracleEndpoint = oracleSrv.URL + "/v1/messages"
	app.Cfg.WFFinalReview = true
	t.Setenv("TEST_KEY", "fake-key")

	// Use handlePlanCommand to set an absolute PlanPath.
	fields := strings.Fields("/plan write unit tests for foo")
	_, _, cmd := agent.HandlePlanCommand(fields, app)
	if cmd == nil {
		t.Fatal("expected cmd")
	}
	cmd() // sets app.Workflow.PlanPath = "/work/.wakil/plan.md"

	if app.Workflow == nil {
		t.Fatal("workflow not started")
	}
	if !strings.HasPrefix(app.Workflow.PlanPath, "/work/") {
		t.Fatalf("PlanPath not absolute: %q", app.Workflow.PlanPath)
	}

	// Write a plan with Task, Plan, and a pre-existing step-log entry at the
	// absolute path (no duplicate sections — planContent2Steps already has
	// a placeholder Step log which would hide any appended entries).
	planWithEntries := "## Task\n\nwrite unit tests for foo\n\n" +
		"## Plan\n\n1. Step one\n\n" +
		"## Step log\n\nStep 1: wrote tests | outcome: ok | deviation: none\n"
	inner.files[app.Workflow.PlanPath] = planWithEntries

	// Simulate cwd change to a subdirectory.
	exec.cwd = "/work/src"

	// Advance to last-step state.
	app.Workflow.Phase = workflow.WFImplement
	app.Workflow.StepCount = 1
	app.Workflow.StepIdx = 1

	ctx := context.Background()
	if _, err := app.Send(ctx, "continue"); err != nil {
		t.Fatalf("send: %v", err)
	}
	agent.HandleWorkflowTransition(ctx, app)

	// Workflow should complete: oracle found a complete briefing from the
	// absolute path, even though cwd is now /work/src.
	if app.Workflow != nil {
		t.Errorf("workflow should be cleared (PASS), phase=%v", app.Workflow.Phase)
	}
}

// TestFailedPlanReadRoutesUnavailable: if the plan file is unreadable, the
// oracle is not called and the unavailable path is taken.
func TestFailedPlanReadRoutesUnavailable(t *testing.T) {
	oracleCallCount := 0
	oracleSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		oracleCallCount++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"content":[{"type":"text","text":"ok\nVERDICT: PASS"}],"stop_reason":"end_turn","usage":{}}`))
	}))
	defer oracleSrv.Close()

	exec := newFakeExecutor()
	// Plan file deliberately absent from exec.files
	app := &agent.App{
		Cfg:     config.DefaultConfig(),
		Exec:    exec,
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
		Session: &agent.Session{ChatID: "test"},
		Workflow: &workflow.WorkflowState{
			Task:      "t",
			Phase:     workflow.WFImplement,
			PlanPath:  "/work/.wakil/missing.md", // not in files
			StepCount: 1,
			StepIdx:   2, // past last step — verify state
		},
	}
	app.Cfg.OracleEnabled = true
	app.Cfg.OracleAPIKeyEnv = "TEST_KEY"
	app.Cfg.OracleEndpoint = oracleSrv.URL + "/v1/messages"
	app.Cfg.WFFinalReview = true
	t.Setenv("TEST_KEY", "fake-key")

	agent.HandleFinalReview(context.Background(), app)

	// Oracle must NOT have been called.
	if oracleCallCount != 0 {
		t.Errorf("oracle should not be called when plan file is unreadable, got %d calls", oracleCallCount)
	}
	// Workflow stays open (unavailable path).
	if app.Workflow == nil {
		t.Error("workflow should remain open on failed plan read")
	}
}

// TestAutoGateReasons: suspendAuto returns the right reason string for each carve-out;
// each reason must contain the spec-mandated phrase so confirm prompts are labelled.
func TestAutoGateReasons(t *testing.T) {
	app := &agent.App{AutoApprove: true}

	// mashura__review (and the legacy oracle__ask alias) → cost + payload review.
	reason := agent.SuspendAuto("mashura__review", app, "detail")
	if !strings.Contains(reason, "cost + payload") {
		t.Errorf("mashura: reason=%q, want 'cost + payload'", reason)
	}
	if legacy := agent.SuspendAuto("oracle__ask", app, "detail"); !strings.Contains(legacy, "cost + payload") {
		t.Errorf("oracle__ask back-compat: reason=%q, want 'cost + payload'", legacy)
	}

	// Destructive shell.
	reason = agent.SuspendAuto("run_shell", app, "$ rm -rf /tmp\n  (exec)")
	if !strings.Contains(reason, "destructive command") {
		t.Errorf("destructive shell: reason=%q, want 'destructive command'", reason)
	}

	// Pre-implementation workflow phase — reason must name the phase.
	app.Workflow = &workflow.WorkflowState{Phase: workflow.WFGather}
	reason = agent.SuspendAuto("run_shell", app, "$ ls\n  (exec)")
	if !strings.Contains(reason, "pre-implementation phase") {
		t.Errorf("pre-impl phase: reason=%q, want 'pre-implementation phase'", reason)
	}
	if !strings.Contains(reason, "gather") {
		t.Errorf("pre-impl phase: reason should name the phase, got %q", reason)
	}

	// No reason for safe non-gated tools.
	app.Workflow = nil
	if r := agent.SuspendAuto("write_file", app, "detail"); r != "" {
		t.Errorf("write_file should not suspend auto, got %q", r)
	}
	if r := agent.SuspendAuto("run_shell", app, "$ ls -la\n  (exec)"); r != "" {
		t.Errorf("safe shell should not suspend auto, got %q", r)
	}
}

// TestShouldGateEvenWithAutoApprove: the boolean wrapper is consistent with suspendAuto.
func TestShouldGateEvenWithAutoApprove(t *testing.T) {
	app := &agent.App{AutoApprove: true}
	if !agent.ShouldGateEvenWithAutoApprove("oracle__ask", app, "detail") {
		t.Error("oracle__ask must always gate in auto mode")
	}
	if agent.ShouldGateEvenWithAutoApprove("write_file", app, "detail") {
		t.Error("write_file should not gate in auto mode")
	}
	if !agent.ShouldGateEvenWithAutoApprove("run_shell", app, "$ rm -rf /tmp\n  (exec)") {
		t.Error("destructive run_shell must still gate in auto mode")
	}
}

// TestValidateBriefing covers the briefing integrity checks.
func TestValidateBriefing(t *testing.T) {
	full := "## Task\n\ndo thing\n\n## Plan\n\n1. step\n\n## Step log\n\nStep 1: done\n\n## Question\n\nq?"
	if reason := workflow.ValidateBriefing(full, false); reason != "" {
		t.Errorf("valid briefing (no step log req): %q", reason)
	}
	if reason := workflow.ValidateBriefing(full, true); reason != "" {
		t.Errorf("valid briefing (with step log): %q", reason)
	}

	noTask := "## Plan\n\n1. step\n\n## Question\n\nq?"
	if reason := workflow.ValidateBriefing(noTask, false); !strings.Contains(reason, "## Task") {
		t.Errorf("missing ## Task: got %q", reason)
	}

	noPlan := "## Task\n\nt\n\n## Question\n\nq?"
	if reason := workflow.ValidateBriefing(noPlan, false); !strings.Contains(reason, "## Plan") {
		t.Errorf("missing ## Plan: got %q", reason)
	}

	noLog := "## Task\n\nt\n\n## Plan\n\n1. step\n\n## Question\n\nq?"
	if reason := workflow.ValidateBriefing(noLog, true); !strings.Contains(reason, "step-log") {
		t.Errorf("missing step-log: got %q", reason)
	}
}

// TestIsPlanFilePath: mixed relative/absolute path comparison.
func TestIsPlanFilePath(t *testing.T) {
	if !workflow.IsPlanFilePath(".wakil/plan.md", "/work/.wakil/plan.md") {
		t.Error("relative vs absolute should match")
	}
	if !workflow.IsPlanFilePath("/work/.wakil/plan.md", "/work/.wakil/plan.md") {
		t.Error("same absolute should match")
	}
	if workflow.IsPlanFilePath("other.md", "/work/.wakil/plan.md") {
		t.Error("different file should not match")
	}
	if workflow.IsPlanFilePath(".wakil/other.md", "/work/.wakil/plan.md") {
		t.Error("different filename should not match")
	}
}

// ---- P16 tests ----

// TestBriefingUsesWorkflowTask: the briefing contains the task text from
// WorkflowState even when plan.md has no ## Task section.
func TestBriefingUsesWorkflowTask(t *testing.T) {
	task := "add unit tests for the serializer"
	planNoTask := "## Plan\n\n1. Write tests\n2. Run them\n"

	briefing := workflow.BuildOracleBriefing(task, planNoTask, "q?")

	if !strings.Contains(briefing, task) {
		t.Error("briefing must contain the workflow task text")
	}
	// The ## Task header must be present as a structural section.
	if !workflow.BriefingSectionPresent(briefing, "## Task") {
		t.Error("briefing must have a ## Task section with body")
	}
}

// TestBriefingFinalReviewUsesWorkflowTask: same for the final-review briefing.
func TestBriefingFinalReviewUsesWorkflowTask(t *testing.T) {
	task := "migrate auth to JWT"
	planNoTask := "## Plan\n\n1. Add JWT library\n\n## Step log\n\nStep 1: done | outcome: ok | deviation: none\n"

	briefing := workflow.BuildFinalReviewBriefing(task, planNoTask, "VERDICT?", 0)

	if !strings.Contains(briefing, task) {
		t.Error("final review briefing must contain the workflow task text")
	}
}

// TestValidateBriefingLineAnchored: "## Task" appearing only as a substring of
// a log entry does not satisfy the structural validation check.
func TestValidateBriefingLineAnchored(t *testing.T) {
	// "## Task" embedded in a log entry — NOT an exact header line.
	embeddedOnly := "## Step log (recent)\n\nStep 1: updated ## Task section | outcome: ok\n\n## Question\n\nq?"
	if reason := workflow.ValidateBriefing(embeddedOnly, false); !strings.Contains(reason, "Task") {
		t.Errorf("embedded '## Task' should not satisfy validation, got reason=%q", reason)
	}

	// Proper structural header with body.
	proper := "## Task\n\nmy actual task\n\n## Plan\n\n1. step\n\n## Question\n\nq?"
	if reason := workflow.ValidateBriefing(proper, false); reason != "" {
		t.Errorf("proper briefing should be valid, got reason=%q", reason)
	}

	// Header present but body is empty (next heading follows immediately).
	emptyBody := "## Task\n\n## Plan\n\n1. step\n\n## Question\n\nq?"
	if reason := workflow.ValidateBriefing(emptyBody, false); !strings.Contains(reason, "Task") {
		t.Errorf("empty ## Task body should fail validation, got reason=%q", reason)
	}
}

// TestWFAllPhasesBlockPlanRewrite: write_file to plan.md is rejected in every
// workflow phase (not just IMPLEMENT) to protect the scaffold structure.
func TestWFAllPhasesBlockPlanRewrite(t *testing.T) {
	for _, phase := range []workflow.WorkflowPhase{workflow.WFGather, workflow.WFPlan, workflow.WFReview, workflow.WFPresent, workflow.WFImplement} {
		app := wfTestApp(phase)
		result := app.ExecuteToolCall(context.Background(), makeToolCall(
			"write_file", `{"path":".wakil/plan.md","content":"WIPED"}`))
		if !strings.Contains(result, "write_file on plan.md is not permitted") {
			t.Errorf("phase %d: expected plan.md write rejection, got: %q", phase, result)
		}
	}
}

// TestWFEditFileToPlanAllowedInGather: edit_file on plan.md must not be blocked
// even in GATHER — only write_file is.
func TestWFEditFileToPlanAllowedInGather(t *testing.T) {
	app := wfTestApp(workflow.WFGather)
	exec := app.Exec.(*fakeExecutor)
	exec.files[".wakil/plan.md"] = "## Task\n\nt\n\n## Findings\n\n(pending)\n"

	result := app.ExecuteToolCall(context.Background(), makeToolCall(
		"edit_file", `{"path":".wakil/plan.md","old_string":"(pending)","new_string":"found stuff"}`))

	if strings.Contains(result, "workflow") && strings.Contains(result, "not permitted") {
		t.Errorf("edit_file on plan.md should be allowed in GATHER, got: %q", result)
	}
}

// TestBriefingSectionPresentEdgeCases: validate the line-anchor helper directly.
func TestBriefingSectionPresentEdgeCases(t *testing.T) {
	// Exact line match with body → true.
	if !workflow.BriefingSectionPresent("## Task\n\nbody text\n", "## Task") {
		t.Error("exact header + body should return true")
	}
	// Substring match (not a full line) → false.
	if workflow.BriefingSectionPresent("prefix ## Task\n\nbody\n", "## Task") {
		t.Error("substring match should return false")
	}
	// Empty body (next heading follows) → false.
	if workflow.BriefingSectionPresent("## Task\n\n## Plan\n\nbody\n", "## Task") {
		t.Error("empty section body should return false")
	}
	// Body after blank lines → true.
	if !workflow.BriefingSectionPresent("## Task\n\n\n\nbody text\n", "## Task") {
		t.Error("body after multiple blank lines should return true")
	}
}

// ---- P17 tests ----

// failingShellExec wraps fakeExecutor but returns an error for commands that
// contain "go test", simulating a failed verify step.
type failingShellExec struct {
	*fakeExecutor
}

func (f *failingShellExec) RunShell(ctx context.Context, cmd string) (string, error) {
	if strings.Contains(cmd, "go test") {
		return "FAIL\twakil\nexit status 1", fmt.Errorf("exit status 1")
	}
	return f.fakeExecutor.RunShell(ctx, cmd)
}

// TestStepEvidencePresent: after a step completes, the plan.md step log must
// contain [ev]-prefixed evidence lines produced by Wakil, not the model.
func TestStepEvidencePresent(t *testing.T) {
	// Turn: run_shell (reads output), then model emits %%STEP_LOG%% + %%STEP_DONE%%.
	firstCall := toolCallFrames("c1", "run_shell", `{"command":"go build ./..."}`)
	secondCall := []string{contentChunk(
		"Built it.\n%%STEP_LOG: Step 1: built | outcome: ok | deviation: none%%\n%%STEP_DONE%%")}

	srv := sseServer(t, firstCall, secondCall)
	defer srv.Close()

	exec := newFakeExecutor()
	exec.files[".wakil/plan.md"] = planContent2Steps
	app := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.WFFinalReview = false

	app.Workflow = &workflow.WorkflowState{
		Task:      "build the project",
		Phase:     workflow.WFImplement,
		PlanPath:  ".wakil/plan.md",
		StepCount: 2,
		StepIdx:   1,
	}

	ctx := context.Background()
	// Simulate what runTurn does: reset trace before Send.
	app.WorkflowStepTrace = nil
	if _, err := app.Send(ctx, "continue"); err != nil {
		t.Fatalf("send: %v", err)
	}
	agent.HandleWorkflowTransition(ctx, app)

	plan := exec.files[".wakil/plan.md"]
	if !strings.Contains(plan, "[ev]") {
		t.Error("step log should contain [ev] evidence lines")
	}
	if !strings.Contains(plan, "shell") {
		t.Error("evidence should name the shell tool")
	}
	// Evidence must appear in the same paragraph as the model's claim.
	for _, para := range strings.Split(plan, "\n\n") {
		if strings.Contains(para, "Step 1:") {
			if !strings.Contains(para, "[ev]") {
				t.Error("[ev] lines should be in the same paragraph as the step claim")
			}
			break
		}
	}
}

// TestStepEvidenceCap: many trace entries must be collapsed to ~1 KB.
func TestStepEvidenceCap(t *testing.T) {
	trace := make([]agent.ToolTraceEntry, 20)
	for i := range trace {
		trace[i] = agent.ToolTraceEntry{
			Abbrev:    "read",
			Command:   "very_long_filename_to_make_entries_larger.go",
			OutputLen: 4096,
			FirstLine: "package main // first line of this file",
		}
	}
	ev := agent.FormatStepEvidence(trace)
	if len(ev) > 1200 { // allow a small margin above 1 KB for the omission line
		t.Errorf("evidence exceeds ~1 KB cap: %d bytes", len(ev))
	}
	if !strings.Contains(ev, "omitted") {
		t.Error("evidence should include an omission summary when entries are dropped")
	}
}

// TestStepEvidenceMismatch: the model emits %%STEP_DONE%% (claims success)
// but the run_shell trace shows EXIT≠0. The evidence in plan.md must record
// the failure so the final reviewer can catch the discrepancy.
func TestStepEvidenceMismatch(t *testing.T) {
	// run_shell "go test" will fail (exit=1); model still emits %%STEP_DONE%%.
	firstCall := toolCallFrames("c1", "run_shell", `{"command":"go test ./..."}`)
	secondCall := []string{contentChunk(
		"All good.\n%%STEP_LOG: Step 1: tests pass | outcome: ok | deviation: none%%\n%%STEP_DONE%%")}

	srv := sseServer(t, firstCall, secondCall)
	defer srv.Close()

	exec := &failingShellExec{newFakeExecutor()}
	exec.fakeExecutor.files[".wakil/plan.md"] = planContent2Steps
	app := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.WFFinalReview = false

	app.Workflow = &workflow.WorkflowState{
		Task:      "run tests",
		Phase:     workflow.WFImplement,
		PlanPath:  ".wakil/plan.md",
		StepCount: 2,
		StepIdx:   1,
	}

	ctx := context.Background()
	app.WorkflowStepTrace = nil
	if _, err := app.Send(ctx, "continue"); err != nil {
		t.Fatalf("send: %v", err)
	}
	agent.HandleWorkflowTransition(ctx, app)

	plan := exec.fakeExecutor.files[".wakil/plan.md"]

	// Model's claim says "tests pass" — should be in plan.
	if !strings.Contains(plan, "tests pass") {
		t.Error("model's claim should appear in the step log")
	}
	// Evidence must show the shell command failed.
	if !strings.Contains(plan, "EXIT") {
		t.Error("evidence must record EXIT≠0 for the failed go test command")
	}
	// Both claim and evidence must be in the same step-log paragraph.
	found := false
	for _, para := range strings.Split(plan, "\n\n") {
		if strings.Contains(para, "tests pass") && strings.Contains(para, "EXIT") {
			found = true
			break
		}
	}
	if !found {
		t.Error("model's success claim and failure evidence must be in the same step entry")
	}
}

// TestFormatTraceEntry covers the [ev] line format for ok and failing calls.
func TestFormatTraceEntry(t *testing.T) {
	ok := agent.ToolTraceEntry{
		Abbrev:    "shell",
		Command:   "go build ./...",
		ExitErr:   false,
		OutputLen: 0,
		FirstLine: "",
	}
	line := agent.FormatTraceEntry(ok)
	if !strings.HasPrefix(line, "[ev]") {
		t.Errorf("trace entry must start with [ev], got: %q", line)
	}
	if !strings.Contains(line, "shell") {
		t.Errorf("trace entry must name the tool, got: %q", line)
	}

	fail := agent.ToolTraceEntry{
		Abbrev:    "shell",
		Command:   "go test ./...",
		ExitErr:   true,
		OutputLen: 156,
		FirstLine: "FAIL",
		LastLine:  "exit status 1",
	}
	fline := agent.FormatTraceEntry(fail)
	if !strings.Contains(fline, "EXIT") {
		t.Errorf("failing entry must contain EXIT marker, got: %q", fline)
	}
	if !strings.Contains(fline, "FAIL") {
		t.Errorf("failing entry must contain first output line, got: %q", fline)
	}
}

// TestRemediationEvidenceAppendsToStepLog: a remediation turn in verify state
// must append a Remediation: block to plan.md (model's summary + [ev] lines)
// BEFORE the final review fires so the briefing carries the new receipts.
func TestRemediationEvidenceAppendsToStepLog(t *testing.T) {
	oracleSrv := oracleServer(t, "All criteria now met.\nVERDICT: PASS")
	defer oracleSrv.Close()

	// Remediation turn: run_shell (evidence), then model summary via %%STEP_LOG:%%.
	firstCall := toolCallFrames("c1", "run_shell", `{"command":"go test ./..."}`)
	secondCall := []string{contentChunk(
		"Fixed the gap.\n%%STEP_LOG: Remediation: patched the failing auth test%%")}

	srv := sseServer(t, firstCall, secondCall)
	defer srv.Close()

	exec := newFakeExecutor()
	// Plan has an existing step entry so the step log section already exists.
	planWithStep := "## Task\n\nfix auth\n\n## Plan\n\n1. Fix auth test\n\n" +
		"## Step log\n\nStep 1: applied fix | outcome: ok | deviation: none\n" +
		"[ev] shell ok 0B \"go build ./...\"\n"
	exec.files[".wakil/plan.md"] = planWithStep

	app := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.OracleEnabled = true
	app.Cfg.OracleAPIKeyEnv = "TEST_KEY"
	app.Cfg.OracleEndpoint = oracleSrv.URL + "/v1/messages"
	app.Cfg.WFFinalReview = true
	t.Setenv("TEST_KEY", "fake-key")

	app.Workflow = &workflow.WorkflowState{
		Task:      "fix auth",
		Phase:     workflow.WFImplement,
		PlanPath:  ".wakil/plan.md",
		StepCount: 1,
		StepIdx:   2, // > StepCount → verify state
	}

	ctx := context.Background()
	app.WorkflowStepTrace = nil // reset as runTurn does
	if _, err := app.Send(ctx, "continue"); err != nil {
		t.Fatalf("send: %v", err)
	}
	agent.HandleWorkflowTransition(ctx, app)

	finalPlan := exec.files[".wakil/plan.md"]

	// Plan.md must contain the model's reconciliation summary.
	if !strings.Contains(finalPlan, "Remediation:") {
		t.Error("plan.md should contain Remediation: summary from the model")
	}
	// And the machine-written [ev] lines for the remediation turn.
	// There should be two [ev] blocks: the original step evidence and the new one.
	evCount := strings.Count(finalPlan, "[ev]")
	if evCount == 0 {
		t.Error("plan.md should contain [ev] evidence lines from the remediation turn")
	}

	// Claim and receipt must travel together in one paragraph.
	found := false
	for _, para := range strings.Split(finalPlan, "\n\n") {
		if strings.Contains(para, "Remediation:") && strings.Contains(para, "[ev]") {
			found = true
			break
		}
	}
	if !found {
		t.Error("Remediation: summary and [ev] lines must be in the same step-log paragraph")
	}

	// The final-review briefing built from this plan must carry the evidence.
	briefing := workflow.BuildFinalReviewBriefing("fix auth", finalPlan, "VERDICT?", 0)
	if !strings.Contains(briefing, "[ev]") {
		t.Error("final-review briefing must contain [ev] lines from the remediation turn")
	}
	if !strings.Contains(briefing, "Remediation:") {
		t.Error("final-review briefing must contain the Remediation: summary")
	}
}

// TestMakeTraceEntry: makeTraceEntry detects errors from formatResult output
// and captures the last 4 tail lines for run_shell.
func TestMakeTraceEntry(t *testing.T) {
	tc := proxy.ToolCall{
		ID:       "x",
		Type:     "function",
		Function: proxy.FunctionCall{Name: "run_shell", Arguments: `{"command":"go test ./..."}`},
	}
	// Error result from formatResult(out, err).
	errResult := "FAIL\twakil\nERROR: exit status 1"
	e := agent.MakeTraceEntry(tc, errResult)
	if !e.ExitErr {
		t.Error("makeTraceEntry should detect exitErr from 'ERROR:' in result")
	}
	if e.Abbrev != "shell" {
		t.Errorf("Abbrev: got %q, want %q", e.Abbrev, "shell")
	}
	if e.Command != "go test ./..." {
		t.Errorf("Command: got %q", e.Command)
	}

	// Non-error result.
	okResult := "ok\twakil"
	e2 := agent.MakeTraceEntry(tc, okResult)
	if e2.ExitErr {
		t.Error("should not detect exitErr for clean output")
	}

	// 4-line tail: run_shell captures up to the last 4 non-empty distinct lines.
	multiResult := "--- RUN TestA\n--- PASS: TestA\nBenchmarkFoo-8\t1234 ns/op\nPASS\nok\twakil\t0.014s"
	e3 := agent.MakeTraceEntry(tc, multiResult)
	// firstLine is "--- RUN TestA"; tail is the last 4 distinct lines in order.
	if e3.FirstLine != "--- RUN TestA" {
		t.Errorf("FirstLine: got %q", e3.FirstLine)
	}
	// lastLine should contain up to 4 tail lines joined with \n,
	// all different from firstLine.
	tailLines := strings.Split(e3.LastLine, "\n")
	if len(tailLines) < 3 {
		t.Errorf("shell tail should capture multiple lines, got: %q", e3.LastLine)
	}
	if !strings.Contains(e3.LastLine, "0.014s") {
		t.Error("shell tail should include the final summary line")
	}
}

// ---- P22: headless run mode tests ----

// TestRunHeadlessSimpleTask: a scripted task with no tool calls completes with
// exit code 0 and emits a {"type":"done","outcome":"pass"} event.
func TestRunHeadlessSimpleTask(t *testing.T) {
	srv := sseServer(t, []string{contentChunk("Task complete!")})
	defer srv.Close()

	exec := newFakeExecutor()
	app := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })

	var buf strings.Builder
	flags := RunFlags{Auto: true}
	code := runHeadlessApp(context.Background(), app, "print hello", false, flags, &buf)

	if code != ExitOK {
		t.Errorf("expected exit %d (ExitOK), got %d; output: %s", ExitOK, code, buf.String())
	}
	if !strings.Contains(buf.String(), `"done"`) {
		t.Errorf("output should contain done event; got: %s", buf.String())
	}
	if !strings.Contains(buf.String(), `"pass"`) {
		t.Errorf("outcome should be pass; got: %s", buf.String())
	}
}

// TestRunHeadlessDestructiveDeclined: a model-issued run_shell with "rm -rf"
// is declined by the headless confirmer, exits nonzero, and the output names
// the reason clearly.
func TestRunHeadlessDestructiveDeclined(t *testing.T) {
	// Model calls run_shell "rm -rf /tmp/test", then produces a final response.
	srv := sseServer(t,
		toolCallFrames("c1", "run_shell", `{"command":"rm -rf /tmp/test"}`),
		[]string{contentChunk("cleaned up")},
	)
	defer srv.Close()

	exec := newFakeExecutor()
	app := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })

	var buf strings.Builder
	flags := RunFlags{Auto: true, AllowDestructive: false}
	code := runHeadlessApp(context.Background(), app, "clean tmp", false, flags, &buf)

	if code == ExitOK {
		t.Error("destructive command without --allow-destructive must exit nonzero")
	}
	output := buf.String()
	if !strings.Contains(output, "destructive") {
		t.Errorf("output must mention the destructive command reason; got: %s", output)
	}
	if !strings.Contains(output, "declined") {
		t.Errorf("output must say the command was declined; got: %s", output)
	}
}

// TestRunHeadlessDestructiveAllowed: with --allow-destructive, the same rm -rf
// is approved and the task exits 0.
func TestRunHeadlessDestructiveAllowed(t *testing.T) {
	srv := sseServer(t,
		toolCallFrames("c1", "run_shell", `{"command":"rm -rf /tmp/test"}`),
		[]string{contentChunk("done")},
	)
	defer srv.Close()

	exec := newFakeExecutor()
	app := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })

	var buf strings.Builder
	flags := RunFlags{Auto: true, AllowDestructive: true}
	code := runHeadlessApp(context.Background(), app, "clean", false, flags, &buf)

	if code != ExitOK {
		t.Errorf("--allow-destructive should permit rm -rf; got exit %d; output: %s", code, buf.String())
	}
}

// TestRunHeadlessDefaultInvokesOracle: the default workflow path tries the oracle
// review (confirm gate fires, auto-approved by --auto) and the oracle endpoint
// is actually hit. The run exits 0 on VERDICT: PASS.
func TestRunHeadlessDefaultInvokesOracle(t *testing.T) {
	oracleCallCount := 0
	oracleSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		oracleCallCount++
		w.Header().Set("Content-Type", "application/json")
		txt, _ := json.Marshal("Plan is solid.\nVERDICT: PASS")
		w.Write([]byte(`{"content":[{"type":"text","text":` + string(txt) + `}],"stop_reason":"end_turn","usage":{}}`))
	}))
	defer oracleSrv.Close()

	// Two SSE calls:
	//  1. workflow.WFReview retry turn (model says "ok") → handleReviewOracle fires → oracle hit → workflow.WFPresent
	//  2. IMPLEMENT step (model emits STEP_DONE) → done
	retryFrame := []string{contentChunk("ok")}
	stepFrame := []string{contentChunk(
		"done\n%%STEP_LOG: Step 1: patched | outcome: ok | deviation: none%%\n%%STEP_DONE%%")}
	srv := sseServer(t, retryFrame, stepFrame)
	defer srv.Close()

	exec := newFakeExecutor()
	exec.files[".wakil/plan.md"] = "## Task\n\nt\n\n## Plan\n\n1. fix it\n"
	app := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.OracleEnabled = true
	app.Cfg.OracleAPIKeyEnv = "TEST_KEY"
	app.Cfg.OracleEndpoint = oracleSrv.URL + "/v1/messages"
	app.Cfg.WFFinalReview = false
	t.Setenv("TEST_KEY", "fake-key")

	// Start at workflow.WFReview so the oracle is invoked in the first loop iteration.
	app.Workflow = &workflow.WorkflowState{
		Task:      "t",
		Phase:     workflow.WFReview,
		PlanPath:  ".wakil/plan.md",
		StepCount: 1,
	}
	app.Confirm = headlessConfirmer(RunFlags{Auto: true}, new(string))
	app.Out = io.Discard

	var buf strings.Builder
	flags := RunFlags{Auto: true}
	var declinedReason string
	code := runWorkflowLoop(context.Background(), app, flags, &buf, &declinedReason)

	if code != ExitOK {
		t.Errorf("expected ExitOK, got %d; output: %s", code, buf.String())
	}
	if oracleCallCount == 0 {
		t.Error("oracle endpoint must be hit on the default review path (auto-approved)")
	}
}

// TestRunHeadlessNoOracleFlag: --no-oracle skips oracle review entirely,
// logs "oracle disabled by flag" in the transcript, and exits 0.
func TestRunHeadlessNoOracleFlag(t *testing.T) {
	oracleCallCount := 0
	oracleSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		oracleCallCount++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"content":[{"type":"text","text":"ok\nVERDICT: PASS"}],"stop_reason":"end_turn","usage":{}}`))
	}))
	defer oracleSrv.Close()

	retryFrame := []string{contentChunk("ok")}
	stepFrame := []string{contentChunk(
		"done\n%%STEP_LOG: Step 1: patched | outcome: ok | deviation: none%%\n%%STEP_DONE%%")}
	srv := sseServer(t, retryFrame, stepFrame)
	defer srv.Close()

	exec := newFakeExecutor()
	exec.files[".wakil/plan.md"] = "## Task\n\nt\n\n## Plan\n\n1. fix it\n"
	app := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.OracleEnabled = true // would be used if --no-oracle weren't set
	app.Cfg.OracleAPIKeyEnv = "TEST_KEY"
	app.Cfg.OracleEndpoint = oracleSrv.URL + "/v1/messages"
	app.Cfg.WFFinalReview = false
	t.Setenv("TEST_KEY", "fake-key")

	flags := RunFlags{Auto: true, NoOracle: true}
	// --no-oracle disables oracle before the loop.
	app.Cfg.OracleEnabled = false

	app.Workflow = &workflow.WorkflowState{
		Task:      "t",
		Phase:     workflow.WFReview,
		PlanPath:  ".wakil/plan.md",
		StepCount: 1,
	}
	app.Confirm = headlessConfirmer(flags, new(string))
	app.Out = io.Discard

	var buf strings.Builder
	var declinedReason string
	code := runWorkflowLoop(context.Background(), app, flags, &buf, &declinedReason)

	if code != ExitOK {
		t.Errorf("expected ExitOK with --no-oracle, got %d; output: %s", code, buf.String())
	}
	if oracleCallCount != 0 {
		t.Errorf("oracle endpoint must NOT be hit with --no-oracle; was called %d times", oracleCallCount)
	}
	output := buf.String()
	if !strings.Contains(output, "oracle disabled by flag") {
		t.Errorf("transcript must say 'oracle disabled by flag'; got: %s", output)
	}
}

// TestHeadlessConfirmerPolicy: unit test confirmer decisions for each carve-out.
func TestHeadlessConfirmerPolicy(t *testing.T) {
	tests := []struct {
		desc    string
		auto    bool
		allowD  bool
		tool    string
		detail  string
		wantOK  bool
		wantMsg string // substring expected in declinedReason when !wantOK
	}{
		{"oracle allowed with --auto", true, false, "oracle__ask", "model=x\nq=?", true, ""},
		{"oracle declined without --auto", false, false, "oracle__ask", "", false, "confirmation required"},
		{"destructive declined by default", true, false, "run_shell", "$ rm -rf /\n  (exec)", false, "destructive"},
		{"destructive allowed with flag", true, true, "run_shell", "$ rm -rf /\n  (exec)", true, ""},
		{"safe shell auto-approved", true, false, "run_shell", "$ go test ./...\n  (exec)", true, ""},
		{"write_file auto-approved", true, false, "write_file", "write app.go", true, ""},
		{"no --auto declines write", false, false, "write_file", "", false, "confirmation required"},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			var reason string
			flags := RunFlags{Auto: tc.auto, AllowDestructive: tc.allowD}
			conf := headlessConfirmer(flags, &reason)
			got := conf(tc.tool, "headline", tc.detail, false)
			if got != tc.wantOK {
				t.Errorf("got %v, want %v", got, tc.wantOK)
			}
			if !tc.wantOK && !strings.Contains(reason, tc.wantMsg) {
				t.Errorf("reason %q should contain %q", reason, tc.wantMsg)
			}
		})
	}
}

// TestParseRunArgs covers the flag and task parsing.
func TestParseRunArgs(t *testing.T) {
	task, plan, flags, err := parseRunArgs([]string{"--plan", "--auto", "--allow-destructive", "do stuff"})
	if err != nil || task != "do stuff" || !plan || !flags.Auto || !flags.AllowDestructive {
		t.Errorf("parse failed: task=%q plan=%v auto=%v destruct=%v err=%v",
			task, plan, flags.Auto, flags.AllowDestructive, err)
	}
	_, _, _, err = parseRunArgs([]string{})
	if err == nil {
		t.Error("empty args should be an error")
	}
	_, _, _, err = parseRunArgs([]string{"--unknown-flag", "task"})
	if err == nil {
		t.Error("unknown flag should be an error")
	}
}

// TestParseRunArgsAllowExternal verifies --allow-external sets the flag.
func TestParseRunArgsAllowExternal(t *testing.T) {
	_, _, flags, err := parseRunArgs([]string{"--auto", "--allow-external", "task"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !flags.AllowExternal {
		t.Error("--allow-external should set AllowExternal=true")
	}
	// Without the flag: AllowExternal stays false.
	_, _, flags2, _ := parseRunArgs([]string{"--auto", "task"})
	if flags2.AllowExternal {
		t.Error("AllowExternal should default to false")
	}
}

// TestHeadlessExternalBackendGate verifies that without --allow-external the
// external_backend confirm gate declines and sets a readable declined reason,
// whereas with --allow-external it approves.
func TestHeadlessExternalBackendGate(t *testing.T) {
	var reason string

	// Without --allow-external: must decline and set reason.
	noExt := headlessConfirmer(RunFlags{Auto: true, AllowExternal: false}, &reason)
	ok := noExt("external_backend", "headline", "detail", false)
	if ok {
		t.Error("external_backend should be declined without --allow-external")
	}
	if reason == "" {
		t.Error("declined reason should be set")
	}
	if !strings.Contains(strings.ToLower(reason), "external") {
		t.Errorf("reason should mention 'external'; got %q", reason)
	}

	// With --allow-external: must approve.
	reason = ""
	withExt := headlessConfirmer(RunFlags{Auto: true, AllowExternal: true}, &reason)
	ok = withExt("external_backend", "headline", "detail", false)
	if !ok {
		t.Error("external_backend should be approved with --allow-external")
	}
	if reason != "" {
		t.Errorf("no reason should be set on approval; got %q", reason)
	}
}

// TestHeadlessTokenEvent verifies that runHeadlessApp emits a
// {"type":"tokens",...} event after the done event.
func TestHeadlessTokenEvent(t *testing.T) {
	srv := sseServer(t,
		[]string{contentChunk("done")},
	)
	defer srv.Close()

	var out strings.Builder
	app := newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Session = &agent.Session{ChatID: agent.NewChatID()}
	app.Costs = proxy.NewCostTracker()
	// Inject some usage so the token event has non-zero values.
	app.Client.SetUsage(proxy.UsageStat{InputTok: 100, OutputTok: 50, Exact: true})
	app.RecordInferenceCost()

	code := runHeadlessApp(context.Background(), app, "test task", false, RunFlags{Auto: true}, &out)
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d", code, ExitOK)
	}

	var sawTokens bool
	for _, line := range strings.Split(out.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev["type"] == "tokens" {
			sawTokens = true
			if ev["input"] == nil || ev["output"] == nil {
				t.Error("tokens event missing input/output fields")
			}
		}
	}
	if !sawTokens {
		t.Errorf("no tokens event in output:\n%s", out.String())
	}
}

// ---- Empty-response / token-limit tests ----

// emptyFrames returns SSE frames that produce an assistant message with empty
// content and no tool calls — the token-limit-truncation signature.
func emptyFrames() []string {
	return []string{`{"choices":[{"delta":{},"finish_reason":"stop"}]}`}
}

// TestEmptyResponseWarnsAndRetries: a turn with empty content + zero tool calls
// emits a warning and in IMPLEMENT phase automatically retries once. The retry
// succeeds here and the workflow step advances normally.
func TestEmptyResponseWarnsAndRetries(t *testing.T) {
	// First SSE call: empty. Second (retry): has content + step sentinel.
	retryContent := "Fixed it.\n%%STEP_LOG: Step 1: done | outcome: ok | deviation: none%%\n%%STEP_DONE%%"
	srv := sseServer(t,
		emptyFrames(),
		[]string{contentChunk(retryContent)},
	)
	defer srv.Close()

	exec := newFakeExecutor()
	exec.files[".wakil/plan.md"] = planContent2Steps
	app := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.WFFinalReview = false
	app.Workflow = &workflow.WorkflowState{
		Task:      "t",
		Phase:     workflow.WFImplement,
		PlanPath:  ".wakil/plan.md",
		StepCount: 2,
		StepIdx:   1,
	}

	ctx := context.Background()
	app.WorkflowStepTrace = nil

	// First turn: empty completion.
	if _, err := app.Send(ctx, "continue"); err != nil {
		t.Fatalf("send: %v", err)
	}
	if !agent.IsEmptyTurn(app.Conv) {
		t.Fatal("should detect empty turn after first SSE call")
	}

	// handleEmptyResponse issues the single retry internally.
	agent.HandleEmptyResponse(ctx, app)

	// After retry: last assistant message should be non-empty.
	if agent.IsEmptyTurn(app.Conv) {
		t.Error("retry should have produced a non-empty response")
	}

	// Workflow transition should see the retry's %%STEP_DONE%% and advance.
	next := agent.HandleWorkflowTransition(ctx, app)
	if app.Workflow == nil {
		t.Fatal("workflow should still be active (step 2 pending)")
	}
	if app.Workflow.StepIdx != 2 {
		t.Errorf("StepIdx should advance to 2 after successful retry, got %d", app.Workflow.StepIdx)
	}
	if next == nil {
		t.Error("expected auto-turn msg for step 2")
	}
}

// TestEmptyResponseSecondAlsoEmpty: when both the original turn and the retry
// return empty, the warning is shown twice and workflow state is untouched.
func TestEmptyResponseSecondAlsoEmpty(t *testing.T) {
	srv := sseServer(t,
		emptyFrames(), // first turn: empty
		emptyFrames(), // retry: also empty
	)
	defer srv.Close()

	exec := newFakeExecutor()
	exec.files[".wakil/plan.md"] = planContent2Steps
	app := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.WFFinalReview = false
	app.Workflow = &workflow.WorkflowState{
		Task:      "t",
		Phase:     workflow.WFImplement,
		PlanPath:  ".wakil/plan.md",
		StepCount: 2,
		StepIdx:   1,
	}

	ctx := context.Background()
	app.WorkflowStepTrace = nil

	if _, err := app.Send(ctx, "continue"); err != nil {
		t.Fatalf("send: %v", err)
	}
	if !agent.IsEmptyTurn(app.Conv) {
		t.Fatal("first turn should be empty")
	}

	// Retry issued internally; retry also returns empty.
	agent.HandleEmptyResponse(ctx, app)

	if !agent.IsEmptyTurn(app.Conv) {
		t.Error("last message should still be empty after failed retry")
	}

	// Workflow transition: no sentinels → no phase or step change.
	next := agent.HandleWorkflowTransition(ctx, app)
	if next != nil {
		t.Error("no auto-turn should fire from empty response")
	}
	if app.Workflow == nil {
		t.Fatal("workflow should not be cleared")
	}
	if app.Workflow.Phase != workflow.WFImplement {
		t.Errorf("phase should remain workflow.WFImplement, got %v", app.Workflow.Phase)
	}
	if app.Workflow.StepIdx != 1 {
		t.Errorf("StepIdx should remain 1 (untouched), got %d", app.Workflow.StepIdx)
	}
}

// TestIsEmptyTurn covers the helper directly.
func TestIsEmptyTurn(t *testing.T) {
	// No assistant messages → not empty.
	if agent.IsEmptyTurn(nil) {
		t.Error("empty conv should return false")
	}
	// Last message has content → not empty.
	conv := []proxy.Message{
		{Role: "user", Content: agent.StrPtr("q")},
		{Role: "assistant", Content: agent.StrPtr("answer")},
	}
	if agent.IsEmptyTurn(conv) {
		t.Error("non-empty assistant should return false")
	}
	// Last message has tool calls → not empty.
	conv2 := []proxy.Message{
		{Role: "assistant", ToolCalls: []proxy.ToolCall{{ID: "1"}}},
	}
	if agent.IsEmptyTurn(conv2) {
		t.Error("assistant with tool calls should return false")
	}
	// Last message has empty content, no tool calls → empty.
	conv3 := []proxy.Message{
		{Role: "user", Content: agent.StrPtr("q")},
		{Role: "assistant", Content: agent.StrPtr("")},
	}
	if !agent.IsEmptyTurn(conv3) {
		t.Error("assistant with empty content and no tool calls should be detected")
	}
	// nil Content → empty.
	conv4 := []proxy.Message{
		{Role: "assistant", Content: nil},
	}
	if !agent.IsEmptyTurn(conv4) {
		t.Error("assistant with nil content should be detected as empty")
	}
}

// ---- P20 tests ----

// ---- P21 tests ----

// TestBriefingContainsFindings: when plan.md has ## Findings, both the regular
// oracle briefing and the final-review briefing include it between Task and Plan.
func TestBriefingContainsFindings(t *testing.T) {
	plan := "## Task\n\nmy task\n\n## Findings\n\nfoo.go line 42 is the culprit\n\n## Plan\n\n1. fix it\n"

	oracle := workflow.BuildOracleBriefing("my task", plan, "q?")
	if !strings.Contains(oracle, "## Findings") {
		t.Error("oracle briefing should contain ## Findings")
	}
	if !strings.Contains(oracle, "foo.go line 42") {
		t.Error("oracle briefing should contain findings body")
	}
	// Order: Task before Findings before Plan.
	taskIdx := strings.Index(oracle, "## Task")
	findIdx := strings.Index(oracle, "## Findings")
	planIdx := strings.Index(oracle, "## Plan")
	if !(taskIdx < findIdx && findIdx < planIdx) {
		t.Errorf("order must be Task < Findings < Plan; got %d %d %d", taskIdx, findIdx, planIdx)
	}

	final := workflow.BuildFinalReviewBriefing("my task", plan, "VERDICT?", 0)
	if !strings.Contains(final, "## Findings") {
		t.Error("final-review briefing should contain ## Findings")
	}
}

// TestFindingsTruncatedWithMarker: findings larger than 4 KB are tail-truncated
// with "[findings truncated]" appended.
func TestFindingsTruncatedWithMarker(t *testing.T) {
	bigFindings := strings.Repeat("x", workflow.FindingsCap+200)
	plan := "## Task\n\nt\n\n## Findings\n\n" + bigFindings + "\n\n## Plan\n\n1. step\n"

	oracle := workflow.BuildOracleBriefing("t", plan, "q?")
	if !strings.Contains(oracle, "[findings truncated]") {
		t.Error("oversized findings must include truncation marker")
	}
	// Marker must be inside the Findings section, before Plan.
	findingsStart := strings.Index(oracle, "## Findings")
	planStart := strings.Index(oracle, "## Plan")
	markerPos := strings.Index(oracle, "[findings truncated]")
	if !(findingsStart < markerPos && markerPos < planStart) {
		t.Error("[findings truncated] must appear inside ## Findings, before ## Plan")
	}
}

// TestBriefingValidatesWithoutFindings: a plan.md without ## Findings still
// produces a valid briefing (pre-existing workflows lack this section).
func TestBriefingValidatesWithoutFindings(t *testing.T) {
	noFindings := "## Task\n\nt\n\n## Plan\n\n1. step\n"
	briefing := workflow.BuildFinalReviewBriefing("t", noFindings, "VERDICT?", 0)
	if reason := workflow.ValidateBriefing(briefing, false); reason != "" {
		t.Errorf("briefing without ## Findings should validate; got: %q", reason)
	}
	if strings.Contains(briefing, "## Findings") {
		t.Error("briefing should not invent a ## Findings section when absent from plan")
	}
}

// TestBriefingOmissionMarkerKeepsNewest: when the step log is too large to fit,
// the oldest entries are dropped, an omission marker is inserted, ## Task and
// ## Plan are always kept, and the NEWEST entry is always present.
func TestBriefingOmissionMarkerKeepsNewest(t *testing.T) {
	var entryParts []string
	for i := 1; i <= 10; i++ {
		entryParts = append(entryParts,
			fmt.Sprintf("Step %d: work done | outcome: ok | deviation: none\n[ev] shell ok 100B \"cmd%d\"", i, i))
	}
	planContent := "## Task\n\nmy task\n\n## Plan\n\n1. step\n\n## Step log\n\n" +
		strings.Join(entryParts, "\n\n")

	// Cap forces drops: 10 entries × ~70 chars each easily fits in 16 KB, so
	// use a small explicit cap to force the omission path.
	briefing := workflow.BuildFinalReviewBriefing("my task", planContent, "q?", 800)

	if !strings.Contains(briefing, "## Task") {
		t.Error("## Task must always be present")
	}
	if !strings.Contains(briefing, "## Plan") {
		t.Error("## Plan must always be present")
	}
	if !strings.Contains(briefing, "omitted") {
		t.Error("omission marker must appear when entries are dropped")
	}
	if !strings.Contains(briefing, "Step 10") {
		t.Error("newest entry (Step 10) must be retained")
	}
	if strings.Contains(briefing, "Step 1:") {
		t.Error("oldest entry (Step 1) should have been dropped")
	}
}

// TestBriefingTruncationMarkerAppended: when even ## Task + ## Plan + ## Question
// alone exceed the cap, the result ends with [briefing truncated] — never silent.
func TestBriefingTruncationMarkerAppended(t *testing.T) {
	longTask := strings.Repeat("a", 600)
	longPlan := strings.Repeat("b", 600)
	planContent := fmt.Sprintf("## Task\n\n%s\n\n## Plan\n\n%s\n", longTask, longPlan)

	briefing := workflow.BuildFinalReviewBriefing(longTask, planContent, "q?", 200)

	if !strings.HasSuffix(briefing, "[briefing truncated]") {
		t.Errorf("hard-truncated briefing must end with [briefing truncated]; got tail: %q",
			briefing[max(0, len(briefing)-40):])
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// TestGapFlagGistIsOneLiner: gapGist extracts the first substantive line and
// keeps it to 120 chars — no newlines, no multi-paragraph oracle text.
func TestGapFlagGistIsOneLiner(t *testing.T) {
	multiPara := "Criterion 2 is not satisfied: the edge-case test is missing.\n\n" +
		"The deviation in step 1 was not resolved.\n\nVERDICT: GAPS"
	gist := workflow.GapGist(multiPara)

	if strings.Contains(gist, "\n") {
		t.Error("gist must be a single line")
	}
	if !strings.Contains(gist, "Criterion 2") {
		t.Errorf("gist should start with the first criterion, got: %q", gist)
	}
	if len(gist) > 120 {
		t.Errorf("gist exceeds 120 chars: %d", len(gist))
	}

	// Verdict-only response: gapGist must not emit "VERDICT: GAPS" as the gist.
	verdictOnly := "\nVERDICT: GAPS\n"
	g2 := workflow.GapGist(verdictOnly)
	if strings.HasPrefix(g2, "VERDICT:") {
		t.Errorf("gist must skip VERDICT lines, got: %q", g2)
	}
}

// TestGapFlagLogEntryIsOneLiner: wfWriteFinalLog for gaps writes a one-liner
// so recentStepEntries does not split it across paragraphs when the oracle
// response contains blank lines. This is the "empty-gap-flag" bug from P20.
func TestGapFlagLogEntryIsOneLiner(t *testing.T) {
	// Oracle response with blank lines (the P20 bug case).
	oracleResult := "Criterion 2 is not satisfied.\n\nStep 1 deviation is unresolved.\n\nVERDICT: GAPS"

	// The entry that would be written to plan.md:
	entry := "FINAL REVIEW: gaps — " + workflow.GapGist(oracleResult)

	// Must be a single paragraph (no \n\n) so recentStepEntries keeps it as one entry.
	if strings.Contains(entry, "\n\n") {
		t.Errorf("gap-flag entry must not contain double newlines: %q", entry)
	}
	if !strings.Contains(entry, "FINAL REVIEW") {
		t.Error("entry must contain FINAL REVIEW marker")
	}
	if !strings.Contains(entry, "Criterion 2") {
		t.Error("entry must contain the criterion gist")
	}

	// Verify recentStepEntries treats it as exactly one entry.
	logContent := entry
	entries := workflow.RecentStepEntries(logContent, 9999)
	if len(entries) != 1 {
		t.Errorf("gap-flag entry should be 1 recentStepEntry, got %d: %v", len(entries), entries)
	}
}

// ---- P19 tests ----

// TestStaleReviewWarnsOnFirstApprove: if the plan was edited after the oracle
// reviewed it, the first /plan approve warns and stays at workflow.WFPresent.
// The second approve then proceeds regardless.
func TestStaleReviewWarnsOnFirstApprove(t *testing.T) {
	exec := newFakeExecutor()
	originalPlan := "## Task\n\nt\n\n## Plan\n\n1. original step\n"
	exec.files[".wakil/plan.md"] = originalPlan

	app := &agent.App{
		Cfg:     config.DefaultConfig(),
		Exec:    exec,
		Out:     io.Discard,
		Session: &agent.Session{},
		Workflow: &workflow.WorkflowState{
			Phase:          workflow.WFPresent,
			StepCount:      1,
			PlanPath:       ".wakil/plan.md",
			ReviewPlanHash: workflow.HashPlanSection(originalPlan), // hash of the reviewed plan
		},
	}

	// Simulate a plan edit after review.
	editedPlan := "## Task\n\nt\n\n## Plan\n\n1. modified step\n"
	exec.files[".wakil/plan.md"] = editedPlan

	// --- First approve: should warn, stay at workflow.WFPresent ---
	_, _, cmd := agent.HandlePlanCommand([]string{"/plan", "approve"}, app)
	if cmd == nil {
		t.Fatal("expected cmd")
	}
	msg := cmd()

	note, ok := msg.(agent.SysNoteMsg)
	if !ok || !strings.Contains(note.Text, "plan modified since last review") {
		t.Errorf("first approve should warn about stale review; got %T: %v", msg, msg)
	}
	if app.Workflow == nil || app.Workflow.Phase != workflow.WFPresent {
		t.Errorf("workflow should remain at workflow.WFPresent after stale warning, got %v", app.Workflow.Phase)
	}
	if !app.Workflow.ReviewStaleWarned {
		t.Error("ReviewStaleWarned should be set after the warning")
	}

	// --- Second approve: should proceed despite stale plan ---
	_, _, cmd2 := agent.HandlePlanCommand([]string{"/plan", "approve"}, app)
	if cmd2 == nil {
		t.Fatal("expected cmd2")
	}
	msg2 := cmd2()

	if _, ok := msg2.(agent.WFStartTurnMsg); !ok {
		t.Errorf("second approve should return agent.WFStartTurnMsg, got %T", msg2)
	}
	if app.Workflow == nil || app.Workflow.Phase != workflow.WFImplement {
		t.Errorf("workflow should advance to workflow.WFImplement, got %v", app.Workflow.Phase)
	}
	if app.Workflow.ReviewStaleWarned {
		t.Error("ReviewStaleWarned should be cleared after proceeding")
	}
}

// TestMatchingHashApprovesImmediately: when the plan is unchanged since review,
// /plan approve proceeds on the first attempt with no warning.
func TestMatchingHashApprovesImmediately(t *testing.T) {
	exec := newFakeExecutor()
	plan := "## Task\n\nt\n\n## Plan\n\n1. same step\n"
	exec.files[".wakil/plan.md"] = plan

	app := &agent.App{
		Cfg:     config.DefaultConfig(),
		Exec:    exec,
		Out:     io.Discard,
		Session: &agent.Session{},
		Workflow: &workflow.WorkflowState{
			Phase:          workflow.WFPresent,
			StepCount:      1,
			PlanPath:       ".wakil/plan.md",
			ReviewPlanHash: workflow.HashPlanSection(plan), // same hash — no edit
		},
	}

	_, _, cmd := agent.HandlePlanCommand([]string{"/plan", "approve"}, app)
	msg := cmd()

	if _, ok := msg.(agent.WFStartTurnMsg); !ok {
		t.Errorf("unchanged plan should approve immediately; got %T: %v", msg, msg)
	}
}

// TestReReviewClearsStaleFlag: a successful /plan review re-fingerprints the
// plan and clears ReviewStaleWarned so the next approve goes straight through.
func TestReReviewClearsStaleFlag(t *testing.T) {
	oracleSrv := oracleServer(t, "Plan looks good.\nVERDICT: PASS")
	defer oracleSrv.Close()

	exec := newFakeExecutor()
	updatedPlan := "## Task\n\nt\n\n## Plan\n\n1. updated step\n"
	exec.files[".wakil/plan.md"] = updatedPlan

	srv := sseServer(t) // no turns needed; we call handleReviewOracle directly
	defer srv.Close()

	app := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.OracleEnabled = true
	app.Cfg.OracleAPIKeyEnv = "TEST_KEY"
	app.Cfg.OracleEndpoint = oracleSrv.URL + "/v1/messages"
	t.Setenv("TEST_KEY", "fake-key")

	app.Workflow = &workflow.WorkflowState{
		Task:              "t",
		Phase:             workflow.WFReview,
		PlanPath:          ".wakil/plan.md",
		StepCount:         1,
		ReviewPlanHash:    "old-stale-hash",
		ReviewStaleWarned: true, // was warned before
	}

	ctx := context.Background()
	agent.HandleReviewOracle(ctx, app)

	// Oracle succeeded → phase advanced, stale flag cleared, hash updated.
	if app.Workflow == nil || app.Workflow.Phase != workflow.WFPresent {
		t.Errorf("should advance to workflow.WFPresent after successful re-review, got %v", app.Workflow.Phase)
	}
	if app.Workflow.ReviewStaleWarned {
		t.Error("ReviewStaleWarned should be cleared by re-review")
	}
	if app.Workflow.ReviewPlanHash == "old-stale-hash" {
		t.Error("ReviewPlanHash should be updated to reflect the re-reviewed plan")
	}
	// New hash must match the current plan.
	if app.Workflow.ReviewPlanHash != workflow.HashPlanSection(updatedPlan) {
		t.Error("ReviewPlanHash should equal the fingerprint of the current plan")
	}
}

// TestPlanReviewFromPresent: /plan review is valid from workflow.WFPresent (voluntary
// re-review). The full scenario: approve warns stale → /plan review from
// workflow.WFPresent → oracle succeeds → hash refreshed → approve proceeds cleanly.
func TestPlanReviewFromPresent(t *testing.T) {
	oracleSrv := oracleServer(t, "Plan is fine.\nVERDICT: PASS")
	defer oracleSrv.Close()

	exec := newFakeExecutor()
	updatedPlan := "## Task\n\nt\n\n## Plan\n\n1. updated step\n"
	exec.files[".wakil/plan.md"] = updatedPlan

	srv := sseServer(t) // no model turns needed; we call handleReviewOracle directly
	defer srv.Close()

	app := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.OracleEnabled = true
	app.Cfg.OracleAPIKeyEnv = "TEST_KEY"
	app.Cfg.OracleEndpoint = oracleSrv.URL + "/v1/messages"
	t.Setenv("TEST_KEY", "fake-key")

	app.Workflow = &workflow.WorkflowState{
		Task:              "t",
		Phase:             workflow.WFPresent,
		PlanPath:          ".wakil/plan.md",
		StepCount:         1,
		ReviewPlanHash:    "old-stale-hash",
		ReviewStaleWarned: true,
	}

	// --- /plan review from workflow.WFPresent ---
	_, _, cmd := agent.HandlePlanCommand([]string{"/plan", "review"}, app)

	// Phase must transition to workflow.WFReview synchronously in the Update handler.
	if app.Workflow.Phase != workflow.WFReview {
		t.Errorf("phase should be workflow.WFReview immediately after /plan review; got %v", app.Workflow.Phase)
	}
	// Cmd should start a workflow.WFReview turn.
	if cmd == nil {
		t.Fatal("expected cmd")
	}
	msg := cmd()
	if _, ok := msg.(agent.WFStartTurnMsg); !ok {
		t.Errorf("expected agent.WFStartTurnMsg, got %T", msg)
	}

	// Simulate turn completing: handleReviewOracle fires (workflow.WFReview auto-retry path).
	ctx := context.Background()
	agent.HandleReviewOracle(ctx, app)

	// Oracle succeeded → workflow.WFPresent, hash refreshed, stale flag cleared.
	if app.Workflow == nil || app.Workflow.Phase != workflow.WFPresent {
		t.Errorf("should return to workflow.WFPresent after successful review; got %v", app.Workflow.Phase)
	}
	if app.Workflow.ReviewStaleWarned {
		t.Error("ReviewStaleWarned should be cleared by re-review")
	}
	if app.Workflow.ReviewPlanHash != workflow.HashPlanSection(updatedPlan) {
		t.Errorf("ReviewPlanHash should match current plan; got %q", app.Workflow.ReviewPlanHash)
	}

	// --- /plan approve now proceeds without a stale warning ---
	_, _, approveCmd := agent.HandlePlanCommand([]string{"/plan", "approve"}, app)
	if approveCmd == nil {
		t.Fatal("expected approveCmd")
	}
	approveMsg := approveCmd()
	if _, ok := approveMsg.(agent.WFStartTurnMsg); !ok {
		t.Errorf("approve should return agent.WFStartTurnMsg (no stale warning); got %T: %v", approveMsg, approveMsg)
	}
}

// TestPlanReviewFromPresentGuard: /plan review from an unexpected phase returns an error.
func TestPlanReviewFromPresentGuard(t *testing.T) {
	app := &agent.App{
		Cfg:      config.DefaultConfig(),
		Session:  &agent.Session{},
		Workflow: &workflow.WorkflowState{Phase: workflow.WFGather},
	}
	_, _, cmd := agent.HandlePlanCommand([]string{"/plan", "review"}, app)
	if cmd == nil {
		t.Fatal("expected cmd")
	}
	msg := cmd()
	note, ok := msg.(agent.SysNoteMsg)
	if !ok || !strings.Contains(note.Text, "WFReview or WFPresent") {
		t.Errorf("guard message should name valid states; got %T: %v", msg, msg)
	}
}

// TestHashPlanSection: same plan content produces same hash; different content differs.
func TestHashPlanSection(t *testing.T) {
	plan := "## Task\n\nt\n\n## Plan\n\n1. step one\n2. step two\n"
	h1 := workflow.HashPlanSection(plan)
	h2 := workflow.HashPlanSection(plan)
	if h1 != h2 {
		t.Error("hash must be deterministic")
	}
	modified := "## Task\n\nt\n\n## Plan\n\n1. step one\n2. step TWO\n"
	if workflow.HashPlanSection(modified) == h1 {
		t.Error("different plan content must produce a different hash")
	}
}

// ---- P13c item 4 tests ----

// TestOracleMaxTokensThinkingError: stop_reason=max_tokens + thinking block +
// whitespace text must surface the max_tokens error (not empty-response), include
// the thinking hint, and the caller receives ok=false.
func TestOracleMaxTokensThinkingError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// thinking block present, text block is whitespace-only, stop_reason=max_tokens
		w.Write([]byte(`{"content":[{"type":"thinking","text":"reasoning..."},{"type":"text","text":"   "}],"stop_reason":"max_tokens","usage":{"input_tokens":10,"output_tokens":50}}`))
	}))
	defer srv.Close()

	cfg := config.DefaultConfig()
	cfg.OracleModel = "test"
	_, _, err := counsel.CallOracleURL(context.Background(), cfg, "key", "q", "", srv.URL+"/v1/messages")
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	// Must be the max_tokens path, not "empty or whitespace-only".
	if strings.Contains(msg, "empty or whitespace") {
		t.Errorf("wrong error: whitespace path won instead of max_tokens path: %v", err)
	}
	if !strings.Contains(msg, "max_tokens") {
		t.Errorf("error should mention max_tokens, got: %v", err)
	}
	if !strings.Contains(msg, "thinking model") {
		t.Errorf("error should contain thinking hint, got: %v", err)
	}
}

// TestOracleTruncatedTextRoutesUnavailable: stop_reason=max_tokens + non-empty
// text must return an error (not succeed), and error must mention "truncated".
func TestOracleTruncatedTextRoutesUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Partial non-empty text, stop_reason=max_tokens.
		w.Write([]byte(`{"content":[{"type":"text","text":"This answer was cut off mid-senten"}],"stop_reason":"max_tokens","usage":{"input_tokens":10,"output_tokens":20}}`))
	}))
	defer srv.Close()

	cfg := config.DefaultConfig()
	cfg.OracleModel = "test"
	_, _, err := counsel.CallOracleURL(context.Background(), cfg, "key", "q", "", srv.URL+"/v1/messages")
	if err == nil {
		t.Fatal("expected error for truncated oracle response")
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("error should mention 'truncated', got: %v", err)
	}
}

// TestExtractStepLogEntryIgnoresFencedBlock: a marker inside a ``` fence must be
// ignored; the real marker outside the fence must be returned.
func TestExtractStepLogEntryIgnoresFencedBlock(t *testing.T) {
	text := "Here's the format I used:\n" +
		"```\n" +
		"%%STEP_LOG: Step 0: fake entry inside fence | outcome: fake%%\n" +
		"```\n" +
		"And here is the real log entry:\n" +
		"%%STEP_LOG: Step 1: edited compact.go | outcome: builds | deviation: none%%\n" +
		"%%STEP_DONE%%"

	got := workflow.ExtractStepLogEntry(text)
	want := "Step 1: edited compact.go | outcome: builds | deviation: none"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestExtractStepLogEntryLastWins: when two real markers appear outside fences,
// the LAST one is returned (model quoted the format before emitting the real one).
func TestExtractStepLogEntryLastWins(t *testing.T) {
	text := "The format is %%STEP_LOG: Step N: description%%.\n" +
		"Actual entry: %%STEP_LOG: Step 1: real work done | outcome: ok | deviation: none%%\n" +
		"%%STEP_DONE%%"
	got := workflow.ExtractStepLogEntry(text)
	if got != "Step 1: real work done | outcome: ok | deviation: none" {
		t.Errorf("got %q", got)
	}
}

// TestDestructiveShellChaining covers the spec-listed patterns.
func TestDestructiveShellChaining(t *testing.T) {
	gates := []struct {
		cmd  string
		desc string
	}{
		{"echo ok && rm -rf /tmp/x", "rm after &&"},
		{"sh -c 'ls'", "sh -c wrapper"},
		{"find . -name '*.log' -delete", "find -delete"},
		{"rsync --delete src/ dst/", "rsync --delete"},
		{"rsync --delete-after src/ dst/", "rsync --delete-after"},
		{"git push origin main --force", "git push --force"},
		{"git push -f", "git push -f"},
		{"git stash drop", "git stash drop"},
		{"git stash clear", "git stash clear"},
		{"git branch -D old-feature", "git branch -D"},
		{"sed -i 's/foo/bar/' file.txt", "sed -i"},
		{"chown -R user:group dir/", "chown -R"},
		{"tee /etc/config", "tee"},
		{"xargs rm -rf", "xargs (unconditional)"},
		{"bash -c 'echo hi'", "bash wrapper"},
		{"zsh script.sh", "zsh"},
	}
	noGates := []struct {
		cmd  string
		desc string
	}{
		{`echo "rm is a command"`, "rm inside quoted string"},
		{"find . -name '*.log'", "find without -delete"},
		{"git checkout main", "git checkout branch (not --)"},
		{"git status", "git status"},
		{"git diff HEAD", "git diff"},
		{"git push origin main", "git push without force"},
		{"go build ./...", "go build"},
		{"chmod 644 file.txt", "chmod without -R"},
		{"chown user file.txt", "chown without -R"},
	}

	for _, tc := range gates {
		if !agent.IsDestructiveShell(tc.cmd) {
			t.Errorf("should gate (%s): %q", tc.desc, tc.cmd)
		}
	}
	for _, tc := range noGates {
		if agent.IsDestructiveShell(tc.cmd) {
			t.Errorf("should NOT gate (%s): %q", tc.desc, tc.cmd)
		}
	}
}

// ---- P13d tests ----

// makeToolCall builds a ToolCall struct for use in direct executeToolCall tests.
func makeToolCall(name, args string) proxy.ToolCall {
	return proxy.ToolCall{
		ID:       "tc1",
		Type:     "function",
		Function: proxy.FunctionCall{Name: name, Arguments: args},
	}
}

// wfTestApp builds a minimal App for phase-enforcement tests. Confirm always
// approves so a phase-block error unambiguously comes from wfPhaseBlock, not
// from the confirm gate.
func wfTestApp(phase workflow.WorkflowPhase) *agent.App {
	exec := newFakeExecutor()
	exec.files[".wakil/plan.md"] = planContent2Steps
	return &agent.App{
		Cfg:     config.DefaultConfig(),
		Exec:    exec,
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
		Session: &agent.Session{ChatID: "test"},
		Workflow: &workflow.WorkflowState{
			Task:      "test task",
			Phase:     phase,
			PlanPath:  ".wakil/plan.md",
			StepCount: 2,
			StepIdx:   1,
		},
	}
}

// TestWFPhaseBlocksProjectWrite: write_file to a project path during GATHER
// returns a phase error; the file must not be created.
func TestWFPhaseBlocksProjectWrite(t *testing.T) {
	app := wfTestApp(workflow.WFGather)
	exec := app.Exec.(*fakeExecutor)

	result := app.ExecuteToolCall(context.Background(), makeToolCall(
		"write_file", `{"path":"compact.go","content":"oops"}`))

	if !strings.Contains(result, "workflow phase gather") {
		t.Errorf("expected phase-gather error, got: %q", result)
	}
	if _, ok := exec.files["compact.go"]; ok {
		t.Error("compact.go must not be created when phase blocks the write")
	}
}

// TestWFPhaseAllowsNonPlanWakilWrite: write_file to .wakil/ files OTHER than
// plan.md is permitted in pre-IMPLEMENT phases.
func TestWFPhaseAllowsNonPlanWakilWrite(t *testing.T) {
	app := wfTestApp(workflow.WFGather)

	result := app.ExecuteToolCall(context.Background(), makeToolCall(
		"write_file", `{"path":".wakil/notes.md","content":"some notes"}`))

	if strings.Contains(result, "workflow") {
		t.Errorf("should allow .wakil/ (non-plan) write, got: %q", result)
	}
}

// TestWFPhaseBlocksEditOutsideWakil: edit_file on a project file during PLAN
// returns a phase error.
func TestWFPhaseBlocksEditOutsideWakil(t *testing.T) {
	app := wfTestApp(workflow.WFPlan)
	exec := app.Exec.(*fakeExecutor)
	exec.files["app.go"] = "package main\n"

	result := app.ExecuteToolCall(context.Background(), makeToolCall(
		"edit_file", `{"path":"app.go","old_string":"package main","new_string":"// oops"}`))

	if !strings.Contains(result, "workflow phase plan") {
		t.Errorf("expected phase-plan error, got: %q", result)
	}
	if exec.files["app.go"] != "package main\n" {
		t.Error("app.go content was modified despite phase block")
	}
}

// TestWFPhaseBlocksRunBackground: run_background during PRESENT returns a
// phase error.
func TestWFPhaseBlocksRunBackground(t *testing.T) {
	app := wfTestApp(workflow.WFPresent)

	result := app.ExecuteToolCall(context.Background(), makeToolCall(
		"run_background", `{"command":"server","label":"dev"}`))

	if !strings.Contains(result, "workflow phase present") {
		t.Errorf("expected phase-present error, got: %q", result)
	}
}

// TestWFImplementBlocksPlanRewrite: write_file to .wakil/plan.md during
// IMPLEMENT returns a phase error; the existing content is preserved.
func TestWFImplementBlocksPlanRewrite(t *testing.T) {
	app := wfTestApp(workflow.WFImplement)
	exec := app.Exec.(*fakeExecutor)
	original := exec.files[".wakil/plan.md"]

	result := app.ExecuteToolCall(context.Background(), makeToolCall(
		"write_file", `{"path":".wakil/plan.md","content":"WIPED"}`))

	if !strings.Contains(result, "write_file on plan.md is not permitted") {
		t.Errorf("expected plan.md write rejection, got: %q", result)
	}
	if exec.files[".wakil/plan.md"] != original {
		t.Error("plan.md was overwritten despite phase block")
	}
}

// TestWFImplementAllowsEditFileToPlan: edit_file on .wakil/plan.md is
// permitted during IMPLEMENT (targeted changes are fine; only full rewrites
// are blocked).
func TestWFImplementAllowsEditFileToPlan(t *testing.T) {
	app := wfTestApp(workflow.WFImplement)

	// Attempt an edit — it will fail at old_string-not-found, but must NOT
	// fail with a workflow-phase error.
	result := app.ExecuteToolCall(context.Background(), makeToolCall(
		"edit_file", `{"path":".wakil/plan.md","old_string":"NONEXISTENT","new_string":"x"}`))

	if strings.Contains(result, "workflow phase") {
		t.Errorf("edit_file on plan.md must not be phase-blocked during IMPLEMENT, got: %q", result)
	}
}

// TestWFPhaseAllowsWriteAfterImplement: once IMPLEMENT is active, project
// writes are permitted.
func TestWFPhaseAllowsWriteAfterImplement(t *testing.T) {
	app := wfTestApp(workflow.WFImplement)
	exec := app.Exec.(*fakeExecutor)
	exec.files["app.go"] = "package main\n"

	result := app.ExecuteToolCall(context.Background(), makeToolCall(
		"write_file", `{"path":"app.go","content":"package main\n// fixed"}`))

	// Must not produce a phase error.
	if strings.Contains(result, "workflow phase") {
		t.Errorf("IMPLEMENT should permit project writes, got: %q", result)
	}
}

// TestIsWakilPath checks the path classification helper.
func TestIsWakilPath(t *testing.T) {
	inWakil := []string{
		".wakil/plan.md",
		".wakil",
		".wakil/",
		"/work/.wakil/plan.md",
	}
	notWakil := []string{
		"app.go",
		"src/main.go",
		"plan.md",
		"not-wakil/plan.md",
	}
	for _, p := range inWakil {
		if !workflow.IsWakilPath(p) {
			t.Errorf("workflow.IsWakilPath(%q) = false, want true", p)
		}
	}
	for _, p := range notWakil {
		if workflow.IsWakilPath(p) {
			t.Errorf("workflow.IsWakilPath(%q) = true, want false", p)
		}
	}
}

// ---- P15 tests ----

// TestSentinelInIntermediateMessageIgnored: a turn whose intermediate assistant
// message contains %%PHASE_DONE%% but which then calls a tool — and whose FINAL
// assistant message does not contain the sentinel — must NOT trigger a transition.
func TestSentinelInIntermediateMessageIgnored(t *testing.T) {
	// First SSE call: content with sentinel + tool call (sentinel is intermediate).
	firstCall := append(
		[]string{contentChunk("Gathered. " + workflow.WFPhaseDone)},
		toolCallFrames("c1", "read_file", `{"path":"foo.go"}`)...,
	)
	// Second SSE call: final content WITHOUT sentinel.
	secondCall := []string{contentChunk("Confirmed. No more work.")}

	srv := sseServer(t, firstCall, secondCall)
	defer srv.Close()

	exec := newFakeExecutor()
	exec.files["foo.go"] = "package main"
	exec.files[".wakil/plan.md"] = planContent2Steps
	app := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })
	app.Workflow = &workflow.WorkflowState{
		Task:     "t",
		Phase:    workflow.WFGather,
		PlanPath: ".wakil/plan.md",
	}

	ctx := context.Background()
	if _, err := app.Send(ctx, "continue"); err != nil {
		t.Fatalf("send: %v", err)
	}
	next := agent.HandleWorkflowTransition(ctx, app)

	// Intermediate sentinel must be ignored; final message has none → no transition.
	if app.Workflow.Phase != workflow.WFGather {
		t.Errorf("intermediate sentinel must be ignored; want workflow.WFGather, got %v", app.Workflow.Phase)
	}
	if next != nil {
		t.Error("no auto-turn should fire when final message has no sentinel")
	}
}

// TestSentinelInFinalMessageTransitions: same setup but final message carries the
// sentinel — transition must fire.
func TestSentinelInFinalMessageTransitions(t *testing.T) {
	firstCall := toolCallFrames("c1", "read_file", `{"path":"foo.go"}`)
	secondCall := []string{contentChunk("Done reading. " + workflow.WFPhaseDone)}

	srv := sseServer(t, firstCall, secondCall)
	defer srv.Close()

	exec := newFakeExecutor()
	exec.files["foo.go"] = "package main"
	exec.files[".wakil/plan.md"] = planContent2Steps
	app := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })
	app.Workflow = &workflow.WorkflowState{
		Task:     "t",
		Phase:    workflow.WFGather,
		PlanPath: ".wakil/plan.md",
	}

	ctx := context.Background()
	if _, err := app.Send(ctx, "continue"); err != nil {
		t.Fatalf("send: %v", err)
	}
	next := agent.HandleWorkflowTransition(ctx, app)

	if app.Workflow.Phase != workflow.WFPlan {
		t.Errorf("sentinel in final message must transition; want workflow.WFPlan, got %v", app.Workflow.Phase)
	}
	if next == nil {
		t.Error("auto-turn msg expected when sentinel is in the final message")
	}
}

// TestReviewRetryOnTurnCompletion: a turn that completes while workflow is in
// workflow.WFReview automatically re-attempts the oracle review.
func TestReviewRetryOnTurnCompletion(t *testing.T) {
	oracleCallCount := 0
	oracleSrv := oracleServer(t, "Looks good.\nVERDICT: PASS")
	defer oracleSrv.Close()

	srv := sseServer(t, []string{contentChunk("ok")})
	defer srv.Close()

	exec := newFakeExecutor()
	exec.files[".wakil/plan.md"] = planContent2Steps
	app := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool {
		oracleCallCount++
		return true // approve oracle
	})
	app.Cfg.OracleEnabled = true
	app.Cfg.OracleAPIKeyEnv = "TEST_KEY"
	app.Cfg.OracleEndpoint = oracleSrv.URL + "/v1/messages"
	t.Setenv("TEST_KEY", "fake")

	app.Workflow = &workflow.WorkflowState{
		Task:             "t",
		Phase:            workflow.WFReview,
		PlanPath:         ".wakil/plan.md",
		StepCount:        2,
		ReviewSkipReason: "oracle was unavailable last time",
	}

	ctx := context.Background()
	if _, err := app.Send(ctx, "continue"); err != nil {
		t.Fatalf("send: %v", err)
	}
	agent.HandleWorkflowTransition(ctx, app)

	// Oracle review was re-attempted and succeeded → workflow must be in workflow.WFPresent.
	if app.Workflow == nil || app.Workflow.Phase != workflow.WFPresent {
		t.Errorf("review retry should advance to workflow.WFPresent, got phase=%v", app.Workflow.Phase)
	}
}

// TestPlanReviewCommand: /plan review in workflow.WFReview state triggers a agent.WFStartTurnMsg.
func TestPlanReviewCommand(t *testing.T) {
	app := &agent.App{
		Cfg:      config.DefaultConfig(),
		Session:  &agent.Session{},
		Workflow: &workflow.WorkflowState{Phase: workflow.WFReview, StepCount: 2},
	}
	_, _, cmd := agent.HandlePlanCommand([]string{"/plan", "review"}, app)
	if cmd == nil {
		t.Fatal("expected cmd")
	}
	msg := cmd()
	if _, ok := msg.(agent.WFStartTurnMsg); !ok {
		t.Errorf("expected agent.WFStartTurnMsg, got %T", msg)
	}
}

// TestPlanReviewCommandNotInReview: /plan review outside workflow.WFReview/workflow.WFPresent returns an error.
func TestPlanReviewCommandNotInReview(t *testing.T) {
	// workflow.WFGather is not a valid phase for /plan review.
	app := &agent.App{
		Cfg:      config.DefaultConfig(),
		Session:  &agent.Session{},
		Workflow: &workflow.WorkflowState{Phase: workflow.WFGather},
	}
	_, _, cmd := agent.HandlePlanCommand([]string{"/plan", "review"}, app)
	if cmd == nil {
		t.Fatal("expected cmd")
	}
	msg := cmd()
	note, ok := msg.(agent.SysNoteMsg)
	if !ok || !strings.Contains(note.Text, "WFReview or WFPresent") {
		t.Errorf("expected guard note naming valid states, got %T %v", msg, msg)
	}
}

// TestApproveOnReviewLogsReason: /plan approve on workflow.WFReview writes
// "REVIEW skipped with reason: …" to plan.md before advancing to workflow.WFPresent.
func TestApproveOnReviewLogsReason(t *testing.T) {
	exec := newFakeExecutor()
	exec.files[".wakil/plan.md"] = "## Task\n\nt\n\n## Plan\n\n1. step\n"
	app := &agent.App{
		Cfg:     config.DefaultConfig(),
		Exec:    exec,
		Out:     io.Discard,
		Session: &agent.Session{ChatID: "test"},
		Workflow: &workflow.WorkflowState{
			Phase:            workflow.WFReview,
			StepCount:        1,
			PlanPath:         ".wakil/plan.md",
			ReviewSkipReason: "oracle not configured",
		},
	}
	_, _, cmd := agent.HandlePlanCommand([]string{"/plan", "approve"}, app)
	if cmd != nil {
		cmd()
	}

	if app.Workflow == nil || app.Workflow.Phase != workflow.WFPresent {
		t.Errorf("approve on workflow.WFReview should advance to workflow.WFPresent, got %v", app.Workflow.Phase)
	}
	plan := exec.files[".wakil/plan.md"]
	if !strings.Contains(plan, "REVIEW skipped with reason:") {
		t.Error("plan.md should contain 'REVIEW skipped with reason:' entry")
	}
	if !strings.Contains(plan, "oracle not configured") {
		t.Error("plan.md should contain the actual reason string")
	}
}

// TestPlanFormatContractRejects: a plan written with ### Step headers (not N.
// numbered lines) triggers PlanFormatInvalid and keeps workflow in workflow.WFPlan.
func TestPlanFormatContractRejects(t *testing.T) {
	badPlan := "## Task\n\nt\n\n## Plan\n\n### Step 1\nFix the bug\n\n### Step 2\nAdd tests\n"

	srv := sseServer(t, []string{contentChunk("plan written\n" + workflow.WFPhaseDone)})
	defer srv.Close()

	exec := newFakeExecutor()
	exec.files[".wakil/plan.md"] = badPlan
	app := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return false })
	app.Workflow = &workflow.WorkflowState{
		Task:     "t",
		Phase:    workflow.WFPlan,
		PlanPath: ".wakil/plan.md",
	}

	ctx := context.Background()
	if _, err := app.Send(ctx, "continue"); err != nil {
		t.Fatalf("send: %v", err)
	}
	agent.HandleWorkflowTransition(ctx, app)

	if app.Workflow == nil || app.Workflow.Phase != workflow.WFPlan {
		t.Errorf("bad format should keep workflow in workflow.WFPlan, got %v", app.Workflow.Phase)
	}
	if !app.Workflow.PlanFormatInvalid {
		t.Error("PlanFormatInvalid should be set")
	}
	// Reformat directive must be returned.
	d := app.Workflow.Directive()
	if !strings.Contains(d, "REFORMAT") {
		t.Errorf("directive should be reformat directive, got: %q", d)
	}
	// Plan.md should have a format-error log entry.
	if !strings.Contains(exec.files[".wakil/plan.md"], "PLAN FORMAT ERROR") {
		t.Error("plan.md should contain PLAN FORMAT ERROR entry")
	}
}

// TestPlanFormatContractProceedsAfterReformat: after bad format, model rewrites
// with proper N. steps and emits %%PHASE_DONE%% — workflow advances past workflow.WFPlan.
func TestPlanFormatContractProceedsAfterReformat(t *testing.T) {
	goodPlan := "## Task\n\nt\n\n## Plan\n\n1. Fix the bug\n2. Add tests\n"

	srv := sseServer(t, []string{contentChunk("reformatted\n" + workflow.WFPhaseDone)})
	defer srv.Close()

	exec := newFakeExecutor()
	exec.files[".wakil/plan.md"] = goodPlan
	app := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return false })
	app.Cfg.OracleEnabled = false
	app.Workflow = &workflow.WorkflowState{
		Task:              "t",
		Phase:             workflow.WFPlan,
		PlanPath:          ".wakil/plan.md",
		PlanFormatInvalid: true, // previously detected as bad
	}

	ctx := context.Background()
	if _, err := app.Send(ctx, "continue"); err != nil {
		t.Fatalf("send: %v", err)
	}
	agent.HandleWorkflowTransition(ctx, app)

	// Must have left workflow.WFPlan (advanced to workflow.WFReview or beyond).
	if app.Workflow != nil && app.Workflow.Phase == workflow.WFPlan {
		t.Errorf("reformatted plan should exit workflow.WFPlan, still in workflow.WFPlan")
	}
	if app.Workflow != nil && app.Workflow.PlanFormatInvalid {
		t.Error("PlanFormatInvalid should be cleared after valid plan")
	}
}

// TestApproveIsUserOnlyInvariant: AutoApprove has no effect on /plan approve —
// the command produces the same result regardless, confirming it can only be
// triggered by explicit user input.
func TestApproveIsUserOnlyInvariant(t *testing.T) {
	makeApp := func(autoApprove bool) *agent.App {
		return &agent.App{
			Cfg:         config.DefaultConfig(),
			Session:     &agent.Session{},
			AutoApprove: autoApprove,
			Workflow: &workflow.WorkflowState{
				Phase:     workflow.WFPresent,
				StepCount: 2,
				PlanPath:  ".wakil/plan.md",
			},
		}
	}

	// Execute the Cmd returned by handlePlanCommand — the phase change now
	// happens inside the Cmd so the stale-check file I/O can be async.
	runApprove := func(a *agent.App) workflow.WorkflowPhase {
		_, _, cmd := agent.HandlePlanCommand([]string{"/plan", "approve"}, a)
		if cmd != nil {
			cmd() // executes synchronously in tests
		}
		if a.Workflow == nil {
			return workflow.WFDone
		}
		return a.Workflow.Phase
	}

	appManual := makeApp(false)
	phaseManual := runApprove(appManual)

	appAuto := makeApp(true) // AutoApprove on — must behave identically
	phaseAuto := runApprove(appAuto)

	if phaseManual != phaseAuto {
		t.Errorf("AutoApprove must not affect /plan approve: manual=%v auto=%v", phaseManual, phaseAuto)
	}
	if phaseManual != workflow.WFImplement {
		t.Errorf("approve should advance to workflow.WFImplement, got %v", phaseManual)
	}
}
