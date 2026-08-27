package wiring

// conversation_manager.go: the wiring-side implementation of
// sessionclient.ConversationManager (card #148 chunk 7b3 m3).
//
// The ConversationManager sits above the facade and handles conversation
// lifecycle operations (/new, /resume, /handoff). It owns the App factory
// (BuildApp) and the host lifecycle, creating fresh *agent.App instances for
// each new conversation and releasing the old ones.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionclient"
	"github.com/treeol/wakil/internal/core/sessionhost"
	"github.com/treeol/wakil/internal/core/sessionhost/sqlstore"
	"github.com/treeol/wakil/internal/exec"
	"github.com/treeol/wakil/internal/proxy"
)

// Compile-time proof that conversationManager satisfies the interface.
var _ sessionclient.ConversationManager = (*conversationManager)(nil)

// conversationManager implements sessionclient.ConversationManager. It builds
// fresh *agent.App instances, wires them to the session host, and returns
// facades the TUI consumes.
type conversationManager struct {
	cfg         config.Config
	exe         exec.Executor
	principal   core.Principal
	hostOpts    []sessionhost.Option // optional host tuning (test injection)
	adapterOpts []AdapterOption      // optional adapter tuning (test injection)
	resources   *AppResources        // the FIRST facade's resources; rotation closes them
	store       sessionhost.Store    // P1: SQLite-backed event store (nil → MemLog fallback)

	// autoUserOverridden is set when the user toggles /auto mid-session. It
	// clears the AutoExplicit guard so subsequent rotations restore AutoApprove
	// from RepoState instead of re-seeding from the --auto CLI flag. Process-
	// scoped (not persisted to RepoState) so a fresh process start always
	// honors the CLI flag — the override only applies within the current
	// process. See Phase 2 of the handoff-rotation-restore plan.
	// atomic.Bool because it's written from command goroutines and read
	// from rotation goroutines (concurrent unsynchronized access = race).
	autoUserOverridden atomic.Bool
	// modelUserOverridden is set when the user sets /model or /backend
	// mid-session. Same purpose as autoUserOverridden but for ModelExplicit.
	modelUserOverridden atomic.Bool
}

// NewConversationManager creates a ConversationManager from config, executor,
// and principal. The manager creates fresh App instances for each conversation.
// In P1 it opens a workspace-keyed SQLiteStore for the session-host event log
// (card #148 D3); if the store cannot be opened it falls back to the in-memory
// MemLog (best-effort — the session works, but events don't persist).
func NewConversationManager(cfg config.Config, exe exec.Executor, principal core.Principal) (sessionclient.ConversationManager, error) {
	if principal.UserID == "" {
		principal = core.Principal{
			TenantID: event.EmbeddedTenantID,
			UserID:   event.EmbeddedUserID,
			Role:     core.RoleOwner,
		}
	}
	cm := &conversationManager{
		cfg:       cfg,
		exe:       exe,
		principal: principal,
	}
	// P1: open the workspace-keyed SQLite event store. Best-effort — a failure
	// falls back to MemLog (the host works, events just don't persist).
	// Pass the raw workspace PATH (not the wsp_ ID) to SessionHostDBPath —
	// the storage functions hash the path internally, so passing the ID
	// would double-hash and produce a cwd-dependent key.
	wsPath := cfg.WorkspacePath()
	if dbPath := agent.SessionHostDBPath(wsPath); dbPath != "" {
		store, err := sqlstore.NewSQLiteStore(context.Background(), dbPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "sessionhost: failed to open SQLite store, using in-memory:", err)
		} else {
			cm.store = store
		}
	}
	return cm, nil
}

// restoreRepoState loads the per-workspace terminal settings from disk and
// applies them to a freshly built App. Called on every rotation (/new,
// /handoff, /resume) so settings persisted in one session — /auto, /model,
// /counsel, /mashura, /maxpar, /maxctx, /rawtools, /subagent, info-panel —
// survive into the next conversation. This was previously only done at first
// boot (BootstrapTUI); moving it here ensures rotation paths also restore.
//
// For /new and /handoff (fresh conversations): uses RestoreRepoStateApply
// (restores model/backend too, subject to endpoint-match and ModelExplicit
// guards) and re-resolves context limits via a 5s network probe (best-effort:
// on failure/timeout, keeps BuildApp's CtxLimit + adds a note).
//
// For /resume: the caller passes resume=true so RestoreRepoStateResumeApply
// is used instead (skips model/backend to avoid changing them mid-transcript).
// The caller has already installed the loaded session state before calling
// this, so app.SessionWorkspace() resolves correctly.
//
// Phase 2: if the user has toggled /auto (or /model) mid-session, the
// process-local override flag (cm.autoUserOverridden / cm.modelUserOverridden)
// is set. We temporarily clear the corresponding AutoExplicit / ModelExplicit
// flag on the App's config COPY (BuildApp copies config by value, so this
// only affects the new App — not cm.cfg or future Apps) so the restore guard
// in RestoreRepoStateApply/restoreEndpointIndependentLocked fires and restores
// AutoApprove/Model from RepoState. This is process-scoped: a fresh process
// start never sets the override flags, so --auto/--model CLI flags win.
//
// Returns a human-readable note string ("") if nothing was restored.
func (cm *conversationManager) restoreRepoState(ctx context.Context, app *agent.App, resume bool) string {
	st, err := agent.RestoreRepoStateRead(app)
	if err != nil || st == nil {
		return ""
	}
	// Phase 2: clear explicit flags on the App's config copy so the restore
	// guard fires when the user has overridden mid-session. The config is
	// a value type copied by BuildApp, so mutating app.Cfg does NOT affect
	// cm.cfg or future Apps — it only affects this restore call.
	if cm.autoUserOverridden.Load() {
		app.Cfg.AutoExplicit = false
	}
	if cm.modelUserOverridden.Load() {
		app.Cfg.ModelExplicit = false
	}
	var result agent.RestoreRepoStateResult
	if resume {
		result = agent.RestoreRepoStateResumeApply(app, st)
	} else {
		result = agent.RestoreRepoStateApply(app, st)
		// Re-resolve context limits using the literal model/backend strings
		// Apply actually used (not raw RepoState values that may have been
		// skipped by eligibility guards). 5s timeout — best-effort probe.
		if (result.Model != "" || result.Backend != "") && app.Client != nil && app.Client.HTTP != nil {
			probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			cl := agent.ResolveContextLimitForBackendModel(probeCtx, app.Client.HTTP, app.Cfg, result.Backend, result.Model, os.Stderr)
			cancel()
			app.SetCtxLimit(cl)
		}
	}
	return result.Note
}

// transferDestructive transfers the AllowDestructive grant from the old
// conversation's consent snapshot to the new App — in-memory only, never
// persisted to disk. Used exclusively for /handoff proceed: the user already
// authorized destructive ops in the old session, and proceed means
// "continue this work", so the grant carries forward.
//
// Enforces the invariant: AllowDestructive is only set if AutoApprove is
// ALREADY true on the new App (so we never create the forbidden state
// AutoApprove=false + AllowDestructive=true). If the new App's AutoApprove
// is false (e.g. RepoState had it off), the destructive grant is dropped —
// the user can re-enable it with /auto destructive. Checks both the old
// consent (the user had the grant) and the new app's current consent
// (auto is on in the new session too).
func transferDestructive(oldConsent agent.ConsentSnapshot, newApp *agent.App) {
	newConsent := newApp.Consent()
	if oldConsent.AutoApprove && oldConsent.AllowDestructive && newConsent.AutoApprove {
		newApp.SetAllowDestructive(true)
	}
}

// appendStartupNote composes note into app.StartupNote: sets it if empty,
// appends with " | " separator if non-empty. Replaces direct StartupNote
// assignments so composition is consistent across restore, handoff, staging,
// and memory notes.
func appendStartupNote(app *agent.App, note string) {
	if note == "" {
		return
	}
	if app.StartupNote == "" {
		app.StartupNote = note
	} else {
		app.StartupNote += " | " + note
	}
}

// newConversation creates a fresh App + host + facade and creates a session.
func (cm *conversationManager) newConversation(ctx context.Context) (*wiringFacade, error) {
	app, res := BuildApp(cm.cfg, cm.exe, BuildAppOpts{})

	// WithAsyncApproval: the TUI session parks approvals in awaiting_approval
	// (7b2 D25) so the turn goroutine blocks on RespondToApproval instead of an
	// inline resolver. Without it the confirmer uses the sync path with a nil
	// resolver, which declines everything — the interactive TUI would be unable
	// to approve any tool. Test-injected adapter options (cm.adapterOpts)
	// override this default.
	adapterOpts := append([]AdapterOption{WithAsyncApproval()}, cm.adapterOpts...)
	handle, err := NewHostTurnHandle(app, adapterOpts...)
	if err != nil {
		CloseResources(app, res)
		return nil, fmt.Errorf("conversation manager: %w", err)
	}

	// P1: inject the SQLite store if available; else MemLog (host default).
	hostOpts := cm.hostOpts
	if cm.store != nil {
		hostOpts = append([]sessionhost.Option{sessionhost.WithStore(cm.store)}, hostOpts...)
	}
	host := sessionhost.New(handle.Turn, hostOpts...)

	sess, err := host.CreateSession(ctx, cm.principal, core.CreateSessionRequest{
		Workspace: WorkspaceIDFromConfig(cm.cfg),
	})
	if err != nil {
		CloseResources(app, res)
		return nil, fmt.Errorf("conversation manager: %w", err)
	}

	// Initialize app.Session so SaveSession persists the transcript. Without
	// this, app.Session stays nil (BuildApp does not set it) and SaveSession
	// is a no-op — sessions are never written to disk (the same bug the daemon
	// path fixed via the InitNewSession RPC in commit 89b3acb, but the embedded
	// path was missed). Use the chat ID from the proxy client (BuildApp mints
	// one via NewChatID); app.NewConversation sets Session.ChatID, Model,
	// Created, and Workspace from the app's config.
	app.NewConversation(app.Client.ChatID)

	// Wire the Phase 2 override callbacks so /auto and /model toggles in
	// the TUI clear the AutoExplicit/ModelExplicit guards for subsequent
	// rotations. The callbacks are process-local (set on the manager, not
	// persisted to RepoState) so a fresh process start always honors the
	// --auto/--model CLI flags.
	app.OnAutoToggled = cm.SetAutoUserOverridden
	app.OnModelToggled = cm.SetModelUserOverridden

	facade := newWiringFacade(app, handle, host, res, cm.principal)
	facade.setSession(sess.ID)

	// The first conversation manager call stores resources for later cleanup.
	if cm.resources == nil {
		cm.resources = res
	}

	return facade, nil
}

// NewConversation creates a fresh session and returns its facade. current is
// the facade being rotated away from (/new, /reset), or nil at first boot;
// when non-nil the manager finalizes the old conversation's session-history
// entry first (m4b: replaces HandleTUICommand's /new-side finalize, which
// mutated the old App).
func (cm *conversationManager) NewConversation(ctx context.Context, principal core.Principal, current sessionclient.Facade) (sessionclient.Facade, error) {
	if wf, ok := current.(*wiringFacade); ok && wf != nil && wf.app != nil {
		cm.finalizeOldConversation(ctx, wf.app)
	}
	f, err := cm.newConversation(ctx)
	if err != nil {
		return nil, err
	}
	// Restore per-workspace terminal settings from disk so /auto, /model,
	// /counsel, /mashura, etc. survive /new. This was previously only done
	// at first boot (BootstrapTUI); moving it here ensures rotation paths
	// also restore. Fresh conversation — resume=false (model/backend restored).
	note := cm.restoreRepoState(ctx, f.app, false)
	appendStartupNote(f.app, note)
	return f, nil
}

// finalizeOldConversation ingests the just-finalized session into the
// session-history index and records its end-of-session summary (m4b). It
// mirrors the closure the old /new path ran inside HandleTUICommand: snapshot
// the session BEFORE rotation so finalization can run on it while the old
// App is still intact. Best-effort — failures never block rotation.
func (cm *conversationManager) finalizeOldConversation(ctx context.Context, app *agent.App) {
	if app == nil || app.SessionHistory == nil || app.Session == nil {
		return
	}
	sess := *app.Session
	if sess.ChatID == "" {
		return
	}
	finCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	agent.FinalizeSessionHistory(finCtx, app, sess)
}

// ResumeConversation loads an existing session by ID or prefix and returns
// a facade backed by a fresh App with the loaded conversation state.
func (cm *conversationManager) ResumeConversation(ctx context.Context, principal core.Principal, idOrPrefix string) (sessionclient.Facade, error) {
	s, err := agent.LoadSessionScoped(idOrPrefix, agent.SessionScope{All: true})
	if err != nil {
		return nil, fmt.Errorf("resume: %w", err)
	}
	if s == nil {
		return nil, fmt.Errorf("resume: session %q not found", idOrPrefix)
	}

	f, err := cm.newConversation(ctx)
	if err != nil {
		return nil, err
	}

	// Restore the loaded session's state into the App.
	f.app.Client.ChatID = s.ChatID
	f.app.Conv = s.Conv
	f.app.Session = s
	f.app.SetWorkflow(s.SavedWorkflow)

	// Restore endpoint-independent settings from RepoState (AutoApprove,
	// /counsel, /mashura, etc.) — resume=true so model/backend are NOT
	// changed (avoid silently changing the model mid-transcript of a
	// resumed session).
	note := cm.restoreRepoState(ctx, f.app, true)
	appendStartupNote(f.app, note)

	// Apply the saved session's model on resume, with an endpoint-match
	// guard: a model string recorded under one endpoint is meaningless sent
	// to another (different auth, different model namespace). Skip when
	// --model was on the CLI (ModelExplicit) — the CLI flag always wins.
	// Legacy sessions (no EndpointName) are applied unconditionally
	// (backward compatible — old sessions predate endpoint recording).
	if s.Model != "" && !f.app.Cfg.ModelExplicit {
		endpointMatches := s.EndpointName == "" || s.EndpointName == f.app.Cfg.EndpointName
		if endpointMatches {
			agent.ApplyModelOverride(f.app, s.Model)
			// Re-resolve context limits for the restored model (5s probe).
			if f.app.Client != nil && f.app.Client.HTTP != nil {
				probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				cl := agent.ResolveContextLimitForBackendModel(probeCtx, f.app.Client.HTTP, f.app.Cfg, f.app.SelectedBackend, s.Model, os.Stderr)
				cancel()
				f.app.SetCtxLimit(cl)
			}
		}
	}

	return f, nil
}

// HandoffConversation folds the current conversation into a summary and creates
// a new session that carries the folded context (7b3 m4: real implementation —
// the m3 stub computed an empty context and dropped it).
//
// The flow mirrors performHandoff + the old TUI's HandoffMsg handler:
//  1. RunHandoffPipeline (agent): validation, old-session save, summary
//     generation, session-history indexing, durable handoff record.
//  2. Create the new conversation (fresh App/host/facade).
//  3. Seed the new conversation: clear pending images (they belong to the old
//     session), and inject the handoff context as a pinned system message so
//     the next user turn (or the continuation turn) has the context.
//  4. When proceed is true, enqueue the continuation prompt as the new
//     session's first turn — the host drives it, the TUI is passive (same
//     "host enqueues" policy as workflow continuation).
//
// The old facade is NOT closed here — the caller (the TUI) closes it after
// receiving the new facade, so a failure in this function never strands the
// user without a live conversation.
func (cm *conversationManager) HandoffConversation(ctx context.Context, principal core.Principal, current sessionclient.Facade, proceed bool) (sessionclient.Facade, error) {
	wf, ok := current.(*wiringFacade)
	if !ok || wf == nil || wf.app == nil {
		return nil, fmt.Errorf("handoff: invalid wiring facade")
	}

	// Snapshot the old consent BEFORE RunHandoffPipeline so we can transfer
	// the AllowDestructive grant to the new App for /handoff proceed. The
	// pipeline runs against the old App (save+summarize+index+record) and
	// does not mutate consent, but we capture the snapshot here so the
	// transfer is atomic w.r.t. any concurrent consent change.
	oldConsent := wf.app.Consent()

	// 1. Run the handoff pipeline against the old App (save + summarize +
	// index + record). Errors fail the rotation — the old conversation stays
	// live, which is the safe outcome.
	result, err := agent.RunHandoffPipeline(ctx, wf.app)
	if err != nil {
		return nil, fmt.Errorf("handoff: %w", err)
	}

	// 2. Create the new conversation.
	f, err := cm.newConversation(ctx)
	if err != nil {
		return nil, err
	}

	// Restore per-workspace terminal settings from disk so /auto, /model,
	// /counsel, /mashura, etc. survive handoff. Fresh conversation —
	// resume=false (model/backend restored from RepoState).
	restoreNote := cm.restoreRepoState(ctx, f.app, false)
	appendStartupNote(f.app, restoreNote)

	// Transfer the AllowDestructive grant from the old session for proceed
	// ONLY (in-memory, never persisted to disk). Stop mode drops it — the
	// new session starts with a clean destructive state. AutoApprove comes
	// from RepoState (restored above), NOT transferred — so the invariant
	// AutoApprove=false + AllowDestructive=true can never occur.
	if proceed {
		transferDestructive(oldConsent, f.app)
	}

	// 3. Seed: pending images belong to the old session; the handoff context
	// travels as a pinned system message (untrusted-delimiter framing, same
	// mitigation as the old TUI's stop-mode injection).
	f.app.PendingImages = nil
	handoffCtx := agent.BuildHandoffContext(result.Payload, result.OldChatID, wf.app.SessionWorkspace())
	if handoffCtx != "" {
		f.app.Conv = append(f.app.Conv, proxy.Message{
			Role:    "system",
			Content: agent.StrPtr(handoffCtx),
			Pinned:  true,
		})
	}

	// 4. Proceed: the continuation prompt is stored on the facade for the TUI
	// to submit AFTER subscribing to the new session's event stream. This
	// eliminates the race where the host starts the turn and emits
	// TurnStarted before the TUI's subscription is live (the event would be
	// missed, leaving the TUI showing "idle" while the turn runs).
	// The caller (TUI's applyRotation) calls ConsumePendingContinuation
	// after Subscribe + StartEventPump, then submits the prompt via
	// SubmitInput. See sessionclient.ConversationManager.HandoffConversation
	// for the contract.
	if proceed && result.ContinuationPrompt != "" {
		f.mu.Lock()
		f.pendingContinuation = result.ContinuationPrompt
		f.mu.Unlock()
	}
	return f, nil
}

// Close releases the facade and all its resources.
func (cm *conversationManager) Close(f sessionclient.Facade) error {
	wf, ok := f.(*wiringFacade)
	if !ok {
		return fmt.Errorf("close: not a wiring facade")
	}
	return wf.Close()
}

// SetAutoUserOverridden marks that the user has toggled /auto mid-session.
// Process-scoped: subsequent rotations restore AutoApprove from RepoState
// instead of re-seeding from the --auto CLI flag (clearing AutoExplicit's
// guard). Not persisted — a fresh process start always honors the CLI flag.
func (cm *conversationManager) SetAutoUserOverridden() {
	cm.autoUserOverridden.Store(true)
}

// SetModelUserOverridden marks that the user has set /model or /backend
// mid-session. Same purpose as SetAutoUserOverridden but for ModelExplicit.
func (cm *conversationManager) SetModelUserOverridden() {
	cm.modelUserOverridden.Store(true)
}

// WorkspaceIDFromConfig derives a WorkspaceID from the config's effective
// workspace path (7b3 m4: real derivation — the m3 stub used the raw path,
// which fails ID validation for an empty workdir and produces unwieldy IDs
// for long paths). The ID is "wsp_" + the first 16 hex chars of the SHA-256
// of the effective workdir: stable for a given workspace (resumes and
// rotations derive the same ID), collision-safe for practical purposes, and
// independent of path length. An empty effective workdir (a hand-built test
// config) derives the zero-value hash of "" — still a valid, stable ID.
//
// Uses Config.WorkspacePath() (which respects WAKIL_WORKSPACE_PATH) so the ID
// is stable across processes with different cwd — critical for the daemon
// to see the same workspace identity as the TUI.
func WorkspaceIDFromConfig(cfg config.Config) event.WorkspaceID {
	ws := cfg.WorkspacePath()
	sum := sha256.Sum256([]byte(ws))
	return event.WorkspaceID("wsp_" + hex.EncodeToString(sum[:8]))
}
