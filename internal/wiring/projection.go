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
	"github.com/treeol/wakil/internal/core/id"
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
// Messages with no domain-event counterpart are silently dropped (they are
// either client-local display signals or snapshot fields per D24).
//
// The projection is best-effort: a closed session emitter returns
// ErrEmitterClosed from Emit, which is logged and ignored (the session is
// closing; the event is lost by design). Ephemeral Notify calls drop silently.
func projectAgentEvent(emit sessionhost.SessionEmitter, msg any) {
	if emit == nil {
		return
	}
	switch m := msg.(type) {

	// ---- Tool events (turn-scoped durable via session emitter) ----
	// NOTE: tool events are turn-scoped (turnScopedKinds), but the agent
	// sends them through EventSink which is wired to the session emitter.
	// The session emitter REJECTS turn-scoped kinds. This is by design:
	// the wiring adapter handles tool events through the turn-scoped Emit
	// (installed via app.Out and the confirmer), NOT through EventSink.
	// ToolStartMsg and ToolResultMsg arriving through EventSink are a
	// legacy path that should not fire in the wiring configuration.
	// They are silently dropped here to avoid emitter errors.

	case agent.ToolStartMsg:
		// Dropped: tool events go through the turn-scoped emitter path
		// (hostturn.go installs app.Out for streaming; the confirmer
		// handles approvals). The agent sends ToolStartMsg through
		// sendEvent as a display signal for the old TUI path; the wiring
		// adapter does not need it as a domain event because tool-call
		// events are emitted by the host's own tool execution.
		_ = m

	case agent.ToolResultMsg:
		// Dropped: same as ToolStartMsg — the turn-scoped path handles
		// tool completion. The result text goes through app.Out (ProgWriter
		// → MessageDelta). The domain ToolCallCompleted is emitted by the
		// host's tool execution layer, not projected from this message.
		_ = m

	// ---- Subagent events ----

	case agent.SubagentStartMsg:
		// Durable: subagent_spawned.
		subID := subagentIDFromChatID(m.ChatID)
		capability := m.Capability
		if capability == "" {
			capability = "discovery"
		}
		emit.Emit(event.KindSubagentSpawned, event.SubagentSpawned{
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
		emit.Emit(event.KindSubagentCompleted, event.SubagentCompleted{
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
		emit.Emit(event.KindAsyncJobStarted, event.AsyncJobStarted{
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
		emit.Emit(event.KindAsyncJobCompleted, event.AsyncJobCompleted{
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
		if m.Err != nil {
			status = "error"
		}
		emit.Emit(event.KindSideQuestionCompleted, event.SideQuestionCompleted{
			OpID:          opID,
			Status:        status,
			AnswerPreview: "",
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

	default:
		// Unknown message type — drop. New agent message types should be added
		// to this switch with their domain-event mapping.
		_ = msg
	}
}

// Ensure id package is used (for future ID generation needs).
var _ = id.NewSubagentID
