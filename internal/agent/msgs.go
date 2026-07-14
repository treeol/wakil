package agent

import (
	"time"

	"github.com/treeol/wakil/internal/proxy"
)

// StreamChunkMsg is an SSE content delta posted to the TUI event loop.
type StreamChunkMsg struct{ Text string }

// ReasoningChunkMsg is an SSE reasoning_content delta.
type ReasoningChunkMsg struct{ Text string }

// ConfirmReqMsg pauses the agent goroutine and asks the user to approve/decline.
type ConfirmReqMsg struct {
	ToolName   string
	Headline   string
	Detail     string
	ReadAction bool
	RespCh     chan ConfirmChoice
}

// ToolResultMsg is shown after a tool executes.
type ToolResultMsg struct {
	Name   string
	Result string
}

// AgentDoneMsg signals that the current turn finished.
type AgentDoneMsg struct {
	Err        error
	LearnNudge string
	Warn       string
}

// LearnTurnMsg tells the Update loop to start a /learn turn.
type LearnTurnMsg struct{}

// TokRateMsg carries the live token/sec decode estimate.
type TokRateMsg struct{ Tps float64 }

// CompactedMsg tells the TUI that the transcript was compacted.
type CompactedMsg struct{}

// SubagentStartMsg opens a new subagent tab in the main pane.
type SubagentStartMsg struct {
	Task       string
	ChatID     string
	Backend    string   // resolved backend for this dispatch (empty = proxy default)
	Capability string   // "discovery" (default), "edit", or "tools" — drives the sidebar tool list
	Model      string   // child's resolved model (view.model), shown in the sidebar
	ToolNames  []string // tool names for the sidebar; nil for discovery/edit (hardcoded in TUI), populated for "tools"
}

// SubagentActiveMsg marks the moment a dispatched subagent actually starts
// running (its worker acquired a slot under max_parallel_subagents). Between
// SubagentStartMsg and this event the subagent is queued, not running — the
// TUI renders queued tabs with a dimmed dot and active ones with a pulsing dot.
type SubagentActiveMsg struct{ ChatID string }

// SubagentChunkMsg delivers a line of subagent output. ChatID identifies which
// subagent produced it, so the TUI can route concurrent streams to their tabs.
type SubagentChunkMsg struct {
	ChatID string
	Text   string
}

// SubagentFinishedMsg is the display-only early completion event. It is emitted
// from the worker goroutine the moment a child returns — before the result
// enters the results slice and before Phase C's cost fold — so the TUI can
// surface per-child completion at actual completion time rather than waiting
// for the slowest sibling to finish (the SubagentDoneMsg barrier).
//
// Display data only: CostUSD is the child's own total (known worker-side from
// the child's fresh CostTracker snapshot), FilesChanged is the mechanical
// record, SummaryPreview is a short rendering for the sidebar. No parent-state
// mutation happens on this event — the cost fold, grounding, ctx size, and all
// authoritative finalization stay in SubagentDoneMsg (Phase C). The TUI treats
// SubagentDoneMsg as idempotent finalization of a tab that may already be
// visually done via this earlier event.
//
// Delivery is goroutine-safe (Program.Send, same as SubagentChunkMsg and
// SubagentActiveMsg — see cmd/wakil/main.go's EventSink wiring).
type SubagentFinishedMsg struct {
	ChatID         string
	Status         string  // "ok" | "failed" | "incomplete" | "declined"
	CostUSD        float64 // child's own priced total; display-only — the authoritative fold is in SubagentDoneMsg
	FilesChanged   []string
	SummaryPreview string    // first line / short rendering of the summary
	FinishedAt     time.Time // when the child returned (for timestamped display)
}

// SubagentDoneMsg marks the subagent identified by ChatID as finished.
type SubagentDoneMsg struct {
	ChatID       string
	Grounding    []proxy.GroundingEntry
	CtxSize      int
	HardMaxBytes int
	UsedBackend  string // X-Ilm-Backend-Used from the subagent's last Stream call

	// CostUSD is the child's total priced cost (sum of its own CostTracker's
	// priced rows, at the child's own model/backend rate), already folded into
	// the parent's CostTracker by the time this message is sent — see
	// foldSubagentCost at the dispatch_subagent join point (app.go, and Phase C
	// of runParallelSubagentBlock for the parallel path). 0 when nothing was
	// priced or no usage was recorded.
	CostUSD float64

	// FilesChanged is the mechanically-recorded list of canonical workspace
	// paths touched by edit-category tool calls that succeeded. Populated only
	// for edit-tier children; nil for discovery-tier. move_file records both
	// src and dst. Failed tool calls are not recorded. This is ground truth —
	// the model's self-reported files_changed in SubagentSummary is a claim.
	FilesChanged []string
}

// SysNoteMsg delivers a status line into the viewport.
type SysNoteMsg struct{ Text string }

// NewConvMsg signals that the conversation was reset.
// RebuildConv=true tells the TUI to repopulate the viewport from app.Conv
// (used by /resume, which loads a saved transcript rather than starting fresh).
type NewConvMsg struct {
	Note        string
	RebuildConv bool
}

// OpenResumePickerMsg tells the TUI to open the interactive session picker.
// Sessions are pre-loaded (scoped per Scope) off the event loop so a large
// session store never blocks a keystroke; Hidden is how many sessions were
// filtered out by the scope, for the picker's "N hidden — press a" hint.
type OpenResumePickerMsg struct {
	Sessions []Session
	Scope    SessionScope
	Hidden   int
}

// MCPReconnectedMsg is sent after /mcp reconnect completes.
type MCPReconnectedMsg struct {
	Name  string
	Tools []proxy.Tool
}

// WFFinalReviewMsg triggers the closing oracle check.
type WFFinalReviewMsg struct{}

// WFStartTurnMsg tells the Update loop to begin a workflow-driven agent turn.
type WFStartTurnMsg struct {
	Note     string
	UserText string
}

// BackendCtxLimitMsg delivers a newly-resolved context limit to the TUI event
// loop after a /backend switch. The TUI applies it to app.CtxLimit from within
// Update() — not from the probe goroutine — so there is no race with a
// concurrently running agent turn.
type BackendCtxLimitMsg struct {
	Limit ContextLimit
	Note  string // formatted limits line for the viewport; empty on probe failure
}

// ModelListUpdatedMsg carries a freshly-fetched model list for the current
// endpoint. Delivered asynchronously from fetchModelListCmd so the HTTP call
// doesn't block the TUI event loop. Applied to app.ModelList in Update().
type ModelListUpdatedMsg struct {
	Models []string
}

// ProgWriter is an io.Writer that sends StreamChunkMsgs into the event sink.
type ProgWriter struct{ send func(StreamChunkMsg) }

func (w *ProgWriter) Write(p []byte) (int, error) {
	if len(p) > 0 {
		w.send(StreamChunkMsg{Text: string(p)})
	}
	return len(p), nil
}

// NewProgWriter creates a ProgWriter whose chunks are dispatched via send.
func NewProgWriter(send func(StreamChunkMsg)) *ProgWriter {
	return &ProgWriter{send: send}
}
