package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"wakil/internal/config"
	"wakil/internal/counsel"
	"wakil/internal/proxy"
	"wakil/internal/workflow"
)

// mashuraTestApp builds an App in a workflow with a fake executor whose plan.md
// carries the given findings/plan/step-log, ready for the briefing builders.
func mashuraTestApp(t *testing.T, findings, plan, stepLog string) (*App, *fakeExecutor) {
	t.Helper()
	fe := newFakeExecutor()
	planPath := ".wakil/plan.md"
	fe.files[planPath] = "## Findings\n\n" + findings + "\n\n## Plan\n\n" + plan + "\n\n## Step log\n\n" + stepLog + "\n"
	app := &App{
		Cfg:      config.Config{OracleAPIKeyEnv: "MASHURA_TEST_KEY", OracleModel: "test-model", OracleMaxTokens: 4096, OracleEnabled: true},
		Exec:     fe,
		Client:   &proxy.Client{},
		Out:      io.Discard,
		Workflow: &workflow.WorkflowState{Task: "ship the feature", PlanPath: planPath, Phase: workflow.WFImplement, StepIdx: 2, StepCount: 5},
	}
	return app, fe
}

func tcArgs(name, args string) proxy.ToolCall {
	return proxy.ToolCall{Function: proxy.FunctionCall{Name: name, Arguments: args}}
}

// Each tool's briefing carries the shared, authoritative core (task + workflow
// position + cwd) plus its own tool-specific section.
func TestMashuraBriefingsCarryCoreAndSpecificSection(t *testing.T) {
	app, _ := mashuraTestApp(t, "the datastore is Postgres", "1. wire it up", "[step 1] wired")

	cases := []struct {
		name     string
		tc       proxy.ToolCall
		specific []string // sections/markers unique to this tool
	}{
		{"mashura__review", tcArgs("mashura__review", `{"focus":"check the migration"}`),
			[]string{"## Findings", "## Plan", "## Focus", "check the migration"}},
		{"mashura__decide", tcArgs("mashura__decide", `{"question":"which db","options":"Postgres\nMySQL"}`),
			[]string{"## Findings", "## Decision", "## Options", "MySQL"}},
		{"mashura__check", tcArgs("mashura__check", `{"claim":"step 1 is done"}`),
			[]string{"## Claim", "step 1 is done"}},
		{"mashura__debug", tcArgs("mashura__debug", `{"symptom":"tests hang"}`),
			[]string{"## Symptom", "tests hang"}},
	}

	ctx := context.Background()
	for _, c := range cases {
		var briefing string
		var err error
		switch c.name {
		case "mashura__review":
			_, briefing, _, err = app.mashuraReview(ctx, c.tc)
		case "mashura__decide":
			_, briefing, _, err = app.mashuraDecide(ctx, c.tc)
		case "mashura__check":
			_, briefing, _, err = app.mashuraCheck(ctx, c.tc)
		case "mashura__debug":
			_, briefing, _, err = app.mashuraDebug(ctx, c.tc)
		}
		if err != nil {
			t.Errorf("%s: unexpected error: %v", c.name, err)
			continue
		}
		// Shared core, always present.
		for _, core := range []string{"## Task", "ship the feature", "## Workflow position", "## Working directory"} {
			if !strings.Contains(briefing, core) {
				t.Errorf("%s briefing missing shared-core marker %q", c.name, core)
			}
		}
		// Tool-specific sections.
		for _, s := range c.specific {
			if !strings.Contains(briefing, s) {
				t.Errorf("%s briefing missing specific marker %q\n---\n%s", c.name, s, briefing)
			}
		}
	}
}

// debug includes the recent tool-call evidence with the FULL EXIT≠0 tail — more
// than the step log's 4-line cap.
func TestMashuraDebugIncludesFullErrorTail(t *testing.T) {
	app, _ := mashuraTestApp(t, "f", "p", "s")

	// A failing run_shell whose output has 20 distinct lines. The step-log cap
	// keeps only the last 4; the debug error tail keeps the last 15.
	var out strings.Builder
	out.WriteString("ERROR: build failed\n")
	for i := 1; i <= 20; i++ {
		fmt.Fprintf(&out, "ERRLINE-%02d\n", i)
	}
	app.recordRecentTrace(tcArgs("run_shell", `{"command":"go build ./..."}`), out.String())

	ctx := context.Background()
	_, briefing, _, err := app.mashuraDebug(ctx, tcArgs("mashura__debug", `{"symptom":"build keeps failing"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(briefing, "## Recent tool calls") {
		t.Fatal("debug briefing missing the recent tool-call section")
	}
	// ERRLINE-06 is inside the 15-line generous tail but outside the 4-line cap.
	if !strings.Contains(briefing, "ERRLINE-06") {
		t.Errorf("debug briefing lacks the generous EXIT≠0 tail (no ERRLINE-06)\n---\n%s", briefing)
	}
	// The trailing lines are present too.
	if !strings.Contains(briefing, "ERRLINE-20") {
		t.Error("debug briefing lacks the most recent error line")
	}
	// The question template asks for the cheapest next experiment.
	q, _, _, _ := app.mashuraDebug(ctx, tcArgs("mashura__debug", `{"symptom":"x"}`))
	if !strings.Contains(q, "cheapest next experiment") {
		t.Errorf("debug question template = %q, want it to ask for the cheapest next experiment", q)
	}
}

// decide gives the mashūra both the findings and the options, and instructs it to
// flag any option that contradicts a stated finding.
func TestMashuraDecideFlagsContradiction(t *testing.T) {
	app, _ := mashuraTestApp(t, "the datastore is Postgres (decided in GATHER)", "1. build", "[step 1] done")
	q, briefing, _, err := app.mashuraDecide(context.Background(), tcArgs("mashura__decide",
		`{"question":"which database driver","options":"pgx (Postgres)\nmysql-connector (MySQL)"}`))
	if err != nil {
		t.Fatal(err)
	}
	// Both the constraint (finding) and the contradicting option must be present so
	// the mashūra can detect the contradiction.
	if !strings.Contains(briefing, "Postgres") {
		t.Error("decide briefing missing the Postgres finding")
	}
	if !strings.Contains(briefing, "MySQL") {
		t.Error("decide briefing missing the contradicting MySQL option")
	}
	if !strings.Contains(q, "contradicts a stated finding") {
		t.Errorf("decide question = %q, want it to ask to flag contradictions", q)
	}
}

// check uses a lower max_tokens than review, and stays under its cap.
func TestMashuraCheckStaysUnderTokenCap(t *testing.T) {
	app, _ := mashuraTestApp(t, "f", "p", "s")

	check := app.mashuraMaxTokensFor("mashura__check")
	review := app.mashuraMaxTokensFor("mashura__review")
	if check > mashuraCheckMaxTokens {
		t.Errorf("check max_tokens = %d, want <= %d", check, mashuraCheckMaxTokens)
	}
	if check >= review {
		t.Errorf("check max_tokens (%d) must be less than review (%d)", check, review)
	}

	// Per-tool override is honored.
	app.Cfg.MashuraToolMaxTokens = map[string]int{"check": 512}
	if got := app.mashuraMaxTokensFor("mashura__check"); got != 512 {
		t.Errorf("check override = %d, want 512", got)
	}
}

// All four tools (and the legacy oracle__ask alias) hit the human gate and are
// never auto-approved.
func TestMashuraToolsAlwaysGate(t *testing.T) {
	app := &App{AutoApprove: true}
	for _, name := range []string{"mashura__review", "mashura__debug", "mashura__decide", "mashura__check", "oracle__ask"} {
		if reason := SuspendAuto(name, app, "detail"); reason == "" {
			t.Errorf("%s: suspendAuto returned no reason — it would auto-approve", name)
		}
		if !ShouldGateEvenWithAutoApprove(name, app, "detail") {
			t.Errorf("%s: must gate even in auto mode", name)
		}
	}
}

// The human gate fires for every tool: a declining confirmer short-circuits the
// call (no network, no cost) and the real tool name reaches Confirm.
func TestMashuraGateDeclineShortCircuits(t *testing.T) {
	os.Setenv("MASHURA_TEST_KEY", "x")
	defer os.Unsetenv("MASHURA_TEST_KEY")
	app, _ := mashuraTestApp(t, "f", "1. do it", "[step 1] done")

	var gated []string
	app.Confirm = func(toolName, _, _ string, _ bool) bool {
		gated = append(gated, toolName)
		return false
	}
	calls := map[string]string{
		"mashura__review": `{"focus":"x"}`,
		"mashura__debug":  `{"symptom":"x"}`,
		"mashura__decide": `{"question":"x","options":"a\nb"}`,
		"mashura__check":  `{"claim":"x"}`,
		"oracle__ask":     `{"question":"x"}`,
	}
	for name, args := range calls {
		got := app.handleMashura(context.Background(), name, tcArgs(name, args))
		if got != "[declined by user]" {
			t.Errorf("%s: got %q, want decline", name, got)
		}
	}
	for name := range calls {
		found := false
		for _, g := range gated {
			if g == name {
				found = true
			}
		}
		if !found {
			t.Errorf("%s never reached the confirm gate", name)
		}
	}
}

// Fail-closed: a tool with its required intent missing returns an ERROR and never
// reaches the gate or the network.
func TestMashuraFailsClosed(t *testing.T) {
	os.Setenv("MASHURA_TEST_KEY", "x")
	defer os.Unsetenv("MASHURA_TEST_KEY")
	app, _ := mashuraTestApp(t, "f", "p", "s")
	app.Confirm = func(_, _, _ string, _ bool) bool {
		t.Fatal("gate must not fire when required intent is missing")
		return false
	}
	cases := map[string]string{
		"mashura__debug":  `{"files":["x"]}`,       // no symptom
		"mashura__decide": `{"question":"x"}`,      // no options
		"mashura__check":  `{"evidence_file":"x"}`, // no claim
	}
	for name, args := range cases {
		got := app.handleMashura(context.Background(), name, tcArgs(name, args))
		if !strings.HasPrefix(got, "ERROR:") {
			t.Errorf("%s with missing intent: got %q, want ERROR", name, got)
		}
	}
}

func TestMashuraToolDefs(t *testing.T) {
	defs := mashuraToolDefs()
	want := map[string]bool{"mashura__review": false, "mashura__debug": false, "mashura__decide": false, "mashura__check": false}
	for _, d := range defs {
		if _, ok := want[d.Function.Name]; ok {
			want[d.Function.Name] = true
		} else {
			t.Errorf("unexpected mashura tool %q", d.Function.Name)
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("missing mashura tool %q", name)
		}
	}
}

func TestDetectStruggle(t *testing.T) {
	fail := ToolTraceEntry{Abbrev: "shell", Command: "go test", ExitErr: true, FirstLine: "FAIL"}
	ok := ToolTraceEntry{Abbrev: "read", Command: "main.go"}

	if _, d := DetectStruggle([]ToolTraceEntry{fail}); d {
		t.Error("single entry should not trigger")
	}
	if _, d := DetectStruggle([]ToolTraceEntry{ok, ok2()}); d {
		t.Error("two unrelated ok calls should not trigger")
	}
	if s, d := DetectStruggle([]ToolTraceEntry{fail, fail}); !d || !strings.Contains(s, "repeated failures") {
		t.Errorf("two failures should trigger; got %q, %v", s, d)
	}
	twice := ToolTraceEntry{Abbrev: "shell", Command: "make"}
	if s, d := DetectStruggle([]ToolTraceEntry{twice, twice}); !d || !strings.Contains(s, "twice") {
		t.Errorf("same command twice should trigger; got %q, %v", s, d)
	}
	w := ToolTraceEntry{Abbrev: "edit", Command: "x.go"}
	if s, d := DetectStruggle([]ToolTraceEntry{w, ok, w, ok, w}); !d || !strings.Contains(s, "rewrote") {
		t.Errorf("three rewrites should trigger; got %q, %v", s, d)
	}
}

func ok2() ToolTraceEntry { return ToolTraceEntry{Abbrev: "list", Command: "."} }

// ── P27: source-reading pipeline tests ───────────────────────────────────────

// Wakil reads the current bytes from disk — the briefing contains file contents
// even though the model supplied no content field.
func TestSourcesReadFromDisk(t *testing.T) {
	app, fe := mashuraTestApp(t, "f", "1. do it", "[step 1] done")
	fe.files["cmd/main.go"] = "package main\n\nfunc main() {}\n"

	_, briefing, receipts, err := app.mashuraReview(context.Background(),
		tcArgs("mashura__review", `{"focus":"check it","paths":["cmd/main.go"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(briefing, "package main") {
		t.Error("briefing should contain file contents read from disk")
	}
	if !strings.Contains(briefing, "## Sources") {
		t.Error("briefing should have a ## Sources section")
	}
	if len(receipts) != 1 || receipts[0].Path != "cmd/main.go" {
		t.Errorf("expected 1 receipt for cmd/main.go, got %v", receipts)
	}
}

// A free-text "context" field in the tool args (legacy / model hallucination)
// is silently ignored — it never appears in the briefing as a Sources section.
func TestContextFieldIgnoredNotSources(t *testing.T) {
	app, _ := mashuraTestApp(t, "f", "1. do it", "[step 1] done")

	_, briefing, _, err := app.mashuraReview(context.Background(),
		tcArgs("mashura__review", `{"focus":"check it","context":"PASTED_CONTENT_SHOULD_NOT_APPEAR"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(briefing, "PASTED_CONTENT_SHOULD_NOT_APPEAR") {
		t.Error("model-pasted context field must not appear in the briefing")
	}
}

// Oversized file: contents are clipped at the per-file cap with an explicit
// marker; the omitted portion is not silently discarded.
func TestSourcesOversizedFileClipped(t *testing.T) {
	app, fe := mashuraTestApp(t, "f", "1. do it", "[step 1] done")
	// Build a file larger than mashuraSourceFileCap.
	var sb strings.Builder
	for i := 0; i < mashuraSourceFileCap/20+10; i++ {
		fmt.Fprintf(&sb, "line %05d: padding padding padding\n", i+1)
	}
	bigFile := sb.String()
	totalLines := strings.Count(bigFile, "\n") + 1
	fe.files["big.go"] = bigFile

	_, briefing, receipts, err := app.mashuraReview(context.Background(),
		tcArgs("mashura__review", `{"focus":"x","paths":["big.go"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(briefing, "truncated") {
		t.Error("oversized file must show truncation marker in briefing")
	}
	if len(receipts) != 1 || !receipts[0].Clipped {
		t.Error("receipt for oversized file must have Clipped=true")
	}
	if receipts[0].Lines >= totalLines {
		t.Errorf("clipped receipt should show fewer lines than total; got %d/%d", receipts[0].Lines, totalLines)
	}
}

// A file is dropped whole (not partially clipped) when the total budget is
// exhausted; its omission is reported explicitly in the briefing and receipts.
func TestSourcesTotalBudgetDropsWhole(t *testing.T) {
	app, fe := mashuraTestApp(t, "f", "1. do it", "[step 1] done")
	// Four files at the per-file cap fill 4×32 KB = 128 KB of the 150 KB budget.
	// The fifth file (>32 KB) would require 32 KB more but only 22 KB remain → omitted.
	bigContent := strings.Repeat("padding ", (mashuraSourceFileCap/8)+1) // > 32 KB
	paths := make([]string, 0, 5)
	for i := 1; i <= 4; i++ {
		name := fmt.Sprintf("big%d.go", i)
		fe.files[name] = bigContent
		paths = append(paths, name)
	}
	fe.files["overflow.go"] = bigContent
	paths = append(paths, "overflow.go")

	pathsJSON, _ := json.Marshal(paths)
	_, briefing, receipts, err := app.mashuraReview(context.Background(),
		tcArgs("mashura__review", fmt.Sprintf(`{"focus":"x","paths":%s}`, pathsJSON)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(briefing, "overflow.go") {
		t.Error("omitted file should still be mentioned in the briefing")
	}
	if !strings.Contains(briefing, "omitted") {
		t.Error("omission must be reported explicitly in the briefing")
	}
	var omitted bool
	for _, r := range receipts {
		if r.Path == "overflow.go" && r.Omitted {
			omitted = true
		}
	}
	if !omitted {
		t.Error("receipt for overflow.go must have Omitted=true")
	}
}

// A bad path returns an error before the gate or any API call fires.
func TestSourcesBadPathErrorsPreAPI(t *testing.T) {
	os.Setenv("MASHURA_TEST_KEY", "x")
	defer os.Unsetenv("MASHURA_TEST_KEY")

	app, _ := mashuraTestApp(t, "f", "1. do it", "[step 1] done")
	app.Confirm = func(_, _, _ string, _ bool) bool {
		t.Fatal("gate must not fire when path is bad")
		return false
	}

	got := app.handleMashura(context.Background(), "mashura__review",
		tcArgs("mashura__review", `{"focus":"x","paths":["nonexistent/path.go"]}`))
	if !strings.HasPrefix(got, "ERROR:") {
		t.Errorf("bad path should return ERROR, got: %q", got)
	}
	if !strings.Contains(got, "nonexistent/path.go") {
		t.Errorf("error should name the bad path, got: %q", got)
	}
}

// path_ranges: only the specified line span is included.
func TestSourcesPathRange(t *testing.T) {
	app, fe := mashuraTestApp(t, "f", "1. do it", "[step 1] done")
	var sb strings.Builder
	for i := 1; i <= 50; i++ {
		fmt.Fprintf(&sb, "LINE_%03d\n", i)
	}
	fe.files["range_test.go"] = sb.String()

	_, briefing, _, err := app.mashuraReview(context.Background(),
		tcArgs("mashura__review", `{"focus":"x","path_ranges":[{"path":"range_test.go","start_line":10,"end_line":20}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(briefing, "LINE_010") {
		t.Error("briefing should contain line 10")
	}
	if !strings.Contains(briefing, "LINE_020") {
		t.Error("briefing should contain line 20")
	}
	if strings.Contains(briefing, "LINE_001") {
		t.Error("briefing should not contain line 1 (outside range)")
	}
	if strings.Contains(briefing, "LINE_050") {
		t.Error("briefing should not contain line 50 (outside range)")
	}
}

// Directory expansion: a directory path expands to its source files.
func TestSourcesDirectoryExpansion(t *testing.T) {
	app, fe := mashuraTestApp(t, "f", "1. do it", "[step 1] done")
	fe.dirs["mydir"] = true
	fe.files["mydir/a.go"] = "package a"
	fe.files["mydir/b.go"] = "package b"
	// git ls-files is mocked via shellResult to return the two source files.
	fe.shellResult = "mydir/a.go\nmydir/b.go\n"

	_, briefing, receipts, err := app.mashuraReview(context.Background(),
		tcArgs("mashura__review", `{"focus":"x","paths":["mydir"]}`))
	if err != nil {
		t.Fatalf("directory expansion failed: %v", err)
	}
	if !strings.Contains(briefing, "package a") {
		t.Error("briefing should contain expanded file contents")
	}
	if !strings.Contains(briefing, "package b") {
		t.Error("briefing should contain second expanded file")
	}
	if len(receipts) != 2 {
		t.Errorf("expected 2 receipts for expanded directory, got %d", len(receipts))
	}
	// Verify git ls-files was called.
	if len(fe.shellCalls) == 0 {
		t.Error("RunShell should be called for directory expansion")
	}
	gitCmd := false
	for _, c := range fe.shellCalls {
		if strings.Contains(c, "git ls-files") {
			gitCmd = true
		}
	}
	if !gitCmd {
		t.Error("directory expansion should use git ls-files to respect .gitignore")
	}
}

// Read receipts are emitted to a.Out and, when in a workflow, appended to the step log.
func TestSourcesReadReceiptsLogged(t *testing.T) {
	os.Setenv("MASHURA_TEST_KEY", "x")
	defer os.Unsetenv("MASHURA_TEST_KEY")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	app, fe := mashuraTestApp(t, "f", "1. do it", "[step 1] done")
	fe.files["src/lib.go"] = "package lib"
	app.Cfg.OracleEndpoint = srv.URL + "/v1/messages"

	var outBuf strings.Builder
	app.Out = &outBuf
	app.Confirm = func(_, _, _ string, _ bool) bool { return true }

	app.handleMashura(context.Background(), "mashura__review",
		tcArgs("mashura__review", `{"focus":"x","paths":["src/lib.go"]}`))

	// Receipt must appear in a.Out.
	if !strings.Contains(outBuf.String(), "[mashura] sent") {
		t.Errorf("read receipt not emitted to Out; got: %q", outBuf.String())
	}
	if !strings.Contains(outBuf.String(), "src/lib.go") {
		t.Errorf("receipt must name the file; got: %q", outBuf.String())
	}

	// Receipt must also be appended to the step log.
	planContent := fe.files[".wakil/plan.md"]
	if !strings.Contains(planContent, "[mashura] sent") {
		t.Error("read receipt should be appended to step log when in workflow")
	}
}

// ── P26: multi-model panel tests ──────────────────────────────────────────────

// panelFlagsGaps is the fail-closed gating predicate: ALL responding models must
// PASS; any GAPS (or no responders) keeps the workflow open.
func TestPanelFlagsGaps(t *testing.T) {
	pass := counsel.PanelMemberResult{Model: "m1", Answer: "all good.\nVERDICT: PASS"}
	gaps := counsel.PanelMemberResult{Model: "m2", Answer: "missing step 3.\nVERDICT: GAPS"}
	errR := counsel.PanelMemberResult{Model: "m3", Err: fmt.Errorf("timeout")}

	if panelFlagsGaps([]counsel.PanelMemberResult{pass, pass}) {
		t.Error("all PASS: should not flag gaps")
	}
	if !panelFlagsGaps([]counsel.PanelMemberResult{pass, gaps}) {
		t.Error("PASS + GAPS: fail-closed — should flag gaps")
	}
	if !panelFlagsGaps([]counsel.PanelMemberResult{errR}) {
		t.Error("all errors (no responders): fail-closed — should flag gaps")
	}
	if panelFlagsGaps([]counsel.PanelMemberResult{errR, pass}) {
		t.Error("error + PASS: error excluded from verdict — should pass")
	}
	if !panelFlagsGaps([]counsel.PanelMemberResult{errR, gaps}) {
		t.Error("error + GAPS: should flag gaps")
	}
	if !panelFlagsGaps(nil) {
		t.Error("empty results (no responders): fail-closed — should flag gaps")
	}
}

// The human gate fires exactly once for the whole panel — not once per member.
func TestPanelSingleGateForWholePanel(t *testing.T) {
	os.Setenv("MASHURA_TEST_KEY", "x")
	defer os.Unsetenv("MASHURA_TEST_KEY")

	// A fake oracle endpoint that returns valid Anthropic-format responses.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	app, _ := mashuraTestApp(t, "f", "1. do it", "[step 1] done")
	// Two-model panel (both anthropic so they share the test endpoint).
	app.Cfg.MashuraPanels = map[string]config.MashuraPanelConfig{
		"default": {
			Models: []string{"anthropic:claude-opus-4-8", "anthropic:claude-fable-5"},
			Mode:   "panel",
		},
	}
	app.Cfg.OracleEndpoint = srv.URL + "/v1/messages"

	confirmCount := 0
	app.Confirm = func(_, _, _ string, _ bool) bool {
		confirmCount++
		return true // approve
	}

	got := app.handleMashura(context.Background(), "mashura__review",
		tcArgs("mashura__review", `{"focus":"check it"}`))

	// Gate fires exactly once for the whole panel.
	if confirmCount != 1 {
		t.Errorf("confirm count = %d, want 1 (single gate for panel)", confirmCount)
	}
	// Multi-model result includes per-model section labels.
	if !strings.Contains(got, "── claude-opus-4-8 ──") {
		t.Errorf("result missing model section label; got: %q", got)
	}
	if !strings.Contains(got, "── claude-fable-5 ──") {
		t.Errorf("result missing second model label; got: %q", got)
	}
}

// Cost tracker records each panel member's usage under its own per-model row
// "mashura·<model>", so billing is split by model (P30).
func TestPanelCostPerModel(t *testing.T) {
	os.Setenv("MASHURA_TEST_KEY", "x")
	defer os.Unsetenv("MASHURA_TEST_KEY")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 10 input + 5 output tokens each call.
		w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`))
	}))
	defer srv.Close()

	app, _ := mashuraTestApp(t, "f", "1. do it", "[step 1] done")
	app.Costs = proxy.NewCostTracker()
	app.Cfg.MashuraPanels = map[string]config.MashuraPanelConfig{
		"default": {
			Models: []string{"anthropic:model-a", "anthropic:model-b"},
			Mode:   "panel",
		},
	}
	app.Cfg.OracleEndpoint = srv.URL + "/v1/messages"
	app.Confirm = func(_, _, _ string, _ bool) bool { return true }

	app.handleMashura(context.Background(), "mashura__review",
		tcArgs("mashura__review", `{"focus":"x"}`))

	_, rows := app.Costs.Snapshot()

	// Each panel member gets its own row: "mashura·anthropic:model-a" and
	// "mashura·anthropic:model-b". No row should use the aggregate "mashura" key.
	for _, r := range rows {
		if r.Source == proxy.CostSourceMashura {
			t.Errorf("found aggregate %q row — expected per-model rows only", proxy.CostSourceMashura)
		}
	}

	// RecordOracleCostFor uses r.Model (the bare model ID, without provider prefix).
	keyA := proxy.CostSourceMashuraPrefix + "model-a"
	keyB := proxy.CostSourceMashuraPrefix + "model-b"
	var foundA, foundB bool
	for _, r := range rows {
		switch r.Source {
		case keyA:
			foundA = true
			if r.Calls != 1 || r.InputTok != 10 || r.OutputTok != 5 {
				t.Errorf("model-a row: calls=%d in=%d out=%d, want 1/10/5", r.Calls, r.InputTok, r.OutputTok)
			}
		case keyB:
			foundB = true
			if r.Calls != 1 || r.InputTok != 10 || r.OutputTok != 5 {
				t.Errorf("model-b row: calls=%d in=%d out=%d, want 1/10/5", r.Calls, r.InputTok, r.OutputTok)
			}
		}
	}
	if !foundA {
		t.Errorf("missing per-model row %q; rows: %+v", keyA, rows)
	}
	if !foundB {
		t.Errorf("missing per-model row %q; rows: %+v", keyB, rows)
	}
}

// Per-call panel override: passing "panel" in the tool args routes to that
// panel instead of the default.
func TestPanelPerCallOverride(t *testing.T) {
	os.Setenv("MASHURA_TEST_KEY", "x")
	defer os.Unsetenv("MASHURA_TEST_KEY")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"content":[{"type":"text","text":"resilient answer"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	app, _ := mashuraTestApp(t, "f", "1. do it", "[step 1] done")
	app.Cfg.MashuraPanels = map[string]config.MashuraPanelConfig{
		"default":   {Models: []string{"anthropic:default-model"}, Mode: "panel"},
		"resilient": {Models: []string{"anthropic:fallback-model"}, Mode: "fallback"},
	}
	app.Cfg.OracleEndpoint = srv.URL + "/v1/messages"

	var capturedDetail string
	app.Confirm = func(_, _, detail string, _ bool) bool {
		capturedDetail = detail
		return false // decline — we just want to inspect the detail
	}

	// Pass the panel override in the JSON args.
	app.handleMashura(context.Background(), "mashura__review",
		tcArgs("mashura__review", `{"focus":"x","panel":"resilient"}`))

	// The detail should show "fallback-model", not "default-model".
	if !strings.Contains(capturedDetail, "fallback-model") {
		t.Errorf("detail does not reference the overridden panel model; got: %q", capturedDetail)
	}
}

// Config: the canonical mashura_* keys win over the legacy oracle_* spelling.
func TestMashuraConfigAliasWins(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.json"
	if err := os.WriteFile(path, []byte(`{
		"base_url": "http://x:1",
		"oracle_max_tokens": 4096,
		"oracle_timeout_seconds": 300,
		"mashura_max_tokens": 8192,
		"mashura_timeout_seconds": 120
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig([]string{"--config", path})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OracleMaxTokens != 8192 {
		t.Errorf("OracleMaxTokens = %d, want 8192 (mashura_* wins)", cfg.OracleMaxTokens)
	}
	if cfg.OracleTimeoutSeconds != 120 {
		t.Errorf("OracleTimeoutSeconds = %d, want 120 (mashura_* wins)", cfg.OracleTimeoutSeconds)
	}
}

// --- P32 addendum: auto-counsel tests ---

// counselServer returns a test server that responds to oracle calls with a fixed
// JSON answer and records how many times it was called.
func counselServer(t *testing.T) (*httptest.Server, *int) {
	t.Helper()
	count := new(int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*count++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"content":[{"type":"text","text":"diagnosis: fix the loop exit condition"}],"stop_reason":"end_turn","usage":{"input_tokens":100,"output_tokens":50}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, count
}

// buildStruggleTraces returns recentTraces that trigger DetectStruggle (two
// consecutive failures on the same command).
func buildStruggleTraces() []ToolTraceEntry {
	e := ToolTraceEntry{
		Abbrev:    "shell",
		Command:   "go test ./...",
		ExitErr:   true,
		FirstLine: "FAIL",
	}
	return []ToolTraceEntry{e, e} // same command, both fail → streak triggers
}

// TestAutoCounselBareRunZeroCalls confirms that without --auto-counsel
// (AutoCounsel=false) the struggle detector fires no mashūra call.
func TestAutoCounselBareRunZeroCalls(t *testing.T) {
	srv, count := counselServer(t)
	os.Setenv("MASHURA_TEST_KEY", "x")
	defer os.Unsetenv("MASHURA_TEST_KEY")

	app := &App{
		Cfg: config.Config{
			OracleAPIKeyEnv:      "MASHURA_TEST_KEY",
			OracleModel:          "anthropic:test-model",
			OracleMaxTokens:      4096,
			OracleEnabled:        true,
			OracleEndpoint:       srv.URL + "/v1/messages",
			OracleTimeoutSeconds: 5,
		},
		Client:      &proxy.Client{},
		Out:         io.Discard,
		Confirm:     func(_, _, _ string, _ bool) bool { return true },
		// AutoCounsel intentionally off (default):
		AutoCounsel: false,
		MaxCounsel:  0,
	}
	app.recentTraces = buildStruggleTraces()

	app.maybeSuggestDebug(context.Background())

	if *count != 0 {
		t.Errorf("bare run: expected 0 oracle calls, got %d", *count)
	}
	if app.CounselCallsCount() != 0 {
		t.Errorf("bare run: CounselCallsCount = %d, want 0", app.CounselCallsCount())
	}
}

// TestAutoCounselRespectsMaxCounsel verifies that repeated struggle signals are
// capped at MaxCounsel and no further oracle calls are made after the cap.
func TestAutoCounselRespectsMaxCounsel(t *testing.T) {
	srv, count := counselServer(t)
	os.Setenv("MASHURA_TEST_KEY", "x")
	defer os.Unsetenv("MASHURA_TEST_KEY")

	const cap = 2
	app := &App{
		Cfg: config.Config{
			OracleAPIKeyEnv:      "MASHURA_TEST_KEY",
			OracleModel:          "anthropic:test-model",
			OracleMaxTokens:      4096,
			OracleEnabled:        true,
			OracleEndpoint:       srv.URL + "/v1/messages",
			OracleTimeoutSeconds: 5,
		},
		Client:      &proxy.Client{},
		Exec:        newFakeExecutor(),
		Out:         io.Discard,
		Confirm:     func(_, _, _ string, _ bool) bool { return true },
		AutoCounsel: true,
		MaxCounsel:  cap,
	}

	// Fire maybeSuggestDebug more times than the cap with distinct symptoms.
	for i := 0; i < cap+3; i++ {
		// Each iteration produces a distinct symptom to bypass the per-symptom
		// dedup (we want to test the overall cap, not the per-symptom guard).
		app.struggleSuggested = nil // reset dedup so new symptom is seen
		app.recentTraces = buildStruggleTraces()
		app.maybeSuggestDebug(context.Background())
	}

	if app.CounselCallsCount() != cap {
		t.Errorf("auto-counsel: expected exactly %d call(s), got %d", cap, app.CounselCallsCount())
	}
	if *count != cap {
		t.Errorf("auto-counsel: expected %d oracle HTTP call(s), got %d", cap, *count)
	}
}

// --- P33 tests: CounselMode ---

// TestCounselSuggestModeHintOnly verifies that CounselMode="suggest" prints a
// hint but fires zero oracle HTTP calls.
func TestCounselSuggestModeHintOnly(t *testing.T) {
	srv, count := counselServer(t)
	os.Setenv("MASHURA_TEST_KEY", "x")
	defer os.Unsetenv("MASHURA_TEST_KEY")

	var outBuf strings.Builder
	app := &App{
		Cfg: config.Config{
			OracleAPIKeyEnv:      "MASHURA_TEST_KEY",
			OracleModel:          "anthropic:test-model",
			OracleMaxTokens:      4096,
			OracleEnabled:        true,
			OracleEndpoint:       srv.URL + "/v1/messages",
			OracleTimeoutSeconds: 5,
		},
		Client:      &proxy.Client{},
		Out:         &outBuf,
		Confirm:     func(_, _, _ string, _ bool) bool { return true },
		CounselMode: "suggest",
		MaxCounsel:  3,
	}
	app.recentTraces = buildStruggleTraces()
	app.maybeSuggestDebug(context.Background())

	if *count != 0 {
		t.Errorf("suggest mode: expected 0 oracle calls, got %d", *count)
	}
	if !strings.Contains(outBuf.String(), "struggle detected") {
		t.Errorf("suggest mode: expected hint in output; got: %q", outBuf.String())
	}
}

// TestCounselAutoFiresUpToCapThenReverts verifies that CounselMode="auto" fires
// oracle calls up to MaxCounsel and then falls back to hint text.
func TestCounselAutoFiresUpToCapThenReverts(t *testing.T) {
	srv, count := counselServer(t)
	os.Setenv("MASHURA_TEST_KEY", "x")
	defer os.Unsetenv("MASHURA_TEST_KEY")

	var outBuf strings.Builder
	const cap = 2
	app := &App{
		Cfg: config.Config{
			OracleAPIKeyEnv:      "MASHURA_TEST_KEY",
			OracleModel:          "anthropic:test-model",
			OracleMaxTokens:      4096,
			OracleEnabled:        true,
			OracleEndpoint:       srv.URL + "/v1/messages",
			OracleTimeoutSeconds: 5,
		},
		Client:      &proxy.Client{},
		Exec:        newFakeExecutor(),
		Out:         &outBuf,
		Confirm:     func(_, _, _ string, _ bool) bool { return true },
		CounselMode: "auto",
		MaxCounsel:  cap,
	}

	// Fire cap+2 times with distinct symptoms to test both auto and revert paths.
	for i := 0; i < cap+2; i++ {
		app.struggleSuggested = nil
		app.recentTraces = buildStruggleTraces()
		app.maybeSuggestDebug(context.Background())
	}

	if app.CounselCallsCount() != cap {
		t.Errorf("auto mode: expected %d oracle call(s), got %d", cap, app.CounselCallsCount())
	}
	if *count != cap {
		t.Errorf("auto mode: expected %d HTTP call(s), got %d", cap, *count)
	}
	// After the cap, output should contain the suggest hint ("struggle detected").
	if !strings.Contains(outBuf.String(), "struggle detected") {
		t.Errorf("auto mode post-cap: expected suggest hint in output; got: %q", outBuf.String())
	}
}

// TestCounselCapSurvivesAutoApprove verifies that the per-turn cap holds even
// when AutoApprove=true (/auto mode), i.e. the cap is hard.
func TestCounselCapSurvivesAutoApprove(t *testing.T) {
	srv, count := counselServer(t)
	os.Setenv("MASHURA_TEST_KEY", "x")
	defer os.Unsetenv("MASHURA_TEST_KEY")

	app := &App{
		Cfg: config.Config{
			OracleAPIKeyEnv:      "MASHURA_TEST_KEY",
			OracleModel:          "anthropic:test-model",
			OracleMaxTokens:      4096,
			OracleEnabled:        true,
			OracleEndpoint:       srv.URL + "/v1/messages",
			OracleTimeoutSeconds: 5,
		},
		Client:      &proxy.Client{},
		Exec:        newFakeExecutor(),
		Out:         io.Discard,
		Confirm:     func(_, _, _ string, _ bool) bool { return true },
		CounselMode: "auto",
		MaxCounsel:  1,
		AutoApprove: true, // /auto is on — cap must still hold
	}

	for i := 0; i < 3; i++ {
		app.struggleSuggested = nil
		app.recentTraces = buildStruggleTraces()
		app.maybeSuggestDebug(context.Background())
	}

	if app.CounselCallsCount() != 1 {
		t.Errorf("cap+/auto: expected exactly 1 oracle call, got %d", app.CounselCallsCount())
	}
	if *count != 1 {
		t.Errorf("cap+/auto: expected 1 HTTP call, got %d", *count)
	}
}

// TestCounselOffSilent verifies that CounselMode="off" prints a dim note but
// fires zero oracle calls.
func TestCounselOffSilent(t *testing.T) {
	srv, count := counselServer(t)
	os.Setenv("MASHURA_TEST_KEY", "x")
	defer os.Unsetenv("MASHURA_TEST_KEY")

	var outBuf strings.Builder
	app := &App{
		Cfg: config.Config{
			OracleAPIKeyEnv:      "MASHURA_TEST_KEY",
			OracleModel:          "anthropic:test-model",
			OracleMaxTokens:      4096,
			OracleEnabled:        true,
			OracleEndpoint:       srv.URL + "/v1/messages",
			OracleTimeoutSeconds: 5,
		},
		Client:      &proxy.Client{},
		Out:         &outBuf,
		Confirm:     func(_, _, _ string, _ bool) bool { return true },
		CounselMode: "off",
	}
	app.recentTraces = buildStruggleTraces()
	app.maybeSuggestDebug(context.Background())

	if *count != 0 {
		t.Errorf("off mode: expected 0 oracle calls, got %d", *count)
	}
	if !strings.Contains(outBuf.String(), "off") {
		t.Errorf("off mode: expected 'off' in output; got: %q", outBuf.String())
	}
}

// TestAutoCounselInjectsIntoConv verifies that a fired auto-counsel call
// appends a synthetic assistant+tool pair into Conv so the model sees the
// diagnosis in its next turn.
func TestAutoCounselInjectsIntoConv(t *testing.T) {
	srv, _ := counselServer(t)
	os.Setenv("MASHURA_TEST_KEY", "x")
	defer os.Unsetenv("MASHURA_TEST_KEY")

	app := &App{
		Cfg: config.Config{
			OracleAPIKeyEnv:      "MASHURA_TEST_KEY",
			OracleModel:          "anthropic:test-model",
			OracleMaxTokens:      4096,
			OracleEnabled:        true,
			OracleEndpoint:       srv.URL + "/v1/messages",
			OracleTimeoutSeconds: 5,
		},
		Client:      &proxy.Client{},
		Exec:        newFakeExecutor(),
		Out:         io.Discard,
		Confirm:     func(_, _, _ string, _ bool) bool { return true },
		AutoCounsel: true,
		MaxCounsel:  3,
	}
	app.recentTraces = buildStruggleTraces()
	before := len(app.Conv)

	app.maybeSuggestDebug(context.Background())

	if app.CounselCallsCount() != 1 {
		t.Fatalf("expected 1 auto-counsel call; got %d", app.CounselCallsCount())
	}
	// Should have injected 2 messages: assistant (tool call) + tool (result).
	added := len(app.Conv) - before
	if added != 2 {
		t.Errorf("expected 2 injected Conv messages, got %d", added)
	}
	// The injected assistant message must have a ToolCall with mashura__debug.
	assistMsg := app.Conv[before]
	if assistMsg.Role != "assistant" || len(assistMsg.ToolCalls) == 0 {
		t.Errorf("injected assistant message malformed: role=%q calls=%d", assistMsg.Role, len(assistMsg.ToolCalls))
	}
	if assistMsg.ToolCalls[0].Function.Name != "mashura__debug" {
		t.Errorf("injected tool call name = %q, want mashura__debug", assistMsg.ToolCalls[0].Function.Name)
	}
	// The injected tool result must reference the same call ID.
	toolMsg := app.Conv[before+1]
	if toolMsg.Role != "tool" || toolMsg.ToolCallID != assistMsg.ToolCalls[0].ID {
		t.Errorf("injected tool result malformed: role=%q id=%q (want %q)", toolMsg.Role, toolMsg.ToolCallID, assistMsg.ToolCalls[0].ID)
	}
}
