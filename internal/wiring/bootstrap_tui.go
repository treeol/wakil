// bootstrap_tui.go: the TUI bootstrap surface (card #148 chunk 7b3 m4c).
//
// This is the single entry point cmd/wakil/main.go calls to run the TUI
// through the session host: it constructs the ConversationManager, creates
// (or resumes) the first conversation, subscribes the event stream, and
// returns everything the TUI needs (facade, manager, principal, delivery
// hook). main.go stops importing internal/agent entirely (Gate #1, TUI half).
//
// The resume path (wakil --resume <id>) is served by ResumeConversation —
// the manager loads the session and restores its state (transcript, chat ID,
// workflow) into a fresh App, exactly as main.go's old inline resume did.

package wiring

import (
	"context"
	"fmt"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionclient"
	"github.com/treeol/wakil/internal/exec"
)

// TUIRuntime bundles what the TUI bootstrap needs from wiring. The TUI holds
// the facade (all conversation interaction) and the manager (rotation); the
// principal identifies the local user for host calls.
type TUIRuntime struct {
	Manager   sessionclient.ConversationManager
	Facade    sessionclient.Facade
	Principal core.Principal
}

// BootstrapTUI builds the ConversationManager and the first conversation.
//
// resumeID, when non-empty, resumes that session (ID or unique prefix) into
// the first facade — the --resume flag path. An empty resumeID creates a
// fresh conversation. The startup note (resume or fresh) is left on the App
// for the TUI's Init to consume via ConsumeStartupNote.
//
// deliver is the event-delivery callback (the TUI's tea.Program.Send once
// constructed; until then the pump is NOT running — StartEventPump begins
// delivery when the caller is ready). nil deliver means the caller subscribes
// later (tests).
//
// The returned cleanup function must be deferred by the caller: it closes the
// facade (stopping the pump, cancelling detached jobs) and releases the
// manager's resources.
func BootstrapTUI(cfg config.Config, exe exec.Executor, resumeID string, deliver func(event.Event)) (*TUIRuntime, func(), error) {
	principal := core.Principal{
		TenantID: event.EmbeddedTenantID,
		UserID:   event.EmbeddedUserID,
		Role:     core.RoleOwner,
	}

	mgr, err := NewConversationManager(cfg, exe, principal)
	if err != nil {
		return nil, nil, fmt.Errorf("tui bootstrap: %w", err)
	}

	ctx := context.Background()
	var f sessionclient.Facade
	if resumeID != "" {
		f, err = mgr.ResumeConversation(ctx, principal, resumeID)
		if err != nil {
			return nil, nil, fmt.Errorf("tui bootstrap: %w", err)
		}
	} else {
		f, err = mgr.NewConversation(ctx, principal)
		if err != nil {
			return nil, nil, fmt.Errorf("tui bootstrap: %w", err)
		}
	}

	// Subscribe the event stream and arm the pump (not started until the
	// caller invokes StartEventPump). Subscription begins at seq 0 so the
	// TUI sees the full session history — the resume path rebuilds its
	// viewport from the snapshot's Conv, and live events replay from the
	// start (the projection skips what it already rendered via Seq
	// bookkeeping in a later refinement; for now the TUI is the only
	// consumer and renders from the snapshot, ignoring replayed durable
	// events it already folded).
	wf, ok := f.(*wiringFacade)
	if ok && deliver != nil {
		if _, err := wf.Subscribe(ctx, principal, wf.sessionID, 0, deliver); err != nil {
			_ = mgr.Close(f)
			return nil, nil, fmt.Errorf("tui bootstrap: subscribe: %w", err)
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

// StartEventPump begins event delivery for the runtime's facade. Called by
// the TUI bootstrap once the tea.Program exists (so deliver =
// prog.Send is safe to call from the pump goroutine).
func (r *TUIRuntime) StartEventPump(ctx context.Context) {
	if wf, ok := r.Facade.(*wiringFacade); ok {
		wf.StartEventPump(ctx)
	}
}
