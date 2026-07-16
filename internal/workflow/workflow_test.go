package workflow

import (
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"
)

// TestPhaseName verifies that each WorkflowPhase maps to its expected name.
func TestPhaseName(t *testing.T) {
	cases := []struct {
		phase WorkflowPhase
		want  string
	}{
		{WFGather, "gather"},
		{WFPlan, "plan"},
		{WFReview, "review"},
		{WFPresent, "present"},
		{WFImplement, "implement"},
		{WFDone, "done"},
	}
	for _, tc := range cases {
		w := &WorkflowState{Phase: tc.phase}
		if got := w.PhaseName(); got != tc.want {
			t.Errorf("PhaseName(%d) = %q, want %q", tc.phase, got, tc.want)
		}
	}
}

// TestEffectiveOracleMode verifies the resolution order:
// per-run override → config → default.
func TestEffectiveOracleMode(t *testing.T) {
	// Default.
	w := &WorkflowState{}
	cfg := config.DefaultConfig()
	if got := w.EffectiveOracleMode(cfg); got != "on-deviation" {
		t.Errorf("default mode = %q, want %q", got, "on-deviation")
	}

	// Config override.
	cfg.WFOracleMode = "every-step"
	if got := w.EffectiveOracleMode(cfg); got != "every-step" {
		t.Errorf("config mode = %q, want %q", got, "every-step")
	}

	// Per-run override wins over config.
	w.OracleMode = "phases-only"
	if got := w.EffectiveOracleMode(cfg); got != "phases-only" {
		t.Errorf("override mode = %q, want %q", got, "phases-only")
	}
}

// TestSidebarLabel verifies the sidebar label for various workflow states.
func TestSidebarLabel(t *testing.T) {
	cases := []struct {
		name  string
		state WorkflowState
		want  string
	}{
		{"gather", WorkflowState{Phase: WFGather}, "gather"},
		{"plan", WorkflowState{Phase: WFPlan}, "plan"},
		{"implement 2/5", WorkflowState{Phase: WFImplement, StepCount: 5, StepIdx: 2}, "implement 2/5"},
		{"verify when all steps done", WorkflowState{Phase: WFImplement, StepCount: 5, StepIdx: 6}, "verify"},
		{"with oracle mode suffix", WorkflowState{Phase: WFGather, OracleMode: "every-step"}, "gather ·every-step"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.state.SidebarLabel(); got != tc.want {
				t.Errorf("SidebarLabel() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCountPlanSteps verifies numbered-step parsing from the ## Plan section.
func TestCountPlanSteps(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    int
	}{
		{"no plan section", "## Task\n\ntask text", 0},
		{"empty plan", "## Plan\n\n(pending plan phase)", 0},
		{"three steps with dots", "## Plan\n\n1. First\n2. Second\n3. Third\n", 3},
		{"three steps with parens", "## Plan\n\n1) First\n2) Second\n3) Third\n", 3},
		{"mixed formats", "## Plan\n\n1. First\n2) Second\n3. Third\n", 3},
		{"with sub-bullets", "## Plan\n\n1. First\n   - sub a\n   - sub b\n2. Second\n", 2},
		{"clipped at next h2", "## Plan\n\n1. First\n2. Second\n\n## Step log\n\nStep 1: foo\n", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CountPlanSteps(tc.content); got != tc.want {
				t.Errorf("CountPlanSteps() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestExtractPlanSection verifies extraction of markdown sections.
func TestExtractPlanSection(t *testing.T) {
	content := "## Task\n\ndo the thing\n\n## Plan\n\n1. Step one\n\n## Step log\n\nStep 1: done"

	task := ExtractPlanSection(content, "## Task")
	if task != "do the thing" {
		t.Errorf("Task = %q, want %q", task, "do the thing")
	}

	plan := ExtractPlanSection(content, "## Plan")
	if plan != "1. Step one" {
		t.Errorf("Plan = %q, want %q", plan, "1. Step one")
	}

	log := ExtractPlanSection(content, "## Step log")
	if log != "Step 1: done" {
		t.Errorf("Step log = %q, want %q", log, "Step 1: done")
	}

	missing := ExtractPlanSection(content, "## Nonexistent")
	if missing != "" {
		t.Errorf("Nonexistent section = %q, want empty", missing)
	}
}

// TestDetectPhaseMarkers verifies detection of phase-completion sentinels.
func TestDetectPhaseMarkers(t *testing.T) {
	cases := []struct {
		name       string
		text       string
		phaseDone  bool
		stepDone   bool
		stepFailed bool
	}{
		{"no markers", "just some text", false, false, false},
		{"phase done", "work complete %%PHASE_DONE%%", true, false, false},
		{"step done", "step finished %%STEP_DONE%%", false, true, false},
		{"step failed", "step failed %%STEP_FAILED%%", false, false, true},
		{"all three", "%%STEP_DONE%% %%STEP_FAILED%% %%PHASE_DONE%%", true, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conv := []proxy.Message{
				{Role: "user", Content: strPtr("question")},
				{Role: "assistant", Content: strPtr(tc.text)},
			}
			pd, sd, sf := DetectPhaseMarkers(conv)
			if pd != tc.phaseDone || sd != tc.stepDone || sf != tc.stepFailed {
				t.Errorf("DetectPhaseMarkers() = (%v,%v,%v), want (%v,%v,%v)",
					pd, sd, sf, tc.phaseDone, tc.stepDone, tc.stepFailed)
			}
		})
	}
}

// TestExtractStepLogEntry verifies extraction of the last %%STEP_LOG:%% entry,
// skipping fenced code blocks.
func TestExtractStepLogEntry(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{"no entry", "just text\nno markers", ""},
		{"single entry", "%%STEP_LOG: Step 1: did thing | outcome: ok%%", "Step 1: did thing | outcome: ok"},
		{"last entry wins", "%%STEP_LOG: Step 1: first%%\n%%STEP_LOG: Step 2: second%%", "Step 2: second"},
		{"skip fenced", "```\n%%STEP_LOG: Step 1: in fence%%\n```\n%%STEP_LOG: Step 2: real%%", "Step 2: real"},
		{"no closing marker", "%%STEP_LOG: Step 1: unclosed entry", "Step 1: unclosed entry"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractStepLogEntry(tc.text); got != tc.want {
				t.Errorf("ExtractStepLogEntry() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestWFFlagsGaps verifies verdict parsing: PASS → false, GAPS → true,
// no verdict → true (fail-closed).
func TestWFFlagsGaps(t *testing.T) {
	if WFFlagsGaps("VERDICT: PASS") {
		t.Error("VERDICT: PASS should not flag gaps")
	}
	if !WFFlagsGaps("VERDICT: GAPS") {
		t.Error("VERDICT: GAPS should flag gaps")
	}
	if !WFFlagsGaps("no verdict line here") {
		t.Error("missing verdict should fail-closed (flag gaps)")
	}
	if !WFFlagsGaps("") {
		t.Error("empty response should fail-closed")
	}
}

// TestIsPreImplementPhase verifies the phase classification.
func TestIsPreImplementPhase(t *testing.T) {
	pre := []WorkflowPhase{WFGather, WFPlan, WFReview, WFPresent}
	for _, p := range pre {
		if !IsPreImplementPhase(p) {
			t.Errorf("IsPreImplementPhase(%d) = false, want true", p)
		}
	}
	if IsPreImplementPhase(WFImplement) {
		t.Error("IsPreImplementPhase(WFImplement) should be false")
	}
	if IsPreImplementPhase(WFDone) {
		t.Error("IsPreImplementPhase(WFDone) should be false")
	}
}

// TestIsWakilPath verifies .wakil/ path detection.
func TestIsWakilPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{".wakil", true},
		{".wakil/plan.md", true},
		{"/work/.wakil/plan.md", true},
		{"/work/.wakil", true},
		{".wakil/sub/deep.md", true},
		{"src/main.go", false},
		{"/etc/passwd", false},
		{".wakil2/plan.md", false},
	}
	for _, tc := range cases {
		if got := IsWakilPath(tc.path); got != tc.want {
			t.Errorf("IsWakilPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// TestWFAppendToStepLog verifies appending entries to the step log section.
func TestWFAppendToStepLog(t *testing.T) {
	// No existing section.
	content := "## Task\n\ntask\n\n## Plan\n\n1. Step"
	result := WFAppendToStepLog(content, "Step 1: done")
	if !strings.Contains(result, "## Step log") {
		t.Error("WFAppendToStepLog should create ## Step log section when absent")
	}
	if !strings.Contains(result, "Step 1: done") {
		t.Error("WFAppendToStepLog should contain the entry")
	}

	// Existing section — append.
	content2 := "## Task\n\ntask\n\n## Step log\n\nStep 1: done"
	result2 := WFAppendToStepLog(content2, "Step 2: also done")
	if strings.Count(result2, "## Step log") != 1 {
		t.Error("WFAppendToStepLog should not duplicate the section header")
	}
	if !strings.Contains(result2, "Step 1: done") || !strings.Contains(result2, "Step 2: also done") {
		t.Error("WFAppendToStepLog should preserve existing entries")
	}
}

// TestHashPlanSection verifies that the hash changes when the plan changes.
func TestHashPlanSection(t *testing.T) {
	plan1 := "## Plan\n\n1. First\n2. Second"
	plan2 := "## Plan\n\n1. First\n2. Second\n3. Third"
	h1 := HashPlanSection(plan1)
	h2 := HashPlanSection(plan2)
	if h1 == "" {
		t.Fatal("HashPlanSection returned empty string")
	}
	if h1 == h2 {
		t.Error("hashes should differ when plan content differs")
	}
	if h1 != HashPlanSection(plan1) {
		t.Error("hash should be deterministic for same content")
	}
}

// TestCapFindings verifies that findings are truncated when they exceed the cap.
func TestCapFindings(t *testing.T) {
	short := "short findings"
	if got := CapFindings(short); got != short {
		t.Errorf("CapFindings(short) = %q, want %q", got, short)
	}

	long := strings.Repeat("x", FindingsCap+100)
	got := CapFindings(long)
	if len(got) > FindingsCap+50 { // allow for truncation marker
		t.Errorf("CapFindings(long) len = %d, want <= %d", len(got), FindingsCap+50)
	}
	if !strings.Contains(got, "[findings truncated]") {
		t.Error("CapFindings should append truncation marker")
	}
}

// TestBuildOracleBriefing verifies that a well-formed plan produces a valid briefing.
func TestBuildOracleBriefing(t *testing.T) {
	planContent := "## Task\n\ndo the thing\n\n## Findings\n\nfound stuff\n\n## Plan\n\n1. Step one\n2. Step two\n\n## Step log\n\nStep 1: done"
	briefing := BuildOracleBriefing("do the thing", planContent, "is the plan sound?")
	if briefing == "" {
		t.Fatal("BuildOracleBriefing returned empty string")
	}
	if !strings.Contains(briefing, "## Task") {
		t.Error("briefing should contain ## Task")
	}
	if !strings.Contains(briefing, "## Plan") {
		t.Error("briefing should contain ## Plan")
	}
	if !strings.Contains(briefing, "is the plan sound?") {
		t.Error("briefing should contain the question")
	}
}

// TestValidateBriefing verifies briefing validation.
func TestValidateBriefing(t *testing.T) {
	valid := "## Task\n\ntask\n\n## Plan\n\n1. Step\n\n## Step log\n\nStep 1: done"
	if reason := ValidateBriefing(valid, false); reason != "" {
		t.Errorf("ValidateBriefing(valid, false) = %q, want empty", reason)
	}
	if reason := ValidateBriefing(valid, true); reason != "" {
		t.Errorf("ValidateBriefing(valid, true) = %q, want empty", reason)
	}

	missingTask := "## Plan\n\n1. Step"
	if reason := ValidateBriefing(missingTask, false); reason == "" {
		t.Error("ValidateBriefing(missingTask) should return a reason")
	}

	// requireStepLog with no step log entries.
	noStepLog := "## Task\n\ntask\n\n## Plan\n\n1. Step"
	if reason := ValidateBriefing(noStepLog, true); reason == "" {
		t.Error("ValidateBriefing(noStepLog, true) should return a reason")
	}
}

// TestWFInitPlanContent verifies the initial plan scaffold.
func TestWFInitPlanContent(t *testing.T) {
	content := WFInitPlanContent("test task")
	if !strings.Contains(content, "## Task") {
		t.Error("WFInitPlanContent should contain ## Task")
	}
	if !strings.Contains(content, "## Findings") {
		t.Error("WFInitPlanContent should contain ## Findings")
	}
	if !strings.Contains(content, "## Plan") {
		t.Error("WFInitPlanContent should contain ## Plan")
	}
	if !strings.Contains(content, "test task") {
		t.Error("WFInitPlanContent should contain the task")
	}
	// Step log should be absent.
	if strings.Contains(content, "## Step log") {
		t.Error("WFInitPlanContent should NOT contain ## Step log")
	}
}

// strPtr is a test helper for creating *string values.
func strPtr(s string) *string { return &s }
