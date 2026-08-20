package connect

import (
	"testing"
	"time"

	"github.com/treeol/wakil/internal/core/event"
)

// TestEventRoundTrip verifies that every event kind survives a
// core→proto→core round-trip without data loss.
func TestEventRoundTrip(t *testing.T) {
	now := time.Now().Truncate(time.Microsecond) // proto timestamp precision
	sid := event.SessionID("ses_test123")
	tid := event.TenantID("tnt_local")
	wid := event.WorkspaceID("wks_test")
	uid := event.UserID("usr_test")
	turnID := event.TurnID("trn_test123")
	tcID := event.ToolCallID("tc_test123")
	apID := event.ApprovalID("apr_test123")
	saID := event.SubagentID("sub_test123")
	opID := event.OpID("op_test123")

	cases := []event.Event{
		{TenantID: tid, SessionID: sid, Seq: 1, Ts: now, Kind: event.KindSessionCreated, Payload: event.SessionCreated{WorkspaceID: wid, AgentName: "main", CreatedBy: uid}},
		{TenantID: tid, SessionID: sid, Seq: 2, Ts: now, Kind: event.KindTurnStarted, Payload: event.TurnStarted{TurnID: turnID, TurnIndex: 1}},
		{TenantID: tid, SessionID: sid, Seq: 0, Ts: now, Kind: event.KindMessageDelta, Payload: event.MessageDelta{Text: "hello"}},
		{TenantID: tid, SessionID: sid, Seq: 3, Ts: now, Kind: event.KindMessageCommitted, Payload: event.MessageCommitted{TurnID: turnID, Text: "committed"}},
		{TenantID: tid, SessionID: sid, Seq: 0, Ts: now, Kind: event.KindReasoningDelta, Payload: event.ReasoningDelta{Text: "thinking"}},
		{TenantID: tid, SessionID: sid, Seq: 4, Ts: now, Kind: event.KindToolCallStarted, Payload: event.ToolCallStarted{TurnID: turnID, ToolCallID: tcID, Name: "edit_file", ArgDigest: "abc123"}},
		{TenantID: tid, SessionID: sid, Seq: 5, Ts: now, Kind: event.KindToolCallCompleted, Payload: event.ToolCallCompleted{ToolCallID: tcID, Name: "edit_file", Status: "ok", ResultPreview: "done", DurationMs: 42}},
		{TenantID: tid, SessionID: sid, Seq: 6, Ts: now, Kind: event.KindApprovalRequested, Payload: event.ApprovalRequested{ApprovalID: apID, ToolName: "edit_file", Headline: "Apply edit?", Detail: "diff...", ReadAction: false}},
		{TenantID: tid, SessionID: sid, Seq: 7, Ts: now, Kind: event.KindApprovalResolved, Payload: event.ApprovalResolved{ApprovalID: apID, Outcome: "approved", Reason: "", Resolver: uid}},
		{TenantID: tid, SessionID: sid, Seq: 8, Ts: now, Kind: event.KindSubagentSpawned, Payload: event.SubagentSpawned{SubagentID: saID, Task: "find things", Capability: "discovery", Backend: "openai", Model: "gpt-4", ToolNames: []string{"grep", "ls"}}},
		{TenantID: tid, SessionID: sid, Seq: 0, Ts: now, Kind: event.KindSubagentProgress, Payload: event.SubagentProgress{SubagentID: saID, Text: "working...", Finished: true, FinishedStatus: "ok", FinishedCostUSD: 0.01, FinishedFilesN: 3}},
		{TenantID: tid, SessionID: sid, Seq: 9, Ts: now, Kind: event.KindSubagentCompleted, Payload: event.SubagentCompleted{SubagentID: saID, Status: "ok", SummaryPreview: "found 3 things", Err: "", CostUSD: 0.02, FilesChanged: []string{"a.go", "b.go"}, Grounding: []string{"note1"}, CtxSize: 1000, HardMaxBytes: 50000, UsedBackend: "openai"}},
		{TenantID: tid, SessionID: sid, Seq: 10, Ts: now, Kind: event.KindMemoryProposed, Payload: event.MemoryProposed{Key: "arch/test", Kind: "note", Writer: "agent"}},
		{TenantID: tid, SessionID: sid, Seq: 11, Ts: now, Kind: event.KindGuardTriggered, Payload: event.GuardTriggered{Guard: "safety", Message: "blocked"}},
		{TenantID: tid, SessionID: sid, Seq: 12, Ts: now, Kind: event.KindContextWarning, Payload: event.ContextWarning{Message: "context full"}},
		{TenantID: tid, SessionID: sid, Seq: 13, Ts: now, Kind: event.KindTurnCompleted, Payload: event.TurnCompleted{TurnID: turnID, Outcome: "complete", Warn: "", WorkflowWillContinue: false}},
		{TenantID: tid, SessionID: sid, Seq: 14, Ts: now, Kind: event.KindSessionError, Payload: event.SessionError{Reason: "backend_failure", Err: "timeout"}},
		{TenantID: tid, SessionID: sid, Seq: 15, Ts: now, Kind: event.KindSessionClosed, Payload: event.SessionClosed{Reason: "closed"}},
		{TenantID: tid, SessionID: sid, Seq: 16, Ts: now, Kind: event.KindUserMessageCommitted, Payload: event.UserMessageCommitted{TurnID: turnID, Text: "user input"}},
		{TenantID: tid, SessionID: sid, Seq: 17, Ts: now, Kind: event.KindConversationCompacted, Payload: event.ConversationCompacted{TurnID: turnID}},
		{TenantID: tid, SessionID: sid, Seq: 18, Ts: now, Kind: event.KindWorkflowTurnStarted, Payload: event.WorkflowTurnStarted{TurnID: turnID, UserText: "next step"}},
		{TenantID: tid, SessionID: sid, Seq: 19, Ts: now, Kind: event.KindWorkflowFinalReview, Payload: event.WorkflowFinalReview{TurnID: turnID}},
		{TenantID: tid, SessionID: sid, Seq: 20, Ts: now, Kind: event.KindAsyncJobStarted, Payload: event.AsyncJobStarted{OpID: opID, Label: "test job"}},
		{TenantID: tid, SessionID: sid, Seq: 21, Ts: now, Kind: event.KindAsyncJobCompleted, Payload: event.AsyncJobCompleted{OpID: opID, Status: "ok", SummaryPreview: "done", Err: ""}},
		{TenantID: tid, SessionID: sid, Seq: 22, Ts: now, Kind: event.KindSideQuestionCompleted, Payload: event.SideQuestionCompleted{OpID: opID, Status: "ok", AnswerPreview: "answer"}},
		{TenantID: tid, SessionID: sid, Seq: 0, Ts: now, Kind: event.KindTokRate, Payload: event.TokRate{Rate: 42.5}},
		{TenantID: tid, SessionID: sid, Seq: 0, Ts: now, Kind: event.KindAsyncJobProgress, Payload: event.AsyncJobProgress{OpID: opID, Text: "50%"}},
		{TenantID: tid, SessionID: sid, Seq: 0, Ts: now, Kind: event.KindSideQuestionProgress, Payload: event.SideQuestionProgress{OpID: opID, Text: "thinking"}},
		{TenantID: tid, SessionID: sid, Seq: 0, Ts: now, Kind: event.KindLearnNudge, Payload: event.LearnNudge{Text: "save this?"}},
		{TenantID: tid, SessionID: sid, Seq: 0, Ts: now, Kind: event.KindSessionNote, Payload: event.SessionNote{Text: "progress..."}},
		{TenantID: tid, SessionID: sid, Seq: 23, Ts: now, Kind: event.KindWorkflowOutcome, Payload: event.WorkflowOutcome{TurnID: turnID, Outcome: "declined", Reason: "user said no"}},
		{TenantID: tid, SessionID: sid, Seq: 0, Ts: now, Kind: event.KindWorkflowWarning, Payload: event.WorkflowWarning{Message: "oracle unavailable"}},
	}

	for _, tc := range cases {
		t.Run(string(tc.Kind), func(t *testing.T) {
			pb, err := eventToProto(tc)
			if err != nil {
				t.Fatalf("eventToProto: %v", err)
			}
			got, err := eventFromProto(pb)
			if err != nil {
				t.Fatalf("eventFromProto: %v", err)
			}
			if got.Kind != tc.Kind {
				t.Errorf("Kind = %q, want %q", got.Kind, tc.Kind)
			}
			if got.TenantID != tc.TenantID {
				t.Errorf("TenantID = %q, want %q", got.TenantID, tc.TenantID)
			}
			if got.SessionID != tc.SessionID {
				t.Errorf("SessionID = %q, want %q", got.SessionID, tc.SessionID)
			}
			if got.Seq != tc.Seq {
				t.Errorf("Seq = %d, want %d", got.Seq, tc.Seq)
			}
			if !got.Ts.Equal(tc.Ts) {
				t.Errorf("Ts = %v, want %v", got.Ts, tc.Ts)
			}
			if got.Payload == nil {
				t.Fatal("Payload is nil")
			}
			// Compare payload by converting both to the same type via the kind.
			if !payloadsEqual(tc.Kind, tc.Payload, got.Payload) {
				t.Errorf("Payload mismatch:\n  got  = %#v\n  want = %#v", got.Payload, tc.Payload)
			}
		})
	}
}

// payloadsEqual compares two payloads of the same kind by re-converting
// them through a simple equality check. Since the payloads are plain structs,
// we use a type switch with reflect-free comparison.
func payloadsEqual(k event.Kind, a, b any) bool {
	switch k {
	case event.KindSessionCreated:
		return a.(event.SessionCreated) == b.(event.SessionCreated)
	case event.KindTurnStarted:
		return a.(event.TurnStarted) == b.(event.TurnStarted)
	case event.KindMessageDelta:
		return a.(event.MessageDelta) == b.(event.MessageDelta)
	case event.KindMessageCommitted:
		return a.(event.MessageCommitted) == b.(event.MessageCommitted)
	case event.KindReasoningDelta:
		return a.(event.ReasoningDelta) == b.(event.ReasoningDelta)
	case event.KindToolCallStarted:
		return a.(event.ToolCallStarted) == b.(event.ToolCallStarted)
	case event.KindToolCallCompleted:
		return a.(event.ToolCallCompleted) == b.(event.ToolCallCompleted)
	case event.KindApprovalRequested:
		return a.(event.ApprovalRequested) == b.(event.ApprovalRequested)
	case event.KindApprovalResolved:
		return a.(event.ApprovalResolved) == b.(event.ApprovalResolved)
	case event.KindSubagentSpawned:
		return eqSlice(a.(event.SubagentSpawned).ToolNames, b.(event.SubagentSpawned).ToolNames) &&
			a.(event.SubagentSpawned).SubagentID == b.(event.SubagentSpawned).SubagentID &&
			a.(event.SubagentSpawned).Task == b.(event.SubagentSpawned).Task &&
			a.(event.SubagentSpawned).Capability == b.(event.SubagentSpawned).Capability &&
			a.(event.SubagentSpawned).Backend == b.(event.SubagentSpawned).Backend &&
			a.(event.SubagentSpawned).Model == b.(event.SubagentSpawned).Model
	case event.KindSubagentProgress:
		return a.(event.SubagentProgress) == b.(event.SubagentProgress)
	case event.KindSubagentCompleted:
		aa, bb := a.(event.SubagentCompleted), b.(event.SubagentCompleted)
		return aa.SubagentID == bb.SubagentID && aa.Status == bb.Status &&
			aa.SummaryPreview == bb.SummaryPreview && aa.Err == bb.Err &&
			aa.CostUSD == bb.CostUSD && eqSlice(aa.FilesChanged, bb.FilesChanged) &&
			eqSlice(aa.Grounding, bb.Grounding) && aa.CtxSize == bb.CtxSize &&
			aa.HardMaxBytes == bb.HardMaxBytes && aa.UsedBackend == bb.UsedBackend
	case event.KindMemoryProposed:
		return a.(event.MemoryProposed) == b.(event.MemoryProposed)
	case event.KindGuardTriggered:
		return a.(event.GuardTriggered) == b.(event.GuardTriggered)
	case event.KindContextWarning:
		return a.(event.ContextWarning) == b.(event.ContextWarning)
	case event.KindTurnCompleted:
		return a.(event.TurnCompleted) == b.(event.TurnCompleted)
	case event.KindSessionError:
		return a.(event.SessionError) == b.(event.SessionError)
	case event.KindSessionClosed:
		return a.(event.SessionClosed) == b.(event.SessionClosed)
	case event.KindUserMessageCommitted:
		return a.(event.UserMessageCommitted) == b.(event.UserMessageCommitted)
	case event.KindConversationCompacted:
		return a.(event.ConversationCompacted) == b.(event.ConversationCompacted)
	case event.KindWorkflowTurnStarted:
		return a.(event.WorkflowTurnStarted) == b.(event.WorkflowTurnStarted)
	case event.KindWorkflowFinalReview:
		return a.(event.WorkflowFinalReview) == b.(event.WorkflowFinalReview)
	case event.KindAsyncJobStarted:
		return a.(event.AsyncJobStarted) == b.(event.AsyncJobStarted)
	case event.KindAsyncJobCompleted:
		return a.(event.AsyncJobCompleted) == b.(event.AsyncJobCompleted)
	case event.KindSideQuestionCompleted:
		return a.(event.SideQuestionCompleted) == b.(event.SideQuestionCompleted)
	case event.KindTokRate:
		return a.(event.TokRate) == b.(event.TokRate)
	case event.KindAsyncJobProgress:
		return a.(event.AsyncJobProgress) == b.(event.AsyncJobProgress)
	case event.KindSideQuestionProgress:
		return a.(event.SideQuestionProgress) == b.(event.SideQuestionProgress)
	case event.KindLearnNudge:
		return a.(event.LearnNudge) == b.(event.LearnNudge)
	case event.KindSessionNote:
		return a.(event.SessionNote) == b.(event.SessionNote)
	case event.KindWorkflowOutcome:
		return a.(event.WorkflowOutcome) == b.(event.WorkflowOutcome)
	case event.KindWorkflowWarning:
		return a.(event.WorkflowWarning) == b.(event.WorkflowWarning)
	default:
		return false
	}
}

func eqSlice[T comparable](a, b []T) bool {
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
