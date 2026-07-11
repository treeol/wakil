package agent

import "github.com/treeol/wakil/internal/proxy"

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
	Task    string
	ChatID  string
	Backend string // resolved backend for this dispatch (empty = proxy default)
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
