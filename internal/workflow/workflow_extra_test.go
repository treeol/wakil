package workflow

import (
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/proxy"
)

// TestStatusString verifies the multi-line status format.
func TestStatusString(t *testing.T) {
	w := &WorkflowState{
		Phase:     WFImplement,
		StepIdx:   3,
		StepCount: 5,
		Task:      "fix the bug",
		PlanPath:  ".wakil/plan.md",
	}
	got := w.StatusString()
	if !strings.Contains(got, "phase: implement") {
		t.Errorf("StatusString should contain phase, got %q", got)
	}
	if !strings.Contains(got, "step: 3/5") {
		t.Errorf("StatusString should contain step, got %q", got)
	}
	if !strings.Contains(got, "task: fix the bug") {
		t.Errorf("StatusString should contain task, got %q", got)
	}
	if !strings.Contains(got, "plan: .wakil/plan.md") {
		t.Errorf("StatusString should contain plan path, got %q", got)
	}
}

// TestDirective verifies that each phase produces the correct directive text.
func TestDirective(t *testing.T) {
	t.Run("gather", func(t *testing.T) {
		w := &WorkflowState{Phase: WFGather, Task: "do stuff", PlanPath: ".wakil/plan.md"}
		d := w.Directive()
		if !strings.Contains(d, "[WORKFLOW GATHER]") {
			t.Error("gather directive should contain [WORKFLOW GATHER]")
		}
		if !strings.Contains(d, "do stuff") {
			t.Error("gather directive should contain the task")
		}
		if !strings.Contains(d, WFPhaseDone) {
			t.Error("gather directive should contain the phase-done sentinel")
		}
	})

	t.Run("plan", func(t *testing.T) {
		w := &WorkflowState{Phase: WFPlan, Task: "do stuff", PlanPath: ".wakil/plan.md"}
		d := w.Directive()
		if !strings.Contains(d, "[WORKFLOW PLAN]") {
			t.Error("plan directive should contain [WORKFLOW PLAN]")
		}
		if !strings.Contains(d, WFPhaseDone) {
			t.Error("plan directive should contain the phase-done sentinel")
		}
	})

	t.Run("plan_format_invalid", func(t *testing.T) {
		w := &WorkflowState{Phase: WFPlan, Task: "do stuff", PlanPath: ".wakil/plan.md", PlanFormatInvalid: true}
		d := w.Directive()
		if !strings.Contains(d, "[WORKFLOW PLAN REFORMAT]") {
			t.Error("plan-format-invalid directive should contain [WORKFLOW PLAN REFORMAT]")
		}
	})

	t.Run("implement_step", func(t *testing.T) {
		w := &WorkflowState{Phase: WFImplement, StepIdx: 2, StepCount: 5, PlanPath: ".wakil/plan.md"}
		d := w.Directive()
		if !strings.Contains(d, "[WORKFLOW IMPLEMENT STEP 2/5]") {
			t.Errorf("implement directive should contain step number, got %q", d)
		}
		if !strings.Contains(d, WFStepDone) {
			t.Error("implement directive should contain step-done sentinel")
		}
	})

	t.Run("implement_oracle_review_step1", func(t *testing.T) {
		w := &WorkflowState{
			Phase:        WFImplement,
			StepIdx:      1,
			StepCount:    3,
			PlanPath:     ".wakil/plan.md",
			OracleReview: "the plan looks risky",
		}
		d := w.Directive()
		if !strings.Contains(d, "Oracle plan review") {
			t.Error("step 1 should inject oracle review")
		}
		if !strings.Contains(d, "the plan looks risky") {
			t.Error("oracle review text should be present")
		}
	})

	t.Run("implement_verify", func(t *testing.T) {
		w := &WorkflowState{Phase: WFImplement, StepIdx: 4, StepCount: 3, PlanPath: ".wakil/plan.md"}
		d := w.Directive()
		if !strings.Contains(d, "[WORKFLOW VERIFY]") {
			t.Error("verify directive should contain [WORKFLOW VERIFY]")
		}
	})

	t.Run("review_retry", func(t *testing.T) {
		w := &WorkflowState{Phase: WFReview, ReviewSkipReason: "oracle unavailable"}
		d := w.Directive()
		if !strings.Contains(d, "[WORKFLOW REVIEW RETRY]") {
			t.Error("review directive should contain [WORKFLOW REVIEW RETRY]")
		}
		if !strings.Contains(d, "oracle unavailable") {
			t.Error("review directive should contain the skip reason")
		}
	})

	t.Run("review_retry_no_reason", func(t *testing.T) {
		w := &WorkflowState{Phase: WFReview}
		d := w.Directive()
		if !strings.Contains(d, "[WORKFLOW REVIEW RETRY]") {
			t.Error("review directive should contain [WORKFLOW REVIEW RETRY]")
		}
		if strings.Contains(d, "Last attempt reason") {
			t.Error("review directive should not contain reason suffix when empty")
		}
	})

	t.Run("present_and_done_return_empty", func(t *testing.T) {
		for _, phase := range []WorkflowPhase{WFPresent, WFDone} {
			w := &WorkflowState{Phase: phase}
			if d := w.Directive(); d != "" {
				t.Errorf("Directive(%d) = %q, want empty", phase, d)
			}
		}
	})
}

// TestTruncate verifies the local truncate helper.
func TestTruncate(t *testing.T) {
	cases := []struct {
		input string
		n     int
		want  string
	}{
		{"short", 10, "short"},
		{"exactly5", 5, "exact…"},
		{"too long text", 5, "too l…"},
		{"", 5, ""},
		{"unicode: 日本語", 9, "unicode: …"},
	}
	for _, tc := range cases {
		got := truncate(tc.input, tc.n)
		if got != tc.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tc.input, tc.n, got, tc.want)
		}
	}
}

// TestCountStepLogEntries verifies counting of "Step " prefixed entries.
func TestCountStepLogEntries(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    int
	}{
		{"no step log section", "## Task\n\ntask", 0},
		{"empty step log", "## Step log\n\n(none yet)", 0},
		{"single entry", "## Step log\n\nStep 1: did thing", 1},
		{"multiple entries", "## Step log\n\nStep 1: did thing\n\nStep 2: did more", 2},
		{"ignores non-step paragraphs", "## Step log\n\nStep 1: foo\n\nsome note\n\nStep 2: bar", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CountStepLogEntries(tc.content); got != tc.want {
				t.Errorf("CountStepLogEntries() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestLastAssistantText verifies extraction of the last assistant message.
func TestLastAssistantText(t *testing.T) {
	t.Run("found", func(t *testing.T) {
		conv := []proxy.Message{
			{Role: "user", Content: strPtr("hello")},
			{Role: "assistant", Content: strPtr("hi there")},
			{Role: "user", Content: strPtr("do stuff")},
			{Role: "assistant", Content: strPtr("done")},
		}
		if got := LastAssistantText(conv); got != "done" {
			t.Errorf("LastAssistantText = %q, want %q", got, "done")
		}
	})

	t.Run("no_assistant", func(t *testing.T) {
		conv := []proxy.Message{{Role: "user", Content: strPtr("hello")}}
		if got := LastAssistantText(conv); got != "" {
			t.Errorf("LastAssistantText = %q, want empty", got)
		}
	})

	t.Run("nil_content", func(t *testing.T) {
		conv := []proxy.Message{{Role: "assistant", Content: nil}}
		if got := LastAssistantText(conv); got != "" {
			t.Errorf("LastAssistantText = %q, want empty", got)
		}
	})

	t.Run("empty", func(t *testing.T) {
		if got := LastAssistantText(nil); got != "" {
			t.Errorf("LastAssistantText(nil) = %q, want empty", got)
		}
	})

	t.Run("skips_nil_content_finds_earlier", func(t *testing.T) {
		conv := []proxy.Message{
			{Role: "assistant", Content: strPtr("first")},
			{Role: "assistant", Content: nil},
		}
		if got := LastAssistantText(conv); got != "first" {
			t.Errorf("LastAssistantText = %q, want %q", got, "first")
		}
	})
}

// TestIsPlanFilePath verifies relative/absolute path matching.
func TestIsPlanFilePath(t *testing.T) {
	cases := []struct {
		name string
		path string
		plan string
		want bool
	}{
		{"exact match", ".wakil/plan.md", ".wakil/plan.md", true},
		{"abs vs rel", "/work/.wakil/plan.md", ".wakil/plan.md", true},
		{"rel vs abs", ".wakil/plan.md", "/work/.wakil/plan.md", true},
		{"different paths", "src/main.go", ".wakil/plan.md", false},
		{"same abs", "/work/.wakil/plan.md", "/work/.wakil/plan.md", true},
		{"different abs", "/other/.wakil/plan.md", "/work/.wakil/plan.md", false},
		{"plan not wakil", "plan.md", "plan.md", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsPlanFilePath(tc.path, tc.plan); got != tc.want {
				t.Errorf("IsPlanFilePath(%q, %q) = %v, want %v", tc.path, tc.plan, got, tc.want)
			}
		})
	}
}

// TestBuildFinalReviewBriefing verifies the final review briefing includes
// all step-log entries and uses the 16 KB default cap.
func TestBuildFinalReviewBriefing(t *testing.T) {
	planContent := "## Task\n\ndo the thing\n\n## Findings\n\nfound stuff\n\n## Plan\n\n1. Step one\n2. Step two\n\n## Step log\n\nStep 1: done\n\nStep 2: also done"
	briefing := BuildFinalReviewBriefing("do the thing", planContent, "are there gaps?", 0)
	if briefing == "" {
		t.Fatal("BuildFinalReviewBriefing returned empty")
	}
	if !strings.Contains(briefing, "## Task") {
		t.Error("briefing should contain ## Task")
	}
	if !strings.Contains(briefing, "Step 1: done") {
		t.Error("briefing should contain first step entry")
	}
	if !strings.Contains(briefing, "Step 2: also done") {
		t.Error("briefing should contain second step entry")
	}
	if !strings.Contains(briefing, "are there gaps?") {
		t.Error("briefing should contain the question")
	}
	if !strings.Contains(briefing, "## Step log\n\n") {
		t.Error("briefing should use '## Step log' header (not 'recent')")
	}
}

// TestBuildFinalReviewBriefingTruncation verifies the hard truncation path.
func TestBuildFinalReviewBriefingTruncation(t *testing.T) {
	// Build content that exceeds a small maxBytes.
	task := strings.Repeat("x", 200)
	planContent := "## Task\n\n" + task + "\n\n## Plan\n\n1. Step\n\n## Step log\n\nStep 1: " + strings.Repeat("y", 200)
	briefing := BuildFinalReviewBriefing(task, planContent, "q", 100) // tiny cap
	if !strings.Contains(briefing, "[briefing truncated]") {
		t.Error("briefing should contain truncation marker when content exceeds cap")
	}
	if len(briefing) > 100+100 { // cap + marker overhead
		t.Errorf("briefing too long: %d bytes (cap was 100)", len(briefing))
	}
}

// TestGapGist verifies extraction of the first non-VERDICT line.
func TestGapGist(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"first non-verdict line", "VERDICT: GAPS\nThe plan is missing tests for auth.", "The plan is missing tests for auth."},
		{"pass verdict", "VERDICT: PASS\nEverything looks good.", "Everything looks good."},
		{"empty lines skipped", "\n\nVERDICT: GAPS\n\nReal issue here", "Real issue here"},
		{"empty input", "", "(gaps flagged — see oracle response)"},
		{"only verdict", "VERDICT: GAPS", "(gaps flagged — see oracle response)"},
		{"long line truncated", "VERDICT: GAPS\n" + strings.Repeat("a", 200), strings.Repeat("a", 120) + "…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := GapGist(tc.input); got != tc.want {
				t.Errorf("GapGist() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestWFEverystepCritical verifies keyword detection for every-step oracle.
func TestWFEverystepCritical(t *testing.T) {
	critical := []string{
		"This is incorrect, please fix",
		"Wrong approach here",
		"There is an error in step 2",
		"The test will fail with this",
		"A problem was found",
		"There is an issue with the logic",
		"I have a concern about this",
		"Something is missing",
		"The implementation is incomplete",
		"This is broken",
		"Not done yet",
		"You should have done X instead",
		"This doesn't work",
		"This does not compile",
	}
	for _, input := range critical {
		if !WFEverystepCritical(input) {
			t.Errorf("WFEverystepCritical(%q) = false, want true", input)
		}
	}

	nonCritical := []string{
		"The plan looks good",
		"Everything is correct and well-structured",
		"proceed to the next step",
		"",
	}
	for _, input := range nonCritical {
		if WFEverystepCritical(input) {
			t.Errorf("WFEverystepCritical(%q) = true, want false", input)
		}
	}

	// Case-insensitive.
	if !WFEverystepCritical("THIS IS WRONG") {
		t.Error("WFEverystepCritical should be case-insensitive")
	}
}

// TestRecentStepEntries verifies windowing and filtering of step-log paragraphs.
func TestRecentStepEntries(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		if got := RecentStepEntries("", 5); got != nil {
			t.Errorf("RecentStepEntries(empty) = %v, want nil", got)
		}
	})

	t.Run("fewer_than_k", func(t *testing.T) {
		log := "Step 1: foo\n\nStep 2: bar"
		got := RecentStepEntries(log, 5)
		if len(got) != 2 {
			t.Fatalf("got %d entries, want 2", len(got))
		}
		if got[0] != "Step 1: foo" || got[1] != "Step 2: bar" {
			t.Errorf("entries = %v", got)
		}
	})

	t.Run("windowed", func(t *testing.T) {
		log := "Step 1: a\n\nStep 2: b\n\nStep 3: c\n\nStep 4: d"
		got := RecentStepEntries(log, 2)
		if len(got) != 2 {
			t.Fatalf("got %d entries, want 2", len(got))
		}
		if got[0] != "Step 3: c" || got[1] != "Step 4: d" {
			t.Errorf("windowed entries = %v, want [Step 3: c, Step 4: d]", got)
		}
	})

	t.Run("skips_placeholders", func(t *testing.T) {
		log := "(none yet)\n\n(pending implementation)\n\nStep 1: foo"
		got := RecentStepEntries(log, 5)
		if len(got) != 1 || got[0] != "Step 1: foo" {
			t.Errorf("RecentStepEntries = %v, want [Step 1: foo]", got)
		}
	})
}
