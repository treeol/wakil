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
	"time"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionclient"
	"github.com/treeol/wakil/internal/core/sessionhost"
	"github.com/treeol/wakil/internal/exec"
	"github.com/treeol/wakil/internal/proxy"
)

// Compile-time proof that conversationManager satisfies the interface.
var _ sessionclient.ConversationManager = (*conversationManager)(nil)

// conversationManager implements sessionclient.ConversationManager. It builds
// fresh *agent.App instances, wires them to the session host, and returns
// facades the TUI consumes.
type conversationManager struct {
	cfg      config.Config
	exe      exec.Executor
	principal core.Principal
	hostOpts []sessionhost.Option // optional host tuning (test injection)
	adapterOpts []AdapterOption  // optional adapter tuning (test injection)
	resources *AppResources      // the FIRST facade's resources; rotation closes them
}

// NewConversationManager creates a ConversationManager from config, executor,
// and principal. The manager creates fresh App instances for each conversation.
func NewConversationManager(cfg config.Config, exe exec.Executor, principal core.Principal) (sessionclient.ConversationManager, error) {
	if principal.UserID == "" {
		principal = core.Principal{
			TenantID: event.EmbeddedTenantID,
			UserID:   event.EmbeddedUserID,
			Role:     core.RoleOwner,
		}
	}
	return &conversationManager{
		cfg:      cfg,
		exe:      exe,
		principal: principal,
	}, nil
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

	host := sessionhost.New(handle.Turn, cm.hostOpts...)

	sess, err := host.CreateSession(ctx, cm.principal, core.CreateSessionRequest{
		Workspace: workspaceIDFromConfig(cm.cfg),
	})
	if err != nil {
		CloseResources(app, res)
		return nil, fmt.Errorf("conversation manager: %w", err)
	}

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
	if !ok {
		return nil, fmt.Errorf("handoff: not a wiring facade")
	}

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

	// 4. Proceed: enqueue the continuation prompt as the new session's first
	// turn. The host drives it; the facade returns immediately.
	if proceed && result.ContinuationPrompt != "" {
		if _, err := f.host.SubmitInput(ctx, principal, core.SubmitInputRequest{
			SessionID: f.sessionID,
			Text:      result.ContinuationPrompt,
		}); err != nil {
			// The continuation could not start (queue full or closing). The
			// handoff itself still succeeded — the new session exists and
			// carries the context as a pinned message. The user can send the
			// prompt themselves; surface the miss via the startup note.
			f.app.StartupNote = "· handoff complete — continuation could not auto-start (" + err.Error() + "); the context is pinned, send your next message"
		}
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

// workspaceIDFromConfig derives a WorkspaceID from the config's working
// directory (7b3 m4: real derivation — the m3 stub used the raw path, which
// fails ID validation for an empty workdir and produces unwieldy IDs for long
// paths). The ID is "wsp_" + the first 16 hex chars of the SHA-256 of the
// effective workdir: stable for a given workspace (resumes and rotations
// derive the same ID), collision-safe for practical purposes, and independent
// of path length. An empty effective workdir (a hand-built test config)
// derives the zero-value hash of "" — still a valid, stable ID.
func workspaceIDFromConfig(cfg config.Config) event.WorkspaceID {
	ws := cfg.WorkDir
	if cfg.ExecMode != "direct" {
		ws = cfg.HostWorkDir
	}
	sum := sha256.Sum256([]byte(ws))
	return event.WorkspaceID("wsp_" + hex.EncodeToString(sum[:8]))
}
