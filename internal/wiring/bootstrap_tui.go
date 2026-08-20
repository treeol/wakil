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
	"os"
	"time"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionclient"
	"github.com/treeol/wakil/internal/exec"
	"github.com/treeol/wakil/internal/proxy"
)

// TUIRuntime bundles what the TUI bootstrap needs from wiring. The TUI holds
// the facade (all conversation interaction) and the manager (rotation); the
// principal identifies the local user for host calls.
type TUIRuntime struct {
	Manager   sessionclient.ConversationManager
	Facade    sessionclient.Facade
	Principal core.Principal
}

// BootstrapTUIOpts carries the TUI-entry-point-specific bootstrap steps that
// differ from a bare conversation (m4c — mirrors what main.go did inline).
// All optional; zero value = plain bootstrap.
type BootstrapTUIOpts struct {
	// AttachImages are pre-loaded images (--attach-image) queued into the
	// first conversation's pending images.
	AttachImages []proxy.ImagePart
	// RestoreRepoState runs the per-workspace terminal-settings restore on a
	// FRESH conversation (never on resume — a resumed session's model/backend
	// must not be silently changed) and composes its note.
	RestoreRepoState bool
	// CounselMode/CounselMax configure the counsel engine (TUI defaults).
	CounselMode string
	CounselMax  int
	// ComposeStartupNotes adds the staging/memory pending-proposal notes to
	// whatever startup note the conversation already has.
	ComposeStartupNotes bool
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
// opts runs the TUI-specific post-construction steps (repo-state restore,
// counsel, attach-images, startup-note composition) — the pieces main.go
// used to do inline before the cut.
//
// The returned cleanup function must be deferred by the caller: it closes the
// facade (stopping the pump, cancelling detached jobs) and releases the
// manager's resources.
func BootstrapTUI(cfg config.Config, exe exec.Executor, resumeID string, deliver func(event.Event), opts BootstrapTUIOpts) (*TUIRuntime, func(), error) {
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
		f, err = mgr.NewConversation(ctx, principal, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("tui bootstrap: %w", err)
		}
	}

	// TUI-specific post-construction steps (m4c — what main.go did inline).
	if wf, ok := f.(*wiringFacade); ok {
		app := wf.app
		// Attach images (--attach-image): queued into the first turn.
		if len(opts.AttachImages) > 0 {
			app.PendingImages = append(app.PendingImages, opts.AttachImages...)
		}
		// Fresh conversation only: per-repo terminal settings restore
		// (a resumed session's model/backend is never silently changed).
		if opts.RestoreRepoState && resumeID == "" {
			result := agent.RestoreRepoState(app)
			if result.Note != "" {
				app.StartupNote = result.Note
			}
			// Re-resolve context limits using the literal restored strings —
			// mirrors resolveBackendCtxCmd's calling convention (reading
			// app.SelectedModel back would be wrong for openai-kind endpoints).
			if result.Model != "" || result.Backend != "" {
				app.CtxLimit = agent.ResolveContextLimitForBackendModel(ctx, app.Client.HTTP, cfg, result.Backend, result.Model, os.Stderr)
			}
		}
		// Counsel mode (TUI defaults).
		if opts.CounselMode != "" {
			app.SetCounselMode(opts.CounselMode)
			app.MaxCounsel = opts.CounselMax
		}
		// Compose staging/memory notes onto whatever exists.
		if opts.ComposeStartupNotes {
			if app.StagingClient != nil {
				scanCtx, scanCancel := context.WithTimeout(ctx, 3*time.Second)
				if res, err := app.StagingClient.Scan(scanCtx, "", 1, ""); err == nil && len(res.Keys) > 0 {
					note := "staging: entries restored"
					if app.StartupNote != "" {
						app.StartupNote += " | " + note
					} else {
						app.StartupNote = note
					}
				}
				scanCancel()
			}
			if app.MemoryStore != nil {
				statsCtx, statsCancel := context.WithTimeout(ctx, 3*time.Second)
				stats, _ := app.MemoryStore.Stats(statsCtx, 5)
				statsCancel()
				if stats != nil && stats.PendingProposed > 0 {
					note := fmt.Sprintf("memory: %d proposals pending", stats.PendingProposed)
					if app.StartupNote != "" {
						app.StartupNote += " | " + note
					} else {
						app.StartupNote = note
					}
				}
			}
		}
	}

	// Subscribe the event stream and arm the pump (not started until the
	// caller invokes StartEventPump). The subscription begins at the session's
	// CURRENT durable head (not seq 0): the TUI hydrates its view from the
	// facade snapshot (Snapshot().Conv for a resumed session), and replaying
	// the durable history would re-open dead confirm gates (replayed
	// ApprovalRequested), re-render committed messages, and re-arm
	// auto-close timers. Live-only delivery: the pump carries everything
	// emitted AFTER the TUI attached. The subscription's gap recovery is
	// anchored at the same head (NewEventPump initialSeq), so a gap between
	// subscribe and pump-start replays only the missed live tail, never the
	// pre-attach history.
	head := event.Seq(0)
	if wf, ok := f.(*wiringFacade); ok {
		if snap, err := wf.SessionSnapshot(ctx, principal, wf.sessionID); err == nil {
			head = snap.LastSeq
		}
		if deliver != nil {
			if _, err := wf.Subscribe(ctx, principal, wf.sessionID, head, deliver); err != nil {
				_ = mgr.Close(f)
				return nil, nil, fmt.Errorf("tui bootstrap: subscribe: %w", err)
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

// StartEventPump begins event delivery for the runtime's facade. Called by
// the TUI bootstrap once the tea.Program exists (so deliver =
// prog.Send is safe to call from the pump goroutine).
func (r *TUIRuntime) StartEventPump(ctx context.Context) {
	if wf, ok := r.Facade.(*wiringFacade); ok {
		wf.StartEventPump(ctx)
	}
}

// SubscribeLive subscribes the runtime facade's event stream with the given
// deliver callback (the tea.Program.Send) and leaves the pump armed for
// StartEventPump. main.go calls this AFTER tea.NewProgram/SetProgramSend,
// because BootstrapTUI cannot subscribe at construction time — prog.Send does
// not exist yet. It mirrors the rotation path (applyRotation), which
// subscribes lazily against programSend.
//
// Without this call on first boot the facade has NO subscription and therefore
// NO pump (StartEventPump is a no-op when Subscribe was never called): the
// host still runs every submitted turn — billing the request — but
// TurnStarted/MessageDelta/TurnCompleted are never delivered to the TUI, so
// the optimistic "streaming" state never clears and the answer never renders.
func (r *TUIRuntime) SubscribeLive(ctx context.Context, deliver func(event.Event)) error {
	if deliver == nil {
		return nil
	}
	wf, ok := r.Facade.(*wiringFacade)
	if !ok || wf == nil {
		return fmt.Errorf("tui bootstrap: subscribe: facade is not a wiring facade")
	}
	head := event.Seq(0)
	if snap, err := wf.SessionSnapshot(ctx, r.Principal, wf.sessionID); err == nil {
		head = snap.LastSeq
	}
	if _, err := wf.Subscribe(ctx, r.Principal, wf.sessionID, head, deliver); err != nil {
		return fmt.Errorf("tui bootstrap: subscribe: %w", err)
	}
	return nil
}
