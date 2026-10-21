package agent

import (
	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/workflow"
)

// app_options.go: construction API for App (WP-6.3-followup).
//
// This file provides functional options and exported setters that allow
// cross-package callers (cmd/wakil, internal/tui tests) to configure an App
// WITHOUT directly keying the deferred fields (Costs, AutoCounsel,
// MaxCounsel, CounselMode, Workflow) in composite literals or post-
// construction assignments. This unblocks the future extraction of these
// fields into unexported sub-structs (costState, turnState) — once all
// external construction and mutation goes through these methods, the fields
// can be unexported without breaking cmd/wakil or test code.
//
// Design decisions (informed by 3-panel Mashūra review):
// - Required deps stay as struct fields in the App literal (Cfg, Client, Exec,
//   Tools, etc.) — these are not being extracted and direct keying is fine.
// - Only the DEFERRED fields (those slated for sub-struct extraction) get
//   options/setters.
// - Post-construction mutation uses exported setters (SetCounselMode,
//   SetWorkflow) following the SetConsent precedent.
// - No NewApp constructor with network I/O — callers handle context limit
//   resolution, backend/model list fetching, store opens, etc. The options
//   are applied to a partially-constructed App literal.

// AppOption is a functional option for constructing an App.
type AppOption func(*App)

// ApplyOptions applies the given options to the App, mutating it in place.
// Called by buildApp after constructing the App literal with required deps.
func (a *App) ApplyOptions(opts ...AppOption) {
	for _, opt := range opts {
		opt(a)
	}
}

// WithCosts sets the cost tracker on the App. Pass nil to disable cost
// tracking (subagents, headless runs that don't need it).
func WithCosts(costs *proxy.CostTracker) AppOption {
	return func(a *App) {
		a.Costs = costs
	}
}

// WithAutoCounsel sets the auto-counsel configuration. When enabled is true,
// mashura__debug fires automatically on struggle detection (bounded by max).
// max=0 with enabled=true uses the default cap (3).
func WithAutoCounsel(enabled bool, max int) AppOption {
	return func(a *App) {
		a.AutoCounsel = enabled
		a.MaxCounsel = max
	}
}

// WithVerifyEnabled enables the workflow verification runner.
func WithVerifyEnabled(enabled bool) AppOption {
	return func(a *App) {
		a.VerifyEnabled = enabled
	}
}

// WithHeadless marks the App as a headless (non-interactive) session.
func WithHeadless(headless bool) AppOption {
	return func(a *App) {
		a.IsHeadless = headless
	}
}

// SetCounselMode sets the counsel mode ("suggest", "auto", or "off").
// This is the exported setter for mid-session mutation (e.g. /counsel command,
// main.go defaults). Follows the SetConsent precedent for cross-package
// mutation of fields slated for sub-struct extraction.
func (a *App) SetCounselMode(mode string) {
	a.CounselMode = mode
}

// SetWorkflow sets the active workflow state. This is the exported setter for
// cross-package mutation (e.g. main.go resume, run.go headless workflow setup).
// Pass nil to clear the workflow. Acquires stateMu so the write is synchronized
// with concurrent readers (GetSessionState, SnapshotSessionState).
func (a *App) SetWorkflow(wf *workflow.WorkflowState) {
	a.stateMu.Lock()
	a.Workflow = wf
	a.stateMu.Unlock()
}

// Workflow ownership model (concurrency contract):
//
//   - WRITERS: the turn goroutine (HandleWorkflowTransition / HandleFinalReview /
//     HandleReviewOracle, hostturn's resolveWaitingState) and idle-time /plan
//     command goroutines. Turns and commands do not overlap (the TUI
//     hard-rejects slash commands mid-turn), but two idle-time commands CAN
//     overlap: each runs in its own Cmd goroutine. Every field WRITE and every
//     pointer replacement therefore goes through UpdateWorkflow / SetWorkflow
//     (ordered against readers), and any write that follows blocking work
//     uses UpdateWorkflowIf so it cannot land on a replaced workflow.
//   - READERS on other goroutines (facade Snapshot/Info per frame, session
//     persistence) must read under stateMu.RLock and must not retain the
//     pointer past the unlock — use WorkflowCopy.
//
// UpdateWorkflow applies fn to the active workflow under stateMu.Lock and
// reports whether a workflow was active. fn must only assign fields: it must
// not call App methods (many take stateMu, and RWMutex is not reentrant) or
// do I/O.
func (a *App) UpdateWorkflow(fn func(wf *workflow.WorkflowState)) bool {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	if a.Workflow == nil {
		return false
	}
	fn(a.Workflow)
	return true
}

// UpdateWorkflowIf is UpdateWorkflow guarded by pointer identity: fn runs only
// if expected is still the active workflow. Use it after blocking work (file
// I/O in a /plan command closure, an oracle call) so a commit cannot land on a
// workflow that was aborted or replaced in the meantime — the TUI runs each
// slash command in its own goroutine, so two quick /plan commands may overlap.
func (a *App) UpdateWorkflowIf(expected *workflow.WorkflowState, fn func(wf *workflow.WorkflowState)) bool {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	if a.Workflow == nil || a.Workflow != expected {
		return false
	}
	fn(a.Workflow)
	return true
}

// WorkflowCopy returns a detached copy of the active workflow (nil when none),
// taken under stateMu.RLock. Safe to read on any goroutine.
func (a *App) WorkflowCopy() *workflow.WorkflowState {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return copyWorkflow(a.Workflow)
}

// copyWorkflow returns a value copy of wf. WorkflowState holds only value
// fields (strings, ints, bools), so a shallow copy is a full copy. Caller must
// hold stateMu (read or write) if wf is the live a.Workflow.
func copyWorkflow(wf *workflow.WorkflowState) *workflow.WorkflowState {
	if wf == nil {
		return nil
	}
	cp := *wf
	return &cp
}

// SetAutoCounsel is the post-construction setter for auto-counsel config.
// Used by run.go (headless) to set the --auto-counsel/--max-counsel flags
// after buildApp returns.
func (a *App) SetAutoCounsel(enabled bool, max int) {
	a.AutoCounsel = enabled
	a.MaxCounsel = max
}
