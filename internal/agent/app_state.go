package agent

// app_state.go: exported state methods and snapshot types for the B2/B5
// concurrency fix. All RPC-driven session settings on App are protected by
// stateMu. Cross-package callers (connect handlers, wiring) use these
// exported methods instead of directly reading/writing App fields.
//
// Lock ordering: saveMu → stateMu → convMu (saveMu acquired first, only in
// SaveSession). callbackMu is always standalone.
//
// Turn-stable fields (SelectedModel, SelectedBackend, CounselMode, etc.)
// are snapshot in prepareTurn under stateMu.Lock and used for the duration
// of the turn. Live fields (RawTools, SubagentModelOverride, etc.) are
// re-read under stateMu.RLock at each use site.
//
// See b2-b5-plan-v3.md for the full architecture and snapshot semantics table.
//
// IMPORTANT: stateMu must NEVER be held across method calls that may
// acquire locks, perform I/O, or invoke callbacks. Snapshot methods copy
// raw fields under stateMu.RLock, release the lock, then call helper
// methods outside the lock. This avoids self-deadlock (e.g. if a helper
// needs stateMu) and avoids holding the lock across potentially blocking
// operations (Exec interface calls, Client methods).

import (
	"github.com/treeol/wakil/internal/workflow"
)

// TurnSettings is a coherent snapshot of turn-stable fields read by the turn
// goroutine at prepareTurn time. It is NOT used by the turn goroutine itself
// (which reads fields directly under stateMu.Lock in prepareTurn); it exists
// for RPC snapshot methods and for future use cases that need a consistent
// view of turn-scoped settings without holding the lock.
type TurnSettings struct {
	SelectedModel       string
	SelectedBackend     string
	DefaultModel        string
	AuxModel            string
	CounselMode         string
	MaxCounsel          int
	RawTools            bool
	CtxMaxCharsOverride int
	SubagentModel       string
	SubagentEndpoint    string
	MaxParallel         int
	ClientModel         string
	ClientBackend       string
	ClientChatID        string
	CtxLimit            ContextLimit
	InfoPanelOpen       bool
}

// SessionStateSnapshot is a coherent snapshot of all fields GetSessionState
// needs. It is fully detached — no shared mutable pointers to App state.
// Workflow is represented as a display label string, not a live pointer.
type SessionStateSnapshot struct {
	// Turn-stable fields (subset of TurnSettings).
	SelectedModel       string
	SelectedBackend     string
	CounselMode         string
	MaxCounsel          int
	RawTools            bool
	CtxMaxCharsOverride int
	SubagentModel       string
	SubagentEndpoint    string
	MaxParallel         int
	InfoPanelOpen       bool

	// Client routing fields.
	ClientModel    string
	ClientBackend  string
	ClientChatID   string
	BaseURL        string
	LastBackend    string
	EffectiveModel string

	// Effective subagent model (resolved display string).
	EffectiveSubagentModel string

	// Lists.
	BackendList []BackendInfo
	ModelList   []string

	// Session.
	SessionLabel string
	ChatID       string

	// Context limit.
	CtxLimit ContextLimit

	// Context usage.
	ContextUsed  int
	ContextExact bool

	// Workspace / exec.
	Workspace string
	Cwd       string
	ExecMode  string

	// Prompt note.
	PromptNote string

	// Workflow display label (detached string, not a pointer).
	WorkflowLabel string

	// Config backend (Cfg.Backend).
	ConfigBackend string
}

// ---- Exported state setters (RPC handlers call these) ----

// SetModelOverride writes the model override under stateMu.Lock. For OpenAI
// kind endpoints it writes Client.ConfiguredModel, Client.Model, and
// Cfg.Endpoint.Model (the effective model for the session). For ILM-proxy
// kind it writes only SelectedModel. The caller is responsible for
// persistence (SaveRepoState) — this method does not touch disk.
//
// Safety: for OpenAI kind, this writes Client.Model which is read by the
// turn goroutine's Stream call. Callers MUST ensure no turn is active
// before calling this for OpenAI kind (via the transition coordinator in
// Phase 5). For ILM-proxy kind, only SelectedModel is written — safe even
// during a turn (the turn goroutine reads it at prepareTurn time).
func (a *App) SetModelOverride(model string) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	a.applyModelOverrideLocked(model)
}

// applyModelOverrideLocked is the unexported version for callers that
// already hold stateMu. This is the existing ApplyModelOverride logic
// moved under the lock.
func (a *App) applyModelOverrideLocked(model string) {
	if a.Cfg.ActiveEndpoint().Kind == "openai" {
		a.Client.ConfiguredModel = model
		a.Client.Model = model
		a.Cfg.Endpoint.Model = model
		a.SelectedModel = ""
		a.defaultModel = model
		return
	}
	a.SelectedModel = model
}

// SetBackendSelection writes the backend and optional model-path selection
// under stateMu.Lock. The arg is "<name>" or "<name>/<model-path>".
func (a *App) SetBackendSelection(arg string) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	if idx := indexByte(arg, '/'); idx >= 0 {
		a.SelectedBackend = arg[:idx]
		a.SelectedModel = arg
	} else {
		a.SelectedBackend = arg
		a.SelectedModel = ""
	}
}

// SetRawToolsValue sets the RawTools flag under stateMu.Lock.
func (a *App) SetRawToolsValue(v bool) {
	a.stateMu.Lock()
	a.RawTools = v
	a.stateMu.Unlock()
}

// SetCounselModeValue sets the counsel mode and max under stateMu.Lock.
func (a *App) SetCounselModeValue(mode string, cap int) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	a.CounselMode = mode
	if mode == "auto" {
		a.MaxCounsel = cap
	}
}

// SetSubagentEndpointOverride sets the subagent endpoint override.
// Pass "" or "inherit" to clear. Under stateMu.Lock.
func (a *App) SetSubagentEndpointOverride(name string) {
	if name == "inherit" {
		name = ""
	}
	a.stateMu.Lock()
	a.SubagentEndpointOverride = name
	a.stateMu.Unlock()
}

// SetSubagentModelOverride sets the subagent model override.
// Pass "" or "inherit" to clear. Under stateMu.Lock.
func (a *App) SetSubagentModelOverride(name string) {
	if name == "inherit" {
		name = ""
	}
	a.stateMu.Lock()
	a.SubagentModelOverride = name
	a.stateMu.Unlock()
}

// SetMaxCtxOverride sets the EffectiveCtxMaxCharsOverride under stateMu.Lock.
// -1 = not set (use config), 0 = disabled, >0 = cap at this many chars.
func (a *App) SetMaxCtxOverride(v int) {
	a.stateMu.Lock()
	a.EffectiveCtxMaxCharsOverride = v
	a.stateMu.Unlock()
}

// SetMaxParallel sets Cfg.MaxParallelSubagents under stateMu.Lock.
func (a *App) SetMaxParallel(n int) {
	a.stateMu.Lock()
	a.Cfg.MaxParallelSubagents = n
	a.stateMu.Unlock()
}

// SetSessionLabelValue sets the session label under stateMu.Lock, then
// saves the session via the detached snapshot path. The label write is
// under stateMu; the save is serialized via saveMu.
func (a *App) SetSessionLabelValue(label string) {
	a.stateMu.Lock()
	if a.Session != nil {
		a.Session.Label = label
	}
	a.stateMu.Unlock()
	a.SaveSession()
}

// ---- Snapshot methods ----

// snapshotTurnSettingsLocked populates a TurnSettings from App fields
// under the held stateMu.RLock. Centralized so field sets cannot drift
// between SnapshotTurnSettings and SnapshotSessionState. Caller MUST
// hold stateMu.RLock.
func (a *App) snapshotTurnSettingsLocked() TurnSettings {
	ts := TurnSettings{
		SelectedModel:       a.SelectedModel,
		SelectedBackend:     a.SelectedBackend,
		DefaultModel:        a.defaultModel,
		AuxModel:            a.Cfg.AuxModel,
		CounselMode:         a.CounselMode,
		MaxCounsel:          a.MaxCounsel,
		RawTools:            a.RawTools,
		CtxMaxCharsOverride: a.EffectiveCtxMaxCharsOverride,
		SubagentModel:       a.SubagentModelOverride,
		SubagentEndpoint:    a.SubagentEndpointOverride,
		MaxParallel:         a.Cfg.MaxParallelSubagents,
		CtxLimit:            a.CtxLimit,
		InfoPanelOpen:       a.InfoPanelOpen,
	}
	if a.Client != nil {
		ts.ClientModel = a.Client.Model
		ts.ClientBackend = a.Client.Backend
		ts.ClientChatID = a.Client.ChatID
	}
	return ts
}

// SnapshotTurnSettings returns a coherent snapshot of turn-stable fields
// under stateMu.RLock. Used by RPC handlers that need a consistent view
// without holding the lock.
func (a *App) SnapshotTurnSettings() TurnSettings {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.snapshotTurnSettingsLocked()
}

// SnapshotSessionState returns a coherent snapshot of all fields
// GetSessionState needs, gathered under a single stateMu.RLock for the
// App-owned fields. The snapshot is fully detached — no shared mutable
// pointers to App state. Workflow is represented as a display label
// string, not a live *WorkflowState pointer.
//
// Method calls that may acquire locks or perform I/O (ContextUsage,
// ContextLimit, Exec.Cwd, etc.) are made OUTSIDE stateMu to avoid
// self-deadlock and holding the lock across potentially blocking operations.
// The App-owned scalar fields are read under stateMu.RLock; the derived
// values are computed after the lock is released using the snapshot's
// stable copies.
func (a *App) SnapshotSessionState() SessionStateSnapshot {
	a.stateMu.RLock()

	// Copy only App-owned scalar fields and stable references under the lock.
	snap := SessionStateSnapshot{
		SelectedModel:       a.SelectedModel,
		SelectedBackend:     a.SelectedBackend,
		CounselMode:         a.CounselMode,
		MaxCounsel:          a.MaxCounsel,
		RawTools:            a.RawTools,
		CtxMaxCharsOverride: a.EffectiveCtxMaxCharsOverride,
		SubagentModel:       a.SubagentModelOverride,
		SubagentEndpoint:    a.SubagentEndpointOverride,
		MaxParallel:         a.Cfg.MaxParallelSubagents,
		InfoPanelOpen:       a.InfoPanelOpen,
		CtxLimit:            a.CtxLimit,
		ConfigBackend:       a.Cfg.Backend,
	}
	// Deep-copy slice fields: BackendInfo.Caps is a []string that must not
	// remain shared with the live App's backing array.
	snap.BackendList = make([]BackendInfo, len(a.BackendList))
	for i, b := range a.BackendList {
		snap.BackendList[i] = BackendInfo{
			Name:     b.Name,
			External: b.External,
			Caps:     append([]string(nil), b.Caps...),
		}
	}
	snap.ModelList = append([]string(nil), a.ModelList...)

	// Copy Client fields (Client is a *proxy.Client — we read its fields
	// under stateMu because prepareTurn also writes them under stateMu).
	if a.Client != nil {
		snap.ClientModel = a.Client.Model
		snap.ClientBackend = a.Client.Backend
		snap.ClientChatID = a.Client.ChatID
		snap.BaseURL = a.Client.BaseURL
	}
	// Copy Session fields.
	if a.Session != nil {
		snap.SessionLabel = a.Session.Label
		snap.ChatID = a.Session.ChatID
	}
	// ChatID fallback: when Session is nil (subagents, early init) or
	// Session.ChatID is empty, fall back to Client.ChatID — same logic as
	// the appChatID helper and SessionChatIDLocked.
	if snap.ChatID == "" && a.Client != nil {
		snap.ChatID = a.Client.ChatID
	}

	// Workflow: read pointer and copy the display label UNDER the lock so
	// the snapshot is fully detached. SidebarLabel is a pure string getter
	// on WorkflowState (no locks, no I/O), so calling it under stateMu.RLock
	// is safe and avoids reading a stale or racing pointer after unlock.
	if a.Workflow != nil {
		snap.WorkflowLabel = a.Workflow.SidebarLabel()
	}

	// Workspace: copy under the lock — SessionWorkspace reads Cfg fields
	// (ExecMode, WorkDir) that share the Cfg struct value with fields
	// written under stateMu by restoreEndpointIndependentLocked (OracleModel,
	// MaxParallelSubagents, etc.). The race detector treats writes to any
	// field in a struct value as racing with reads of any other field in
	// the same allocation, so this must be under the lock.
	snap.Workspace = a.SessionWorkspace()

	a.stateMu.RUnlock()

	// ---- Outside stateMu: compute derived values ----

	// Effective model.
	if snap.SelectedModel != "" {
		snap.EffectiveModel = snap.SelectedModel
	} else {
		snap.EffectiveModel = snap.ClientModel
	}

	// Effective subagent model (calls resolveSubagentEndpointView which
	// reads SubagentEndpointOverride etc. — but we already have those in
	// the snapshot; the method reads them again, which is fine since we're
	// outside the lock now and the values are turn-stable strings).
	snap.EffectiveSubagentModel = a.EffectiveSubagentModel()

	// Context usage and limit (these call a.Client.LastUsage() and read
	// a.CtxLimit — both safe outside the lock since CtxLimit is already
	// in the snapshot and LastUsage is atomic).
	used, exact := a.ContextUsage()
	snap.ContextUsed = used
	snap.ContextExact = exact
	cl := a.ContextLimit()
	snap.CtxLimit = cl

	// Workspace was copied under the lock above.

	// Exec info.
	if a.Exec != nil {
		snap.Cwd = a.Exec.Cwd()
		snap.ExecMode = a.Exec.Describe()
	}

	// Last backend used.
	if a.Client != nil {
		snap.LastBackend = a.Client.LastUsedBackend()
	}

	// Prompt note.
	snap.PromptNote = a.AgentPromptNote()

	// Workflow label was copied under the lock above — no post-unlock
	// workflow access needed.

	return snap
}

// ---- Exported locked getters for cross-package callers ----

// SelectedBackendLocked returns SelectedBackend under stateMu.RLock.
func (a *App) SelectedBackendLocked() string {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.SelectedBackend
}

// SelectedModelLocked returns SelectedModel under stateMu.RLock.
func (a *App) SelectedModelLocked() string {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.SelectedModel
}

// RawToolsLocked returns RawTools under stateMu.RLock.
func (a *App) RawToolsLocked() bool {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.RawTools
}

// EffectiveCtxMaxOverrideLocked returns EffectiveCtxMaxCharsOverride
// under stateMu.RLock.
func (a *App) EffectiveCtxMaxOverrideLocked() int {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.EffectiveCtxMaxCharsOverride
}

// CounselModeLocked returns CounselMode under stateMu.RLock.
func (a *App) CounselModeLocked() string {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.CounselMode
}

// MaxCounselLocked returns MaxCounsel under stateMu.RLock.
func (a *App) MaxCounselLocked() int {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.MaxCounsel
}

// SubagentEndpointOverrideLocked returns SubagentEndpointOverride under
// stateMu.RLock.
func (a *App) SubagentEndpointOverrideLocked() string {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.SubagentEndpointOverride
}

// SubagentModelOverrideLocked returns SubagentModelOverride under
// stateMu.RLock.
func (a *App) SubagentModelOverrideLocked() string {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.SubagentModelOverride
}

// MaxParallelLocked returns Cfg.MaxParallelSubagents under stateMu.RLock.
func (a *App) MaxParallelLocked() int {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.Cfg.MaxParallelSubagents
}

// CtxLimitLocked returns CtxLimit under stateMu.RLock.
func (a *App) CtxLimitLocked() ContextLimit {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.CtxLimit
}

// InfoPanelOpenLocked returns InfoPanelOpen under stateMu.RLock.
func (a *App) InfoPanelOpenLocked() bool {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.InfoPanelOpen
}

// WorkflowLocked returns Workflow under stateMu.RLock. The caller must
// not mutate the returned pointer.
func (a *App) WorkflowLocked() *workflow.WorkflowState {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.Workflow
}

// EffectiveModelLocked returns the effective model under stateMu.RLock.
func (a *App) EffectiveModelLocked() string {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	if a.SelectedModel != "" {
		return a.SelectedModel
	}
	if a.Client != nil {
		return a.Client.Model
	}
	return ""
}

// SessionLabelLocked returns the session label under stateMu.RLock.
func (a *App) SessionLabelLocked() string {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	if a.Session != nil {
		return a.Session.Label
	}
	return ""
}

// SessionChatIDLocked returns the session chat ID under stateMu.RLock.
func (a *App) SessionChatIDLocked() string {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	if a.Session != nil && a.Session.ChatID != "" {
		return a.Session.ChatID
	}
	if a.Client != nil {
		return a.Client.ChatID
	}
	return ""
}

// BackendListLocked returns a copy of BackendList under stateMu.RLock.
func (a *App) BackendListLocked() []BackendInfo {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return append([]BackendInfo(nil), a.BackendList...)
}

// ModelListLocked returns a copy of ModelList under stateMu.RLock.
func (a *App) ModelListLocked() []string {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return append([]string(nil), a.ModelList...)
}

// indexByte is a local strings.IndexByte to avoid importing strings here
// (keeps the import list clean — strings is already imported in other files).
func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
