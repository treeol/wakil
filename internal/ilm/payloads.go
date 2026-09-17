package ilm

import (
	"encoding/json"
)

// Payload types matching the contract's event mapping table.
// Each event type has a payload struct that is marshalled to JSON and
// sent as Event.Payload.

// SessionStartPayload is the payload for session_start events.
type SessionStartPayload struct {
	Source    string   `json:"source"` // "live"
	Workspace string   `json:"workspace"`
	Model     string   `json:"model"`
	System    []string `json:"system,omitempty"`
}

// UserTurnPayload is the payload for user_turn events.
type UserTurnPayload struct {
	Text string `json:"text"` // full user text
}

// AssistantTurnPayload is the payload for assistant_turn events.
type AssistantTurnPayload struct {
	Text      string `json:"text"` // full, may be empty
	Reasoning string `json:"reasoning,omitempty"`
	ModelID   string `json:"model_id,omitempty"`
}

// ToolCallPayload is the payload for tool_call events.
type ToolCallPayload struct {
	Tool   string          `json:"tool"`
	Args   json.RawMessage `json:"args"` // full args object
	CallID string          `json:"call_id"`
	TurnID string          `json:"turn_id,omitempty"`
}

// ToolResultPayload is the payload for tool_result events.
type ToolResultPayload struct {
	Tool             string `json:"tool"`
	CallID           string `json:"call_id"`
	OK               *bool  `json:"ok"`     // bool or null
	Output           string `json:"output"` // bounded to max_output_bytes
	Truncated        bool   `json:"truncated"`
	FullHash         string `json:"full_hash"` // sha256 of untruncated post-redaction output
	FullSize         int    `json:"full_size"`
	SourceIncomplete bool   `json:"source_incomplete,omitempty"`
}

// MemoryOpPayload is the payload for memory_op events.
type MemoryOpPayload struct {
	Op         string `json:"op"` // memory_put, memory_get, etc.
	Key        string `json:"key"`
	Content    string `json:"content,omitempty"`
	Provenance string `json:"provenance,omitempty"`
}

// MashuraCallPayload is the payload for mashura_call events.
type MashuraCallPayload struct {
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args"`
}

// ErrorPayload is the payload for error events.
type ErrorPayload struct {
	Text string `json:"text"`
}

// SessionEndPayload is the payload for session_end events.
type SessionEndPayload struct {
	Outcome string `json:"outcome,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// AssistEventPayload is the payload for assist_event events (C3). Every assist
// response and Wakil's decision are emitted with this payload: the proposed
// action (if any), the server's gate probability and provenance, and Wakil's
// decision (took, ignored, not_applicable, auto) plus the main model's
// subsequent action when Wakil did not take the proposal.
type AssistEventPayload struct {
	// Decision is Wakil's disposition of the proposal.
	//   "took"            — Wakil executed the proposed action.
	//   "ignored"         — Wakil called the main model instead (user declined or assist_auto=false and user said n).
	//   "not_applicable"  — the proposal failed Wakil's allowlist.
	//   "auto"            — assist_auto=true and Wakil executed directly.
	//   "abstain"         — the server abstained (no action proposed).
	//   "error"           — transport error (timeout, non-200, malformed response).
	//   "rejected"        — the server returned 400 (seq not a valid decision point).
	Decision string `json:"decision"`

	// Tool is the proposed tool name (empty when abstain/error).
	Tool string `json:"tool,omitempty"`
	// Args is the proposed tool arguments (empty when abstain/error).
	Args json.RawMessage `json:"args,omitempty"`
	// GateProbability is the server's gate probability (0 when abstain/error).
	GateProbability float64 `json:"gate_probability"`
	// Abstain is true when the server abstained.
	Abstain bool `json:"abstain,omitempty"`
	// Reason is the abstain/error reason (when present).
	Reason string `json:"reason,omitempty"`

	// CandidateProvenance is the server's provenance for the proposal.
	CandidateProvenance json.RawMessage `json:"candidate_provenance,omitempty"`

	// LatencyMS is the /v1/assist round-trip latency.
	LatencyMS float64 `json:"latency_ms,omitempty"`

	// MainModelAction is the main model's subsequent action when Wakil did not
	// take the proposal (decision=ignored or not_applicable). It records what
	// the main model actually did, for agreement measurement.
	MainModelAction string `json:"main_model_action,omitempty"`
}
