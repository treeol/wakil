package tui

import (
	"context"
	"errors"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/proxy"
)

// handleAgentMsg handles the agent-lifecycle and subagent messages that fall
// through to the trailing textarea/viewport forward in Update. Extracted from
// Update's giant switch (WP-6.6 part 3) to keep Update a thin dispatcher.
//
// FORWARDING CONTRACT: these cases do NOT early-return — after they run, Update
// still forwards msg to m.ta.Update(msg) and m.vp.Update(msg). handled reports
// whether the message matched a case here; when false, Update falls through to
// that same trailing forward (which is what an unmatched message got before the
// extraction). handled is informational only — Update ignores it and forwards
// regardless, so do NOT branch on it to skip the forward.
//
// Boundary note: WindowSizeMsg and unconsumed MouseMsg ALSO fall through to the
// trailing forward (they did before this extraction — verified against HEAD);
// only consumed MouseMsg and KeyMsg early-return. Those boundary cases stay in
// Update; this method holds only the agent-lifecycle/subagent cases.
func (m tuiModel) handleAgentMsg(msg tea.Msg, cmds []tea.Cmd) (tuiModel, []tea.Cmd, bool) {
	switch msg := msg.(type) {
	case agent.ReasoningChunkMsg:
		m.reasoning.WriteString(msg.Text)
		m.refreshViewport()

	case agent.StreamChunkMsg:
		// First content delta after reasoning: collapse the thinking block to a
		// single committed iSys line and clear the live reasoning buffer.
		if m.reasoning.Len() > 0 && !m.reasoningDone {
			toks := m.reasoning.Len() / 4
			m.reasoning.Reset()
			m.reasoningDone = true
			m.reasoningExpanded = false
			m.addItem(iSys, dim2(sprint("· thought (~%d tokens)", toks)))
		}
		m.streaming.WriteString(msg.Text)
		m.refreshViewport()

	case agent.TokRateMsg:
		before := m.statusRows()
		m.tps = msg.Tps
		m = m.reflowIfStatusHeightChanged(before)

	case agent.ConfirmReqMsg:
		before := m.statusRows()
		// If the user has reverse-search open when a confirm arrives, abort it
		// so the search prompt doesn't persist into the non-idle state.
		if m.searchActive {
			m.searchExit(false)
		}
		m.state = stateConfirm
		m.pendConf = &msg
		m.flushStreaming()
		m.addItem(iSys, fmtConfirmBlock(msg.Headline, msg.Detail, msg.ReadAction))
		m = m.reflowIfStatusHeightChanged(before)

	case agent.ToolResultMsg:
		m.addItem(iSys, dim2("· "+msg.Name+"\n"+agent.Indent(agent.Truncate(msg.Result, 800))))

	case agent.AgentDoneMsg:
		before := m.statusRows()
		m.flushStreaming()
		// Edge case: turn ended during/after reasoning but before any content
		// (e.g. tool call only, cancellation). Show the collapsed thought if any.
		if m.reasoning.Len() > 0 {
			toks := m.reasoning.Len() / 4
			m.addItem(iSys, dim2(sprint("· thought (~%d tokens)", toks)))
			m.reasoning.Reset()
		}
		m.reasoningDone = false
		m.reasoningExpanded = false
		if msg.Warn != "" {
			// Tidy agent.Yellow warning (e.g. backend stream error) in place of a trace.
			m.addItem(iSys, agent.Yellow(msg.Warn))
		}
		if msg.Err != nil && !errors.Is(msg.Err, context.Canceled) {
			if errors.Is(msg.Err, proxy.ErrBackendStream) {
				m.addItem(iSys, agent.Yellow("⚠ backend stream error"))
			} else {
				m.addItem(iSys, styleErr("error: "+msg.Err.Error()))
			}
		}
		if errors.Is(msg.Err, context.Canceled) {
			m.addItem(iSys, dim2("[turn cancelled]"))
		}
		if msg.LearnNudge != "" {
			m.addItem(iSys, dim2(msg.LearnNudge))
		}
		// Chime on a successful finish, but only for turns long enough to have
		// drawn the user's attention away (skip quick replies and errors/cancels).
		if msg.Err == nil && !m.turnStart.IsZero() && time.Since(m.turnStart) > 3*time.Second {
			cmds = append(cmds, playFinishSound())
		}
		m.turnStart = time.Time{}
		m.tps = 0
		m.state = stateIdle
		m.dotPhase = 0 // return dot to static dim; tick self-terminates (no re-arm at idle)
		m.hadTurn = true
		m.cancel = nil
		m.cancelling = false
		m.clearArm() // a pending cancel arm must not fire into the now-idle state

		// Apply deferred /auto grants: a mid-turn OFF→ON toggle was deferred
		// (pendingAutoGrant) because granting mid-turn would auto-approve tools
		// the user hadn't seen. Now that the turn is truly idle (clean
		// AgentDoneMsg, no workflow continuation, no error), apply the grant.
		// This runs BEFORE flushing queued prompts so the first queued prompt's
		// turn runs under the new consent state. Persist at apply time, not at
		// queue time — a pending grant that was cancelled (second /auto mid-turn)
		// never reaches here because the flag was cleared.
		//
		// pendingDestructiveGrant can be set independently (auto already ON,
		// destructive deferred) — it must apply even without pendingAutoGrant.
		if msg.Err == nil && msg.Warn == "" && !msg.WorkflowWillContinue {
			if m.pendingAutoGrant {
				m.app.SetAutoApprove(true)
				m.app.SaveRepoState(func(s *agent.RepoState) { s.AutoApprove = true })
				m.pendingAutoGrant = false
				m.addItem(iSys, dim2("· auto: granted (pending from mid-turn)"))
			}
			if m.pendingDestructiveGrant {
				// Only meaningful when auto is ON (checked at mid-turn entry,
				// but re-check here in case state changed). AllowDestructive is
				// never persisted to repo-state — it's a session-only grant.
				if m.app.Consent().AutoApprove {
					m.app.SetAllowDestructive(true)
					m.addItem(iSys, dim2("· auto: destructive granted (pending from mid-turn)"))
				}
				m.pendingDestructiveGrant = false
			}
		}

		// Flush queued prompts: only when the turn ended cleanly (no error/cancel,
		// no warning, no workflow auto-continuation pending). The queue survives
		// across cancel, backend warnings, and workflow auto-continuation — it
		// flushes on the next true idle (clean AgentDoneMsg).
		if len(m.queuedPrompts) > 0 && msg.Err == nil && msg.Warn == "" && !msg.WorkflowWillContinue {
			next := m.queuedPrompts[0]
			m.queuedPrompts = m.queuedPrompts[1:]
			var flushCmds []tea.Cmd
			m, flushCmds = m.flushQueuedPrompt(next)
			cmds = append(cmds, flushCmds...)
		}
		m = m.reflowIfStatusHeightChanged(before)

	case agent.CompactedMsg:
		m.addItem(iSys, dim2("· compacted earlier turns"))

	case agent.SubagentStartMsg:
		// Which tab is the user currently viewing? 0 = main. Tracked by n so it
		// survives the prune/index shuffle below.
		focusN := 0
		if m.subCur >= 0 && m.subCur < len(m.subTabs) {
			focusN = m.subTabs[m.subCur].n
		}
		m.subSeq++
		tab := &subTab{
			n:          m.subSeq,
			task:       msg.Task,
			chatID:     msg.ChatID,
			backend:    msg.Backend,
			capability: msg.Capability,
			model:      msg.Model,
			toolNames:  msg.ToolNames,
			buf:        new(strings.Builder),
		}
		m.subTabs = append(m.subTabs, tab)
		// Never steal focus: the user stays on whatever view they are reading
		// (usually main). The new tab is reachable via the tab bar; with
		// parallel dispatch, auto-following would bounce the view N times.
		m.subTabs = pruneSubTabs(m.subTabs, focusN, maxSubTabs)
		m.subCur = tabIndexByN(m.subTabs, focusN)
		// When the first tab appears the tab bar steals one row from vpH.
		// Reflow so m.vp.Height is updated; otherwise the stale (too-tall)
		// viewport overflows View() by one row and Bubble Tea's cursor tracker
		// drifts, causing the bottom tab bar to render at the wrong screen row.
		if len(m.subTabs) == 1 {
			m = m.reflow()
		}

	case agent.SubagentActiveMsg:
		// Queued → running: the worker acquired a parallelism slot.
		for _, t := range m.subTabs {
			if t.chatID == msg.ChatID {
				t.active = true
				break
			}
		}

	case agent.SubagentChunkMsg:
		// Route by ChatID: with parallel dispatch, several tabs may stream at
		// once, so the chunk carries the identity of its producer.
		for _, t := range m.subTabs {
			if t.chatID == msg.ChatID {
				t.buf.WriteString(msg.Text)
				break
			}
		}

	case agent.SubagentFinishedMsg:
		// Display-only early completion: the worker just returned. Flip the tab
		// to a visually-done state immediately — spinner stops, status glyph,
		// cost, files count, summary preview — without waiting for the Phase C
		// barrier. SubagentDoneMsg below is the authoritative finalization; it
		// fills any remaining fields and must not regress a finished tab.
		for _, t := range m.subTabs {
			if t.chatID == msg.ChatID {
				t.finished = true
				t.finishedAt = msg.FinishedAt
				t.finStatus = msg.Status
				t.finCostUSD = msg.CostUSD
				t.finFilesN = len(msg.FilesChanged)
				t.finPreview = msg.SummaryPreview
				break
			}
		}

	case agent.SubagentDoneMsg:
		// Authoritative finalization (Phase C). Fill fields the early event
		// didn't carry (grounding, ctx size, hardMax, usedBackend) and mark the
		// tab fully done. No visual regression: if the tab was already finished
		// via SubagentFinishedMsg, it stays done — we only enrich it.
		found := false
		for _, t := range m.subTabs {
			if t.chatID == msg.ChatID {
				found = true
				t.done = true
				t.finished = true // done implies finished for rendering
				t.grounding = msg.Grounding
				t.ctxSize = msg.CtxSize
				t.hardMaxBytes = msg.HardMaxBytes
				t.usedBackend = msg.UsedBackend
				t.costUSD = msg.CostUSD
				t.filesChanged = msg.FilesChanged
				break
			}
		}
		// Arm a 30s auto-close timer for the done tab. Fire-and-validate: if
		// the tab was already pruned, closed, or is focused when the timer
		// fires, the handler is a no-op. One-shot — not re-armed.
		if found {
			chatID := msg.ChatID
			cmds = append(cmds, tea.Tick(subTabAutoCloseDelay, func(time.Time) tea.Msg {
				return subTabCloseMsg{ChatID: chatID}
			}))
		}

	case subTabCloseMsg:
		// Auto-close: remove the tab if it is done and not currently focused.
		// If focused, skip (one-shot, no re-arm — the tab will be cleaned up
		// by pruneSubTabs when new tabs arrive, or by manual close).
		focusN := 0
		if m.subCur >= 0 && m.subCur < len(m.subTabs) {
			focusN = m.subTabs[m.subCur].n
		}
		oldLen := len(m.subTabs)
		removed := false
		for i, t := range m.subTabs {
			if t.chatID == msg.ChatID && t.done && t.n != focusN {
				m.subTabs = append(m.subTabs[:i], m.subTabs[i+1:]...)
				removed = true
				break
			}
		}
		if removed {
			m.subCur = tabIndexByN(m.subTabs, focusN)
			// Symmetric with SubagentStartMsg's 0→1 reflow: when the last tab
			// disappears the tab bar row is reclaimed by the viewport.
			if oldLen > 0 && len(m.subTabs) == 0 {
				m = m.reflow()
			}
		}

	case agent.LearnTurnMsg:
		if m.state == stateIdle {
			if m.searchActive {
				m.searchExit(false)
			}
			const learnText = "learn this for next time"
			m.addItem(iUser, learnText)
			m.vp.GotoBottom()
			var pair []tea.Cmd
			m, pair = m.startTurn(func(ctx context.Context) tea.Cmd {
				return AdaptCmd(agent.RunTurn(m.app, ctx, learnText))
			})
			cmds = append(cmds, pair...)
		}

	case dotTickMsg:
		// Re-arm only while busy; the tick self-terminates when the model is idle.
		if m.state != stateIdle {
			m.dotPhase = (m.dotPhase + 1) % len(dotPulseShades)
			cmds = append(cmds, startDotTick())
		}

	case armTickMsg:
		// Clear the arm only if this tick belongs to the current arm (stale ticks
		// from a superseded arm are ignored) and the deadline has actually passed.
		if msg.seq == m.armSeq && !m.armUntil.IsZero() && !time.Now().Before(m.armUntil) {
			before := m.statusRows()
			m.clearArm()
			m = m.reflowIfStatusHeightChanged(before)
		}

	case agent.SysNoteMsg:
		m.addItem(iSys, dim2(msg.Text))

	case agent.BackendCtxLimitMsg:
		// Apply the newly-resolved context limit in the event loop — safe because
		// /backend is only available in stateIdle (no concurrent RunTurn goroutine).
		// Reset the pressure warning so it re-evaluates against the new window.
		before := m.statusRows()
		m.app.CtxLimit = msg.Limit
		m.app.CtxPressureWarned = false
		if msg.Note != "" {
			m.addItem(iSys, dim2(msg.Note))
		}
		m = m.reflowIfStatusHeightChanged(before)

	case agent.ModelListUpdatedMsg:
		// Refresh the model list after an endpoint switch so /model and
		// /submodel autocomplete reflects the new endpoint's models. Applied
		// in the event loop — same safety as BackendCtxLimitMsg above.
		m.app.ModelList = msg.Models

	case copiedMsg:
		before := m.statusRows()
		m.flash = sprint("copied %d chars ✓", msg.n)
		m = m.reflowIfStatusHeightChanged(before)

	case clipboardImageMsg:
		// A clipboard read completed (triggered by paste-detection or
		// /image clipboard). On success, queue the image for the next
		// message AND insert its placeholder chip into the text input at
		// the cursor — the compact "[image: clipboard:png · 1.8 MB]" stands
		// in for the pasted image, keeping the input clean. At send time
		// the chip is stripped from the outgoing text (the image travels
		// via PendingImages); deleting the chip un-attaches the image.
		//
		// On failure with a cut stash pending, the detection was likely a
		// false positive (the "garbage" was real text with no image on the
		// clipboard) — restore the cut text so nothing is lost.
		if msg.Err != "" {
			if m.pasteCutStash != "" {
				m.ta.InsertString(m.pasteCutStash)
				m.pasteCutStash = ""
				m.addItem(iSys, dim2("· no image on clipboard — restored the pasted text"))
			} else {
				m.addItem(iSys, styleErr("clipboard: "+msg.Err))
			}
		} else {
			m.pasteCutStash = "" // real image confirmed; the cut garbage stays gone
			m.app.PendingImages = append(m.app.PendingImages, msg.Img)
			chip := msg.Img.Placeholder()
			*m.imageChips = append(*m.imageChips, chip)
			m.ta.InsertString(chip + " ")
		}
		m.refreshViewport()

	case agent.NewConvMsg:
		before := m.statusRows()
		// Clear viewport items so the new conversation starts fresh.
		*m.items = (*m.items)[:0]
		m.streaming.Reset()
		m.reasoning.Reset()
		m.reasoningDone = false
		m.reasoningExpanded = false
		// Clear queued prompts — they belong to the old conversation.
		m.queuedPrompts = nil
		// Clear deferred /auto grants — a pending grant belongs to the old
		// conversation's turn cycle; it must not carry into the new one.
		m.pendingAutoGrant = false
		m.pendingDestructiveGrant = false
		// Chip tracking belongs to the old conversation's draft; the pending
		// images themselves are owned by the App and survive /new on purpose
		// (same as /image <path> queuing before a fresh chat).
		*m.imageChips = (*m.imageChips)[:0]
		if msg.RebuildConv && len(m.app.Conv) > 0 {
			*m.items = convItemsFrom(m.app.Conv)
		}
		m.prefixDirty = true
		m.refreshViewport()
		if msg.Note != "" {
			m.addItem(iSys, dim2(msg.Note))
		}
		m = m.reflowIfStatusHeightChanged(before)

	case agent.HandoffMsg:
		// Handoff: the Cmd closure generated a summary, stored it in memory,
		// and saved the old session. Now we rotate to a new conversation and
		// start the continuation turn — all on the event loop to avoid races.
		//
		// Guard against the race where a turn started while summarization was
		// in flight (e.g., a queued prompt flushed after AgentDoneMsg). Same
		// guard as WFStartTurnMsg: only rotate from idle.
		if m.state != stateIdle {
			// A turn is active — abort the handoff rather than rotating
			// out from under a live turn. The old session is already saved
			// and the summary is in memory; the user can /resume the old
			// session or retry /handoff when idle.
			if msg.Err != nil {
				m.addItem(iSys, dim2("⚠ handoff failed: "+msg.Err.Error()))
			} else {
				m.addItem(iSys, dim2("⚠ handoff aborted — a turn is in progress; retry when idle"))
			}
			break
		}

		before := m.statusRows()

		// If the handoff failed, show the error and do NOT rotate.
		if msg.Err != nil {
			m.addItem(iSys, dim2("⚠ handoff failed: "+msg.Err.Error()))
			m = m.reflowIfStatusHeightChanged(before)
			break
		}

		// Close reverse-search if active (same as WFStartTurnMsg).
		if m.searchActive {
			m.searchExit(false)
		}

		// Revoke all session consent before rotating — the new session starts
		// clean. RevokeAuto clears AutoApprove + AllowDestructive (pair
		// invariant); AllowReads is cleared separately. Persist AutoApprove=
		// false so repo-state doesn't restore it.
		m.app.RevokeAuto()
		m.app.SetAllowReads(false)
		m.app.SaveRepoState(func(s *agent.RepoState) { s.AutoApprove = false })

		// Clear pending images — they belong to the old session.
		m.app.PendingImages = nil

		// Clear workflow — handoff starts a fresh workflow-free session.
		// The summary captures workflow state as text; the new session does
		// not inherit the in-memory workflow engine.
		m.app.Workflow = nil

		// Rotate to a new conversation (use the preallocated chat ID).
		m.app.NewConversation(msg.NewChatID)

		// Clear viewport items (same as NewConvMsg).
		*m.items = (*m.items)[:0]
		m.streaming.Reset()
		m.reasoning.Reset()
		m.reasoningDone = false
		m.reasoningExpanded = false
		m.queuedPrompts = nil
		m.pendingAutoGrant = false
		m.pendingDestructiveGrant = false
		*m.imageChips = (*m.imageChips)[:0]
		m.prefixDirty = true
		m.refreshViewport()

		// Show the handoff note (old → new session IDs).
		if msg.Note != "" {
			m.addItem(iSys, dim2(msg.Note))
		}
		m.addItem(iSys, dim2(sprint("· new session: %s — /resume %s to return to the old one",
			agent.ShortID(msg.NewChatID), agent.ShortID(msg.OldChatID))))
		m.vp.GotoBottom()

		// Start the continuation turn (same pattern as WFStartTurnMsg).
		var pair []tea.Cmd
		m, pair = m.startTurn(func(ctx context.Context) tea.Cmd {
			return AdaptCmd(agent.RunTurn(m.app, ctx, msg.ContinuationPrompt))
		})
		cmds = append(cmds, pair...)
		m = m.reflowIfStatusHeightChanged(before)

	case agent.OpenResumePickerMsg:
		prevH := m.resumePickerHeight()
		m = m.openResumePicker(msg)
		if m.resumePickerHeight() != prevH {
			m = m.reflow()
		}

	case agent.MCPReconnectedMsg:
		// Apply the rebuilt tool list from the Update loop — not from the Cmd
		// goroutine — so there is no race with the agent goroutine reading app.Tools.
		m.app.Tools = msg.Tools
		m.addItem(iSys, dim2(sprint("· reconnected %q (%d tools)", msg.Name, len(msg.Tools))))

	case agent.WFFinalReviewMsg:
		// Closing oracle check, triggered by /plan approve after an every-step
		// critical pause on the last step.
		if m.state != stateIdle || m.app.Workflow == nil {
			break
		}
		if m.searchActive {
			m.searchExit(false)
		}
		m.addItem(iSys, dim2("· running final oracle review"))
		m.vp.GotoBottom()
		{
			var pair []tea.Cmd
			m, pair = m.startTurn(func(ctx context.Context) tea.Cmd {
				return AdaptCmd(agent.RunFinalReview(m.app, ctx))
			})
			cmds = append(cmds, pair...)
		}

	case agent.WFStartTurnMsg:
		// Workflow auto-turn: show a system note and kick off the next agent turn.
		// This message arrives after agent.AgentDoneMsg (same goroutine, same channel),
		// so state is already idle. Guard against the abort-mid-chain race: if the
		// user typed /plan abort between agent.AgentDoneMsg being consumed and this message
		// arriving, Workflow is nil and we must not start a stray turn.
		if m.state != stateIdle || m.app.Workflow == nil {
			break
		}
		if m.searchActive {
			m.searchExit(false)
		}
		m.addItem(iSys, dim2("· "+msg.Note))
		m.vp.GotoBottom()
		var pair []tea.Cmd
		m, pair = m.startTurn(func(ctx context.Context) tea.Cmd {
			return AdaptCmd(agent.RunTurn(m.app, ctx, msg.UserText))
		})
		cmds = append(cmds, pair...)

	default:
		// Not an agent-lifecycle message — let Update's trailing forward handle it.
		return m, cmds, false
	}
	return m, cmds, true
}
