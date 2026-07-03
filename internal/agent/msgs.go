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

// SubagentChunkMsg delivers a line of subagent output.
type SubagentChunkMsg struct{ Text string }

// SubagentDoneMsg marks the subagent as finished.
type SubagentDoneMsg struct {
	Grounding    []proxy.GroundingEntry
	CtxSize      int
	HardMaxBytes int
	UsedBackend  string // X-Ilm-Backend-Used from the subagent's last Stream call
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
