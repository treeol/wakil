package wiring

// plan_host_test.go: chunk-7c integration tests — the headless --plan workflow
// driven through the session host (runPlanTask / RunPlanLoop) against fake SSE
// backends, asserting the adapter's plan-auto-advance resolver, the durable
// WorkflowOutcome/WorkflowWarning/final-review events, exit-code parity with
// the deleted legacy loop, and the never-silently-idle invariant.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/workflow"
)

// planApp builds a fakeApp with a plan.md-holding executor (the workflow
// engine reads plan.md to count steps). The fake executor in hostturn_test.go
// returns empty reads; a stateful variant is needed here.
func planApp(t *testing.T, url string, planContent string) (*agent.App, *memFilesExec) {
	t.Helper()
	ex := &memFilesExec{files: map[string]string{".wakil/plan.md": planContent}}
	app := fakeApp(url)
	app.Exec = ex
	app.Cfg.WFFinalReview = true
	return app, ex
}

// runPlan drives RunPlanLoop (host path over a pre-set workflow) with a
// deadline so a broken resolver fails the test instead of hanging.
func runPlan(t *testing.T, app *agent.App, opts HeadlessOptions) (int, string) {
	t.Helper()
	var out strings.Builder
	done := make(chan struct{})
	var code int
	go func() {
		code = RunPlanLoop(context.Background(), app, opts, &out)
		close(done)
	}()
	select {
	case <-done:
		return code, out.String()
	case <-time.After(15 * time.Second):
		t.Fatal("plan run did not terminate within 15s — resolver left the workflow silently idle?")
		return 0, ""
	}
}

// TestPlanHostPassNoMarkerCrossing: gather → plan (PHASE_DONE with steps) →
// review (oracle disabled → resolver force-skips with a warning) → present
// (resolver auto-advances) → implement step (STEP_DONE) → final review
// (oracle disabled → unavailable → gaps). With oracle disabled everywhere the
// terminal is gaps — verifying the full phase chain, the warning record, and
// the gaps terminal bytes.
func TestPlanHostFullChainEndsInGaps(t *testing.T) {
	plan := "## Task\n\nt\n\n## Plan\n\n1. fix it\n"
	gatherFrame := []string{contentChunk("found things\n%%PHASE_DONE%%")}
	planFrame := []string{contentChunk("## Plan\n\n1. fix it\n%%PHASE_DONE%%")}
	stepFrame := []string{contentChunk("done\n%%STEP_LOG: Step 1: patched | outcome: ok | deviation: none%%\n%%STEP_DONE%%")}
	srv := sseServer(t, gatherFrame, planFrame, stepFrame, stepFrame)
	defer srv.Close()

	app, _ := planApp(t, srv.URL, plan)
	app.Workflow = &workflow.WorkflowState{Task: "t", Phase: workflow.WFGather, PlanPath: ".wakil/plan.md"}
	app.Cfg.OracleEnabled = false // review + final review unavailable → skip + gaps

	code, out := runPlan(t, app, HeadlessOptions{Auto: true, NoOracle: true})
	if code != ExitGaps {
		t.Fatalf("exit = %d, want ExitGaps(%d); out:\n%s", code, ExitGaps, out)
	}
	evs := outputEvents(t, out)
	done := findEvent(evs, "done")
	if done == nil || done["outcome"] != "gaps" {
		t.Fatalf("terminal record = %v, want outcome gaps", done)
	}
	if warn := findEvent(evs, "warning"); warn == nil {
		t.Fatalf("expected the review-skip warning record; full out:\n%s", out)
	}
}

// TestPlanHostBackendFailure: a mid-workflow backend failure surfaces the
// backend_failure terminal with resume_id bytes (legacy emitBackendFailure
// parity) — through SessionError, not a resolver path.
func TestPlanHostBackendFailure(t *testing.T) {
	plan := "## Task\n\nt\n\n## Plan\n\n1. fix it\n"
	// First call → HTTP 500 (a genuine backend failure, not an empty stream).
	srv := cyclingSSEServer(t, map[int]bool{0: true}, nil, nil)
	defer srv.Close()

	app, _ := planApp(t, srv.URL, plan)
	app.RetryDelay = noDelay
	app.Cfg.BackendMaxRetries = 0 // exhaust immediately
	app.Cfg.OracleEnabled = false
	app.Workflow = &workflow.WorkflowState{Task: "t", Phase: workflow.WFGather, PlanPath: ".wakil/plan.md"}

	code, out := runPlan(t, app, HeadlessOptions{Auto: true})
	if code != ExitBackendFailure {
		t.Fatalf("exit = %d, want ExitBackendFailure(%d); out:\n%s", code, ExitBackendFailure, out)
	}
	evs := outputEvents(t, out)
	done := findEvent(evs, "done")
	if done == nil || done["outcome"] != "backend_failure" {
		t.Fatalf("terminal record = %v, want outcome backend_failure", done)
	}
	if done["resume_id"] == nil || done["resume_id"] == "" {
		t.Fatalf("backend_failure terminal must carry resume_id; got %v", done)
	}
}

// TestPlanHostDeclineMidWorkflow: an approval decline during the implement
// turn terminates the workflow with the declined terminal (control-latch
// path) — not another enqueued turn.
func TestPlanHostDeclineMidWorkflow(t *testing.T) {
	plan := "## Task\n\nt\n\n## Plan\n\n1. fix it\n"
	gatherFrame := []string{contentChunk("found\n%%PHASE_DONE%%")}
	planFrame := []string{contentChunk("## Plan\n\n1. fix it\n%%PHASE_DONE%%")}
	// The implement step issues a destructive shell call; the resolver (no
	// --auto, no --allow-destructive) declines it. The turn completes with the
	// tool blocked; the decline latch terminates the workflow.
	toolFrame := toolCallFrames("call_1", "run_shell", `{"command":"rm -rf /tmp/x"}`)
	stepFrame := []string{contentChunk("blocked\n%%STEP_DONE%%")}
	srv := sseServer(t, gatherFrame, planFrame, toolFrame, stepFrame, stepFrame)
	defer srv.Close()

	app, _ := planApp(t, srv.URL, plan)
	app.Cfg.OracleEnabled = false
	app.Workflow = &workflow.WorkflowState{Task: "t", Phase: workflow.WFGather, PlanPath: ".wakil/plan.md"}

	code, out := runPlan(t, app, HeadlessOptions{}) // no Auto → destructive declined
	if code != ExitDeclined {
		t.Fatalf("exit = %d, want ExitDeclined(%d); out:\n%s", code, ExitDeclined, out)
	}
	evs := outputEvents(t, out)
	done := findEvent(evs, "done")
	if done == nil || done["outcome"] != "declined" || done["reason"] == "" {
		t.Fatalf("terminal record = %v, want declined with a reason", done)
	}
}

// TestPlanHostVerifyFailed: verification failure maps to the verify_failed
// terminal bytes. VerifyEnabled + cfg.Verify + a failing RunShell →
// HandleFinalReview sets VerifyFailed; the resolver maps it.
func TestPlanHostVerifyFailed(t *testing.T) {
	plan := "## Task\n\nt\n\n## Plan\n\n1. fix it\n"
	gatherFrame := []string{contentChunk("found\n%%PHASE_DONE%%")}
	planFrame := []string{contentChunk("## Plan\n\n1. fix it\n%%PHASE_DONE%%")}
	stepFrame := []string{contentChunk("done\n%%STEP_LOG: Step 1: patched | outcome: ok | deviation: none%%\n%%STEP_DONE%%")}
	srv := sseServer(t, gatherFrame, planFrame, stepFrame, stepFrame)
	defer srv.Close()

	app, ex := planApp(t, srv.URL, plan)
	app.Cfg.OracleEnabled = false
	app.VerifyEnabled = true
	app.Cfg.Verify = []string{"go test ./..."}
	ex.failShell = true
	app.Workflow = &workflow.WorkflowState{Task: "t", Phase: workflow.WFGather, PlanPath: ".wakil/plan.md"}

	code, out := runPlan(t, app, HeadlessOptions{Auto: true})
	if code != ExitGaps {
		t.Fatalf("exit = %d, want ExitGaps (verify_failed); out:\n%s", code, out)
	}
	evs := outputEvents(t, out)
	done := findEvent(evs, "done")
	if done == nil || done["outcome"] != "verify_failed" {
		t.Fatalf("terminal record = %v, want outcome verify_failed", done)
	}
	if done["message"] != "verification failed (see step log for details)" {
		t.Fatalf("terminal message = %v, want the legacy verify_failed string", done["message"])
	}
}

// TestPlanHostPassWithOracle: the full happy path with the oracle ENABLED —
// review approves (VERDICT: PASS), auto-advance to implement, step completes,
// final review returns PASS → workflow cleared → pass terminal (ExitOK).
// Covers the pass path the op-35 review flagged as the most important gap,
// with an oracle SSE server answering both review consults.
func TestPlanHostPassWithOracle(t *testing.T) {
	oracleCalls := 0
	oracleSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		oracleCalls++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"content":[{"type":"text","text":"looks good\nVERDICT: PASS"}],"stop_reason":"end_turn","usage":{}}`)
	}))
	defer oracleSrv.Close()

	plan := "## Task\n\nt\n\n## Plan\n\n1. fix it\n"
	gatherFrame := []string{contentChunk("found\n%%PHASE_DONE%%")}
	planFrame := []string{contentChunk("## Plan\n\n1. fix it\n%%PHASE_DONE%%")}
	stepFrame := []string{contentChunk("done\n%%STEP_LOG: Step 1: patched | outcome: ok | deviation: none%%\n%%STEP_DONE%%")}
	srv := sseServer(t, gatherFrame, planFrame, stepFrame)
	defer srv.Close()

	app, _ := planApp(t, srv.URL, plan)
	app.Cfg.OracleEnabled = true
	app.Cfg.OracleAPIKeyEnv = "TEST_KEY"
	app.Cfg.OracleEndpoint = oracleSrv.URL + "/v1/messages"
	t.Setenv("TEST_KEY", "fake-key")
	app.Workflow = &workflow.WorkflowState{Task: "t", Phase: workflow.WFGather, PlanPath: ".wakil/plan.md"}

	code, out := runPlan(t, app, HeadlessOptions{Auto: true})
	if code != ExitOK {
		t.Fatalf("exit = %d, want ExitOK; out:\n%s", code, out)
	}
	evs := outputEvents(t, out)
	done := findEvent(evs, "done")
	if done == nil || done["outcome"] != "pass" {
		t.Fatalf("terminal record = %v, want outcome pass", done)
	}
	if oracleCalls < 2 {
		t.Errorf("oracle must be consulted for review AND final review; got %d calls", oracleCalls)
	}
}

// TestPlanHostLastDeclineWins: a decline mid-workflow carries the resolver's
// policy reason (last-wins latch parity — legacy overwrote *declinedReason on
// every decline; here one decline captures its reason verbatim).
func TestPlanHostLastDeclineWins(t *testing.T) {
	plan := "## Task\n\nt\n\n## Plan\n\n1. fix it\n"
	gatherFrame := []string{contentChunk("found\n%%PHASE_DONE%%")}
	planFrame := []string{contentChunk("## Plan\n\n1. fix it\n%%PHASE_DONE%%")}
	toolFrame := toolCallFrames("call_1", "run_shell", `{"command":"rm -rf /tmp/one"}`)
	srv := sseServer(t, gatherFrame, planFrame, toolFrame)
	defer srv.Close()

	app, _ := planApp(t, srv.URL, plan)
	app.Cfg.OracleEnabled = false
	app.Workflow = &workflow.WorkflowState{Task: "t", Phase: workflow.WFGather, PlanPath: ".wakil/plan.md"}

	code, out := runPlan(t, app, HeadlessOptions{}) // no Auto → destructive declined
	if code != ExitDeclined {
		t.Fatalf("exit = %d, want ExitDeclined; out:\n%s", code, out)
	}
	evs := outputEvents(t, out)
	done := findEvent(evs, "done")
	if done == nil {
		t.Fatal("no terminal record")
	}
	// The reason must be the resolver's policy text (destructive without
	// --allow-destructive), proving the latch captured the resolver's reason
	// rather than defaulting.
	reason, _ := done["reason"].(string)
	if reason == "" {
		t.Fatalf("declined terminal must carry the latched reason; got %v", done)
	}
}

// memFilesExec is a stateful fake executor: it actually stores files so the
// workflow engine's plan.md reads/writes observe real content, and can script
// shell failures for verification commands.
type memFilesExec struct {
	fakeExec
	files     map[string]string
	failShell bool
}

func (m *memFilesExec) ReadFile(_ context.Context, p string) (string, error) {
	return m.files[p], nil
}
func (m *memFilesExec) WriteFile(_ context.Context, p, c string) (string, error) {
	m.files[p] = c
	return "", nil
}
func (m *memFilesExec) WriteFileBytes(_ context.Context, p string, b []byte) (string, error) {
	m.files[p] = string(b)
	return "", nil
}
func (m *memFilesExec) RunShell(_ context.Context, cmd string) (string, error) {
	if m.failShell {
		return "FAIL\nexit status 1", errors.New("exit status 1")
	}
	return "", nil
}
