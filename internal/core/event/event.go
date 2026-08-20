// Package event defines the domain event model for wakild (card #148).
//
// This is the transport-free core's only outward-facing vocabulary: the typed,
// sequenced event stream that every client (TUI, Web-UI, CLI/CI) consumes.
// It lives under internal/core so that internal/core never imports api/gen or
// internal/server (the hard rule in docs/design/wakild-foundation.md §2.1) —
// the proto in api/proto is written against these domain types in P2, not the
// other way around.
//
// Two classes of event (plan decision D2):
//
//   - Durable: replayable, cursor-addressable, strictly sequenced per session.
//     A client reconnecting at seq N+1, or a replay from 0, consumes only these
//     and reconstructs the client-visible projection.
//   - Ephemeral: live-only stream notifications (per-token deltas, reasoning,
//     subagent progress). Never part of durable sequence semantics; may be
//     dropped under backpressure.
//
// Events are data-only (plan decision D6): no channels, no callbacks, no reply
// channels. The one current violation (agent.ConfirmReqMsg.RespCh) is handled
// by the approval shim (D5) — this package models approval as a request/result
// pair, never as a synchronous block.
//
// Lifecycle (two validation stages):
//
//   - Draft: a producer constructs an Event with Seq == 0 and calls
//     ValidateDraft. Durable drafts pass; the appender then assigns a sequence
//     at durable append and hands back a committed Event.
//   - Committed: a durable event with Seq >= 1, or an ephemeral event with
//     Seq == 0 (ephemeral events carry no sequence and are never persisted).
//     ValidateCommitted (also exposed as Validate) checks it can be stored or
//     published.
//
// The split exists because Validate() with a single contract cannot serve both:
// a durable draft must have Seq == 0 (pre-append), while a committed durable
// event must have Seq > 0 (post-append). See ValidateDraft vs ValidateCommitted.
package event

import (
	"fmt"
	"time"
)

// Seq is the per-session monotonic event sequence number. It is the ordering
// truth — timestamps are metadata, never ordering (foundation doc §3.4).
//
// 0 means "unassigned" (a draft). The first committed durable event is seq 1.
// Assignment is owned by the sequencer, not by producers: the value is stamped
// at durable append (in P0 by the in-memory event log, in P1 by the store), so
// concurrent producers cannot interleave. Durable committed sequences are
// contiguous per session (gaps are not legal for successful appends).
type Seq uint64

// Class is the durability class of an event.
type Class uint8

const (
	// ClassDurable events are replayable and cursor-addressable.
	ClassDurable Class = iota
	// ClassEphemeral events are live-only stream notifications.
	ClassEphemeral
)

// Kind enumerates every event type in the domain vocabulary. The set mirrors
// the foundation doc's Event contract (§3.4) plus KindMessageCommitted, which
// is the durable counterpart of the ephemeral per-token MessageDelta (D2): a
// replay reconstructs client-visible content from committed blocks, not deltas.
type Kind string

const (
	KindSessionCreated     Kind = "session_created"
	KindTurnStarted        Kind = "turn_started"
	KindMessageDelta       Kind = "message_delta"      // ephemeral: streaming text
	KindMessageCommitted   Kind = "message_committed"  // durable: coalesced message block
	KindReasoningDelta     Kind = "reasoning_delta"    // ephemeral: reasoning_content
	KindToolCallStarted    Kind = "tool_call_started"
	KindToolCallCompleted  Kind = "tool_call_completed"
	KindApprovalRequested  Kind = "approval_requested"
	KindApprovalResolved   Kind = "approval_resolved"
	KindSubagentSpawned    Kind = "subagent_spawned"
	KindSubagentProgress   Kind = "subagent_progress" // ephemeral: live progress
	KindSubagentCompleted  Kind = "subagent_completed"
	KindMemoryProposed     Kind = "memory_proposed"
	KindGuardTriggered     Kind = "guard_triggered"
	KindContextWarning     Kind = "context_warning"
	KindTurnCompleted      Kind = "turn_completed"
	KindSessionError       Kind = "session_error"
	KindSessionClosed      Kind = "session_closed"
	// 7b2 additions (D24/D28/D29): session-scoped + detached-operation events.
	KindUserMessageCommitted   Kind = "user_message_committed"   // durable: user input replay truth
	KindConversationCompacted  Kind = "conversation_compacted"    // durable: compaction boundary
	KindWorkflowTurnStarted    Kind = "workflow_turn_started"    // durable: workflow step submit
	KindWorkflowFinalReview    Kind = "workflow_final_review"    // durable: final-review gate
	KindAsyncJobStarted       Kind = "async_job_started"         // durable: detached job start
	KindAsyncJobCompleted      Kind = "async_job_completed"       // durable: detached job done
	KindSideQuestionCompleted Kind = "side_question_completed"   // durable: side-question done
	KindTokRate                Kind = "tok_rate"                  // ephemeral: token rate display
	KindAsyncJobProgress       Kind = "async_job_progress"        // ephemeral: detached job progress
	KindSideQuestionProgress   Kind = "side_question_progress"    // ephemeral: side-question progress
	KindLearnNudge             Kind = "learn_nudge"               // ephemeral: learn suggestion
	KindSessionNote            Kind = "session_note"              // ephemeral: progress/status note (7b3 m4)
)

// Class returns the durability class of k.
func (k Kind) Class() Class {
	switch k {
	case KindMessageDelta, KindReasoningDelta, KindSubagentProgress,
		KindTokRate, KindAsyncJobProgress, KindSideQuestionProgress,
		KindLearnNudge, KindSessionNote:
		return ClassEphemeral
	default:
		return ClassDurable
	}
}

func (k Kind) String() string { return string(k) }

// Event is the single envelope every domain event travels in (D6): one envelope
// with a typed payload, not one struct per kind duplicating envelope fields.
type Event struct {
	TenantID  TenantID
	SessionID SessionID
	Seq       Seq
	Ts        time.Time
	Kind      Kind
	Payload   any
}

// validateCommon checks the parts of an event that are the same whether it is a
// draft or committed: known kind, exact payload type, envelope IDs, nested
// payload IDs, and timestamp. It does not check Seq (draft and committed differ).
func (e Event) validateCommon() error {
	if e.Kind == "" {
		return fmt.Errorf("event: kind is empty")
	}
	if _, ok := payloadTypes[e.Kind]; !ok {
		return fmt.Errorf("event: unknown kind %q", e.Kind)
	}
	if err := e.TenantID.Validate(); err != nil {
		return fmt.Errorf("event %s: %w", e.Kind, err)
	}
	if err := e.SessionID.Validate(); err != nil {
		return fmt.Errorf("event %s: %w", e.Kind, err)
	}
	if err := validatePayloadType(e.Kind, e.Payload); err != nil {
		return err
	}
	if v, ok := e.Payload.(interface{ Validate() error }); ok {
		if err := v.Validate(); err != nil {
			return fmt.Errorf("event %s: %w", e.Kind, err)
		}
	}
	if e.Ts.IsZero() {
		return fmt.Errorf("event %s: ts is zero", e.Kind)
	}
	return nil
}

// ValidateDraft checks that e is a valid pre-append draft: a producer may
// submit it to the appender, which assigns a sequence and returns a committed
// event. A durable draft has Seq == 0; an ephemeral event always has Seq == 0.
func (e Event) ValidateDraft() error {
	if err := e.validateCommon(); err != nil {
		return err
	}
	if e.Seq != 0 {
		return fmt.Errorf("event %s: draft must have seq 0, got %d", e.Kind, e.Seq)
	}
	return nil
}

// ValidateCommitted checks that e is valid to store or publish. A durable event
// must have Seq >= 1; an ephemeral event must have Seq == 0 (it carries no
// durable sequence and clients must never treat it as a cursor).
func (e Event) ValidateCommitted() error {
	if err := e.validateCommon(); err != nil {
		return err
	}
	switch e.Kind.Class() {
	case ClassDurable:
		if e.Seq == 0 {
			return fmt.Errorf("event %s: committed durable event has unassigned seq", e.Kind)
		}
	case ClassEphemeral:
		if e.Seq != 0 {
			return fmt.Errorf("event %s: ephemeral event must have seq 0, got %d", e.Kind, e.Seq)
		}
	}
	return nil
}

// Validate is ValidateCommitted: it validates an event that has been assigned a
// sequence (or an ephemeral event). Use ValidateDraft for pre-append drafts.
func (e Event) Validate() error { return e.ValidateCommitted() }
