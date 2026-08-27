package event

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"
)

// TestCodecRoundTripAllDurableKinds verifies that every durable event kind's
// payload round-trips through MarshalPayload → UnmarshalPayload with field
// equality. Ephemeral kinds are also tested (the codec is format-agnostic about
// durability — the store rejects ephemeral drafts, not the codec).
func TestCodecRoundTripAllKinds(t *testing.T) {
	for kind := range payloadTypes {
		t.Run(kind.String(), func(t *testing.T) {
			payload := samplePayload(kind)
			if payload == nil {
				t.Fatalf("no sample payload for kind %s", kind)
			}
			data, err := MarshalPayload(kind, payload)
			if err != nil {
				t.Fatalf("MarshalPayload(%s): %v", kind, err)
			}
			got, err := UnmarshalPayload(kind, data)
			if err != nil {
				t.Fatalf("UnmarshalPayload(%s): %v", kind, err)
			}
			// The decoded payload should be the same concrete type.
			gotType := reflect.TypeOf(got)
			wantType := payloadTypes[kind]
			if gotType != wantType {
				t.Fatalf("%s: decoded type %s != expected %s", kind, gotType, wantType)
			}
			// Check field-level equality via a helper that handles slices and times.
			if !payloadsEqual(kind, payload, got) {
				t.Fatalf("%s: round-trip mismatch\nwant: %#v\ngot:  %#v", kind, payload, got)
			}
		})
	}
}

// TestMarshalPayloadRejectsNil verifies that nil and typed-nil payloads are rejected.
func TestMarshalPayloadRejectsNil(t *testing.T) {
	if _, err := MarshalPayload(KindTurnStarted, nil); err == nil {
		t.Fatal("MarshalPayload should reject nil payload")
	}
	// Typed-nil pointer.
	if _, err := MarshalPayload(KindTurnStarted, (*TurnStarted)(nil)); err == nil {
		t.Fatal("MarshalPayload should reject typed-nil pointer")
	}
}

// TestMarshalPayloadRejectsTypeMismatch verifies that a payload of the wrong
// concrete type is rejected.
func TestMarshalPayloadRejectsTypeMismatch(t *testing.T) {
	if _, err := MarshalPayload(KindTurnStarted, SessionCreated{}); err == nil {
		t.Fatal("MarshalPayload should reject type mismatch")
	}
}

// TestMarshalPayloadRejectsNaN verifies that NaN and Inf float fields are rejected.
func TestMarshalPayloadRejectsNaN(t *testing.T) {
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := MarshalPayload(KindTokRate, TokRate{Rate: bad}); err == nil {
			t.Fatalf("MarshalPayload should reject %v", bad)
		}
	}
}

// TestUnmarshalPayloadRejectsUnknownKind verifies that unmarshalling an
// unregistered kind returns an error.
func TestUnmarshalPayloadRejectsUnknownKind(t *testing.T) {
	if _, err := UnmarshalPayload("bogus_kind", []byte("{}")); err == nil {
		t.Fatal("UnmarshalPayload should reject unknown kind")
	}
}

// TestUnmarshalPayloadRejectsMalformedJSON verifies that malformed JSON is rejected.
func TestUnmarshalPayloadRejectsMalformedJSON(t *testing.T) {
	if _, err := UnmarshalPayload(KindTurnStarted, []byte("{not json")); err == nil {
		t.Fatal("UnmarshalPayload should reject malformed JSON")
	}
}

// TestUnmarshalPayloadRejectsInvalidPayload verifies that a payload which
// decodes but fails Validate() is rejected.
func TestUnmarshalPayloadRejectsInvalidPayload(t *testing.T) {
	// TurnStarted requires TurnIndex >= 1; a zero value fails Validate.
	data, err := json.Marshal(TurnStarted{TurnID: "trn_test", TurnIndex: 0})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnmarshalPayload(KindTurnStarted, data); err == nil {
		t.Fatal("UnmarshalPayload should reject invalid decoded payload (TurnIndex=0)")
	}
}

// TestUnmarshalPayloadReturnsValueNotPointer verifies that the decoded payload
// is a value, not a pointer — matching MemLog's in-memory representation.
func TestUnmarshalPayloadReturnsValueNotPointer(t *testing.T) {
	data, err := MarshalPayload(KindTurnStarted, TurnStarted{TurnID: "trn_test", TurnIndex: 1})
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnmarshalPayload(KindTurnStarted, data)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.(*TurnStarted); ok {
		t.Fatal("UnmarshalPayload returned a pointer, expected a value")
	}
	if _, ok := got.(TurnStarted); !ok {
		t.Fatalf("UnmarshalPayload returned %T, expected TurnStarted value", got)
	}
}

// ---- helpers ----

// payloadsEqual compares two payloads of the same kind for field-level equality.
// It handles the nil-vs-empty-slice semantics (treats them as equivalent) and
// time.Time comparison via time.Equal (not reflect.DeepEqual, which fails on
// the monotonic clock component).
func payloadsEqual(kind Kind, want, got any) bool {
	switch k := kind; k {
	case KindSessionCreated:
		w := want.(SessionCreated)
		g := got.(SessionCreated)
		return w.WorkspaceID == g.WorkspaceID && w.AgentName == g.AgentName && w.CreatedBy == g.CreatedBy
	case KindTurnStarted:
		w := want.(TurnStarted)
		g := got.(TurnStarted)
		return w.TurnID == g.TurnID && w.TurnIndex == g.TurnIndex
	case KindMessageDelta:
		return want.(MessageDelta).Text == got.(MessageDelta).Text
	case KindMessageCommitted:
		w := want.(MessageCommitted)
		g := got.(MessageCommitted)
		return w.TurnID == g.TurnID && w.Text == g.Text
	case KindReasoningDelta:
		return want.(ReasoningDelta).Text == got.(ReasoningDelta).Text
	case KindToolCallStarted:
		w := want.(ToolCallStarted)
		g := got.(ToolCallStarted)
		return w.TurnID == g.TurnID && w.ToolCallID == g.ToolCallID && w.Name == g.Name && w.ArgDigest == g.ArgDigest
	case KindToolCallCompleted:
		w := want.(ToolCallCompleted)
		g := got.(ToolCallCompleted)
		return w.ToolCallID == g.ToolCallID && w.Name == g.Name && w.Status == g.Status &&
			w.ResultPreview == g.ResultPreview && w.DurationMs == g.DurationMs
	case KindApprovalRequested:
		w := want.(ApprovalRequested)
		g := got.(ApprovalRequested)
		return w.ApprovalID == g.ApprovalID && w.ToolName == g.ToolName && w.Headline == g.Headline &&
			w.Detail == g.Detail && w.ReadAction == g.ReadAction
	case KindApprovalResolved:
		w := want.(ApprovalResolved)
		g := got.(ApprovalResolved)
		return w.ApprovalID == g.ApprovalID && w.Outcome == g.Outcome && w.Reason == g.Reason && w.Resolver == g.Resolver
	case KindSubagentSpawned:
		w := want.(SubagentSpawned)
		g := got.(SubagentSpawned)
		return w.SubagentID == g.SubagentID && w.Task == g.Task && w.Capability == g.Capability &&
			w.Backend == g.Backend && w.Model == g.Model && stringSlicesEqual(w.ToolNames, g.ToolNames)
	case KindSubagentProgress:
		w := want.(SubagentProgress)
		g := got.(SubagentProgress)
		return w.SubagentID == g.SubagentID && w.Text == g.Text && w.Finished == g.Finished &&
			w.FinishedStatus == g.FinishedStatus && w.FinishedCostUSD == g.FinishedCostUSD && w.FinishedFilesN == g.FinishedFilesN
	case KindSubagentCompleted:
		w := want.(SubagentCompleted)
		g := got.(SubagentCompleted)
		return w.SubagentID == g.SubagentID && w.Status == g.Status && w.SummaryPreview == g.SummaryPreview &&
			w.Err == g.Err && w.CostUSD == g.CostUSD && stringSlicesEqual(w.FilesChanged, g.FilesChanged) &&
			stringSlicesEqual(w.Grounding, g.Grounding) && w.CtxSize == g.CtxSize && w.HardMaxBytes == g.HardMaxBytes && w.UsedBackend == g.UsedBackend
	case KindMemoryProposed:
		w := want.(MemoryProposed)
		g := got.(MemoryProposed)
		return w.Key == g.Key && w.Kind == g.Kind && w.Writer == g.Writer
	case KindGuardTriggered:
		w := want.(GuardTriggered)
		g := got.(GuardTriggered)
		return w.Guard == g.Guard && w.Message == g.Message
	case KindContextWarning:
		return want.(ContextWarning).Message == got.(ContextWarning).Message
	case KindTurnCompleted:
		w := want.(TurnCompleted)
		g := got.(TurnCompleted)
		return w.TurnID == g.TurnID && w.Outcome == g.Outcome && w.Warn == g.Warn && w.WorkflowWillContinue == g.WorkflowWillContinue
	case KindSessionError:
		w := want.(SessionError)
		g := got.(SessionError)
		return w.Reason == g.Reason && w.Err == g.Err
	case KindSessionClosed:
		return want.(SessionClosed).Reason == got.(SessionClosed).Reason
	case KindUserMessageCommitted:
		w := want.(UserMessageCommitted)
		g := got.(UserMessageCommitted)
		return w.TurnID == g.TurnID && w.Text == g.Text
	case KindConversationCompacted:
		return want.(ConversationCompacted).TurnID == got.(ConversationCompacted).TurnID
	case KindWorkflowTurnStarted:
		w := want.(WorkflowTurnStarted)
		g := got.(WorkflowTurnStarted)
		return w.TurnID == g.TurnID && w.UserText == g.UserText
	case KindWorkflowFinalReview:
		return want.(WorkflowFinalReview).TurnID == got.(WorkflowFinalReview).TurnID
	case KindAsyncJobStarted:
		w := want.(AsyncJobStarted)
		g := got.(AsyncJobStarted)
		return w.OpID == g.OpID && w.Label == g.Label
	case KindAsyncJobCompleted:
		w := want.(AsyncJobCompleted)
		g := got.(AsyncJobCompleted)
		return w.OpID == g.OpID && w.Status == g.Status && w.SummaryPreview == g.SummaryPreview && w.Err == g.Err
	case KindSideQuestionCompleted:
		w := want.(SideQuestionCompleted)
		g := got.(SideQuestionCompleted)
		return w.OpID == g.OpID && w.Status == g.Status && w.AnswerPreview == g.AnswerPreview
	case KindTokRate:
		return want.(TokRate).Rate == got.(TokRate).Rate
	case KindAsyncJobProgress:
		w := want.(AsyncJobProgress)
		g := got.(AsyncJobProgress)
		return w.OpID == g.OpID && w.Text == g.Text
	case KindSideQuestionProgress:
		w := want.(SideQuestionProgress)
		g := got.(SideQuestionProgress)
		return w.OpID == g.OpID && w.Text == g.Text
	case KindLearnNudge:
		return want.(LearnNudge).Text == got.(LearnNudge).Text
	case KindSessionNote:
		return want.(SessionNote).Text == got.(SessionNote).Text
	case KindWorkflowOutcome:
		w := want.(WorkflowOutcome)
		g := got.(WorkflowOutcome)
		return w.TurnID == g.TurnID && w.Outcome == g.Outcome && w.Reason == g.Reason
	case KindWorkflowWarning:
		return want.(WorkflowWarning).Message == got.(WorkflowWarning).Message
	case KindTurnSuspended:
		return true // empty struct — always equal
	case KindTurnResumed:
		return true // empty struct — always equal
	default:
		return false
	}
}

// stringSlicesEqual treats nil and empty slices as equivalent.
func stringSlicesEqual(a, b []string) bool {
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

// samplePayload returns a fully-populated, valid payload for the given kind.
// Used by the round-trip test to exercise all fields.
func samplePayload(kind Kind) any {
	switch kind {
	case KindSessionCreated:
		return SessionCreated{WorkspaceID: "wsp_test", AgentName: "wakil", CreatedBy: "usr_local"}
	case KindTurnStarted:
		return TurnStarted{TurnID: "trn_test1", TurnIndex: 1}
	case KindMessageDelta:
		return MessageDelta{Text: "hello world"}
	case KindMessageCommitted:
		return MessageCommitted{TurnID: "trn_test1", Text: "committed text"}
	case KindReasoningDelta:
		return ReasoningDelta{Text: "thinking…"}
	case KindToolCallStarted:
		return ToolCallStarted{TurnID: "trn_test1", ToolCallID: "tcl_test1", Name: "shell", ArgDigest: "sha256:abc"}
	case KindToolCallCompleted:
		return ToolCallCompleted{ToolCallID: "tcl_test1", Name: "shell", Status: "ok", ResultPreview: "done", DurationMs: 42}
	case KindApprovalRequested:
		return ApprovalRequested{ApprovalID: "apr_test1", ToolName: "shell", Headline: "Run?", Detail: "rm -rf /", ReadAction: false}
	case KindApprovalResolved:
		return ApprovalResolved{ApprovalID: "apr_test1", Outcome: "approved", Reason: "", Resolver: "usr_local"}
	case KindSubagentSpawned:
		return SubagentSpawned{SubagentID: "sub_test1", Task: "find x", Capability: "discovery", Backend: "openrouter", Model: "gpt-4", ToolNames: []string{"shell", "read"}}
	case KindSubagentProgress:
		return SubagentProgress{SubagentID: "sub_test1", Text: "scanning…", Finished: true, FinishedStatus: "ok", FinishedCostUSD: 0.01, FinishedFilesN: 3}
	case KindSubagentCompleted:
		return SubagentCompleted{SubagentID: "sub_test1", Status: "ok", SummaryPreview: "found it", Err: "", CostUSD: 0.05, FilesChanged: []string{"a.go", "b.go"}, Grounding: []string{"ref1"}, CtxSize: 1024, HardMaxBytes: 65536, UsedBackend: "openrouter"}
	case KindMemoryProposed:
		return MemoryProposed{Key: "arch/test", Kind: "note", Writer: "usr_local"}
	case KindGuardTriggered:
		return GuardTriggered{Guard: "seccomp", Message: "blocked syscall"}
	case KindContextWarning:
		return ContextWarning{Message: "context near limit"}
	case KindTurnCompleted:
		return TurnCompleted{TurnID: "trn_test1", Outcome: "complete", Warn: "", WorkflowWillContinue: false}
	case KindSessionError:
		return SessionError{Reason: "backend_failure", Err: "connection refused"}
	case KindSessionClosed:
		return SessionClosed{Reason: "closed"}
	case KindUserMessageCommitted:
		return UserMessageCommitted{TurnID: "trn_test1", Text: "user input"}
	case KindConversationCompacted:
		return ConversationCompacted{TurnID: "trn_test1"}
	case KindWorkflowTurnStarted:
		return WorkflowTurnStarted{TurnID: "trn_test1", UserText: "implement X"}
	case KindWorkflowFinalReview:
		return WorkflowFinalReview{TurnID: "trn_test1"}
	case KindAsyncJobStarted:
		return AsyncJobStarted{OpID: "op_test1", Label: "build"}
	case KindAsyncJobCompleted:
		return AsyncJobCompleted{OpID: "op_test1", Status: "ok", SummaryPreview: "built", Err: ""}
	case KindSideQuestionCompleted:
		return SideQuestionCompleted{OpID: "op_test1", Status: "ok", AnswerPreview: "answer"}
	case KindTokRate:
		return TokRate{Rate: 42.5}
	case KindAsyncJobProgress:
		return AsyncJobProgress{OpID: "op_test1", Text: "building…"}
	case KindSideQuestionProgress:
		return SideQuestionProgress{OpID: "op_test1", Text: "asking…"}
	case KindLearnNudge:
		return LearnNudge{Text: "save this?"}
	case KindSessionNote:
		return SessionNote{Text: "progress note"}
	case KindWorkflowOutcome:
		return WorkflowOutcome{TurnID: "trn_test1", Outcome: "declined", Reason: "user declined"}
	case KindWorkflowWarning:
		return WorkflowWarning{Message: "oracle unavailable"}
	case KindTurnSuspended:
		return TurnSuspended{}
	case KindTurnResumed:
		return TurnResumed{}
	default:
		return nil
	}
}
