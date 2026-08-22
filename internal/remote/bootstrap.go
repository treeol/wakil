// bootstrap.go: the remote TUI bootstrap surface (card #148 P2e).
//
// BootstrapRemote is the remote counterpart of wiring.BootstrapTUI. It dials
// the daemon, checks health, builds the ConversationManager, creates (or
// resumes) the first conversation, and returns everything the TUI needs
// (facade, manager, principal, delivery hook).
//
// cmd/wakil/main.go calls this instead of wiring.BootstrapTUI when --daemon
// is set.

package remote

import (
	"context"
	"fmt"
	"time"

	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionclient"
)

// TUIRuntime bundles what the TUI bootstrap needs. Mirrors wiring.TUIRuntime
// but with remote types.
type TUIRuntime struct {
	Manager   sessionclient.ConversationManager
	Facade    sessionclient.Facade
	Principal core.Principal
}

// BootstrapRemote dials the daemon, builds the manager, and creates (or
// resumes) the first conversation.
//
// socketPath is the Unix socket path. resumeID, when non-empty, resumes that
// session into the first facade. The deliver callback is the TUI's
// tea.Program.Send once constructed; nil means the caller subscribes later.
//
// The returned cleanup function must be deferred by the caller: it closes
// the facade and releases the manager's resources.
func BootstrapRemote(ctx context.Context, socketPath string, workspace event.WorkspaceID, resumeID string, deliver func(event.Event)) (*TUIRuntime, func(), error) {
	clients, err := Dial(socketPath)
	if err != nil {
		return nil, nil, err
	}

	// Health check.
	hctx, hcancel := context.WithTimeout(ctx, 5*time.Second)
	defer hcancel()
	if err := CheckHealth(hctx, clients); err != nil {
		clients.Close()
		return nil, nil, err
	}

	principal := core.EmbeddedPrincipal()
	mgr := NewRemoteConversationManager(clients, principal, workspace)

	var f sessionclient.Facade
	if resumeID != "" {
		f, err = mgr.ResumeConversation(ctx, principal, resumeID)
	} else {
		f, err = mgr.NewConversation(ctx, principal, nil)
		if err == nil {
			// Fresh conversation only: apply the daemon's persisted per-workspace
			// terminal settings (repo-state restore). Mirrors embedded BootstrapTUI's
			// RestoreRepoState:true && resumeID=="" — a resumed session's
			// model/backend is never silently changed. The restore summary surfaces
			// via ConsumeStartupNote → the TUI's Init startup-note banner.
			if rf, ok := f.(*RemoteFacade); ok {
				rf.RestoreRepoState()
			}
		}
	}
	if err != nil {
		_ = mgr.CloseManager()
		clients.Close()
		return nil, nil, fmt.Errorf("remote bootstrap: %w", err)
	}

	// Subscribe the event stream (the pump is armed, not started until
	// StartEventPump).
	if deliver != nil {
		if rf, ok := f.(*RemoteFacade); ok {
			head := event.Seq(0)
			if snap, err := rf.SessionSnapshot(ctx, principal, rf.sessionID); err == nil {
				head = snap.LastSeq
			}
			if _, err := rf.Subscribe(ctx, principal, rf.sessionID, head, deliver); err != nil {
				_ = mgr.Close(f)
				return nil, nil, fmt.Errorf("remote bootstrap: subscribe: %w", err)
			}
		}
	}

	cleanup := func() {
		_ = mgr.Close(f)
	}

	return &TUIRuntime{
		Manager:   mgr,
		Facade:    f,
		Principal: principal,
	}, cleanup, nil
}

// StartEventPump begins event delivery for the runtime's facade.
func (r *TUIRuntime) StartEventPump(ctx context.Context) {
	if rf, ok := r.Facade.(*RemoteFacade); ok {
		rf.StartEventPump(ctx)
	}
}

// SubscribeLive subscribes the runtime facade's event stream with the given
// deliver callback. Called by main.go AFTER tea.NewProgram/SetProgramSend.
func (r *TUIRuntime) SubscribeLive(ctx context.Context, deliver func(event.Event)) error {
	if deliver == nil {
		return nil
	}
	rf, ok := r.Facade.(*RemoteFacade)
	if !ok || rf == nil {
		return fmt.Errorf("remote bootstrap: subscribe: facade is not a remote facade")
	}
	head := event.Seq(0)
	if snap, err := rf.SessionSnapshot(ctx, r.Principal, rf.sessionID); err == nil {
		head = snap.LastSeq
	}
	if _, err := rf.Subscribe(ctx, r.Principal, rf.sessionID, head, deliver); err != nil {
		return fmt.Errorf("remote bootstrap: subscribe: %w", err)
	}
	return nil
}
