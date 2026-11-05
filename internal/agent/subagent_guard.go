package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/treeol/wakil/internal/proxy"
	wtools "github.com/treeol/wakil/internal/tools"
)

// subagent_guard.go — harness-side dispatch discipline (0.4.2).
//
// Everything here is HARNESS STATE derived from actual tool calls (the read
// ledger) and dispatch history — never from subagent text. The failure being
// fixed is a subagent that claims work it did not do.
//
// Contract (do not weaken):
//   - The only identical-scope re-run is the single off-task retry, performed
//     by the harness itself (so it happens exactly once, not "when the model
//     remembers").
//   - Every other end of the automatic path is a user-visible notice plus a
//     stop. A stopped task refuses further dispatch until the USER sends a new
//     message. The parent never silently takes over.

// Outcome values carried on SubagentSummary.Outcome / SubagentResult.Status.
const (
	outcomeComplete = "complete"
	outcomePartial  = "partial"
	outcomeRejected = "rejected" // off-task: zero listed files read
	outcomeRefused  = "refused"  // harness refused the dispatch (stop / same scope)
	outcomeFailed   = "failed"   // error / cancel / panic — no usable work
)

// Lineage states.
const (
	lineageOpen    = "open"    // has an unresolved partial; one strictly smaller split allowed
	lineageStopped = "stopped" // notice shown; refuses dispatch until the next user message
	lineageDone    = "done"
)

// dispatchLineage is one task as the harness tracks it: the scope of its first
// dispatch and what has happened to it since.
type dispatchLineage struct {
	files      []string
	hash       string
	state      string
	stopReason string
	splitUsed  bool // the one allowed strictly-smaller follow-up has been taken
}

// dispatchGuard is the parent-side registry of lineages. Zero value is ready.
type dispatchGuard struct {
	mu       sync.Mutex
	lineages []*dispatchLineage
}

// reset forgets all lineages and lifts every stop. Called when the USER sends
// a new message (the answer to "assign differently or take over?"). It is NOT
// called on an async-wake Resume.
func (g *dispatchGuard) reset() {
	g.mu.Lock()
	g.lineages = nil
	g.mu.Unlock()
}

// taskHash is a whitespace-insensitive hash of the task text.
func taskHash(task string) string {
	sum := sha256.Sum256([]byte(strings.Join(strings.Fields(task), " ")))
	return hex.EncodeToString(sum[:8])
}

func sameFileSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	bs := toStrSet(b)
	for _, f := range a {
		if !bs[f] {
			return false
		}
	}
	return true
}

func subsetOf(sub, super []string) bool {
	ss := toStrSet(super)
	for _, f := range sub {
		if !ss[f] {
			return false
		}
	}
	return true
}

// admit decides whether a dispatch may run. It returns the lineage the
// dispatch belongs to, whether it is the split of an open partial, and a
// non-empty refusal reason when the dispatch must NOT run.
func (g *dispatchGuard) admit(files []string, hash string) (l *dispatchLineage, isSplit bool, refuse string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i := len(g.lineages) - 1; i >= 0; i-- {
		cand := g.lineages[i]
		same := cand.hash == hash && sameFileSet(cand.files, files)
		contained := len(files) > 0 && subsetOf(files, cand.files)
		switch cand.state {
		case lineageStopped:
			if same || cand.hash == hash || contained {
				return nil, false, "this task was stopped (" + cand.stopReason + ") — the user must decide; no further automatic dispatch for it"
			}
		case lineageOpen:
			if isStrictlySmallerScope(cand.files, files, cand.hash, hash) {
				if cand.splitUsed {
					return nil, false, "the one allowed split of this partial result was already used — report to the user instead of dispatching again"
				}
				cand.splitUsed = true
				return cand, true, ""
			}
			// Not a strictly smaller scope. It is a re-run of the open partial
			// when it covers the whole original file set (equal OR a superset:
			// widening re-reads everything already covered), or — for a task
			// with no file list, which can only be linked by its text — when the
			// task text is identical.
			rerun := cand.hash == hash && sameFileSet(cand.files, files)
			if len(cand.files) > 0 && subsetOf(cand.files, files) {
				rerun = true
			}
			if rerun {
				return nil, false, "a follow-up to a partial result must cover a strictly smaller file set with different task text; this one covers the same files or more"
			}
		}
	}
	nl := &dispatchLineage{files: append([]string(nil), files...), hash: hash, state: lineageDone}
	g.lineages = append(g.lineages, nl)
	return nl, false, ""
}

func (g *dispatchGuard) setState(l *dispatchLineage, state, reason string) {
	g.mu.Lock()
	l.state = state
	l.stopReason = reason
	g.mu.Unlock()
}

// subagentOutcome classifies a finished summary. Pure.
func subagentOutcome(s SubagentSummary) string {
	if s.Outcome != "" {
		return s.Outcome
	}
	if s.Status == "" {
		return outcomeComplete
	}
	switch s.StopReason {
	case "iteration_limit", "turn_budget_exhausted", "hard_max_shed", "confinement_breaker":
		return outcomePartial
	}
	return outcomeFailed
}

// offTask reports whether a dispatch that carried an explicit file list read
// none of it. Errors/cancellations never count as off-task: a child that never
// got to work has not been shown to ignore its assignment.
func offTask(s SubagentSummary, files []string) bool {
	if len(files) == 0 || s.StopReason == "error" {
		return false
	}
	if sub := subagentOutcome(s); sub == outcomeFailed {
		return false
	}
	return s.FileReadLedger != nil && len(s.FileReadLedger.Read) == 0
}

// assignedFilesBlock is appended to the child's task so it knows its list.
// (Without it the ledger would score the child against a list it never saw.)
func assignedFilesBlock(files []string) string {
	if len(files) == 0 {
		return ""
	}
	return "\n\nAssigned files — read each of them (read_file / read_file_full) before you report:\n- " + strings.Join(files, "\n- ")
}

// offTaskRetryText is the task text of the single off-task retry.
func offTaskRetryText(task string, files []string) string {
	return task + "\n\nYou read none of the assigned files: " + strings.Join(files, ", ") + ". Read them before reporting."
}

// applyResult stamps the harness-derived SubagentResult fields onto the summary
// (outcome, files_covered / files_not_read from the ledger, remaining_scope for
// a partial, files_listed). Idempotent.
func applyResult(s *SubagentSummary, files []string, class string) {
	s.Outcome = subagentOutcome(*s)
	if s.SubagentClass == "" {
		s.SubagentClass = class
	}
	if s.FileReadLedger != nil {
		l := s.FileReadLedger
		s.FilesCovered = l.Read
		s.FilesNotRead = l.NotRead
		s.FilesExtra = l.Extra
		s.FilesListedN = len(l.Listed)
	}
	if s.Outcome == outcomePartial && s.RemainingScope == "" && len(s.FilesNotRead) > 0 {
		s.RemainingScope = "files not read: " + strings.Join(s.FilesNotRead, ", ")
	}
}

// Result projects the summary onto the structured SubagentResult.
func (s SubagentSummary) Result() SubagentResult {
	r := SubagentResult{
		Status:         subagentOutcome(s),
		FilesCovered:   s.FilesCovered,
		FilesNotRead:   s.FilesNotRead,
		Findings:       s.Findings,
		RemainingScope: s.RemainingScope,
		Objective:      s.Objective,
		Uncertainty:    s.Uncertainty,
		FilesChanged:   s.FilesChanged,
		ExternalCalls:  s.ExternalCalls,
	}
	if s.Budget != nil {
		r.Budget = *s.Budget
	}
	return r
}

// rejectedSummary replaces an off-task result: the child's findings are spilled
// to the trace cache and NOT carried into the parent context.
func rejectedSummary(a *App, orig SubagentSummary, files []string, class string, retried bool) SubagentSummary {
	if full := orig.Render(); full != "" {
		if p := wtools.SpillToCache(a.chatID(), "dispatch_subagent_rejected", full); p != "" {
			orig.Uncertainty = append(orig.Uncertainty, "discarded off-task findings spilled to "+p)
		}
	}
	rs := SubagentSummary{
		Objective:      orig.Objective,
		Status:         "incomplete",
		StopReason:     "off_task",
		Outcome:        outcomeRejected,
		RejectReason:   "off-task",
		Retry:          retried,
		Uncertainty:    []string{fmt.Sprintf("rejected: off-task — read 0/%d assigned files; findings discarded", len(files))},
		FileReadLedger: orig.FileReadLedger,
		SubagentClass:  class,
		Budget:         orig.Budget,
	}
	applyResult(&rs, files, class)
	return rs
}

// refusedSummary is returned (without running a child) when the guard refuses.
func refusedSummary(task, reason string) SubagentSummary {
	return SubagentSummary{
		Objective:   task,
		Status:      "incomplete",
		StopReason:  "harness_stop",
		Outcome:     outcomeRefused,
		Uncertainty: []string{"dispatch refused by the harness: " + reason},
		Notice:      "subagent dispatch refused — " + reason,
	}
}

// offTaskNotice / partialNotice are the user-visible notices (exact wording is
// part of the contract).
func offTaskNotice(chatID string, nFiles int) string {
	return fmt.Sprintf("subagent %s off-task twice: read 0/%d assigned files — assign differently or take over?", ShortID(chatID), nFiles)
}

func partialNotice(chatID string, s SubagentSummary) string {
	if s.FilesListedN > 0 {
		return fmt.Sprintf("subagent %s partial: %d/%d files, %d findings — split or take over?", ShortID(chatID), len(s.FilesCovered), s.FilesListedN, len(s.Findings))
	}
	return fmt.Sprintf("subagent %s partial: no file list, %d findings — split or take over?", ShortID(chatID), len(s.Findings))
}

// printSubagentNotice writes a notice to the chat. MAIN GOROUTINE ONLY.
func (a *App) printSubagentNotice(notice string) {
	if notice == "" {
		return
	}
	fmt.Fprintln(a.Out, Yellow("⚠ "+notice))
}

// subagentDoneErr is the Err string for SubagentDoneMsg / async results.
func subagentDoneErr(s SubagentSummary) string {
	if s.Status != "incomplete" {
		return ""
	}
	switch s.Outcome {
	case outcomeRejected:
		return "subagent rejected: off-task"
	case outcomeRefused:
		return "subagent refused: stopped by harness"
	}
	return "subagent incomplete (budget/cancelled)"
}

// subagentFinishedStatus is the status string carried on the early-completion
// event. It rides in the existing free-form status field ("<word>[ files R/L]")
// so no proto/schema change is needed; the TUI formatter parses it.
func subagentFinishedStatus(s SubagentSummary) string {
	word := s.Status
	switch s.Outcome {
	case outcomePartial:
		word = "partial"
	case outcomeRejected:
		word = "rejected"
	case outcomeRefused:
		word = "refused"
	}
	if s.FilesListedN > 0 {
		if word == "" {
			word = "ok"
		}
		return fmt.Sprintf("%s files %d/%d", word, len(s.FilesCovered), s.FilesListedN)
	}
	return word
}

// dispatchTracked is dispatchSubagent behind the harness guard. Same return
// shape. Used by every model-driven dispatch path (sequential handler and the
// parallel/async worker); safe to call from a worker goroutine (it never
// writes a.Out — notices travel on the summary and are printed by the caller
// on the main goroutine).
func (a *App) dispatchTracked(ctx context.Context, task string, progressOut io.Writer, resolvedBackend, capability, model, class string, files []string, chatID string) (SubagentSummary, []proxy.GroundingEntry, int, string, []proxy.CostRow, []string) {
	hash := taskHash(task)
	lin, isSplit, refuse := a.guard.admit(files, hash)
	if refuse != "" {
		s := refusedSummary(task, refuse)
		applyResult(&s, files, class)
		return s, nil, 0, "", nil, nil
	}

	summary, grounding, ctxSize, usedBackend, costRows, changed := a.dispatchSubagent(ctx, task, progressOut, resolvedBackend, capability, model, class, files, chatID)

	retried := false
	if offTask(summary, files) {
		// Single, harness-performed retry: identical scope on purpose, so it
		// bypasses admit/isStrictlySmallerScope. Findings of the rejected run
		// are spilled, not returned.
		_ = rejectedSummary(a, summary, files, class, false) // spills the first run's findings
		retried = true
		s2, g2, c2, ub2, rows2, ch2 := a.dispatchSubagent(ctx, offTaskRetryText(task, files), progressOut, resolvedBackend, capability, model, class, files, chatID)
		grounding = append(grounding, g2...)
		costRows = append(costRows, rows2...)
		changed = append(changed, ch2...)
		ctxSize, usedBackend = c2, ub2
		summary = s2
		if offTask(summary, files) {
			summary = rejectedSummary(a, summary, files, class, true)
			summary.Notice = offTaskNotice(chatID, len(files))
			a.guard.setState(lin, lineageStopped, "off-task twice")
			return summary, grounding, ctxSize, usedBackend, costRows, changed
		}
	}
	applyResult(&summary, files, class)
	summary.Retry = retried

	switch summary.Outcome {
	case outcomePartial:
		empty := len(summary.Findings) == 0 && len(summary.FilesNotRead) == 0
		if isSplit || empty {
			summary.Notice = partialNotice(chatID, summary)
			reason := "partial with nothing left to split"
			if isSplit {
				reason = "second partial on the split"
			}
			a.guard.setState(lin, lineageStopped, reason)
		} else {
			a.guard.setState(lin, lineageOpen, "")
		}
	default:
		a.guard.setState(lin, lineageDone, "")
	}
	return summary, grounding, ctxSize, usedBackend, costRows, changed
}

// BeginUserTurn lifts every dispatch stop and forgets lineages. Called once per
// genuine USER turn (TUI RunTurn, host turn) — never from async-wake Resume or
// the internal stream/empty-response retries, so a stop survives until the
// user has actually answered the notice.
func (a *App) BeginUserTurn() { a.guard.reset() }
