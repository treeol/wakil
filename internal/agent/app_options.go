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
// Pass nil to clear the workflow.
func (a *App) SetWorkflow(wf *workflow.WorkflowState) {
	a.Workflow = wf
}

// SetAutoCounsel is the post-construction setter for auto-counsel config.
// Used by run.go (headless) to set the --auto-counsel/--max-counsel flags
// after buildApp returns.
func (a *App) SetAutoCounsel(enabled bool, max int) {
	a.AutoCounsel = enabled
	a.MaxCounsel = max
}
