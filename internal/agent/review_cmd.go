package agent

// review_cmd.go — /review [ref] slash command (card #188).
//
// Runs a read-only code review over the current git diff by dispatching a
// discovery-tier subagent with a fixed review rubric. The subagent is strictly
// read-only (discovery capability → readOnlyConfirmer) — zero file mutations
// are possible.
//
// Flow:
//  1. Capture the git diff (unstaged by default, or against a ref if given).
//  2. Spill the diff to a cache file so the subagent can read_file it.
//  3. Dispatch a discovery subagent with a task that instructs it to read the
//     diff, examine the changed files for context, and produce structured
//     findings against the review rubric.
//  4. Render the SubagentSummary as a readable review report.
//
// Failure mode: a failed or incomplete review is NEVER reported as "clean".
// If the subagent did not complete or produced no usable output (empty
// Objective), the report says so explicitly — it never claims the diff is
// clean when the review may not have run.

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/treeol/wakil/internal/proxy"
	wtools "github.com/treeol/wakil/internal/tools"
)

// reviewDiffCapBytes is the max diff size we capture for review. Larger than
// the interactive git_diff cap (64KB) because a review needs full context, but
// still bounded to prevent context overflow. Diffs exceeding this are truncated
// with a notice the subagent can see, and the report is labeled partial.
const reviewDiffCapBytes = 128 * 1024

// reviewTimeout is the maximum time a review can run before the context is
// cancelled. Reviews are subagent dispatches and can loop on tool calls;
// without a bound a stuck review would hang the Cmd closure indefinitely.
const reviewTimeout = 5 * time.Minute

// reviewRubric is the fixed rubric the review subagent applies. It covers the
// four areas from the card: correctness, tests, security, style (vs AGENTS.md).
const reviewRubric = `Review the diff against this rubric:

1. CORRECTNESS — logic errors, nil dereference, off-by-one, race conditions, resource leaks, error paths not handled, incorrect API usage.

2. TESTS — are there tests for the new/changed behavior? Do existing tests cover the edge cases? Are test names accurate? Is anything untested that should be?

3. SECURITY — input validation, injection vectors, secret exposure, unsafe deserialization, privilege escalation, path traversal.

4. STYLE — does the code follow conventions in AGENTS.md (if present)? Naming, package structure, error wrapping, comment style.

For each issue found, emit a finding with:
  - summary: ≤200 chars describing the issue and the fix
  - location: file:line (must be a real location from the diff or the file content)
  - kind: "error" for bugs/security, "pattern" for style, "fact" for missing tests
  - weight: "high" for correctness/security bugs, "medium" for missing tests/style issues, "low" for minor nits

If the diff is clean (no issues found), return an empty findings array and note it in uncertainty.
Read the changed files for context — a diff line without surrounding code is ambiguous. Use read_file to see the full function/struct.

Respond with ONLY a valid JSON SubagentSummary object — no prose, no markdown, no code fences.`

// reviewResult holds the data needed to render a review report. It decouples
// the rendering from the dispatch so tests can exercise the renderer without
// a live subagent.
type reviewResult struct {
	summary   SubagentSummary
	refDesc   string
	costRows  []proxy.CostRow
	truncated bool // diff was truncated at reviewDiffCapBytes
}

// handleReviewCommand captures the git diff and dispatches a discovery-tier
// subagent to review it. Returns a formatted review report string.
//
// ref is an optional git revision (e.g., "HEAD~1", "main...feature"). When
// empty, the review covers unstaged working-tree changes (same as bare git_diff).
func handleReviewCommand(ctx context.Context, app *App, ref string) (string, error) {
	if app == nil {
		return "", fmt.Errorf("no app available")
	}
	if app.Exec == nil {
		return "", fmt.Errorf("no executor available")
	}

	// 1. Capture the diff.
	diff, truncated, err := captureReviewDiff(ctx, app, ref)
	if err != nil {
		return "", fmt.Errorf("capture diff: %w", err)
	}
	if strings.TrimSpace(diff) == "" {
		refDesc := "unstaged changes"
		if ref != "" {
			refDesc = ref
		}
		return fmt.Sprintf("review: no changes found (%s)", refDesc), nil
	}

	// 2. Spill the diff to a cache file the subagent can read.
	spillPath := wtools.SpillToCache(app.chatID(), "review_diff", diff)
	if spillPath == "" {
		return "", fmt.Errorf("could not write diff to cache for review")
	}

	// 3. Build the review task for the subagent.
	refDesc := "unstaged working-tree changes"
	if ref != "" {
		refDesc = "diff against " + ref
	}

	task := buildReviewTask(refDesc, spillPath, app.Exec.Cwd(), truncated)

	// 4. Dispatch a discovery-tier subagent (read-only, zero mutations).
	// Use a timeout-bounded context so a stuck review doesn't hang forever.
	reviewCtx, cancel := context.WithTimeout(ctx, reviewTimeout)
	defer cancel()

	summary, _, _, _, costRows, _ := app.dispatchSubagentGated(
		reviewCtx, task, io.Discard, "", wtools.CapabilityDiscovery, "",
	)

	// 5. Render the review report.
	return renderReviewReport(reviewResult{
		summary:   summary,
		refDesc:   refDesc,
		costRows:  costRows,
		truncated: truncated,
	}), nil
}

// buildReviewTask constructs the task string for the review subagent.
func buildReviewTask(refDesc, spillPath, cwd string, truncated bool) string {
	var task strings.Builder
	task.WriteString(fmt.Sprintf("You are reviewing code changes (%s). The full diff is at:\n%s\n", refDesc, spillPath))
	if truncated {
		task.WriteString("\nNote: the diff was truncated — only the first 128KB is included. Earlier changes may be missing.\n")
	}

	// Inject AGENTS.md path if present so the subagent can read it for style.
	if cwd != "" {
		agentsPath := filepath.Join(cwd, agentsMDFilename)
		if _, err := os.Stat(agentsPath); err == nil {
			task.WriteString(fmt.Sprintf("\nAGENTS.md is at: %s — read it for style conventions.\n", agentsPath))
		}
	}

	task.WriteString("\nRead the diff file first. Then read the changed source files for context (use the file paths from the diff). Apply the review rubric below and report findings.\n\n")
	task.WriteString(reviewRubric)
	return task.String()
}

// captureReviewDiff runs git diff and returns the raw output, capped at
// reviewDiffCapBytes. It reuses the same hardening as the git_diff tool
// (no external diff, no textconv, no pager, no hooks).
// Returns (diff, truncated, error) — truncated is true when the output
// exceeded the cap.
func captureReviewDiff(ctx context.Context, app *App, ref string) (string, bool, error) {
	if err := validateGitRef(ref); err != nil {
		return "", false, err
	}

	cmd := gitBaseArgs() + " diff --no-ext-diff --no-textconv"
	if ref != "" {
		cmd += " " + shellQuote(ref)
	}

	full := gitBaseEnv() + fmt.Sprintf(
		`tmp=$(mktemp) && git %s > "$tmp" 2>&1; rc=$?; head -c %d "$tmp"; rm -f "$tmp"; exit $rc`,
		cmd, reviewDiffCapBytes+1)

	out, err := app.Exec.RunShell(ctx, full)
	if err != nil {
		return "", false, fmt.Errorf("%s", classifyGitError(out, err))
	}

	truncated := len(out) > reviewDiffCapBytes
	if truncated {
		out = truncateUTF8(out[:reviewDiffCapBytes], reviewDiffCapBytes)
		out += fmt.Sprintf("\n… [diff truncated at %d bytes — narrow with a ref]", reviewDiffCapBytes)
	}

	if strings.ContainsRune(out, 0) {
		return "", false, fmt.Errorf("binary content in diff — not reviewable")
	}

	return out, truncated, nil
}

// renderReviewReport formats the review result into a readable report.
// A failed or incomplete review is NEVER reported as "clean" — the report
// always distinguishes between "completed and clean" and "did not complete".
func renderReviewReport(r reviewResult) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("## Code Review — %s\n\n", r.refDesc))

	if r.truncated {
		sb.WriteString("⚠ Diff was truncated — review covers only the first 128KB of changes.\n\n")
	}

	// A review that did not complete is never reported as "clean".
	if r.summary.Status == "incomplete" {
		sb.WriteString("⚠ Review subagent did not complete (")
		if r.summary.StopReason != "" {
			sb.WriteString(r.summary.StopReason)
		} else {
			sb.WriteString("budget/cancelled")
		}
		sb.WriteString("). Findings below may be partial or absent.\n\n")
	}

	if len(r.summary.Findings) == 0 {
		// Distinguish "completed and clean" from "incomplete with no findings".
		if r.summary.Status == "incomplete" || r.summary.Objective == "" {
			// The subagent failed or produced no usable output.
			sb.WriteString("⚠ Review could not be completed. No findings were produced.\n")
			if len(r.summary.Uncertainty) > 0 {
				sb.WriteString(strings.Join(r.summary.Uncertainty, "; "))
				sb.WriteString("\n")
			}
		} else {
			sb.WriteString("✓ No issues found. ")
			if len(r.summary.Uncertainty) > 0 {
				sb.WriteString(strings.Join(r.summary.Uncertainty, "; "))
			} else {
				sb.WriteString("The diff looks clean.")
			}
			sb.WriteString("\n")
		}
	} else {
		// Group findings by severity (case-insensitive, with "Other" bucket).
		high := filterFindingsByWeight(r.summary.Findings, "high")
		medium := filterFindingsByWeight(r.summary.Findings, "medium")
		low := filterFindingsByWeight(r.summary.Findings, "low")
		other := filterFindingsOther(r.summary.Findings, "high", "medium", "low")

		if len(high) > 0 {
			sb.WriteString(fmt.Sprintf("### High (%d)\n\n", len(high)))
			for i, f := range high {
				sb.WriteString(formatFinding(i+1, f))
			}
		}
		if len(medium) > 0 {
			sb.WriteString(fmt.Sprintf("### Medium (%d)\n\n", len(medium)))
			for i, f := range medium {
				sb.WriteString(formatFinding(i+1, f))
			}
		}
		if len(low) > 0 {
			sb.WriteString(fmt.Sprintf("### Low (%d)\n\n", len(low)))
			for i, f := range low {
				sb.WriteString(formatFinding(i+1, f))
			}
		}
		if len(other) > 0 {
			sb.WriteString(fmt.Sprintf("### Other (%d)\n\n", len(other)))
			for i, f := range other {
				sb.WriteString(formatFinding(i+1, f))
			}
		}
	}

	// Files checked by the subagent (coverage signal).
	if len(r.summary.Checked) > 0 {
		sb.WriteString("\n### Files examined\n\n")
		for _, c := range r.summary.Checked {
			status := c.Status
			if c.SizeK > 0 {
				status = fmt.Sprintf("%s (%dK)", c.Status, c.SizeK)
			}
			sb.WriteString(fmt.Sprintf("- %s — %s\n", c.Path, status))
		}
	}

	if len(r.summary.Uncertainty) > 0 && len(r.summary.Findings) > 0 {
		sb.WriteString("\n### Notes\n\n")
		for _, u := range r.summary.Uncertainty {
			sb.WriteString(fmt.Sprintf("- %s\n", u))
		}
	}

	// Cost summary.
	totalCost := 0.0
	for _, cr := range r.costRows {
		if cr.Priced {
			totalCost += cr.CostUSD
		}
	}
	if totalCost > 0 {
		sb.WriteString(fmt.Sprintf("\n_(review cost: $%.4f)_\n", totalCost))
	}

	return strings.TrimRight(sb.String(), "\n")
}

// formatFinding renders a single finding as a numbered list item.
func formatFinding(n int, f Finding) string {
	loc := f.Location
	if loc == "" {
		loc = "(no location)"
	}
	return fmt.Sprintf("%d. **%s** `%s`\n   %s\n\n", n, f.Weight, loc, f.Summary)
}

// filterFindingsByWeight returns findings matching the given weight,
// case-insensitively. This handles model outputs like "High" or "HIGH".
func filterFindingsByWeight(findings []Finding, weight string) []Finding {
	target := strings.ToLower(weight)
	var out []Finding
	for _, f := range findings {
		if strings.ToLower(f.Weight) == target {
			out = append(out, f)
		}
	}
	return out
}

// filterFindingsOther returns findings whose weight is not one of the known
// values. This ensures non-canonical weights are still shown to the user
// rather than silently dropped.
func filterFindingsOther(findings []Finding, known ...string) []Finding {
	knownLower := make(map[string]bool, len(known))
	for _, k := range known {
		knownLower[strings.ToLower(k)] = true
	}
	var out []Finding
	for _, f := range findings {
		if !knownLower[strings.ToLower(f.Weight)] {
			out = append(out, f)
		}
	}
	return out
}
