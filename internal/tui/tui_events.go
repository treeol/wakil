package tui

// tui_events.go: the wiring-path event switch (card #148 chunk 7b3 m4b).
//
// On the wiring path the TUI receives event.Event values from the facade's
// event pump (delivered via tea.Program.Send) instead of agent messages.
// This switch maps each event kind to the same TUI state mutations the old
// handleAgentMsg performed for the corresponding agent message, per the
// D2 mapping in the m4b design. The old switch stays live for the legacy
// (App-driven) path until m4c flips the bootstrap; the two never both run —
// a model is either facade-backed (events) or App-backed (agent msgs).
//
// Session guard: every event carries its SessionID; the model caches the
// current facade's session ID and DROPS events from any other session (a
// stale pump delivery racing a rotation). The cached ID is refreshed on
// rotationMsg, before the new pump starts (op-32 review: events processed
// before the swap must not mutate view state).

import (
	"strings"
	"time"

	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionclient"

	tea "github.com/charmbracelet/bubbletea"
)

// sessionclientRepoState aliases the neutral repo-state mutator type for the
// SaveRepoState callback below (the facade's own type name is a mouthful).
type sessionclientRepoState = sessionclient.RepoStateMutator

// handleEventMsg handles one domain event on the wiring path. It mirrors
// handleAgentMsg's forwarding contract: no early return — Update still
// forwards the event to the textarea/viewport (harmless: they ignore unknown
// types), and handled reports whether the event matched.
func (m tuiModel) handleEventMsg(msg tea.Msg, cmds []tea.Cmd) (tuiModel, []tea.Cmd, bool) {
	// Local wiring-path messages first (not event.Event values).
	switch lm := msg.(type) {
	case commandResultMsg:
		return m.applyCommandResult(lm.result, cmds)
	case rotationMsg:
		return m.applyRotation(lm, cmds)
	case startupNoteMsg:
		m.addItem(iSys, dim2(lm.text))
		return m, cmds, true
	case teaErrorMsg:
		m.addItem(iSys, styleErr("subscription error: "+lm.err.Error()))
		return m, cmds, true
	case dotTickMsg:
		// Re-arm only while busy OR an async-job tab is still running. The
		// dotArmed flag keeps exactly one recurring tick chain alive.
		if m.state != stateIdle || m.hasActiveJobTab() {
			m.dotPhase = (m.dotPhase + 1) % len(dotPulseShades)
			if !m.dotArmed {
				m.dotArmed = true
				cmds = append(cmds, startDotTick())
			}
		} else {
			m.dotArmed = false
		}
		return m, cmds, true
	case armTickMsg:
		// Clear the arm only if this tick belongs to the current arm and the
		// deadline has actually passed.
		if lm.seq == m.armSeq && !m.armUntil.IsZero() && !time.Now().Before(m.armUntil) {
			before := m.statusRows()
			m.clearArm()
			m = m.reflowIfStatusHeightChanged(before)
		}
		return m, cmds, true
	case subTabCloseMsg:
		focusN := 0
		if m.subCur >= 0 && m.subCur < len(m.subTabs) {
			focusN = m.subTabs[m.subCur].n
		}
		oldLen := len(m.subTabs)
		removed := false
		for i, t := range m.subTabs {
			match := (lm.ChatID != "" && t.kind == subTabSubagent && t.chatID == lm.ChatID) ||
				(lm.OpID != "" && t.kind == subTabAsyncJob && t.opID == lm.OpID)
			if match && t.done && t.n != focusN {
				m.subTabs = append(m.subTabs[:i], m.subTabs[i+1:]...)
				removed = true
				break
			}
		}
		if removed {
			m.subCur = tabIndexByN(m.subTabs, focusN)
			if oldLen > 0 && len(m.subTabs) == 0 {
				m = m.reflow()
			}
		}
		return m, cmds, true
	case copiedMsg:
		before := m.statusRows()
		if lm.via == copyViaOSC52 {
			m.flash = sprint("sent %d chars via OSC 52 — if paste is empty, enable terminal/tmux clipboard", lm.n)
			m.pendingEscape = lm.escape
		} else {
			m.flash = sprint("copied %d chars ✓", lm.n)
		}
		m = m.reflowIfStatusHeightChanged(before)
		return m, cmds, true
	case clipboardImageMsg:
		// A clipboard read completed (paste-detection or /image clipboard).
		if lm.Err != "" {
			if m.pasteCutStash != "" {
				m.ta.InsertString(m.pasteCutStash)
				m.pasteCutStash = ""
				m.addItem(iSys, dim2("· no image on clipboard — restored the pasted text"))
			} else {
				m.addItem(iSys, styleErr("clipboard: "+lm.Err))
			}
		} else {
			m.pasteCutStash = "" // real image confirmed; the cut garbage stays gone
			m.facade.AddPendingImage(lm.Img)
			chip := lm.Img.Placeholder()
			*m.imageChips = append(*m.imageChips, chip)
			m.ta.InsertString(chip + " ")
		}
		m.refreshViewport()
		return m, cmds, true
	}

	ev, ok := msg.(event.Event)
	if !ok {
		return m, cmds, false
	}
	// Session guard: drop events not belonging to the current conversation.
	// m.sessionID is cached at attach/rotation time (never re-fetched per
	// event — Snapshot() copies the whole conversation).
	if m.sessionID != "" && ev.SessionID != "" && event.SessionID(ev.SessionID) != m.sessionID {
		return m, cmds, true // consumed (stale), not forwarded
	}

	switch ev.Kind {
	case event.KindTurnStarted:
		// The host accepted and started a turn (SubmitInput ack → running).
		// The TUI's display state was already set optimistically at send; this
		// is the authoritative transition (e.g. for workflow-continuation
		// turns the TUI did NOT initiate).
		before := m.statusRows()
		if m.state == stateIdle || m.state == stateWaiting {
			m.state = stateStreaming
			m.turnStart = time.Now()
			m.tps = 0
			m.followBottom = true
			m.vp.GotoBottom()
			var dotCmd tea.Cmd
			m, dotCmd = m.startDotTickIfUnarmed()
			if dotCmd != nil {
				cmds = append(cmds, dotCmd)
			}
		}
		m = m.reflowIfStatusHeightChanged(before)

	case event.KindTurnSuspended:
		// The turn paused on pending async work (Mashūra panel, detached
		// shell, discovery subagent). Transition to stateWaiting so the
		// status line shows "waiting" and Enter-while-waiting can
		// cancel-and-send.
		before := m.statusRows()
		if m.state == stateStreaming {
			m.state = stateWaiting
			m = m.reflowIfStatusHeightChanged(before)
		}

	case event.KindTurnResumed:
		// A suspended turn resumed after an async completion arrived.
		// Transition back to stateStreaming — UNLESS the user already
		// initiated a cancel-and-send from stateWaiting (cancelling=true).
		// In that case the cancel is in flight; the TurnResumed event is a
		// race with the cancel and must NOT restore stateStreaming, or
		// subsequent prompts would take the streaming-queue path (no
		// flushOnCancel) and get stuck. The in-flight cancel will produce
		// a TurnCompleted{cancelled} which flushes the queued prompt.
		before := m.statusRows()
		if m.state == stateWaiting && !m.cancelling {
			m.state = stateStreaming
			m = m.reflowIfStatusHeightChanged(before)
		}

	case event.KindMessageDelta:
		p := ev.Payload.(event.MessageDelta)
		// First content after reasoning collapses the thinking block (same
		// rule as the StreamChunkMsg handler).
		if m.reasoning.Len() > 0 && !m.reasoningDone {
			toks := m.reasoning.Len() / 4
			m.reasoning.Reset()
			m.reasoningDone = true
			m.reasoningExpanded = false
			m.addItem(iDiag, dim2(sprint("· thought (~%d tokens)", toks)))
		}
		m.streaming.WriteString(p.Text)
		m.refreshViewport()

	case event.KindReasoningDelta:
		p := ev.Payload.(event.ReasoningDelta)
		m.reasoning.WriteString(p.Text)
		m.refreshViewport()

	case event.KindTokRate:
		p := ev.Payload.(event.TokRate)
		before := m.statusRows()
		m.tps = p.Rate
		if p.Rate > 0 {
			m.lastTps = p.Rate
		}
		m = m.reflowIfStatusHeightChanged(before)

	case event.KindToolCallStarted:
		// ArgDigest carries the display command (projection convention).
		p := ev.Payload.(event.ToolCallStarted)
		before := m.statusRows()
		tool := &runningToolState{
			toolCallID: string(p.ToolCallID),
			name:       p.Name,
			command:    p.ArgDigest,
		}
		m.runningTool = tool
		m.lastTool = tool
		m = m.reflowIfStatusHeightChanged(before)

	case event.KindToolCallCompleted:
		p := ev.Payload.(event.ToolCallCompleted)
		if m.runningTool != nil && m.runningTool.toolCallID == string(p.ToolCallID) {
			before := m.statusRows()
			m.runningTool = nil
			m = m.reflowIfStatusHeightChanged(before)
		}

	case event.KindApprovalRequested:
		p := ev.Payload.(event.ApprovalRequested)
		before := m.statusRows()
		if m.searchActive {
			m.searchExit(false)
		}
		m.state = stateConfirm
		m.pendApproval = &pendingApprovalState{
			approvalID: string(p.ApprovalID),
			toolName:   p.ToolName,
			headline:   p.Headline,
			detail:     p.Detail,
			readAction: p.ReadAction,
		}
		m.flushStreaming()
		m.addItem(iSys, fmtConfirmBlock(p.Headline, p.Detail, p.ReadAction))
		m = m.reflowIfStatusHeightChanged(before)

	case event.KindApprovalResolved:
		// The turn resumes server-side; the TUI's gate is answered. Only the
		// matching approval clears the pending state (a stale resolution for
		// an already-answered approval is ignored).
		p := ev.Payload.(event.ApprovalResolved)
		before := m.statusRows()
		if m.pendApproval != nil && m.pendApproval.approvalID == string(p.ApprovalID) {
			m.pendApproval = nil
			m.state = stateStreaming
			m = m.reflowIfStatusHeightChanged(before)
		}

	case event.KindTurnCompleted:
		p := ev.Payload.(event.TurnCompleted)
		m, cmds = m.finishWiringTurn(p, cmds)

	case event.KindSessionError:
		p := ev.Payload.(event.SessionError)
		m.addItem(iSys, styleErr("error: "+p.Err))
		m = m.clearWiringTurnState()

	case event.KindSubagentSpawned:
		p := ev.Payload.(event.SubagentSpawned)
		m, cmds = m.spawnSubTab(p, cmds)

	case event.KindSubagentProgress:
		p := ev.Payload.(event.SubagentProgress)
		if p.Finished {
			// Display-only early completion (the old SubagentFinishedMsg).
			for _, t := range m.subTabs {
				if t.chatID == string(p.SubagentID) {
					t.finished = true
					t.finStatus = p.FinishedStatus
					t.finCostUSD = p.FinishedCostUSD
					t.finFilesN = p.FinishedFilesN
					t.finPreview = p.Text
					break
				}
			}
			break
		}
		if p.Text == "[active]" {
			// Queued → running: the worker acquired a parallelism slot.
			for _, t := range m.subTabs {
				if t.chatID == string(p.SubagentID) {
					t.active = true
					break
				}
			}
			break
		}
		for _, t := range m.subTabs {
			if t.chatID == string(p.SubagentID) {
				t.buf.WriteString(p.Text)
				break
			}
		}

	case event.KindSubagentCompleted:
		p := ev.Payload.(event.SubagentCompleted)
		m, cmds = m.completeSubTab(p, cmds)

	case event.KindAsyncJobStarted:
		p := ev.Payload.(event.AsyncJobStarted)
		m, cmds = m.startJobTab(p, cmds)

	case event.KindAsyncJobProgress:
		p := ev.Payload.(event.AsyncJobProgress)
		for _, t := range m.subTabs {
			if t.kind == subTabAsyncJob && t.opID == string(p.OpID) {
				if !t.done {
					appendAsyncJobStatus(t, p.Text)
				}
				break
			}
		}

	case event.KindAsyncJobCompleted:
		p := ev.Payload.(event.AsyncJobCompleted)
		m, cmds = m.completeJobTab(p, cmds)

	case event.KindSideQuestionProgress:
		p := ev.Payload.(event.SideQuestionProgress)
		if m.sideQuestion != nil {
			m.sideQuestion.buf.WriteString(p.Text)
		}

	case event.KindSideQuestionCompleted:
		p := ev.Payload.(event.SideQuestionCompleted)
		if m.sideQuestion != nil {
			switch p.Status {
			case "error":
				m.addItem(iSys, dim2("≫ side question error: "+p.AnswerPreview))
			default:
				if out := strings.TrimSpace(m.sideQuestion.buf.String()); out != "" {
					m.addItem(iSys, dim2("≫ "+out))
				} else if p.AnswerPreview != "" {
					m.addItem(iSys, dim2("≫ "+p.AnswerPreview))
				} else {
					m.addItem(iSys, dim2("≫ (side question returned no output)"))
				}
			}
			m.sideQuestion = nil
		}
		if m.sideQuestionCancel != nil {
			m.sideQuestionCancel()
			m.sideQuestionCancel = nil
		}

	case event.KindSessionNote:
		p := ev.Payload.(event.SessionNote)
		m.addItem(iSys, dim2(p.Text))

	case event.KindLearnNudge:
		p := ev.Payload.(event.LearnNudge)
		m.addItem(iSys, dim2(p.Text))

	case event.KindWorkflowTurnStarted:
		// Audit marker: the host already enqueued the continuation (the TUI
		// is passive on the wiring path — "host enqueues" decision). Render a
		// progress note so the workflow advance is visible.
		m.addItem(iSys, dim2("· workflow: next step submitted"))

	case event.KindWorkflowFinalReview:
		m.addItem(iSys, dim2("· running final oracle review"))

	case event.KindConversationCompacted:
		m.addItem(iDiag, dim2("· compacted earlier turns"))

	case event.KindUserMessageCommitted, event.KindMessageCommitted:
		// Replay truth: the live viewport renders user input locally at send
		// time and assistant text via MessageDelta; the committed blocks are
		// what a REPLAY would reconstruct from. No live handling.

	case event.KindSessionClosed:
		p := ev.Payload.(event.SessionClosed)
		m.addItem(iSys, dim2("· session closed: "+p.Reason))

	default:
		return m, cmds, false
	}
	return m, cmds, true
}

// finishWiringTurn is the TurnCompleted handler: the wiring-path equivalent
// of the AgentDoneMsg case. Outcome drives the display classification; the
// deferred-grant and queue-flush gates key off Outcome/WorkflowWillContinue.
func (m tuiModel) finishWiringTurn(p event.TurnCompleted, cmds []tea.Cmd) (tuiModel, []tea.Cmd) {
	before := m.statusRows()
	m.flushStreaming()
	// Edge case: turn ended during/after reasoning but before any content.
	if m.reasoning.Len() > 0 {
		toks := m.reasoning.Len() / 4
		m.addItem(iDiag, dim2(sprint("· thought (~%d tokens)", toks)))
		m.reasoning.Reset()
	}
	m.reasoningDone = false
	m.reasoningExpanded = false

	switch p.Outcome {
	case "cancelled":
		m.addItem(iSys, dim2("[turn cancelled]"))
	case "stream_error":
		// The host emits SessionError immediately AFTER this event; its Err
		// string carries the detail (including the tidy stream-warn text).
		// No duplicate render here.
	case "empty":
		m.addItem(iSys, dim2("· (empty response)"))
	}

	// Chime on a successful finish of a long-enough turn.
	if p.Outcome == "complete" && !m.turnStart.IsZero() && time.Since(m.turnStart) > 3*time.Second {
		cmds = append(cmds, playFinishSound())
	}

	// Deferred /auto grants + queue flush: only on a clean, terminal turn with
	// no queued follow-up (WorkflowWillContinue covers the workflow case; a
	// non-empty host queue means another turn starts immediately).
	clean := p.Outcome == "complete" && !p.WorkflowWillContinue
	// flushOnIdle: after any non-error turn completion that leaves the system
	// idle (no workflow continuation), flush a queued prompt. This covers
	// the case where the user queued a prompt mid-turn and then cancelled —
	// the prompt should auto-flush instead of staying stuck. Errors are
	// excluded (the system may be in a bad state); workflow continuations
	// are excluded (another turn starts immediately). "empty" is treated as
	// a terminal success — the model returned nothing but the system is idle
	// and the queue should drain.
	flushOnIdle := (p.Outcome == "cancelled" || p.Outcome == "complete" || p.Outcome == "empty") && !p.WorkflowWillContinue
	m = m.clearWiringTurnState()
	if clean {
		if m.pendingAutoGrant {
			m.facade.SetAutoApprove(true)
			m.facade.SaveRepoState(func(s *sessionclientRepoState) { s.AutoApprove = true })
			m.pendingAutoGrant = false
			m.addItem(iSys, dim2("· auto: granted (pending from mid-turn)"))
		}
		if m.pendingDestructiveGrant {
			if m.facade.Consent().AutoApprove {
				m.facade.SetAllowDestructive(true)
				m.addItem(iSys, dim2("· auto: destructive granted (pending from mid-turn)"))
			}
			m.pendingDestructiveGrant = false
		}
	}
	if flushOnIdle && len(m.queuedPrompts) > 0 {
		next := m.queuedPrompts[0]
		m.queuedPrompts = m.queuedPrompts[1:]
		var flushCmds []tea.Cmd
		m, flushCmds = m.flushQueuedPrompt(next.text)
		cmds = append(cmds, flushCmds...)
	}
	m = m.reflowIfStatusHeightChanged(before)
	return m, cmds
}

// clearWiringTurnState resets the per-turn display state at turn end.
func (m tuiModel) clearWiringTurnState() tuiModel {
	m.turnStart = time.Time{}
	m.tps = 0
	m.runningTool = nil
	m.lastTool = nil
	m.state = stateIdle
	m.dotPhase = 0
	m.hadTurn = true
	m.cancel = nil
	m.cancelling = false
	m.flushOnCancel = false
	m.pendApproval = nil // safety: a lost ApprovalResolved must not wedge the gate
	m.clearArm()
	// Cancel any running side question at turn end (same policy as the old
	// path); its Done event renders the output and cleans up.
	if m.sideQuestionCancel != nil {
		m.sideQuestionCancel()
		m.sideQuestionCancel = nil
	}
	return m
}
