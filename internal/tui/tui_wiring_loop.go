package tui

// tui_wiring_loop.go: wiring-path local messages — the command-result and
// rotation appliers (m4b stage 3). Both run ONLY in Update (the event loop):
// their Cmds merely deliver results; all model mutation happens here, which
// keeps the Bubble Tea single-threaded model contract (op-32 review:
// rotating/rotating-flag writes must never happen on a Cmd goroutine).

import (
	"context"
	"time"

	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionclient"

	tea "github.com/charmbracelet/bubbletea"
)

// commandResultMsg carries a DispatchCommand result back to the event loop
// (the dispatch itself ran in a Cmd goroutine — slow commands must not block
// Update).
type commandResultMsg struct {
	result sessionclient.CommandResult
}

// startupNoteMsg renders the bootstrap startup note (replaces the old
// agent.SysNoteMsg Init dance on the wiring path).
type startupNoteMsg struct {
	text string
}

// applyCommandResult interprets a CommandResult on the event loop: notices,
// submits, rotations, the resume picker, clipboard requests, compaction
// markers. After EVERY handled result the snapshot is conceptually dirty —
// commands mutate authoritative state facade-side with no event (D24) — but
// the TUI reads through Snapshot()/Info() on demand (no cached copy to
// refresh), so nothing needs an explicit re-fetch here.
func (m tuiModel) applyCommandResult(cr sessionclient.CommandResult, cmds []tea.Cmd) (tuiModel, []tea.Cmd, bool) {
	if !cr.Handled {
		return m, cmds, false
	}
	if cr.Quit {
		return m, append(cmds, tea.Quit), true
	}
	if cr.Notice != "" {
		m.addItem(iSys, dim2(cr.Notice))
	}
	if cr.Compacted {
		m.addItem(iDiag, dim2("· compacted earlier turns"))
	}
	if cr.ClipboardImage {
		cmds = append(cmds, readClipboardCmd())
	}
	if cr.SideQuestion != "" {
		m = m.startSideQuestion(cr.SideQuestion)
	}
	if cr.ResumePicker {
		// Bare /resume: open the picker with the workspace-scoped list.
		scope := sessionclient.SessionScope{Workspace: ""}
		if snap, ok := m.snapshot(); ok {
			scope.Workspace = snap.Workspace
		}
		sessions, hidden, err := m.facade.ListSessions(scope)
		if err != nil {
			m.addItem(iSys, dim2("resume: "+err.Error()))
			return m, cmds, true
		}
		prevH := m.resumePickerHeight()
		m = m.openResumePicker(sessions, scope, hidden)
		if m.resumePickerHeight() != prevH {
			m = m.reflow()
		}
	}
	if cr.Submit != "" {
		// Command-initiated turn (e.g. /learn, /plan approve, /remember):
		// route through the same submit path as a user send. No image chips.
		m.followBottom = true
		m.vp.GotoBottom()
		before := m.statusRows()
		m.state = stateStreaming
		m.turnStart = time.Now()
		m = m.reflowIfStatusHeightChanged(before)
		if _, err := m.facade.SubmitInput(context.Background(), m.principal, core.SubmitInputRequest{
			SessionID: m.sessionID,
			Text:      cr.Submit,
		}); err != nil {
			m.addItem(iSys, styleErr("submit failed: "+err.Error()))
			m.state = stateIdle
			m.turnStart = time.Time{}
		}
	}
	if cr.Rotate != nil {
		switch cr.Rotate.Type {
		case "new":
			m.rotating = true
			cmds = append(cmds, m.beginRotation(rotationRequest{kind: rotateNew}))
		case "resume":
			m.rotating = true
			// The id/prefix travels via Rotate.Session when the manager
			// resolved it; /resume <id> dispatch left it nil — the manager
			// re-resolves from Rotate.Session.ChatID only when present, so
			// carry what we have.
			id := ""
			if cr.Rotate.Session != nil {
				id = cr.Rotate.Session.ChatID
			}
			cmds = append(cmds, m.beginRotation(rotationRequest{kind: rotateResume, sessionID: id}))
		case "handoff":
			m.rotating = true
			cmds = append(cmds, m.beginRotation(rotationRequest{kind: rotateHandoff, proceed: cr.Rotate.Proceed}))
		}
	}
	return m, cmds, true
}

// applyRotation swaps the conversation after a rotation Cmd completed:
//  1. swap facade/sessionID refs (the manager/principal are stable),
//  2. rebuild view state (items, tabs, queue, chips — everything belonging
//     to the old conversation),
//  3. THEN subscribe the new facade at its durable head and start the pump —
//     after the swap, so the session guard accepts the new session's events
//     (op-32 review: events delivered before the swap would be dropped).
func (m tuiModel) applyRotation(rm rotationMsg, cmds []tea.Cmd) (tuiModel, []tea.Cmd, bool) {
	m.rotating = false
	if rm.failed {
		m.addItem(iSys, dim2("⚠ rotation failed: "+rm.err.Error()))
		if rm.note != "" {
			m.addItem(iSys, dim2(rm.note))
		}
		return m, cmds, true
	}
	if rm.facade == nil {
		m.addItem(iSys, dim2("⚠ rotation returned no conversation"))
		return m, cmds, true
	}

	before := m.statusRows()
	m.facade = rm.facade
	snap := rm.facade.Snapshot()
	m.sessionID = snap.SessionID

	// Rebuild view state (same reset as the old NewConvMsg handler).
	m.state = stateIdle
	m.cancelling = false
	m.flushOnCancel = false
	m.turnStart = time.Time{}
	m.tps = 0
	m.dotPhase = 0
	if m.sideQuestionCancel != nil {
		m.sideQuestionCancel()
		m.sideQuestionCancel = nil
	}
	m.sideQuestion = nil
	m.clearArm()
	*m.items = (*m.items)[:0]
	m.streaming.Reset()
	m.reasoning.Reset()
	m.reasoningDone = false
	m.reasoningExpanded = false
	m.followBottom = true
	if len(m.queuedPrompts) > 0 {
		m.addItem(iSys, dim2(sprint("· queue cleared (%d prompts dropped)", len(m.queuedPrompts))))
	}
	m.queuedPrompts = nil
	m.runningTool = nil
	m.lastTool = nil
	m.pendingAutoGrant = false
	m.pendingDestructiveGrant = false
	*m.imageChips = (*m.imageChips)[:0]
	m.pendApproval = nil
	if len(m.subTabs) > 0 {
		m.subTabs = nil
		m.subCur = -1
		m = m.reflow()
	}
	// Resume path: rebuild the conversation view from the new snapshot.
	if len(snap.Conv) > 0 {
		*m.items = convItemsFrom(snap.Conv)
	}
	m.prefixDirty = true
	m.refreshViewport()

	m.addItem(iSys, dim2("· new conversation: "+formatShortID(snap.ChatID)))
	if rm.note != "" {
		m.addItem(iSys, dim2(rm.note))
	}
	m.followBottom = true
	m.vp.GotoBottom()

	// Restore info panel visibility from the new facade (WP-9.1: the TUI
	// cached infoPanel.active locally and never re-read it on rotation, so
	// the panel's open/closed state was lost on /new and /handoff).
	wasOpen := m.infoPanel.active
	m.infoPanel.active = rm.facade.Info().InfoPanelOpen
	if m.infoPanel.active != wasOpen {
		m = m.reflow()
	}

	// Subscribe live-only (at the durable head) and start delivery AFTER the
	// swap — the session guard now accepts the new session's events. The
	// deliver callback is the tea.Program's Send, installed at bootstrap via
	// SetProgramSend (the TUI package's replacement for main.go's globalProg).
	if programSend != nil {
		ctx := context.Background()
		go func(f sessionclient.Facade, sid event.SessionID, principal core.Principal) {
			head := event.Seq(0)
			if ss, err := f.SessionSnapshot(ctx, principal, sid); err == nil {
				head = ss.LastSeq
			}
			if _, err := f.Subscribe(ctx, principal, sid, head, func(ev event.Event) {
				programSend(ev)
			}); err != nil {
				programSend(teaErrorMsg{err})
				return
			}
			f.StartEventPump(ctx)
			// /handoff proceed: submit the continuation prompt AFTER the
			// subscription and pump are live, so TurnStarted arrives through
			// the event stream (eliminates the race where the host emits it
			// before the TUI is subscribed).
			if prompt := f.ConsumePendingContinuation(); prompt != "" {
				if _, err := f.SubmitInput(ctx, principal, core.SubmitInputRequest{
					SessionID: sid,
					Text:      prompt,
				}); err != nil {
					// SubmitInput failed (queue full or closing). The
					// handoff itself succeeded — the new session exists and
					// carries the context as a pinned message. Surface the
					// miss so the user can send the prompt themselves.
					programSend(startupNoteMsg{text: "· handoff complete — continuation could not auto-start (" + err.Error() + "); the context is pinned, send your next message"})
				}
			}
		}(rm.facade, m.sessionID, m.principal)
	}
	m = m.reflowIfStatusHeightChanged(before)
	return m, cmds, true
}

// teaErrorMsg surfaces an asynchronous error (subscribe failure) through the
// event loop as a display note.
type teaErrorMsg struct{ err error }

// programSend is the tea.Program's Send, installed by the bootstrap once the
// program is constructed (TUI-package-local replacement for main.go's
// globalProg — it never leaves this package).
var programSend func(tea.Msg)

// SetProgramSend installs the program's Send function. Called from the TUI
// bootstrap (main.go) right after tea.NewProgram.
func SetProgramSend(send func(tea.Msg)) { programSend = send }
