package protoconv

import (
	"reflect"
	"testing"
	"time"

	v1alpha1 "github.com/treeol/wakil/api/gen/wakil/v1alpha1"
	"github.com/treeol/wakil/internal/core/event"
)

// TestEventRoundTrip exercises EventToProto → EventFromProto for a representative
// subset of event kinds. The conversion is a 32-kind switch in both directions;
// this test catches drift between the ToProto and FromProto switches (e.g. a new
// kind added to ToProto but missing from FromProto, or a field renamed on one side
// but not the other). It does NOT exhaustively test every kind — that's the job
// of TestAllKindsRoundTrip below, which iterates the payloadTypes map.
func TestEventRoundTrip(t *testing.T) {
	ts := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		ev   event.Event
	}{
		{
			name: "SessionCreated",
			ev: event.Event{
				Kind:      event.KindSessionCreated,
				TenantID:  "tnt_1",
				SessionID: "ses_1",
				Seq:       1,
				Ts:        ts,
				Payload: event.SessionCreated{
					WorkspaceID: "wsp_abc",
					AgentName:   "main",
					CreatedBy:   "usr_1",
				},
			},
		},
		{
			name: "TurnStarted",
			ev: event.Event{
				Kind: event.KindTurnStarted,
				Seq:  2,
				Ts:   ts,
				Payload: event.TurnStarted{
					TurnID:    "turn_1",
					TurnIndex: 1,
				},
			},
		},
		{
			name: "MessageCommitted",
			ev: event.Event{
				Kind: event.KindMessageCommitted,
				Seq:  3,
				Ts:   ts,
				Payload: event.MessageCommitted{
					TurnID: "turn_1",
					Text:   "hello world",
				},
			},
		},
		{
			name: "ToolCallCompleted",
			ev: event.Event{
				Kind: event.KindToolCallCompleted,
				Seq:  4,
				Ts:   ts,
				Payload: event.ToolCallCompleted{
					ToolCallID:    "tc_1",
					Name:          "read_file",
					Status:        "ok",
					ResultPreview: "package main...",
					DurationMs:    42,
				},
			},
		},
		{
			name: "SubagentCompleted",
			ev: event.Event{
				Kind: event.KindSubagentCompleted,
				Seq:  5,
				Ts:   ts,
				Payload: event.SubagentCompleted{
					SubagentID:     "sub_1",
					Status:         "ok",
					SummaryPreview: "found it",
					Err:            "",
					CostUSD:        0.5,
					FilesChanged:   []string{"a.go", "b.go"},
					Grounding:      []string{"ref1"},
					CtxSize:        4096,
					HardMaxBytes:   160000,
					UsedBackend:    "anthropic",
				},
			},
		},
		{
			name: "TurnCompleted",
			ev: event.Event{
				Kind: event.KindTurnCompleted,
				Seq:  6,
				Ts:   ts,
				Payload: event.TurnCompleted{
					TurnID:               "turn_1",
					Outcome:              "complete",
					Warn:                 "",
					WorkflowWillContinue: true,
				},
			},
		},
		{
			name: "WorkflowOutcome",
			ev: event.Event{
				Kind: event.KindWorkflowOutcome,
				Seq:  7,
				Ts:   ts,
				Payload: event.WorkflowOutcome{
					TurnID:  "turn_1",
					Outcome: "gaps",
					Reason:  "3 steps incomplete",
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pb, err := EventToProto(tc.ev)
			if err != nil {
				t.Fatalf("EventToProto: %v", err)
			}
			got, err := EventFromProto(pb)
			if err != nil {
				t.Fatalf("EventFromProto: %v", err)
			}
			if got.Kind != tc.ev.Kind {
				t.Errorf("Kind: got %q, want %q", got.Kind, tc.ev.Kind)
			}
			if got.TenantID != tc.ev.TenantID {
				t.Errorf("TenantID: got %q, want %q", got.TenantID, tc.ev.TenantID)
			}
			if got.SessionID != tc.ev.SessionID {
				t.Errorf("SessionID: got %q, want %q", got.SessionID, tc.ev.SessionID)
			}
			if got.Seq != tc.ev.Seq {
				t.Errorf("Seq: got %d, want %d", got.Seq, tc.ev.Seq)
			}
			if !got.Ts.Equal(tc.ev.Ts) {
				t.Errorf("Ts: got %v, want %v", got.Ts, tc.ev.Ts)
			}
			// Payload round-trip: compare via DeepEqual after normalizing nil slices
			// to empty slices (proto round-trip may produce nil vs []string{}).
			if !payloadsEqual(t, got.Payload, tc.ev.Payload) {
				t.Errorf("Payload mismatch:\n  got:  %#v\n  want: %#v", got.Payload, tc.ev.Payload)
			}
		})
	}
}

// TestAllKindsRoundTrip iterates every event kind and verifies the proto
// conversion works in both directions. This catches any kind that was added to
// the event package but not wired into the protoconv switches, or a mismatch
// between the To and From direction. Uses a zero-value payload for each kind —
// the point is conversion parity, not field-level accuracy (that's covered by
// TestEventRoundTrip for representative kinds).
//
// The kind list must stay in sync with event.payloadTypes. The event package's
// own exhaustive test (event_test.go) catches missing payloadTypes entries;
// this test catches missing protoconv switch cases.
func TestAllKindsRoundTrip(t *testing.T) {
	allKinds := []event.Kind{
		event.KindSessionCreated,
		event.KindTurnStarted,
		event.KindMessageDelta,
		event.KindMessageCommitted,
		event.KindReasoningDelta,
		event.KindToolCallStarted,
		event.KindToolCallCompleted,
		event.KindApprovalRequested,
		event.KindApprovalResolved,
		event.KindSubagentSpawned,
		event.KindSubagentProgress,
		event.KindSubagentCompleted,
		event.KindMemoryProposed,
		event.KindGuardTriggered,
		event.KindContextWarning,
		event.KindTurnCompleted,
		event.KindSessionError,
		event.KindSessionClosed,
		event.KindUserMessageCommitted,
		event.KindConversationCompacted,
		event.KindWorkflowTurnStarted,
		event.KindWorkflowFinalReview,
		event.KindAsyncJobStarted,
		event.KindAsyncJobCompleted,
		event.KindSideQuestionCompleted,
		event.KindTokRate,
		event.KindAsyncJobProgress,
		event.KindSideQuestionProgress,
		event.KindLearnNudge,
		event.KindSessionNote,
		event.KindWorkflowOutcome,
		event.KindWorkflowWarning,
	}

	// Map each kind to a factory that produces a non-zero payload for it.
	// Zero-value payloads may not survive round-trip for types with int32
	// narrowing (0 → 0 is fine) but we want to catch missing switch cases,
	// not field-level accuracy.
	payloads := map[event.Kind]any{
		event.KindSessionCreated:       event.SessionCreated{WorkspaceID: "wsp_1", CreatedBy: "usr_1"},
		event.KindTurnStarted:           event.TurnStarted{TurnID: "t1", TurnIndex: 1},
		event.KindMessageDelta:          event.MessageDelta{Text: "hi"},
		event.KindMessageCommitted:      event.MessageCommitted{TurnID: "t1", Text: "hi"},
		event.KindReasoningDelta:        event.ReasoningDelta{Text: "thinking"},
		event.KindToolCallStarted:       event.ToolCallStarted{TurnID: "t1", ToolCallID: "tc1", Name: "read_file"},
		event.KindToolCallCompleted:     event.ToolCallCompleted{ToolCallID: "tc1", Name: "read_file", Status: "ok"},
		event.KindApprovalRequested:     event.ApprovalRequested{ApprovalID: "ap1", ToolName: "run_shell"},
		event.KindApprovalResolved:      event.ApprovalResolved{ApprovalID: "ap1", Outcome: "approved"},
		event.KindSubagentSpawned:       event.SubagentSpawned{SubagentID: "sub1", Capability: "discovery"},
		event.KindSubagentProgress:      event.SubagentProgress{SubagentID: "sub1", Text: "working"},
		event.KindSubagentCompleted:     event.SubagentCompleted{SubagentID: "sub1", Status: "ok"},
		event.KindMemoryProposed:        event.MemoryProposed{Key: "test", Kind: "note", Writer: "main"},
		event.KindGuardTriggered:        event.GuardTriggered{Guard: "g1", Message: "blocked"},
		event.KindContextWarning:        event.ContextWarning{Message: "ctx full"},
		event.KindTurnCompleted:         event.TurnCompleted{TurnID: "t1", Outcome: "complete"},
		event.KindSessionError:          event.SessionError{Reason: "backend_failure"},
		event.KindSessionClosed:         event.SessionClosed{Reason: "done"},
		event.KindUserMessageCommitted:  event.UserMessageCommitted{TurnID: "t1", Text: "go"},
		event.KindConversationCompacted: event.ConversationCompacted{TurnID: "t1"},
		event.KindWorkflowTurnStarted:   event.WorkflowTurnStarted{TurnID: "t1", UserText: "step 1"},
		event.KindWorkflowFinalReview:   event.WorkflowFinalReview{TurnID: "t1"},
		event.KindAsyncJobStarted:        event.AsyncJobStarted{OpID: "op1", Label: "test"},
		event.KindAsyncJobCompleted:     event.AsyncJobCompleted{OpID: "op1", Status: "ok"},
		event.KindSideQuestionCompleted: event.SideQuestionCompleted{OpID: "op1", Status: "ok"},
		event.KindTokRate:               event.TokRate{Rate: 42.5},
		event.KindAsyncJobProgress:       event.AsyncJobProgress{OpID: "op1", Text: "50%"},
		event.KindSideQuestionProgress:  event.SideQuestionProgress{OpID: "op1", Text: "thinking"},
		event.KindLearnNudge:            event.LearnNudge{Text: "save this?"},
		event.KindSessionNote:           event.SessionNote{Text: "progress"},
		event.KindWorkflowOutcome:       event.WorkflowOutcome{TurnID: "t1", Outcome: "gaps"},
		event.KindWorkflowWarning:       event.WorkflowWarning{Message: "oracle skipped"},
	}

	if len(allKinds) != len(payloads) {
		t.Fatalf("kind list (%d) and payload map (%d) size mismatch — missing payload factory",
			len(allKinds), len(payloads))
	}

	for _, kind := range allKinds {
		t.Run(string(kind), func(t *testing.T) {
			p, ok := payloads[kind]
			if !ok {
				t.Fatalf("no payload factory for kind %q", kind)
			}
			pb, err := EventToProto(event.Event{Kind: kind, Payload: p})
			if err != nil {
				t.Fatalf("EventToProto(%s): %v", kind, err)
			}

			got, err := EventFromProto(pb)
			if err != nil {
				t.Fatalf("EventFromProto(%s): %v", kind, err)
			}
			if got.Kind != kind {
				t.Errorf("Kind: got %q, want %q", got.Kind, kind)
			}
			if got.Payload == nil {
				t.Errorf("Payload is nil after round-trip")
			}
		})
	}
}

// TestEventFromProtoNilTimestamp verifies that a proto Event with a nil Ts
// converts to a zero time.Time (not a panic).
func TestEventFromProtoNilTimestamp(t *testing.T) {
	pb := &v1alpha1.Event{
		Kind:    string(event.KindSessionNote),
		Payload: &v1alpha1.Event_SessionNote{SessionNote: &v1alpha1.SessionNotePayload{Text: "hi"}},
	}
	got, err := EventFromProto(pb)
	if err != nil {
		t.Fatalf("EventFromProto: %v", err)
	}
	if !got.Ts.IsZero() {
		t.Errorf("Ts: expected zero, got %v", got.Ts)
	}
}

// TestPayloadToProtoUnknownKind verifies that an unknown kind returns an error,
// not a panic or nil.
func TestPayloadToProtoUnknownKind(t *testing.T) {
	_, err := PayloadToProto(event.Kind("bogus"), nil)
	if err == nil {
		t.Fatal("expected error for unknown kind, got nil")
	}
}

// TestPayloadFromProtoNil verifies that a nil payload returns an error.
func TestPayloadFromProtoNil(t *testing.T) {
	_, err := PayloadFromProto(event.KindSessionCreated, nil)
	if err == nil {
		t.Fatal("expected error for nil payload, got nil")
	}
}

// TestPayloadFromProtoMismatchedKind verifies that a payload wrapper from an
// unknown type (not in the switch) returns an error. Note: PayloadFromProto
// switches on the wrapper TYPE, not the kind parameter — so passing a known
// wrapper with a mismatched kind does NOT error (the kind is only used for
// the error message). This test passes a bare struct that is not a recognized
// wrapper type.
func TestPayloadFromProtoMismatchedKind(t *testing.T) {
	// A raw string is not a recognized wrapper type.
	_, err := PayloadFromProto(event.KindSessionCreated, "not a wrapper")
	if err == nil {
		t.Fatal("expected error for unknown payload type, got nil")
	}
}

// TestSessionRoundTrip verifies SessionToProto → SessionFromProto preserves all
// fields including zero-time handling (zero timestamps are omitted in proto).
func TestSessionRoundTrip(t *testing.T) {
	created := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)
	s := SessionFields{
		ID:        "ses_1",
		TenantID:  "tnt_1",
		Workspace: "wsp_abc",
		State:     "running",
		LastSeq:   42,
		CreatedBy: "usr_1",
		Title:     "test session",
		CreatedAt: created,
	}

	pb := SessionToProto(s)
	if pb.Id != s.ID {
		t.Errorf("ID: got %q, want %q", pb.Id, s.ID)
	}
	if pb.CreatedAt.AsTime().Equal(created) {
		// ok
	} else {
		t.Errorf("CreatedAt: got %v, want %v", pb.CreatedAt.AsTime(), created)
	}
	if pb.ClosedAt != nil {
		t.Errorf("ClosedAt: expected nil for zero time, got %v", pb.ClosedAt)
	}

	got := SessionFromProto(pb)
	if got != s {
		t.Errorf("SessionFromProto mismatch:\n  got:  %#v\n  want: %#v", got, s)
	}
}

// TestSessionRoundTripZeroTimes verifies that zero timestamps are omitted in
// proto and round-trip back to zero.
func TestSessionRoundTripZeroTimes(t *testing.T) {
	s := SessionFields{
		ID:    "ses_1",
		State: "idle",
	}
	pb := SessionToProto(s)
	if pb.CreatedAt != nil {
		t.Errorf("CreatedAt: expected nil for zero time, got %v", pb.CreatedAt)
	}
	if pb.ClosedAt != nil {
		t.Errorf("ClosedAt: expected nil for zero time, got %v", pb.ClosedAt)
	}
	got := SessionFromProto(pb)
	if !got.CreatedAt.IsZero() {
		t.Errorf("CreatedAt: expected zero, got %v", got.CreatedAt)
	}
	if !got.ClosedAt.IsZero() {
		t.Errorf("ClosedAt: expected zero, got %v", got.ClosedAt)
	}
}

// payloadsEqual compares two event payloads for semantic equality, treating
// nil slices and empty slices as equal (proto round-trip may produce either).
func payloadsEqual(t *testing.T, got, want any) bool {
	t.Helper()
	// For subagent payloads with slices, normalize nil → empty before DeepEqual.
	switch w := want.(type) {
	case event.SubagentCompleted:
		g, ok := got.(event.SubagentCompleted)
		if !ok {
			return false
		}
		return equalStrSlices(g.FilesChanged, w.FilesChanged) &&
			equalStrSlices(g.Grounding, w.Grounding) &&
			g.SubagentID == w.SubagentID &&
			g.Status == w.Status &&
			g.SummaryPreview == w.SummaryPreview &&
			g.Err == w.Err &&
			g.CostUSD == w.CostUSD &&
			g.CtxSize == w.CtxSize &&
			g.HardMaxBytes == w.HardMaxBytes &&
			g.UsedBackend == w.UsedBackend
	}
	// Default: use reflect.DeepEqual
	return reflect.DeepEqual(got, want)
}

func equalStrSlices(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}