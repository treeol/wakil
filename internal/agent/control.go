package agent

// control.go: the TUI→App mutation seam (card #148 chunk 6, deliverable 5
// step 1/2). This is an INTERIM, throwaway P0 seam — not the D7 Service
// boundary and not a wire contract. Several methods carry Go callbacks or
// internal types (SaveRepoState's func(*RepoState), StartSideQuestion's
// context.CancelFunc, SetWorkflow's *workflow.WorkflowState, SetTools's
// []proxy.Tool) that cannot cross a wire (D6 data-only doctrine). The P2 wire
// surface remains SessionService +
// data-only request/result types; these interfaces exist only so the TUI
// stops writing App fields directly. Their value is contingent on the
// turn-driving chunk (deliverable 7) removing *agent.App from the TUI
// entirely; until then they are a tested convention, not a compiler-enforced
// ownership boundary (App's fields stay exported, and tuiModel still holds
// *agent.App for reads and turn-driving).
//
// Split by intent, not by implementation:
//
//   - Control: genuine session/consent/persistence commands a user triggers.
//   - StateApply: round-trip runtime results produced by background/agent work
//     (ctx-limit probes, model-list fetches, MCP reconnects) that the TUI event
//     loop currently writes back onto App. These are NOT user commands and must
//     not be mistaken for a wire-addressable control surface.
//
// Both are implemented by *App and bound to the same object in NewTUIModel.

import (
	"context"

	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/workflow"
)

// Control is the mutation surface for user/session commands (chunk 6). *App
// implements it. Every method here has the same semantics as the App method
// of the same name; see those methods for the thread-safety contract.
type Control interface {
	// Consent (CAS-backed; see consent.go).
	SetAutoApprove(v bool)
	SetAllowDestructive(v bool)
	RevokeAuto()

	// Session lifecycle / persistence.
	NewConversation(chatID string)
	SaveSession()
	SetWorkflow(wf *workflow.WorkflowState) // nil clears
	StartSideQuestion(ctx context.Context, question string) context.CancelFunc

	// Transcript / startup note.
	AppendSystemMessage(m proxy.Message) // locks convMu (matches NewConversation)
	ConsumeStartupNote() string          // returns + clears; single-goroutine by lifecycle

	// Repo-state + info-panel mirror.
	SaveRepoState(mutate func(*RepoState))
	SetInfoPanelOpen(open bool)
}

// StateApply is the mutation surface for round-trip runtime results (chunk 6).
// These are applied by the TUI event loop from agent-produced messages
// (BackendCtxLimitMsg, ModelListUpdatedMsg, MCPReconnectedMsg). They are NOT
// user commands; when turns route through the host (deliverable 7) the agent
// will update its own state atomically and these TUI-loop writes disappear.
type StateApply interface {
	SetCtxLimit(lim ContextLimit) // also resets CtxPressureWarned
	SetModelList(models []string)
	SetTools(tools []proxy.Tool)

	// Pending-image operations (ownership: the TUI reconciles display chips;
	// these methods own the App-side slice).
	ReplacePendingImages(imgs []proxy.ImagePart) // copies the input slice
	AddPendingImage(img proxy.ImagePart)
	ClearPendingImages()
}

// Compile-time proof that App satisfies both seams.
var (
	_ Control    = (*App)(nil)
	_ StateApply = (*App)(nil)
)

// AppendSystemMessage appends one message to the conversation transcript under
// convMu (matching NewConversation/SaveSession/ConvSnapshot). This is the
// control-surface replacement for the former raw `m.app.Conv = append(...)`
// site (the handoff-context injection), which bypassed the lock.
func (a *App) AppendSystemMessage(m proxy.Message) {
	a.convMu.Lock()
	a.Conv = append(a.Conv, m)
	a.convMu.Unlock()
}

// SetConv replaces the conversation transcript under convMu. Used by the
// daemon-side LoadSession handler to restore a resumed session's conversation
// (mirrors the embedded ResumeConversation's `f.app.Conv = s.Conv` assignment,
// but under the lock since the daemon handler is not on the turn goroutine).
func (a *App) SetConv(conv []proxy.Message) {
	a.convMu.Lock()
	a.Conv = append([]proxy.Message(nil), conv...)
	a.convMu.Unlock()
}

// ConsumeStartupNote returns the pending startup note and clears it. It is
// single-goroutine by lifecycle (written once at startup before the TUI runs,
// consumed once by tuiModel.Init before any turn starts), NOT atomic — do not
// call it concurrently with a writer.
func (a *App) ConsumeStartupNote() string {
	note := a.StartupNote
	a.StartupNote = ""
	return note
}

// SetCtxLimit sets the resolved per-slot context window and resets the
// pressure-warning latch. Now acquires stateMu so CtxLimit and
// CtxPressureWarned writes are synchronized with concurrent readers
// (activeThresholds, ContextLimit, GetSessionState).
func (a *App) SetCtxLimit(lim ContextLimit) {
	a.stateMu.Lock()
	a.CtxLimit = lim
	a.CtxPressureWarned = false
	a.stateMu.Unlock()
}

// SetModelList replaces the model list used for /model and /submodel
// autocomplete.
func (a *App) SetModelList(models []string) {
	a.ModelList = models
}

// SetTools replaces the tool list (e.g. after an MCP reconnect rebuilds it).
func (a *App) SetTools(tools []proxy.Tool) {
	a.Tools = tools
}

// ReplacePendingImages replaces the pending-image slice wholesale. It copies the
// input slice so the caller's backing array is not retained (a caller mutating
// its slice afterward must not change App state).
func (a *App) ReplacePendingImages(imgs []proxy.ImagePart) {
	if imgs == nil {
		a.PendingImages = nil
		return
	}
	a.PendingImages = append([]proxy.ImagePart(nil), imgs...)
}

// AddPendingImage appends one pending image.
func (a *App) AddPendingImage(img proxy.ImagePart) {
	a.PendingImages = append(a.PendingImages, img)
}

// ClearPendingImages drops all pending images.
func (a *App) ClearPendingImages() {
	a.PendingImages = nil
}
