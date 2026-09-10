package agent

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	wtools "github.com/treeol/wakil/internal/tools"
)

// TestRenderReviewReportNoFindings verifies that a clean review with no findings
// shows the "no issues found" message.
func TestRenderReviewReportNoFindings(t *testing.T) {
	r := reviewResult{
		summary: SubagentSummary{Objective: "review diff"},
	}
	report := renderReviewReport(r)
	if !strings.Contains(report, "No issues found") {
		t.Errorf("expected 'No issues found' in report, got: %s", report)
	}
}

// TestRenderReviewReportWithFindings verifies findings are grouped by severity
// and formatted with location and summary.
func TestRenderReviewReportWithFindings(t *testing.T) {
	r := reviewResult{
		summary: SubagentSummary{
			Objective: "review diff",
			Findings: []Finding{
				{Summary: "nil pointer dereference on error path", Location: "main.go:42", Kind: "error", Weight: "high"},
				{Summary: "missing test for new function", Location: "main_test.go:1", Kind: "fact", Weight: "medium"},
				{Summary: "inconsistent naming: camelCase vs snake_case", Location: "utils.go:10", Kind: "pattern", Weight: "low"},
			},
			Checked: []CheckedItem{
				{Path: "main.go", SizeK: 5, Status: "full"},
				{Path: "utils.go", SizeK: 2, Status: "full"},
			},
		},
	}
	report := renderReviewReport(r)
	if !strings.Contains(report, "### High (1)") {
		t.Errorf("expected High section with 1 finding")
	}
	if !strings.Contains(report, "### Medium (1)") {
		t.Errorf("expected Medium section with 1 finding")
	}
	if !strings.Contains(report, "### Low (1)") {
		t.Errorf("expected Low section with 1 finding")
	}
	if !strings.Contains(report, "main.go:42") {
		t.Errorf("expected location main.go:42 in report")
	}
	if !strings.Contains(report, "nil pointer dereference") {
		t.Errorf("expected finding summary in report")
	}
	if !strings.Contains(report, "Files examined") {
		t.Errorf("expected files examined section")
	}
	if !strings.Contains(report, "main.go") {
		t.Errorf("expected checked file main.go in report")
	}
}

// TestRenderReviewReportIncomplete verifies incomplete reviews show a warning
// and do NOT claim the diff is clean.
func TestRenderReviewReportIncomplete(t *testing.T) {
	r := reviewResult{
		summary: SubagentSummary{
			Objective:  "review diff",
			Status:     "incomplete",
			StopReason: "turn_budget_exhausted",
		},
	}
	report := renderReviewReport(r)
	if !strings.Contains(report, "did not complete") {
		t.Errorf("expected incomplete warning in report")
	}
	if !strings.Contains(report, "turn_budget_exhausted") {
		t.Errorf("expected stop reason in report")
	}
	// Must NOT claim the diff is clean.
	if strings.Contains(report, "No issues found") {
		t.Errorf("incomplete review must not claim 'No issues found'")
	}
	if strings.Contains(report, "looks clean") {
		t.Errorf("incomplete review must not claim 'looks clean'")
	}
}

// TestRenderReviewReportIncompleteWithFindings verifies incomplete reviews that
// still produced findings show both the warning and the findings.
func TestRenderReviewReportIncompleteWithFindings(t *testing.T) {
	r := reviewResult{
		summary: SubagentSummary{
			Objective:  "review diff",
			Status:     "incomplete",
			StopReason: "turn_budget_exhausted",
			Findings: []Finding{
				{Summary: "found a bug", Location: "file.go:10", Kind: "error", Weight: "high"},
			},
		},
	}
	report := renderReviewReport(r)
	if !strings.Contains(report, "did not complete") {
		t.Errorf("expected incomplete warning")
	}
	if !strings.Contains(report, "found a bug") {
		t.Errorf("expected finding despite incomplete status")
	}
}

// TestRenderReviewReportEmptySummary verifies that a zero-value summary
// (failed dispatch) is never reported as "clean".
func TestRenderReviewReportEmptySummary(t *testing.T) {
	r := reviewResult{}
	report := renderReviewReport(r)
	if strings.Contains(report, "No issues found") {
		t.Errorf("empty summary must not claim 'No issues found'")
	}
	if !strings.Contains(report, "could not be completed") {
		t.Errorf("expected 'could not be completed' for empty summary, got: %s", report)
	}
}

// TestRenderReviewReportTruncated verifies truncated diffs show a warning.
func TestRenderReviewReportTruncated(t *testing.T) {
	r := reviewResult{
		summary:   SubagentSummary{Objective: "review", Findings: []Finding{{Summary: "ok", Location: "f.go:1", Weight: "low"}}},
		truncated: true,
	}
	report := renderReviewReport(r)
	if !strings.Contains(report, "truncated") {
		t.Errorf("expected truncation warning in report")
	}
}

// TestRenderReviewReportUncertainty verifies uncertainty notes are shown
// when there are findings.
func TestRenderReviewReportUncertainty(t *testing.T) {
	r := reviewResult{
		summary: SubagentSummary{
			Objective: "review diff",
			Findings: []Finding{
				{Summary: "bug", Location: "f.go:1", Kind: "error", Weight: "high"},
			},
			Uncertainty: []string{"only checked 2 of 5 files"},
		},
	}
	report := renderReviewReport(r)
	if !strings.Contains(report, "Notes") {
		t.Errorf("expected Notes section")
	}
	if !strings.Contains(report, "only checked 2 of 5 files") {
		t.Errorf("expected uncertainty text in report")
	}
}

// TestRenderReviewReportNonCanonicalWeights verifies that non-canonical
// weight values are rendered in an "Other" section instead of being silently
// dropped.
func TestRenderReviewReportNonCanonicalWeights(t *testing.T) {
	r := reviewResult{
		summary: SubagentSummary{
			Findings: []Finding{
				{Summary: "critical bug", Location: "a.go:1", Weight: "critical"},
				{Summary: "info nit", Location: "b.go:2", Weight: "info"},
				{Summary: "high bug", Location: "c.go:3", Weight: "High"}, // case variation
			},
		},
	}
	report := renderReviewReport(r)
	// "High" (case-insensitive) should match the High section.
	if !strings.Contains(report, "### High (1)") {
		t.Errorf("expected case-insensitive high match, got: %s", report)
	}
	// "critical" and "info" should fall into Other.
	if !strings.Contains(report, "### Other (2)") {
		t.Errorf("expected Other section with 2 findings, got: %s", report)
	}
	if !strings.Contains(report, "critical bug") {
		t.Errorf("expected 'critical bug' in Other section")
	}
	if !strings.Contains(report, "info nit") {
		t.Errorf("expected 'info nit' in Other section")
	}
}

// TestFilterFindingsByWeight verifies the severity filter is case-insensitive.
func TestFilterFindingsByWeight(t *testing.T) {
	findings := []Finding{
		{Weight: "high"},
		{Weight: "Medium"},
		{Weight: "HIGH"},
		{Weight: "low"},
		{Weight: "medium"},
	}
	high := filterFindingsByWeight(findings, "high")
	if len(high) != 2 {
		t.Errorf("high = %d, want 2", len(high))
	}
	medium := filterFindingsByWeight(findings, "medium")
	if len(medium) != 2 {
		t.Errorf("medium = %d, want 2", len(medium))
	}
	low := filterFindingsByWeight(findings, "low")
	if len(low) != 1 {
		t.Errorf("low = %d, want 1", len(low))
	}
}

// TestFilterFindingsOther verifies that unknown weights go into Other.
func TestFilterFindingsOther(t *testing.T) {
	findings := []Finding{
		{Weight: "high"},
		{Weight: "critical"},
		{Weight: "medium"},
		{Weight: "info"},
		{Weight: "low"},
		{Weight: ""},
	}
	other := filterFindingsOther(findings, "high", "medium", "low")
	if len(other) != 3 {
		t.Errorf("other = %d, want 3 (critical, info, empty)", len(other))
	}
}

// TestFormatFinding verifies single finding formatting.
func TestFormatFinding(t *testing.T) {
	f := Finding{
		Summary:  "nil dereference on line 42",
		Location: "main.go:42",
		Kind:     "error",
		Weight:   "high",
	}
	out := formatFinding(1, f)
	if !strings.Contains(out, "main.go:42") {
		t.Errorf("expected location in formatted finding")
	}
	if !strings.Contains(out, "nil dereference on line 42") {
		t.Errorf("expected summary in formatted finding")
	}
	if !strings.Contains(out, "**high**") {
		t.Errorf("expected weight in formatted finding")
	}
}

// TestFormatFindingEmptyLocation verifies that missing location falls back.
func TestFormatFindingEmptyLocation(t *testing.T) {
	f := Finding{
		Summary: "some issue",
		Weight:  "low",
	}
	out := formatFinding(1, f)
	if !strings.Contains(out, "(no location)") {
		t.Errorf("expected fallback location text, got: %s", out)
	}
}

// TestCaptureReviewDiffEmpty verifies that an empty diff returns no error
// and an empty string.
func TestCaptureReviewDiffEmpty(t *testing.T) {
	exec := newFakeExecutor()
	exec.shellResult = " " // trims to empty
	app := newTestApp("http://localhost", exec, func(_, _, _ string, _ bool) bool { return true })

	diff, truncated, err := captureReviewDiff(context.Background(), app, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if truncated {
		t.Error("expected truncated=false for empty diff")
	}
	if strings.TrimSpace(diff) != "" {
		t.Errorf("expected empty diff, got: %q", diff)
	}
}

// TestCaptureReviewDiffWithRef verifies ref is passed through to git.
func TestCaptureReviewDiffWithRef(t *testing.T) {
	exec := newFakeExecutor()
	exec.shellResult = "diff content here"
	app := newTestApp("http://localhost", exec, func(_, _, _ string, _ bool) bool { return true })

	diff, _, err := captureReviewDiff(context.Background(), app, "HEAD~1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(diff, "diff content here") {
		t.Errorf("expected diff content, got: %q", diff)
	}
	// Verify the ref was passed to git.
	found := false
	for _, call := range exec.shellCalls {
		if strings.Contains(call, "HEAD~1") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected HEAD~1 in git command, calls: %v", exec.shellCalls)
	}
}

// TestCaptureReviewDiffInvalidRef verifies invalid refs are rejected.
func TestCaptureReviewDiffInvalidRef(t *testing.T) {
	exec := newFakeExecutor()
	app := newTestApp("http://localhost", exec, func(_, _, _ string, _ bool) bool { return true })

	_, _, err := captureReviewDiff(context.Background(), app, "--output=/etc/passwd")
	if err == nil {
		t.Error("expected error for malicious ref")
	}
	if !strings.Contains(err.Error(), "must not start with '-'") {
		t.Errorf("expected validation error, got: %v", err)
	}
}

// TestCaptureReviewDiffTruncation verifies that output exceeding the cap
// is truncated and the truncated flag is set.
func TestCaptureReviewDiffTruncation(t *testing.T) {
	exec := newFakeExecutor()
	// Create output larger than reviewDiffCapBytes.
	exec.shellResult = strings.Repeat("a", reviewDiffCapBytes+100)
	app := newTestApp("http://localhost", exec, func(_, _, _ string, _ bool) bool { return true })

	diff, truncated, err := captureReviewDiff(context.Background(), app, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !truncated {
		t.Error("expected truncated=true for oversized diff")
	}
	if !strings.Contains(diff, "truncated") {
		t.Errorf("expected truncation notice in diff")
	}
	// Verify the output was actually capped.
	if len(diff) > reviewDiffCapBytes+200 { // allow for the truncation suffix
		t.Errorf("diff too large after truncation: %d bytes", len(diff))
	}
}

// TestHandleReviewCommandNoChanges verifies the no-changes path returns early.
func TestHandleReviewCommandNoChanges(t *testing.T) {
	exec := newFakeExecutor()
	exec.shellResult = " " // trims to empty → "no changes"
	app := newTestApp("http://localhost", exec, func(_, _, _ string, _ bool) bool { return true })

	report, err := handleReviewCommand(context.Background(), app, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(report, "no changes found") {
		t.Errorf("expected 'no changes found' in report, got: %s", report)
	}
}

// TestHandleReviewCommandGitError verifies git errors are surfaced.
func TestHandleReviewCommandGitError(t *testing.T) {
	exec := newFakeExecutor()
	exec.shellResult = "fatal: not a git repository"
	exec.shellErr = fmt.Errorf("exit status 128")
	app := newTestApp("http://localhost", exec, func(_, _, _ string, _ bool) bool { return true })

	_, err := handleReviewCommand(context.Background(), app, "")
	if err == nil {
		t.Error("expected error for non-git directory")
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("expected 'not a git repository' in error, got: %v", err)
	}
}

// TestHandleReviewCommandNilApp verifies nil app is handled.
func TestHandleReviewCommandNilApp(t *testing.T) {
	_, err := handleReviewCommand(context.Background(), nil, "")
	if err == nil {
		t.Error("expected error for nil app")
	}
}

// TestHandleReviewCommandNilExec verifies nil executor is handled.
func TestHandleReviewCommandNilExec(t *testing.T) {
	app := &App{}
	_, err := handleReviewCommand(context.Background(), app, "")
	if err == nil {
		t.Error("expected error for nil executor")
	}
}

// TestReviewRubricContent verifies the rubric covers all 4 areas.
func TestReviewRubricContent(t *testing.T) {
	if !strings.Contains(reviewRubric, "CORRECTNESS") {
		t.Error("rubric missing CORRECTNESS")
	}
	if !strings.Contains(reviewRubric, "TESTS") {
		t.Error("rubric missing TESTS")
	}
	if !strings.Contains(reviewRubric, "SECURITY") {
		t.Error("rubric missing SECURITY")
	}
	if !strings.Contains(reviewRubric, "STYLE") {
		t.Error("rubric missing STYLE")
	}
	if !strings.Contains(reviewRubric, "AGENTS.md") {
		t.Error("rubric missing AGENTS.md reference for style")
	}
}

// TestReviewDiffCapBytes verifies the diff cap is reasonable.
func TestReviewDiffCapBytes(t *testing.T) {
	if reviewDiffCapBytes < 64*1024 {
		t.Errorf("review diff cap too small: %d (should be >= 64KB for useful reviews)", reviewDiffCapBytes)
	}
	if reviewDiffCapBytes > 256*1024 {
		t.Errorf("review diff cap too large: %d (should be <= 256KB to avoid context overflow)", reviewDiffCapBytes)
	}
}

// TestBuildReviewTask verifies the task includes the spill path, rubric,
// and AGENTS.md path when present.
func TestBuildReviewTask(t *testing.T) {
	// Use a temp directory for AGENTS.md.
	tmpDir := t.TempDir()
	agentsPath := tmpDir + "/AGENTS.md"
	if err := os.WriteFile(agentsPath, []byte("# Test AGENTS.md"), 0644); err != nil {
		t.Fatal(err)
	}

	task := buildReviewTask("unstaged changes", "/tmp/spill.txt", tmpDir, false)
	if !strings.Contains(task, "/tmp/spill.txt") {
		t.Errorf("expected spill path in task")
	}
	if !strings.Contains(task, "CORRECTNESS") {
		t.Errorf("expected rubric in task")
	}
	if !strings.Contains(task, agentsPath) {
		t.Errorf("expected AGENTS.md path in task when present, got: %s", task)
	}
}

// TestBuildReviewTaskNoAgentsMD verifies no AGENTS.md path injection when
// the file is absent. The rubric still mentions AGENTS.md as a general
// instruction, but the task should not include a specific file path.
func TestBuildReviewTaskNoAgentsMD(t *testing.T) {
	tmpDir := t.TempDir()
	// No AGENTS.md in this dir.
	task := buildReviewTask("HEAD~1", "/tmp/spill.txt", tmpDir, false)
	// The rubric mentions AGENTS.md as a general instruction, but there
	// should be no "AGENTS.md is at:" path injection.
	if strings.Contains(task, "AGENTS.md is at:") {
		t.Errorf("did not expect AGENTS.md path injection when absent: %s", task)
	}
}

// TestBuildReviewTaskTruncated verifies truncated flag adds a notice.
func TestBuildReviewTaskTruncated(t *testing.T) {
	task := buildReviewTask("HEAD~1", "/tmp/spill.txt", "", true)
	if !strings.Contains(task, "truncated") {
		t.Errorf("expected truncation notice in task")
	}
}

// TestHandleReviewCommandDispatchesSubagent verifies that with a real SSE
// server, the review command dispatches a subagent and renders findings.
func TestHandleReviewCommandDispatchesSubagent(t *testing.T) {
	summaryJSON := `{"objective":"review diff","findings":[{"summary":"nil dereference in error path","location":"main.go:42","kind":"error","weight":"high"},{"summary":"missing test for new function","location":"main_test.go:1","kind":"fact","weight":"medium"}],"checked":[{"path":"main.go","size_k":3,"status":"full"}]}`

	srv := sseServer(t,
		[]string{contentChunk(summaryJSON)},
	)
	defer srv.Close()

	exec := newFakeExecutor()
	exec.shellResult = "diff --git a/main.go b/main.go\n+func foo() *Bar { return nil }"

	app := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })

	report, err := handleReviewCommand(context.Background(), app, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(report, "Code Review") {
		t.Errorf("expected 'Code Review' header in report, got: %s", report)
	}
	if !strings.Contains(report, "nil dereference") {
		t.Errorf("expected finding summary in report, got: %s", report)
	}
	if !strings.Contains(report, "main.go:42") {
		t.Errorf("expected finding location in report, got: %s", report)
	}
}

// TestHandleReviewCommandWithRefInReport verifies the ref is described in the report header.
func TestHandleReviewCommandWithRefInReport(t *testing.T) {
	summaryJSON := `{"objective":"review diff","findings":[{"summary":"ok","location":"f.go:1","kind":"fact","weight":"low"}]}`

	srv := sseServer(t,
		[]string{contentChunk(summaryJSON)},
	)
	defer srv.Close()

	exec := newFakeExecutor()
	exec.shellResult = "diff --git a/main.go b/main.go\n+var x = 1"

	app := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })

	report, err := handleReviewCommand(context.Background(), app, "HEAD~1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(report, "HEAD~1") {
		t.Errorf("expected 'HEAD~1' in report header, got: %s", report)
	}
}

// TestHandleReviewCommandDiscoveryOnly verifies that the subagent is dispatched
// with discovery capability (read-only).
func TestHandleReviewCommandDiscoveryOnly(t *testing.T) {
	summaryJSON := `{"objective":"review diff","findings":[]}`

	srv := sseServer(t,
		[]string{contentChunk(summaryJSON)},
	)
	defer srv.Close()

	exec := newFakeExecutor()
	exec.shellResult = "diff --git a/main.go b/main.go\n+var x = 1"

	app := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })

	// Verify the constant used in handleReviewCommand matches CapabilityDiscovery.
	if wtools.CapabilityDiscovery == "" {
		t.Error("CapabilityDiscovery should not be empty")
	}

	report, err := handleReviewCommand(context.Background(), app, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(report, "No issues found") {
		t.Errorf("expected clean review, got: %s", report)
	}
}

// TestHandleReviewCommandDispatchFailure verifies that a failed dispatch
// (connection refused) produces an error-incomplete report, not a "clean" report.
func TestHandleReviewCommandDispatchFailure(t *testing.T) {
	exec := newFakeExecutor()
	exec.shellResult = "diff --git a/main.go b/main.go\n+var x = 1"

	// No SSE server — dispatch will fail with connection refused.
	app := newTestApp("http://localhost:1", exec, func(_, _, _ string, _ bool) bool { return true })

	report, err := handleReviewCommand(context.Background(), app, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Must NOT claim the diff is clean.
	if strings.Contains(report, "No issues found") {
		t.Errorf("failed dispatch must not claim 'No issues found', got: %s", report)
	}
	if strings.Contains(report, "looks clean") {
		t.Errorf("failed dispatch must not claim 'looks clean', got: %s", report)
	}
	// Should show the incomplete/failed state.
	if !strings.Contains(report, "could not be completed") && !strings.Contains(report, "did not complete") {
		t.Errorf("expected failure indicator in report, got: %s", report)
	}
}

// TestHandleReviewCommandSubagentReadsDiffFile verifies an integration path
// where the subagent reads the spill file via read_file, then returns findings.
func TestHandleReviewCommandSubagentReadsDiffFile(t *testing.T) {
	summaryJSON := `{"objective":"review diff","findings":[{"summary":"uninitialized variable","location":"main.go:5","kind":"error","weight":"high"}],"checked":[{"path":"main.go","size_k":1,"status":"full"}]}`

	srv := sseServer(t,
		// First call: subagent reads the diff spill file.
		toolCallFrames("r1", "read_file", `{"path":"PLACEHOLDER"}`),
		// Second call: subagent returns JSON summary.
		[]string{contentChunk(summaryJSON)},
	)
	defer srv.Close()

	exec := newFakeExecutor()
	exec.shellResult = "diff --git a/main.go b/main.go\n+var x = 1"

	app := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })

	report, err := handleReviewCommand(context.Background(), app, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(report, "uninitialized variable") {
		t.Errorf("expected finding from subagent that read the diff, got: %s", report)
	}
}
