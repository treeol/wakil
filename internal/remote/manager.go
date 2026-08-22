// manager.go: the remote ConversationManager implementation (card #148 P2e).
//
// RemoteConversationManager implements sessionclient.ConversationManager by
// creating sessions on the daemon and returning RemoteFacade instances. It
// is the remote counterpart of wiring.wiringConversationManager.
//
// The manager owns the Clients bundle (shared across all facades it creates).
// Close releases the clients.

package remote

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"
	v1alpha1 "github.com/treeol/wakil/api/gen/wakil/v1alpha1"
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionclient"
	"github.com/treeol/wakil/internal/protoconv"
	"github.com/treeol/wakil/internal/proxy"
)

// Compile-time proof that RemoteConversationManager satisfies the interface.
var _ sessionclient.ConversationManager = (*RemoteConversationManager)(nil)

// RemoteConversationManager creates and closes remote facades backed by the
// daemon. It is created by BootstrapRemote (bootstrap.go).
type RemoteConversationManager struct {
	clients   *Clients
	principal core.Principal
	workspace event.WorkspaceID
}

// NewRemoteConversationManager creates a manager backed by the given clients.
func NewRemoteConversationManager(clients *Clients, principal core.Principal, workspace event.WorkspaceID) *RemoteConversationManager {
	return &RemoteConversationManager{
		clients:   clients,
		principal: principal,
		workspace: workspace,
	}
}

// CloseManager releases the manager's resources (the shared clients bundle).
// This is NOT the ConversationManager.Close(f) method — it's the manager's
// own cleanup, called by the bootstrap cleanup function.
func (m *RemoteConversationManager) CloseManager() error {
	if m.clients != nil {
		m.clients.Close()
	}
	return nil
}

// NewConversation creates a new session on the daemon and returns a facade
// backed by it. The current facade (if any) is NOT closed by this method —
// the caller (the TUI) closes it via Close.
func (m *RemoteConversationManager) NewConversation(ctx context.Context, principal core.Principal, current sessionclient.Facade) (sessionclient.Facade, error) {
	f := newRemoteFacade(m.clients, principal, m.workspace)

	// Create the session on the daemon.
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := m.clients.Session.CreateSession(ctx, connect.NewRequest(&v1alpha1.CreateSessionRequest{
		Workspace: string(m.workspace),
	}))
	if err != nil {
		return nil, fmt.Errorf("remote: NewConversation: CreateSession: %w", err)
	}
	s := protoconv.SessionFromProto(resp.Msg)
	f.setSession(event.SessionID(s.ID))
	f.SetTitle(s.Title)

	// Initialize the daemon's App for a new conversation — sets app.Session and
	// app.Client.ChatID so SaveSession persists the transcript to disk. Without
	// this, the daemon's app.Session stays nil and sessions are never saved.
	if _, err := m.clients.SessionState.InitNewSession(ctx, connect.NewRequest(&v1alpha1.InitNewSessionRequest{
		SessionId: s.ID,
	})); err != nil {
		return nil, fmt.Errorf("remote: NewConversation: InitNewSession: %w", err)
	}

	return f, nil
}

// ResumeConversation loads an existing session by ID or prefix and returns a
// facade backed by it. The daemon's App is restored from disk (Conv, ChatID,
// Session, Workflow) via the LoadSession RPC, and the conversation transcript
// is projected into the facade for TUI display.
func (m *RemoteConversationManager) ResumeConversation(ctx context.Context, principal core.Principal, idOrPrefix string) (sessionclient.Facade, error) {
	f := newRemoteFacade(m.clients, principal, m.workspace)

	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	// 1. Load the session from disk on the daemon side — restores app.Conv,
	//    app.Client.ChatID, app.Session, app.Workflow. Returns the display
	//    transcript for the TUI.
	loadResp, err := m.clients.SessionState.LoadSession(rpcCtx, connect.NewRequest(&v1alpha1.LoadSessionRequest{
		IdOrPrefix: idOrPrefix,
	}))
	if err != nil {
		return nil, fmt.Errorf("remote: ResumeConversation: LoadSession: %w", err)
	}

	// 2. Create a new session on the daemon for event subscription (the loaded
	//    session's events are historical; the TUI subscribes live-only at the
	//    new session's head, same as the embedded rotation path).
	createResp, err := m.clients.Session.CreateSession(rpcCtx, connect.NewRequest(&v1alpha1.CreateSessionRequest{
		Workspace: string(m.workspace),
	}))
	if err != nil {
		return nil, fmt.Errorf("remote: ResumeConversation: CreateSession: %w", err)
	}
	s := protoconv.SessionFromProto(createResp.Msg)
	f.setSession(event.SessionID(s.ID))

	// 3. Populate the facade's conversation from the loaded transcript.
	if loadResp.Msg.Title != "" {
		f.SetTitle(loadResp.Msg.Title)
	}
	conv := make([]proxy.Message, 0, len(loadResp.Msg.Conv))
	for _, cm := range loadResp.Msg.Conv {
		msg := proxy.Message{Role: cm.Role, Name: cm.Name}
		if cm.Content != nil {
			content := cm.GetContent()
			msg.Content = &content
		}
		conv = append(conv, msg)
	}
	if len(conv) > 0 {
		f.SetConv(conv)
	}

	return f, nil
}

// HandoffConversation creates a new session that carries a folded context.
// In P2e the daemon does not support handoff folding; this method falls back
// to creating a new conversation. A future "handoff" RPC could fold
// server-side.
func (m *RemoteConversationManager) HandoffConversation(ctx context.Context, principal core.Principal, current sessionclient.Facade, proceed bool) (sessionclient.Facade, error) {
	// P2e: no daemon-side handoff support. Create a new conversation.
	// The TUI's handoff flow (summarize + new session) runs locally; the
	// daemon just gets a new session. This is the simplest correct behavior.
	return m.NewConversation(ctx, principal, current)
}

// Close releases a facade's resources (the event pump, the session on the
// daemon).
func (m *RemoteConversationManager) Close(f sessionclient.Facade) error {
	// Close the facade. The facade's Close closes the session on the daemon
	// and stops the event pump.
	if rf, ok := f.(*RemoteFacade); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = rf.CloseSession(ctx, m.principal, rf.sessionID)
	}
	return f.Close()
}
