package workflow

import (
	"fmt"
	"hash/fnv"
	"path/filepath"
	"regexp"
	"strings"

	"wakil/internal/config"
	"wakil/internal/proxy"
)

// WorkflowPhase represents the current phase of a /plan workflow.
type WorkflowPhase int

const (
	WFGather    WorkflowPhase = iota
	WFPlan
	WFReview   // oracle checkpoint — no model turn driven here
	WFPresent  // plan shown to user; waits for /plan approve
	WFImplement
	WFDone
)

// Phase-completion sentinels output by the model to signal a transition.
const (
	WFPhaseDone  = "%%PHASE_DONE%%"
	WFStepDone   = "%%STEP_DONE%%"
	WFStepFailed = "%%STEP_FAILED%%"

	// WFStepLogMark is the opening of the step-log line the model emits.
	// Full format:  %%STEP_LOG: Step N: <action> | outcome: <…> | deviation: <…>%%
	// Wakil extracts the entry and appends deterministically; the model must
	// never write directly to ## Step log.
	WFStepLogMark = "%%STEP_LOG:"
)

// WFBriefingCap is the maximum byte length of an oracle briefing (12 KB).
// Oldest step-log entries are dropped first when the cap is exceeded.
const WFBriefingCap = 12 * 1024

// WFStepLogK is the default number of recent step-log entries included in a briefing.
const WFStepLogK = 5

// WorkflowState holds the runtime state of an active /plan workflow.
type WorkflowState struct {
	Task              string
	Phase             WorkflowPhase
	StepCount         int    // parsed from ## Plan; 0 until plan is written
	StepIdx           int    // current step 1-based; 0 = not yet started
	PlanPath          string // absolute path to .wakil/plan.md, resolved at workflow start
	OracleReview      string // advisory oracle critique received during REVIEW phase
	OracleMode        string // per-run override: "every-step"|"on-deviation"|"phases-only"|"" (use config)
	ReviewSkipReason  string // reason stored when oracle review was unavailable or incomplete
	PlanFormatInvalid bool   // ## Plan is non-empty but has no parseable numbered steps
	ReviewPlanHash    string // FNV-64a of ## Plan section at last successful REVIEW
	ReviewStaleWarned bool   // true after the first approve warned about a stale plan
}

// EffectiveOracleMode resolves the oracle consultation schedule in priority order:
// per-run override (--oracle flag) → config WFOracleMode → hardcoded default.
func (w *WorkflowState) EffectiveOracleMode(cfg config.Config) string {
	if w.OracleMode != "" {
		return w.OracleMode
	}
	if cfg.WFOracleMode != "" {
		return cfg.WFOracleMode
	}
	return "on-deviation"
}

func (w *WorkflowState) PhaseName() string {
	switch w.Phase {
	case WFGather:
		return "gather"
	case WFPlan:
		return "plan"
	case WFReview:
		return "review"
	case WFPresent:
		return "present"
	case WFImplement:
		return "implement"
	case WFDone:
		return "done"
	default:
		return "?"
	}
}

// SidebarLabel returns a short label for the TUI sidebar.
// When StepIdx > StepCount in IMPLEMENT the workflow is in the post-step
// final-review state; show "verify" so the user knows all steps ran.
// The per-run oracle mode suffix (·mode) is shown when an override is active.
func (w *WorkflowState) SidebarLabel() string {
	var base string
	switch {
	case w.Phase == WFImplement && w.StepCount > 0 && w.StepIdx > w.StepCount:
		base = "verify" // all steps done, awaiting final review outcome
	case w.Phase == WFImplement && w.StepCount > 0:
		base = fmt.Sprintf("implement %d/%d", w.StepIdx, w.StepCount)
	default:
		base = w.PhaseName()
	}
	if w.OracleMode != "" {
		return base + " ·" + w.OracleMode
	}
	return base
}

// StatusString returns a multi-line description for /plan status.
func (w *WorkflowState) StatusString() string {
	return fmt.Sprintf("phase: %s  step: %d/%d\ntask: %s\nplan: %s",
		w.PhaseName(), w.StepIdx, w.StepCount, w.Task, w.PlanPath)
}

// Directive returns the text prepended to the user message for the current
// phase turn. Returns "" for phases that do not drive a model turn (REVIEW,
// PRESENT, DONE).
func (w *WorkflowState) Directive() string {
	switch w.Phase {
	case WFGather:
		return fmt.Sprintf(
			"[WORKFLOW GATHER] Investigate (read-only) what is needed for:\n"+
				"%s\n\n"+
				"– Use read_file, list_dir, search_files, find_files only; make no code modifications.\n"+
				"– Identify the relevant sites in at most ~5 reads, then write your findings to %s under ## Findings.\n"+
				"  Keep findings focused: relevant files, code sections, open questions — do not exhaustively search.\n"+
				"– When the investigation is complete, end your response with exactly: %s\n"+
				"  (The sentinel must appear in your final text, not inside <thinking> or reasoning blocks — those are discarded.)",
			w.Task, w.PlanPath, WFPhaseDone)

	case WFPlan:
		// Reformat directive: plan was written but steps are unparseable.
		if w.PlanFormatInvalid {
			return fmt.Sprintf(
				"[WORKFLOW PLAN REFORMAT] The plan was written but contains no parseable numbered steps. "+
					"## Plan must use 'N. description' lines at the top level (e.g. '1. Fix the serializer'). "+
					"Sub-bullets (– or *) are allowed beneath each step, but headers (### Step 1) are not valid — "+
					"use the N. format only. "+
					"Read %s ## Plan, rewrite it with the correct N. format, then emit: %s\n"+
					"  (The sentinel must appear in your final text, not inside <thinking> or reasoning blocks.)",
				w.PlanPath, WFPhaseDone)
		}
		return fmt.Sprintf(
			"[WORKFLOW PLAN] Write an implementation plan for:\n"+
				"%s\n\n"+
				"– Read %s ## Findings for context.\n"+
				"– Write a numbered plan under ## Plan in %s. "+
				"Each step is a line beginning 'N.' at the top level (e.g. '1. Fix the serializer'). "+
				"Sub-bullets are allowed beneath each step. Do not use headers for steps.\n"+
				"– When the plan is fully written, end your response with exactly: %s\n"+
				"  (The sentinel must appear in your final text, not inside <thinking> or reasoning blocks — those are discarded.)",
			w.Task, w.PlanPath, w.PlanPath, WFPhaseDone)

	case WFImplement:
		// Verify state: all steps ran but final review flagged gaps.
		// The user can ask the model to fix the gaps; the turn completing
		// automatically re-triggers the final review.
		if w.StepIdx > w.StepCount {
			return "[WORKFLOW VERIFY] All implementation steps are complete but the final review " +
				"flagged unresolved gaps. Address only the flagged criteria visible in this conversation. " +
				"When your work is done, end your response with exactly one line:\n" +
				"  " + WFStepLogMark + " Remediation: <one-sentence reconciliation summary>%%\n" +
				"Then the final review will re-run automatically. " +
				"Do NOT emit %%STEP_DONE%% or %%PHASE_DONE%% — only the %%STEP_LOG: Remediation:…%% line."
		}

		oracleCtx := ""
		if w.OracleReview != "" && w.StepIdx == 1 {
			// Inject advisory oracle review only on the first step.
			oracleCtx = "\n\nOracle plan review (advisory, do not auto-apply):\n" +
				truncate(w.OracleReview, 500) + "\n"
		}
		return fmt.Sprintf(
			"[WORKFLOW IMPLEMENT STEP %d/%d] Execute step %d from %s ## Plan.%s\n"+
				"– Make only the changes required by this step.\n"+
				"– Do NOT edit %s ## Step log directly — Wakil manages that section.\n"+
				"– When done (success or failure) emit a step-log line in this exact format:\n"+
				"  %sStep %d: <action taken> | outcome: <result> | deviation: <none or description>%%%%\n"+
				"– If this step fails or you must deviate, explain why and then emit the line above and end with: %s\n"+
				"– If successful, emit the line above and end with: %s\n"+
				"  (Both the %%%%STEP_LOG%%%%: entry and the terminal sentinel must appear in your final text, not inside <thinking> or reasoning blocks.)",
			w.StepIdx, w.StepCount, w.StepIdx, w.PlanPath, oracleCtx,
			w.PlanPath,
			WFStepLogMark, w.StepIdx, WFStepFailed, WFStepDone)

	case WFReview:
		// Retry directive: oracle was unavailable on the first attempt.
		// Any completed turn in WFReview re-attempts the oracle automatically;
		// this directive tells the model what state it is in.
		reason := ""
		if w.ReviewSkipReason != "" {
			reason = " Last attempt reason: " + w.ReviewSkipReason + "."
		}
		return "[WORKFLOW REVIEW RETRY] The oracle plan review is pending." + reason +
			" When this turn completes the review will be re-attempted automatically. " +
			"You may respond normally or provide additional context about the plan."

	default:
		return ""
	}
}

// truncate is a package-local helper used by Directive.
func truncate(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// ExtractStepLogEntry scans text line by line, skipping fenced code blocks
// (``` … ```), and returns the LAST %%STEP_LOG: …%% entry found outside a
// fence. Taking the last match means a model that both quotes the format in an
// explanation and emits the real sentinel afterwards returns the real one.
// Returns "" if no sentinel is found outside a fenced region.
func ExtractStepLogEntry(text string) string {
	var result string
	inFence := false
	for _, line := range strings.Split(text, "\n") {
		// Toggle fence state on ``` boundaries (any indent).
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		idx := strings.Index(line, WFStepLogMark)
		if idx < 0 {
			continue
		}
		afterMark := line[idx+len(WFStepLogMark):]
		closeIdx := strings.Index(afterMark, "%%")
		var entry string
		if closeIdx >= 0 {
			entry = strings.TrimSpace(afterMark[:closeIdx])
		} else {
			// No closing %% on this line — take the remainder as best-effort.
			entry = strings.TrimSpace(afterMark)
		}
		if entry != "" {
			result = entry // overwrite to keep the last match
		}
	}
	return result
}

// CountStepLogEntries returns the number of step entries (lines starting with
// "Step ") in the ## Step log section of planContent.
func CountStepLogEntries(planContent string) int {
	stepLog := ExtractPlanSection(planContent, "## Step log")
	count := 0
	for _, e := range RecentStepEntries(stepLog, 9999) {
		if strings.HasPrefix(e, "Step ") {
			count++
		}
	}
	return count
}

// LastAssistantText returns the content of the last assistant message in conv,
// or "" if none exists.
func LastAssistantText(conv []proxy.Message) string {
	for i := len(conv) - 1; i >= 0; i-- {
		if conv[i].Role == "assistant" && conv[i].Content != nil {
			return *conv[i].Content
		}
	}
	return ""
}

// DetectPhaseMarkers scans the last assistant message in conv for the three
// phase-completion sentinels. Only the most-recent assistant message is checked.
func DetectPhaseMarkers(conv []proxy.Message) (phaseDone, stepDone, stepFailed bool) {
	for i := len(conv) - 1; i >= 0; i-- {
		if conv[i].Role == "assistant" && conv[i].Content != nil {
			text := *conv[i].Content
			phaseDone = strings.Contains(text, WFPhaseDone)
			stepDone = strings.Contains(text, WFStepDone)
			stepFailed = strings.Contains(text, WFStepFailed)
			return
		}
	}
	return
}

// CountPlanSteps returns the number of numbered items in the ## Plan section of
// planContent. Accepts "1." or "1)" prefixes. Returns 0 if the section is absent.
func CountPlanSteps(planContent string) int {
	idx := strings.Index(planContent, "## Plan")
	if idx < 0 {
		return 0
	}
	section := planContent[idx:]
	// Clip at next ## heading.
	if next := strings.Index(section[7:], "\n##"); next >= 0 {
		section = section[:next+7]
	}
	re := regexp.MustCompile(`(?m)^\d+[.)]\s+`)
	return len(re.FindAllString(section, -1))
}

// assembleBriefing builds an oracle briefing from pre-extracted parts.
//
// ## Task and ## Plan are always included intact. Step-log entries are windowed
// from the most recent end: oldest are dropped first with an explicit
// "[… N earlier entries omitted …]" marker so the oracle knows evidence was
// withheld. Hard tail-truncation is the absolute last resort; it always appends
// "[briefing truncated]" so the oracle can flag the incomplete input.
//
// task comes from WorkflowState.Task (Wakil's authoritative record) and is
// never read from plan.md, so model edits to ## Task cannot corrupt the briefing.
// FindingsCap is the maximum bytes kept from the ## Findings section in a
// briefing. Findings are written oldest-first during GATHER, so early facts
// are the most relevant — tail-truncation is acceptable here.
const FindingsCap = 4 * 1024

// CapFindings returns findings truncated to FindingsCap bytes, appending
// "[findings truncated]" when the cap bites.
func CapFindings(findings string) string {
	if len(findings) <= FindingsCap {
		return findings
	}
	return findings[:FindingsCap] + "\n[findings truncated]"
}

func assembleBriefing(task, findings, plan string, entries []string, question, stepLogHeader string, maxBytes int) string {
	build := func(kept []string, omitted int) string {
		var sb strings.Builder
		if task != "" {
			fmt.Fprintf(&sb, "## Task\n\n%s\n\n", task)
		}
		if findings != "" {
			fmt.Fprintf(&sb, "## Findings\n\n%s\n\n", CapFindings(findings))
		}
		if plan != "" {
			fmt.Fprintf(&sb, "## Plan\n\n%s\n\n", plan)
		}
		if len(kept) > 0 || omitted > 0 {
			sb.WriteString(stepLogHeader + "\n\n")
			if omitted > 0 {
				fmt.Fprintf(&sb, "[… %d earlier entries omitted …]\n\n", omitted)
			}
			if len(kept) > 0 {
				sb.WriteString(strings.Join(kept, "\n\n"))
				sb.WriteString("\n\n")
			}
		}
		fmt.Fprintf(&sb, "## Question\n\n%s", question)
		return sb.String()
	}

	// Fast path: everything fits.
	if result := build(entries, 0); len(result) <= maxBytes {
		return result
	}

	// Drop oldest entries, inserting an omission marker at the drop point.
	for dropped := 1; dropped <= len(entries); dropped++ {
		if result := build(entries[dropped:], dropped); len(result) <= maxBytes {
			return result
		}
	}

	// Even with no step-log entries, still too big: hard tail truncation.
	result := build(nil, 0)
	if len(result) > maxBytes {
		result = result[:maxBytes] + "\n[briefing truncated]"
	}
	return result
}

// BuildOracleBriefing builds an oracle briefing for the standard review question.
func BuildOracleBriefing(task, planContent, question string) string {
	findings := ExtractPlanSection(planContent, "## Findings")
	plan := ExtractPlanSection(planContent, "## Plan")
	stepLog := ExtractPlanSection(planContent, "## Step log")
	entries := RecentStepEntries(stepLog, WFStepLogK)
	return assembleBriefing(task, findings, plan, entries, question, "## Step log (recent)", WFBriefingCap)
}

// ExtractPlanSection returns the trimmed body of the named markdown section
// (e.g. "## Task"). Returns "" if the section is absent.
//
// Clipping uses "\n## " (two # then space) to match only level-2 headings, not
// level-3+ sub-headings like "### Step 1" that may appear within a section body.
func ExtractPlanSection(content, header string) string {
	idx := strings.Index(content, header)
	if idx < 0 {
		return ""
	}
	start := idx + len(header)
	// Skip past the header line.
	if nl := strings.Index(content[start:], "\n"); nl >= 0 {
		start += nl + 1
	}
	rest := content[start:]
	// Clip at the next level-2 (## ) heading — "## Title" but not "### Sub".
	if next := strings.Index(rest, "\n## "); next >= 0 {
		rest = rest[:next]
	}
	return strings.TrimSpace(rest)
}

// RecentStepEntries returns up to k non-empty step-log paragraphs, most-recent last.
func RecentStepEntries(stepLog string, k int) []string {
	if stepLog == "" {
		return nil
	}
	var entries []string
	for _, p := range strings.Split(stepLog, "\n\n") {
		p = strings.TrimSpace(p)
		if p != "" && p != "(none yet)" && p != "(pending implementation)" {
			entries = append(entries, p)
		}
	}
	if len(entries) <= k {
		return entries
	}
	return entries[len(entries)-k:]
}

// WFAppendToStepLog appends entry to planContent. If the content already
// contains a ## Step log section the entry is appended after it; otherwise the
// section header is created first. This is the only path that writes to
// ## Step log — the model is never instructed to edit that section.
func WFAppendToStepLog(planContent, entry string) string {
	trimmed := strings.TrimRight(planContent, "\n")
	if !strings.Contains(planContent, "## Step log") {
		return trimmed + "\n\n## Step log\n\n" + entry + "\n"
	}
	return trimmed + "\n\n" + entry + "\n"
}

// HashPlanSection returns a short FNV-64a fingerprint of the ## Plan section
// extracted from planContent. Used for stale-review detection: if the hash
// at /plan approve differs from the hash stored at REVIEW success, the plan
// was modified after the oracle reviewed it.
func HashPlanSection(planContent string) string {
	section := ExtractPlanSection(planContent, "## Plan")
	h := fnv.New64a()
	h.Write([]byte(section))
	return fmt.Sprintf("%016x", h.Sum64())
}

// IsPreImplementPhase returns true for workflow phases in which implementation
// writes are not yet permitted (GATHER, PLAN, REVIEW, PRESENT).
func IsPreImplementPhase(phase WorkflowPhase) bool {
	return phase == WFGather || phase == WFPlan || phase == WFReview || phase == WFPresent
}

// IsWakilPath returns true when path points inside the .wakil/ workflow
// directory. Handles both relative paths (.wakil/plan.md) and absolute paths
// used by the executor in Docker mode (/work/.wakil/plan.md).
func IsWakilPath(path string) bool {
	p := filepath.ToSlash(filepath.Clean(path))
	return p == ".wakil" ||
		strings.HasPrefix(p, ".wakil/") ||
		strings.Contains(p, "/.wakil/") ||
		strings.HasSuffix(p, "/.wakil")
}

// IsPlanFilePath returns true when path refers to the same file as planPath.
// It handles mixed relative/absolute comparisons: ".wakil/plan.md" and
// "/work/.wakil/plan.md" are treated as the same file because the relative
// path is a suffix of the absolute path.
func IsPlanFilePath(path, planPath string) bool {
	c1 := filepath.ToSlash(filepath.Clean(path))
	c2 := filepath.ToSlash(filepath.Clean(planPath))
	if c1 == c2 {
		return true
	}
	// One absolute, one relative: check suffix.
	return strings.HasSuffix(c2, "/"+c1) || strings.HasSuffix(c1, "/"+c2)
}

// ValidateBriefing verifies that briefing carries enough evidence to be useful
// to the oracle. Returns "" when valid; returns a short reason string otherwise.
// requireStepLog=true (final review) additionally demands at least one step entry.
//
// Headers are validated structurally: the header must be an exact line ("^## Task$")
// followed by at least one non-empty, non-header body line. A log entry that
// merely contains the string "## Task" does not satisfy this check.
func ValidateBriefing(briefing string, requireStepLog bool) string {
	if !BriefingSectionPresent(briefing, "## Task") {
		return "## Task section missing or empty"
	}
	if !BriefingSectionPresent(briefing, "## Plan") {
		return "## Plan section missing or empty"
	}
	if requireStepLog {
		// Check both "## Step log" (final review) and "## Step log (recent)" (REVIEW).
		stepLog := ExtractPlanSection(briefing, "## Step log")
		if stepLog == "" {
			stepLog = ExtractPlanSection(briefing, "## Step log (recent)")
		}
		if len(RecentStepEntries(stepLog, 9999)) == 0 {
			return "no step-log entries"
		}
	}
	return ""
}

// BriefingSectionPresent returns true when header is an exact line in text
// AND is immediately followed (after optional blank lines) by at least one
// non-empty line that is not itself a level-2 heading. This prevents a log
// entry that contains "## Task" as a substring from satisfying the check.
func BriefingSectionPresent(text, header string) bool {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if line != header {
			continue
		}
		// Scan forward for the first non-blank line.
		for j := i + 1; j < len(lines); j++ {
			l := strings.TrimSpace(lines[j])
			if l == "" {
				continue
			}
			// Hit the next level-2 heading before any body — section is empty.
			if strings.HasPrefix(l, "## ") {
				return false
			}
			return true
		}
		return false // EOF without body
	}
	return false // header line not found
}

// BuildFinalReviewBriefing includes ALL step-log entries (k=9999) so the
// closing review has the complete history. maxBytes defaults to 16 KB (0 = default)
// because evidence-bearing logs are legitimately bigger than claims-only ones.
func BuildFinalReviewBriefing(task, planContent, question string, maxBytes int) string {
	if maxBytes <= 0 {
		maxBytes = 16 * 1024
	}
	findings := ExtractPlanSection(planContent, "## Findings")
	plan := ExtractPlanSection(planContent, "## Plan")
	stepLog := ExtractPlanSection(planContent, "## Step log")
	entries := RecentStepEntries(stepLog, 9999)
	return assembleBriefing(task, findings, plan, entries, question, "## Step log", maxBytes)
}

// GapGist returns a one-line summary from an oracle gap-flagging response —
// the first substantive, non-verdict line, truncated to 120 chars. Storing the
// gist (not the full multi-paragraph oracle response) keeps the gap-flag log
// entry short so it does not crowd out the remediation evidence that follows.
// It also prevents the oracle response's own blank lines from splitting the
// entry across multiple RecentStepEntries paragraphs.
func GapGist(oracleResult string) string {
	for _, line := range strings.Split(oracleResult, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "VERDICT:") {
			continue
		}
		return truncate(line, 120)
	}
	return "(gaps flagged — see oracle response)"
}

// WFFlagsGaps reads the structured verdict line that the final-review question
// instructs the oracle to emit:
//
//	VERDICT: PASS  →  no gaps, workflow may complete
//	VERDICT: GAPS  →  one or more criteria unmet or deviations unresolved
//
// If no verdict line is found the function returns true (fail-closed): an
// unparseable response is treated the same as a flagged one so the gate
// cannot be silently bypassed by a model that skips the verdict.
func WFFlagsGaps(response string) bool {
	for _, line := range strings.Split(response, "\n") {
		switch strings.TrimSpace(line) {
		case "VERDICT: PASS":
			return false
		case "VERDICT: GAPS":
			return true
		}
	}
	return true // fail-closed: no structured verdict → treat as gaps
}

// WFEverystepCritical returns true when an every-step oracle critique indicates
// a substantive problem that warrants pausing before the next step.
func WFEverystepCritical(response string) bool {
	lower := strings.ToLower(response)
	for _, kw := range []string{
		"incorrect", "wrong", "error", "fail", "problem",
		"issue", "concern", "missing", "incomplete", "broken",
		"not done", "should have", "doesn't", "does not",
	} {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// WFInitPlanContent returns the initial markdown content for a new plan.md.
// ## Step log is deliberately absent from the scaffold — Wakil appends that
// section on first write, preventing the duplicate-section issue that arises
// when the model writes a full plan.md that also includes the header.
func WFInitPlanContent(task string) string {
	return fmt.Sprintf(
		"## Task\n\n%s\n\n"+
			"## Findings\n\n(pending gather phase)\n\n"+
			"## Plan\n\n(pending plan phase)\n",
		task)
}
