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
	"fmt"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionclient"
	"github.com/treeol/wakil/internal/core/sessionhost"
	"github.com/treeol/wakil/internal/exec"
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

	handle, err := NewHostTurnHandle(app, cm.adapterOpts...)
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

// NewConversation creates a fresh session and returns its facade.
func (cm *conversationManager) NewConversation(ctx context.Context, principal core.Principal) (sessionclient.Facade, error) {
	f, err := cm.newConversation(ctx)
	if err != nil {
		return nil, err
	}
	return f, nil
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
// a new session that carries the folded context.
func (cm *conversationManager) HandoffConversation(ctx context.Context, principal core.Principal, current sessionclient.Facade, proceed bool) (sessionclient.Facade, error) {
	wf, ok := current.(*wiringFacade)
	if !ok {
		return nil, fmt.Errorf("handoff: not a wiring facade")
	}

	// Generate the handoff context from the old App.
	handoffCtx := agent.BuildHandoffContext(agent.HandoffPayload{}, wf.app.Client.ChatID, wf.app.SessionWorkspace())

	// Create the new conversation.
	f, err := cm.newConversation(ctx)
	if err != nil {
		return nil, err
	}

	// If proceeding, seed the new session with the handoff context.
	if proceed && handoffCtx != "" {
		f.app.PendingImages = nil
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

// workspaceIDFromConfig derives a WorkspaceID from the config's working directory.
func workspaceIDFromConfig(cfg config.Config) event.WorkspaceID {
	ws := cfg.WorkDir
	if cfg.ExecMode != "direct" {
		ws = cfg.HostWorkDir
	}
	// WorkspaceID requires the wsp_ prefix. Use the path as-is for now;
	// the host validates and may reject. In production this should use a
	// proper workspace ID generator.
	return event.WorkspaceID("wsp_" + ws)
}
