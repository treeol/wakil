package event

import (
	"fmt"
)

// Payloads for every event kind. These are plain data — no channels, no
// callbacks, no reply channels (D6). They carry only what a client needs to
// render or a replay needs to reconstruct the client-visible projection.
//
// Required fields are validated in each payload's Validate() method and
// invoked from Event.validateCommon. Optional/display fields (previews, detail
// text) are intentionally not validated for non-emptiness — only the fields a
// store or wire contract depends on are.

// SessionCreated is the payload for KindSessionCreated.
type SessionCreated struct {
	WorkspaceID WorkspaceID
	AgentName   string // human-facing agent identity (revision pinning arrives with the store)
	CreatedBy   UserID
}

func (p SessionCreated) Validate() error {
	if err := p.WorkspaceID.Validate(); err != nil {
		return err
	}
	if err := p.CreatedBy.Validate(); err != nil {
		return err
	}
	return nil
}

// TurnStarted is the payload for KindTurnStarted.
type TurnStarted struct {
	TurnID TurnID
	// TurnIndex is the 1-based per-session turn ordinal.
	TurnIndex uint64
}

func (p TurnStarted) Validate() error {
	if err := p.TurnID.Validate(); err != nil {
		return err
	}
	if p.TurnIndex == 0 {
		return fmt.Errorf("TurnStarted: turn_index must be >= 1")
	}
	return nil
}

// MessageDelta is the ephemeral streaming-text payload (KindMessageDelta).
// Not durable: a replay consumes MessageCommitted blocks instead.
//
// It is presentation streaming and is NOT guaranteed to concatenate to
// MessageCommitted.Text: the stream may include tool/status rendering lines
// that are not part of the authoritative assistant response. Consumers must
// treat it as display-only; the durable MessageCommitted is the replay truth.
type MessageDelta struct {
	Text string
}

// MessageCommitted is the durable coalesced-message payload (KindMessageCommitted).
// It is the replay counterpart of the live MessageDelta stream (D2). TurnID
// attributes the block for replay ordering when subagents interleave.
type MessageCommitted struct {
	TurnID TurnID
	Text   string
}

func (p MessageCommitted) Validate() error {
	if err := p.TurnID.Validate(); err != nil {
		return err
	}
	return nil
}

// ReasoningDelta is the ephemeral reasoning_content payload (KindReasoningDelta).
type ReasoningDelta struct {
	Text string
}

// ToolCallStarted is the payload for KindToolCallStarted.
type ToolCallStarted struct {
	TurnID     TurnID
	ToolCallID ToolCallID
	Name       string
	// ArgDigest is a hash, not the raw payload (loop-signature; §4.3 tool_calls).
	ArgDigest string
}

func (p ToolCallStarted) Validate() error {
	if err := p.TurnID.Validate(); err != nil {
		return err
	}
	if err := p.ToolCallID.Validate(); err != nil {
		return err
	}
	if p.Name == "" {
		return fmt.Errorf("ToolCallStarted: name is empty")
	}
	return nil
}

// ToolCallCompleted is the payload for KindToolCallCompleted.
type ToolCallCompleted struct {
	ToolCallID ToolCallID
	Name       string
	// Status is one of "ok" | "error" | "declined".
	Status string
	// ResultPreview is a bounded display summary; the full result is a blob.
	ResultPreview string
	DurationMs    int64
}

func (p ToolCallCompleted) Validate() error {
	if err := p.ToolCallID.Validate(); err != nil {
		return err
	}
	if p.Name == "" {
		return fmt.Errorf("ToolCallCompleted: name is empty")
	}
	switch p.Status {
	case "ok", "error", "declined":
	default:
		return fmt.Errorf("ToolCallCompleted: invalid status %q", p.Status)
	}
	if p.DurationMs < 0 {
		return fmt.Errorf("ToolCallCompleted: negative duration_ms")
	}
	return nil
}

// ApprovalRequested is the payload for KindApprovalRequested. It is a
// notification in P0 (the sync Confirmer still drives resolution — shim D5);
// in P2 it becomes the authoritative request a client answers via
// RespondToApproval.
type ApprovalRequested struct {
	ApprovalID ApprovalID
	ToolName   string
	Headline   string
	Detail     string
	ReadAction bool
}

func (p ApprovalRequested) Validate() error {
	if err := p.ApprovalID.Validate(); err != nil {
		return err
	}
	if p.ToolName == "" {
		return fmt.Errorf("ApprovalRequested: tool_name is empty")
	}
	return nil
}

// ApprovalResolved is the payload for KindApprovalResolved.
type ApprovalResolved struct {
	ApprovalID ApprovalID
	// Outcome is one of "approved" | "declined" | "allowed_reads".
	Outcome string
	// Reason carries the resolver's human-readable rationale on a decline (e.g.
	// "destructive command declined: rm -rf …", "blocked by policy: …"). Optional:
	// empty for approved/allowed_reads resolutions. It is data (D6) — a decline
	// reason must not be reconstructed by the consumer from ToolName, because the
	// real reason is dynamic (command text, policy rule names, flag state).
	Reason string
	// Resolver is who resolved the approval. Identity from day one (D4): the
	// P0 shim records the submitter principal (TurnInput.UserID), so it is
	// populated even in embedded mode. Empty only if no principal context
	// exists (a programming error in the shim).
	Resolver UserID
}

func (p ApprovalResolved) Validate() error {
	if err := p.ApprovalID.Validate(); err != nil {
		return err
	}
	switch p.Outcome {
	case "approved", "declined", "allowed_reads":
	default:
		return fmt.Errorf("ApprovalResolved: invalid outcome %q", p.Outcome)
	}
	// Resolver is optional in P0 (shim); enforce non-empty from P2 when a real
	// principal answers over the wire.
	return nil
}

// SubagentSpawned is the payload for KindSubagentSpawned.
type SubagentSpawned struct {
	SubagentID SubagentID
	Task       string
	Capability string // "discovery" | "edit" | "tools"
}

func (p SubagentSpawned) Validate() error {
	if err := p.SubagentID.Validate(); err != nil {
		return err
	}
	switch p.Capability {
	case "discovery", "edit", "tools":
	default:
		return fmt.Errorf("SubagentSpawned: invalid capability %q", p.Capability)
	}
	return nil
}

// SubagentProgress is the ephemeral live-progress payload (KindSubagentProgress).
type SubagentProgress struct {
	SubagentID SubagentID
	Text       string
}

// SubagentCompleted is the payload for KindSubagentCompleted.
type SubagentCompleted struct {
	SubagentID SubagentID
	// Status is one of "ok" | "failed" | "incomplete" | "declined".
	Status string
	// SummaryPreview is a short rendering for the sidebar; the full summary is
	// delivered out-of-band.
	SummaryPreview string
}

func (p SubagentCompleted) Validate() error {
	if err := p.SubagentID.Validate(); err != nil {
		return err
	}
	switch p.Status {
	case "ok", "failed", "incomplete", "declined":
	default:
		return fmt.Errorf("SubagentCompleted: invalid status %q", p.Status)
	}
	return nil
}

// MemoryProposed is the payload for KindMemoryProposed (propose-then-promote).
type MemoryProposed struct {
	Key    string
	Kind   string // "note" | "decision" | "summary" | ...
	Writer string
}

// GuardTriggered is the payload for KindGuardTriggered (sidecar guard).
type GuardTriggered struct {
	Guard   string
	Message string
}

// ContextWarning is the payload for KindContextWarning (sidecar predictive).
type ContextWarning struct {
	Message string
}

// TurnCompleted is the payload for KindTurnCompleted.
type TurnCompleted struct {
	TurnID TurnID
	// Outcome is one of "complete" | "empty" | "stream_error" | "cancelled".
	Outcome string
	// Warn carries a non-fatal warning the user should see (D28 stream-warn
	// parity): a retry-exhausted stream error that the user should be told
	// about ("backend unreachable, /resume to continue") but that is not an
	// error outcome. Empty when there is no warning. Added in 7b2.
	Warn string
	// WorkflowWillContinue is true when the workflow engine detected a
	// transition and the TUI should auto-submit the next workflow step
	// (D28). Replaces AgentDoneMsg.WorkflowWillContinue. The TUI's
	// queue-flush/auto-grant gate keys off this field. Added in 7b2.
	WorkflowWillContinue bool
}

func (p TurnCompleted) Validate() error {
	if err := p.TurnID.Validate(); err != nil {
		return err
	}
	switch p.Outcome {
	case "complete", "empty", "stream_error", "cancelled":
	default:
		return fmt.Errorf("TurnCompleted: invalid outcome %q", p.Outcome)
	}
	return nil
}

// SessionError is the payload for KindSessionError.
type SessionError struct {
	Reason string // e.g. "daemon_restart", "backend_failure"
	Err    string
}

func (p SessionError) Validate() error {
	if p.Reason == "" {
		return fmt.Errorf("SessionError: reason is empty")
	}
	return nil
}

// SessionClosed is the payload for KindSessionClosed.
type SessionClosed struct {
	Reason string
}

func (p SessionClosed) Validate() error {
	if p.Reason == "" {
		return fmt.Errorf("SessionClosed: reason is empty")
	}
	return nil
}

// ---- 7b2 event payloads (D24/D28/D29) ----

// UserMessageCommitted is the durable user-side transcript event (D24 replay
// truth). The host emits it on SubmitInput, carrying the TurnID and the
// submitted Text. A replay reconstructs the full transcript from
// UserMessageCommitted (user) + MessageCommitted (assistant) +
// ConversationCompacted (compaction boundary).
type UserMessageCommitted struct {
	TurnID TurnID
	Text   string
}

func (p UserMessageCommitted) Validate() error {
	if err := p.TurnID.Validate(); err != nil {
		return err
	}
	if p.Text == "" {
		return fmt.Errorf("UserMessageCommitted: text is empty")
	}
	return nil
}

// ConversationCompacted is the durable compaction-boundary event (D24). It marks
// where the transcript was compacted; a replay sees this as a boundary and
// does not try to reconstruct the pre-compaction messages.
type ConversationCompacted struct {
	// TurnID is the turn during which compaction occurred.
	TurnID TurnID
}

func (p ConversationCompacted) Validate() error {
	return p.TurnID.Validate()
}

// WorkflowTurnStarted is the durable workflow-step-submit event (D28). The
// adapter emits it when HandleWorkflowTransition fires a transition; the TUI
// sees it and submits the next workflow step's input.
type WorkflowTurnStarted struct {
	TurnID TurnID
	// UserText is the next step's input text (the workflow prompt).
	UserText string
}

func (p WorkflowTurnStarted) Validate() error {
	return p.TurnID.Validate()
}

// WorkflowFinalReview is the durable final-review-gate event (D28). The adapter
// emits it when the workflow engine reaches the final-review phase; the TUI
// submits a final-review input.
type WorkflowFinalReview struct {
	TurnID TurnID
}

func (p WorkflowFinalReview) Validate() error {
	return p.TurnID.Validate()
}

// AsyncJobStarted is the durable detached-async-job start event (D29). It
// carries the operation ID and a bounded description. Detached jobs outlive the
// turn and emit through the session-scoped emitter.
type AsyncJobStarted struct {
	OpID  OpID
	Label string
}

func (p AsyncJobStarted) Validate() error {
	return p.OpID.Validate()
}

// AsyncJobCompleted is the durable detached-async-job completion event (D29).
type AsyncJobCompleted struct {
	OpID   OpID
	Status string // "ok" | "error" | "cancelled"
	// SummaryPreview is a bounded display summary.
	SummaryPreview string
}

func (p AsyncJobCompleted) Validate() error {
	if err := p.OpID.Validate(); err != nil {
		return err
	}
	switch p.Status {
	case "ok", "error", "cancelled":
	default:
		return fmt.Errorf("AsyncJobCompleted: invalid status %q", p.Status)
	}
	return nil
}

// SideQuestionCompleted is the durable side-question completion event (D29).
type SideQuestionCompleted struct {
	OpID   OpID
	Status string // "ok" | "error" | "cancelled"
	// AnswerPreview is a bounded display summary.
	AnswerPreview string
}

func (p SideQuestionCompleted) Validate() error {
	if err := p.OpID.Validate(); err != nil {
		return err
	}
	switch p.Status {
	case "ok", "error", "cancelled":
	default:
		return fmt.Errorf("SideQuestionCompleted: invalid status %q", p.Status)
	}
	return nil
}

// TokRate is the ephemeral token-rate display payload (D24). Display-only;
// not part of the durable stream.
type TokRate struct {
	Rate float64
}

// AsyncJobProgress is the ephemeral detached-job progress payload (D29).
type AsyncJobProgress struct {
	OpID  OpID
	Text  string
}

// SideQuestionProgress is the ephemeral side-question progress payload (D29).
type SideQuestionProgress struct {
	OpID OpID
	Text string
}

// LearnNudge is the ephemeral learn-suggestion payload (D24). It is NOT folded
// into turn_completed (host-reserved kind — illegal); it is an ephemeral
// advisory the TUI renders as a transient notification.
type LearnNudge struct {
	Text string
}

// SessionNote is the ephemeral progress/status note payload (7b3 m4). It
// replaces agent.SysNoteMsg on the wiring path: workflow progress lines
// (wfProgNote), handoff progress ("saving session…", "generating summary…"),
// policy auto-approve notices, and similar human-facing status text that
// occurs DURING a turn or between commands. It is display-only — never folded
// into the durable stream (replay reconstructs state from durable events, not
// from notes) and carries no identity beyond the session.
type SessionNote struct {
	Text string
}
