package wiring

// projection.go: agent-message → domain-event projection (card #148 chunk 7b3).
//
// The agent loop emits various message types through app.EventSink (subagent
// start/chunk/done, async-job start/chunk/done, side-question chunk/done,
// tool start/result, compacted, sys notes, etc.). The adapter projects these
// to domain events on the session-scoped emitter so a TUI consuming the event
// stream sees the same signals it would have received through the old
// globalProg.Send path.
//
// This is the 7b3 projection surface. It is called from the session-scoped
// EventSink installed on the hostTurn. Each case maps an agent message type
// to a domain event (durable or ephemeral per D24's five-class split).
//
// Messages that have NO domain-event counterpart (e.g. SysNoteMsg — client-
// local display, not a durable fact) are dropped or mapped to ephemeral
// notifications. The mapping table is defined in the plan (D24) and the
// command/message mapping matrix (7b3 m1).
//
// ID mapping: agent messages carry proxy-native IDs (ChatID for subagents,
// ToolCallID from the proxy response). Domain events carry prefixed IDs
// (sub_*, tcl_*, op_*). The projection mints domain IDs from the proxy IDs
// using a deterministic prefix-stripping scheme: the proxy ID body (UUID)
// is reused with the domain prefix. This is stable: the same proxy ID always
// maps to the same domain ID within a session, so a chunk→done sequence
// references the same SubagentID/OpID.

import (
	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionhost"
)

// subagentIDFromChatID mints a domain SubagentID from the agent's ChatID.
// The ChatID is a UUID; the domain ID reuses the body with the sub_ prefix.
// If the ChatID is empty or already prefixed, it is used as-is (validation
// will reject it if malformed — the emit is best-effort).
func subagentIDFromChatID(chatID string) event.SubagentID {
	if chatID == "" {
		return ""
	}
	// If already has the prefix, use as-is.
	if len(chatID) > 4 && chatID[:4] == "sub_" {
		return event.SubagentID(chatID)
	}
	return event.SubagentID("sub_" + chatID)
}

// opIDFromString mints a domain OpID from an agent operation ID string.
// Agent OpIDs are either "op-N" (async registry) or "job-bgN" (detached shells).
// The domain OpID reuses the body with the op_ prefix.
func opIDFromString(opID string) event.OpID {
	if opID == "" {
		return ""
	}
	// If already has the prefix, use as-is.
	if len(opID) > 3 && opID[:3] == "op_" {
		return event.OpID(opID)
	}
	return event.OpID("op_" + opID)
}

// toolCallIDFromString mints a domain ToolCallID from the agent's ToolCallID.
// The agent's ToolCallID comes from the proxy response (typically "call_<n>").
// The domain ID reuses the body with the tcl_ prefix.
func toolCallIDFromString(tcID string) event.ToolCallID {
	if tcID == "" {
		return ""
	}
	if len(tcID) > 4 && tcID[:4] == "tcl_" {
		return event.ToolCallID(tcID)
	}
	return event.ToolCallID("tcl_" + tcID)
}

// projectAgentEvent projects an agent message (received through app.EventSink)
// to domain events on the session-scoped emitter. It is the single projection
// point: every agent message type that the TUI previously received via
// globalProg.Send is mapped here to an event.Event on the session-scoped
// emitter.
//
// turnID is the current turn's ID; it stamps turn-scoped tool events
// (ToolCallStarted.TurnID is a required field). Empty when the projection
// runs in a detached context (no live turn) — tool events never fire there
// (the EventSink closure routes them to the turn emitter only).
//
// Messages with no domain-event counterpart are silently dropped (they are
// either client-local display signals or snapshot fields per D24).
//
// The projection is best-effort: a closed session emitter returns
// ErrEmitterClosed from Emit, which is silently dropped (the session is
// closing; the event is lost by design). Ephemeral Notify calls drop silently.
func projectAgentEvent(emit sessionhost.SessionEmitter, turnID event.TurnID, msg any) {
	if emit == nil {
		return
	}
	switch m := msg.(type) {

	// ---- Tool events (turn-scoped durable via the TURN emitter) ----
	// NOTE: the session emitter REJECTS turn-scoped kinds (host.go's
	// turnScopedKinds — a tool event must never land after its turn's
	// TurnCompleted). The hostTurn's EventSink closure therefore routes
	// ToolStartMsg/ToolResultMsg to the LIVE TURN's emitter, and this
	// projection handles them there. When projectAgentEvent is called with
	// the session emitter (detached contexts), these cases never fire — the
	// closure filters them out first.

	case agent.ToolStartMsg:
		// Durable (turn-scoped): tool_call_started.
		_ = emit.Emit(event.KindToolCallStarted, event.ToolCallStarted{
			TurnID:     turnID,
			ToolCallID: toolCallIDFromString(m.ToolCallID),
			Name:       m.Name,
			ArgDigest:  m.Command,
		})

	case agent.ToolResultMsg:
		// Durable (turn-scoped): tool_call_completed. The full result already
		// streams via app.Out (ProgWriter → MessageDelta); the event carries
		// the truncated Result as the preview.
		_ = emit.Emit(event.KindToolCallCompleted, event.ToolCallCompleted{
			ToolCallID:    toolCallIDFromString(m.ToolCallID),
			Name:          m.Name,
			Status:        "ok",
			ResultPreview: m.Result,
		})

	// ---- Subagent events ----

	case agent.SubagentStartMsg:
		// Durable: subagent_spawned.
		subID := subagentIDFromChatID(m.ChatID)
		capability := m.Capability
		if capability == "" {
			capability = "discovery"
		}
		_ = emit.Emit(event.KindSubagentSpawned, event.SubagentSpawned{
			SubagentID: subID,
			Task:       m.Task,
			Capability: capability,
			Backend:    m.Backend,
			Model:      m.Model,
			ToolNames:  append([]string(nil), m.ToolNames...),
		})

	case agent.SubagentActiveMsg:
		// Ephemeral: the worker acquired a parallelism slot (queued → running).
		// No dedicated domain event — map to subagent_progress ephemeral
		// with a status marker so the TUI can update the tab's visual state.
		subID := subagentIDFromChatID(m.ChatID)
		emit.Notify(event.KindSubagentProgress, event.SubagentProgress{
			SubagentID: subID,
			Text:       "[active]",
		})

	case agent.SubagentChunkMsg:
		// Ephemeral: subagent_progress.
		subID := subagentIDFromChatID(m.ChatID)
		emit.Notify(event.KindSubagentProgress, event.SubagentProgress{
			SubagentID: subID,
			Text:       m.Text,
		})

	case agent.SubagentFinishedMsg:
		// Display-only early completion. No durable event — the authoritative
		// completion is SubagentDoneMsg → subagent_completed. The TUI can
		// update the tab's visual state from this ephemeral signal: the
		// structured Finished fields carry what the old SubagentFinishedMsg
		// handler set on the tab (status, cost, files count, preview).
		subID := subagentIDFromChatID(m.ChatID)
		emit.Notify(event.KindSubagentProgress, event.SubagentProgress{
			SubagentID:      subID,
			Text:            m.SummaryPreview,
			Finished:        true,
			FinishedStatus:  m.Status,
			FinishedCostUSD: m.CostUSD,
			FinishedFilesN:  len(m.FilesChanged),
		})

	case agent.SubagentDoneMsg:
		// Durable: subagent_completed.
		subID := subagentIDFromChatID(m.ChatID)
		status := "ok"
		if m.Err != "" {
			status = "failed"
		}
		groundingLabels := make([]string, 0, len(m.Grounding))
		for _, g := range m.Grounding {
			groundingLabels = append(groundingLabels, g.Label)
		}
		_ = emit.Emit(event.KindSubagentCompleted, event.SubagentCompleted{
			SubagentID:     subID,
			Status:         status,
			SummaryPreview: "",
			Err:            m.Err,
			CostUSD:        m.CostUSD,
			FilesChanged:   append([]string(nil), m.FilesChanged...),
			Grounding:      groundingLabels,
			CtxSize:        m.CtxSize,
			HardMaxBytes:   m.HardMaxBytes,
			UsedBackend:    m.UsedBackend,
		})

	// ---- Async job events ----

	case agent.AsyncJobStartMsg:
		// Durable: async_job_started.
		opID := opIDFromString(m.OpID)
		_ = emit.Emit(event.KindAsyncJobStarted, event.AsyncJobStarted{
			OpID:  opID,
			Label: m.Label,
		})

	case agent.AsyncJobChunkMsg:
		// Ephemeral: async_job_progress.
		opID := opIDFromString(m.OpID)
		emit.Notify(event.KindAsyncJobProgress, event.AsyncJobProgress{
			OpID: opID,
			Text: m.Text,
		})

	case agent.AsyncJobDoneMsg:
		// Durable: async_job_completed.
		opID := opIDFromString(m.OpID)
		status := "ok"
		if m.Err != "" {
			status = "error"
		}
		_ = emit.Emit(event.KindAsyncJobCompleted, event.AsyncJobCompleted{
			OpID:           opID,
			Status:         status,
			SummaryPreview: m.Result,
			Err:            m.Err,
		})

	// ---- Side question events ----

	case agent.SideQuestionChunkMsg:
		// Ephemeral: side_question_progress.
		opID := opIDFromString(string(m.ID))
		emit.Notify(event.KindSideQuestionProgress, event.SideQuestionProgress{
			OpID: opID,
			Text: m.Text,
		})

	case agent.SideQuestionDoneMsg:
		// Durable: side_question_completed.
		opID := opIDFromString(string(m.ID))
		status := "ok"
		answerPreview := ""
		if m.Err != nil {
			status = "error"
			answerPreview = m.Err.Error()
		}
		_ = emit.Emit(event.KindSideQuestionCompleted, event.SideQuestionCompleted{
			OpID:          opID,
			Status:        status,
			AnswerPreview: answerPreview,
		})

	// ---- Other agent messages (client-local, no domain event) ----

	case agent.SysNoteMsg:
		// Ephemeral session note (7b3 m4): status/progress lines that occur
		// during a turn — workflow progress (wfProgNote), handoff progress
		// ("saving session…"), policy auto-approve notices. Display-only; the
		// TUI renders them as dimmed iSys items. Command-driven notes
		// (HandleTUICommand returning SysNoteMsg) never reach the EventSink —
		// DispatchCommand translates them to CommandResult.Notice — so this
		// case fires only for in-turn notes.
		emit.Notify(event.KindSessionNote, event.SessionNote{Text: m.Text})

	case agent.CompactedMsg:
		// Durable: conversation_compacted. The TurnID is not available from
		// the agent message — the host knows the current turn and will emit
		// its own conversation_compacted if needed. For now, emit with a
		// zero TurnID; the host's own compaction event (if any) is the
		// authoritative one.
		// NOTE: KindConversationCompacted is NOT in turnScopedKinds or
		// hostReservedKinds, so the session emitter accepts it.
		// However, the host already emits this in finishTurn if compaction
		// occurred during the turn. This projection is a fallback for the
		// case where the agent compacts outside the host's knowledge.
		// Dropped for now — the host's own emission is authoritative.
		_ = m

	case agent.BackendCtxLimitMsg:
		// Snapshot field (D24: query-state, not event). The TUI reads it from
		// ClientSnapshot.ContextLimit. No event.
		_ = m

	case agent.ModelListUpdatedMsg:
		// Snapshot field (D24: query-state, not event). The TUI reads it from
		// ClientSnapshot.ModelList. No event.
		_ = m

	case agent.MCPReconnectedMsg:
		// Snapshot field (D24: query-state, not event). The TUI reads the
		// rebuilt tool list from ClientSnapshot.Tools. No event.
		_ = m

	case agent.TokRateMsg:
		// Ephemeral: tok_rate. Already handled by the session-scoped OnTokRate
		// callback (installed permanently on the hostTurn). This case should
		// not fire — TokRate goes through OnTokRate, not EventSink.
		_ = m

	case agent.AgentDoneMsg:
		// Host-reserved: KindTurnCompleted is emitted by the host's
		// finishTurn, not by the projection. The LearnNudge field, if
		// non-empty, is projected as an ephemeral learn_nudge event.
		if m.LearnNudge != "" {
			emit.Notify(event.KindLearnNudge, event.LearnNudge{
				Text: m.LearnNudge,
			})
		}

	case agent.TurnSuspendedSignal:
		// Ephemeral: the turn has paused on pending async work. The TUI
		// transitions to stateWaiting and shows "waiting" instead of
		// "streaming". Enables input-while-waiting (cancel + send).
		emit.Notify(event.KindTurnSuspended, event.TurnSuspended{})

	case agent.TurnResumedSignal:
		// Ephemeral: the suspended turn resumed after an async completion.
		// The TUI transitions back from stateWaiting to stateStreaming.
		emit.Notify(event.KindTurnResumed, event.TurnResumed{})

	default:
		// Unknown message type — drop. New agent message types should be added
		// to this switch with their domain-event mapping.
		_ = msg
	}
}
