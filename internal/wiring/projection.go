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

import (
	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionhost"
)

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
// closing; the event is lost by design).
func projectAgentEvent(emit sessionhost.SessionEmitter, msg any) {
	if emit == nil {
		return
	}
	switch m := msg.(type) {
	// ---- Subagent events ----
	case agent.SubagentStartMsg:
		// Durable: subagent_spawned. The TUI's tab routing uses ChatID; the
		// domain uses SubagentID. The adapter maps ChatID → SubagentID (a
		// stable translation, not a fresh ID per callback — D24).
		// TODO(7b3): generate/mint a SubagentID from the ChatID.
		_ = m // projection lands with the full SubagentID mapping in 7b3 m2

	case agent.SubagentActiveMsg:
		// Ephemeral: the worker acquired a parallelism slot (queued → running).
		// No domain event for this yet — it is a display-only state transition.
		// The TUI's tab rendering keys on this. Map to subagent_progress
		// ephemeral or drop (D24: client-local signal).
		_ = m

	case agent.SubagentChunkMsg:
		// Ephemeral: subagent_progress.
		emit.Notify(event.KindSubagentProgress, event.SubagentProgress{
			// SubagentID mapping TODO(7b3 m2)
			Text: m.Text,
		})

	case agent.SubagentFinishedMsg:
		// Display-only early completion. No durable event (the authoritative
		// completion is SubagentDoneMsg → subagent_completed).
		_ = m

	case agent.SubagentDoneMsg:
		// Durable: subagent_completed.
		_ = m // projection with full fields lands in 7b3 m2

	// ---- Async job events ----
	case agent.AsyncJobStartMsg:
		// Durable: async_job_started.
		_ = m // projection with OpID mapping lands in 7b3 m2

	case agent.AsyncJobChunkMsg:
		// Ephemeral: async_job_progress.
		_ = m

	case agent.AsyncJobDoneMsg:
		// Durable: async_job_completed.
		_ = m

	// ---- Side question events ----
	case agent.SideQuestionChunkMsg:
		// Ephemeral: side_question_progress.
		_ = m

	case agent.SideQuestionDoneMsg:
		// Durable: side_question_completed.
		_ = m

	// ---- Tool events ----
	case agent.ToolStartMsg:
		// Durable: tool_call_started. The TUI's status line shows the running
		// tool's command; tool_call_started carries ArgDigest (hash, not raw).
		// D24: raw command text must NOT go into the durable event.
		_ = m

	case agent.ToolResultMsg:
		// Durable: tool_call_completed.
		_ = m

	// ---- Other agent messages (client-local, no domain event) ----
	case agent.SysNoteMsg:
		// Client-local display signal (D24). No domain event — the TUI renders
		// it as a dimmed iSys item. In the event-stream model, these become
		// CommandResult.Notice strings, not events.
		_ = m

	case agent.CompactedMsg:
		// Durable: conversation_compacted.
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

	default:
		// Unknown message type — drop. New agent message types should be added
		// to this switch with their domain-event mapping.
		_ = m
	}
}
