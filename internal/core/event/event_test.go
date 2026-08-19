package event

import (
	"testing"
	"time"
)

func ts() time.Time { return time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC) }

// validDraft builds a durable draft (Seq 0) with a correctly-typed payload.
func validDraft(kind Kind, payload any) Event {
	return Event{
		TenantID:  EmbeddedTenantID,
		SessionID: SessionID("ses_test"),
		Seq:       0,
		Ts:        ts(),
		Kind:      kind,
		Payload:   payload,
	}
}

// committedDraft builds a committed durable event (Seq 1) for ValidateCommitted.
func committedDraft(kind Kind, payload any) Event {
	e := validDraft(kind, payload)
	e.Seq = 1
	return e
}

func durablePayloads() map[Kind]any {
	return map[Kind]any{
		KindSessionCreated:    SessionCreated{WorkspaceID: "wsp_1", CreatedBy: EmbeddedUserID},
		KindTurnStarted:       TurnStarted{TurnID: "trn_1", TurnIndex: 1},
		KindMessageCommitted:  MessageCommitted{TurnID: "trn_1", Text: "hi"},
		KindToolCallStarted:   ToolCallStarted{TurnID: "trn_1", ToolCallID: "tcl_1", Name: "run_shell", ArgDigest: "abc"},
		KindToolCallCompleted: ToolCallCompleted{ToolCallID: "tcl_1", Name: "run_shell", Status: "ok"},
		KindApprovalRequested: ApprovalRequested{ApprovalID: "apr_1", ToolName: "run_shell", Headline: "h", Detail: "d"},
		KindApprovalResolved:  ApprovalResolved{ApprovalID: "apr_1", Outcome: "approved"},
		KindSubagentSpawned:   SubagentSpawned{SubagentID: "sub_1", Task: "t", Capability: "discovery"},
		KindSubagentCompleted: SubagentCompleted{SubagentID: "sub_1", Status: "ok"},
		KindMemoryProposed:    MemoryProposed{Key: "k", Kind: "note", Writer: "w"},
		KindGuardTriggered:    GuardTriggered{Guard: "g", Message: "m"},
		KindContextWarning:    ContextWarning{Message: "m"},
		KindTurnCompleted:     TurnCompleted{TurnID: "trn_1", Outcome: "complete"},
		KindSessionError:      SessionError{Reason: "daemon_restart", Err: "e"},
		KindSessionClosed:     SessionClosed{Reason: "done"},
	}
}

func TestRegistryCompleteness(t *testing.T) {
	// Every Kind constant must have exactly one payload type entry, and the
	// payload-type table must contain no key without a matching Kind.
	allKinds := []Kind{
		KindSessionCreated, KindTurnStarted, KindMessageDelta, KindMessageCommitted,
		KindReasoningDelta, KindToolCallStarted, KindToolCallCompleted,
		KindApprovalRequested, KindApprovalResolved, KindSubagentSpawned,
		KindSubagentProgress, KindSubagentCompleted, KindMemoryProposed,
		KindGuardTriggered, KindContextWarning, KindTurnCompleted, KindSessionError,
		KindSessionClosed,
	}
	seen := map[Kind]bool{}
	for _, k := range allKinds {
		seen[k] = true
		if _, ok := payloadTypes[k]; !ok {
			t.Errorf("kind %q has no payloadTypes entry", k)
		}
	}
	for k := range payloadTypes {
		if !seen[k] {
			t.Errorf("payloadTypes has entry %q with no Kind constant in the exhaustive list", k)
		}
	}
}

func TestValidateDraftAcceptsEveryDurableKind(t *testing.T) {
	for kind, payload := range durablePayloads() {
		if err := validDraft(kind, payload).ValidateDraft(); err != nil {
			t.Errorf("%s: valid draft rejected: %v", kind, err)
		}
	}
}

func TestValidateCommittedAcceptsEveryDurableKind(t *testing.T) {
	for kind, payload := range durablePayloads() {
		if err := committedDraft(kind, payload).ValidateCommitted(); err != nil {
			t.Errorf("%s: valid committed event rejected: %v", kind, err)
		}
	}
}

func TestValidateAcceptsEphemeralKinds(t *testing.T) {
	ephemeral := map[Kind]any{
		KindMessageDelta:     MessageDelta{Text: "tok"},
		KindReasoningDelta:   ReasoningDelta{Text: "reasoning"},
		KindSubagentProgress: SubagentProgress{SubagentID: "sub_1", Text: "p"},
	}
	for kind, payload := range ephemeral {
		if err := validDraft(kind, payload).ValidateDraft(); err != nil {
			t.Errorf("%s: valid ephemeral draft rejected: %v", kind, err)
		}
		if err := validDraft(kind, payload).ValidateCommitted(); err != nil {
			t.Errorf("%s: valid ephemeral committed rejected: %v", kind, err)
		}
	}
}

func TestEphemeralEventWithNonzeroSeqRejected(t *testing.T) {
	e := validDraft(KindMessageDelta, MessageDelta{Text: "tok"})
	e.Seq = 42
	if err := e.ValidateCommitted(); err == nil {
		t.Fatal("ephemeral event with seq 42 should be rejected, got nil")
	}
}

func TestDurableDraftWithNonzeroSeqRejected(t *testing.T) {
	e := validDraft(KindTurnStarted, TurnStarted{TurnID: "trn_1", TurnIndex: 1})
	e.Seq = 5
	if err := e.ValidateDraft(); err == nil {
		t.Fatal("draft with seq 5 should be rejected, got nil")
	}
}

func TestCommittedDurableWithZeroSeqRejected(t *testing.T) {
	e := validDraft(KindTurnStarted, TurnStarted{TurnID: "trn_1", TurnIndex: 1})
	if err := e.ValidateCommitted(); err == nil {
		t.Fatal("committed durable with seq 0 should be rejected, got nil")
	}
}

func TestMismatchedPayloadRejected(t *testing.T) {
	e := validDraft(KindTurnStarted, SessionClosed{Reason: "x"})
	if err := e.ValidateDraft(); err == nil {
		t.Fatal("expected error for kind/payload mismatch, got nil")
	}
}

func TestNilPayloadRejected(t *testing.T) {
	e := validDraft(KindTurnStarted, nil)
	if err := e.ValidateDraft(); err == nil {
		t.Fatal("expected error for nil payload, got nil")
	}
}

func TestTypedNilPointerPayloadRejected(t *testing.T) {
	var p *TurnStarted // nil pointer
	e := validDraft(KindTurnStarted, p)
	if err := e.ValidateDraft(); err == nil {
		t.Fatal("expected error for typed-nil pointer payload, got nil")
	}
}

func TestPointerPayloadAccepted(t *testing.T) {
	// A non-nil pointer to the correct type is canonicalized and accepted.
	e := validDraft(KindTurnStarted, &TurnStarted{TurnID: "trn_1", TurnIndex: 1})
	if err := e.ValidateDraft(); err != nil {
		t.Fatalf("non-nil pointer payload rejected: %v", err)
	}
}

func TestWrongPrefixEnvelopeIDsRejected(t *testing.T) {
	e := validDraft(KindTurnStarted, TurnStarted{TurnID: "trn_1", TurnIndex: 1})
	e.SessionID = "tcl_wrong"
	if err := e.ValidateDraft(); err == nil {
		t.Fatal("session_id with wrong prefix should be rejected")
	}
	e = validDraft(KindTurnStarted, TurnStarted{TurnID: "trn_1", TurnIndex: 1})
	e.TenantID = "bad"
	if err := e.ValidateDraft(); err == nil {
		t.Fatal("tenant_id with wrong prefix should be rejected")
	}
}

func TestWrongPrefixPayloadIDsRejected(t *testing.T) {
	e := validDraft(KindTurnStarted, TurnStarted{TurnID: "ses_wrong", TurnIndex: 1})
	if err := e.ValidateDraft(); err == nil {
		t.Fatal("payload TurnID with wrong prefix should be rejected")
	}
	e = validDraft(KindToolCallStarted, ToolCallStarted{TurnID: "trn_1", ToolCallID: "apr_wrong", Name: "x"})
	if err := e.ValidateDraft(); err == nil {
		t.Fatal("payload ToolCallID with wrong prefix should be rejected")
	}
}

func TestPayloadEnumValidation(t *testing.T) {
	if err := validDraft(KindToolCallCompleted, ToolCallCompleted{ToolCallID: "tcl_1", Name: "x", Status: "bogus"}).ValidateDraft(); err == nil {
		t.Fatal("invalid status should be rejected")
	}
	if err := validDraft(KindTurnCompleted, TurnCompleted{TurnID: "trn_1", Outcome: "bogus"}).ValidateDraft(); err == nil {
		t.Fatal("invalid outcome should be rejected")
	}
	if err := validDraft(KindTurnStarted, TurnStarted{TurnID: "trn_1", TurnIndex: 0}).ValidateDraft(); err == nil {
		t.Fatal("zero turn_index should be rejected")
	}
	if err := validDraft(KindToolCallCompleted, ToolCallCompleted{ToolCallID: "tcl_1", Name: "x", Status: "ok", DurationMs: -1}).ValidateDraft(); err == nil {
		t.Fatal("negative duration should be rejected")
	}
}

func TestUnknownKindRejected(t *testing.T) {
	e := validDraft(Kind("bogus"), TurnStarted{TurnID: "trn_1", TurnIndex: 1})
	if err := e.ValidateDraft(); err == nil {
		t.Fatal("expected error for unknown kind, got nil")
	}
}

func TestZeroTsRejected(t *testing.T) {
	e := validDraft(KindTurnStarted, TurnStarted{TurnID: "trn_1", TurnIndex: 1})
	e.Ts = time.Time{}
	if err := e.ValidateDraft(); err == nil {
		t.Fatal("expected error for zero ts, got nil")
	}
}

func TestKindClass(t *testing.T) {
	if KindMessageDelta.Class() != ClassEphemeral {
		t.Error("message_delta should be ephemeral")
	}
	if KindReasoningDelta.Class() != ClassEphemeral {
		t.Error("reasoning_delta should be ephemeral")
	}
	if KindSubagentProgress.Class() != ClassEphemeral {
		t.Error("subagent_progress should be ephemeral")
	}
	if KindTurnStarted.Class() != ClassDurable {
		t.Error("turn_started should be durable")
	}
	if KindMessageCommitted.Class() != ClassDurable {
		t.Error("message_committed should be durable")
	}
}

func TestIDValidation(t *testing.T) {
	if _, err := NewSessionID("ses_abc"); err != nil {
		t.Errorf("valid session id rejected: %v", err)
	}
	if _, err := NewTenantID("tnt_local"); err != nil {
		t.Errorf("valid tenant id rejected: %v", err)
	}
	if _, err := NewSessionID("ses_"); err == nil {
		t.Error("empty body accepted")
	}
	if _, err := NewSessionID("tcl_abc"); err == nil {
		t.Error("wrong prefix accepted")
	}
	if _, err := NewSessionID("abc"); err == nil {
		t.Error("missing prefix accepted")
	}
}

func TestIDValidateMethod(t *testing.T) {
	if err := SessionID("ses_ok").Validate(); err != nil {
		t.Errorf("valid session id failed Validate(): %v", err)
	}
	if err := SessionID("tcl_wrong").Validate(); err == nil {
		t.Error("wrong-prefix session id passed Validate()")
	}
	if err := TurnID("").Validate(); err == nil {
		t.Error("empty turn id passed Validate()")
	}
}

func TestCheckIDPanicsOnUnknownKind(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("checkID with unknown kind should panic")
		}
	}()
	_ = checkID("tenent", "anything") // typo — must panic, not silently pass
}

func TestEmbeddedIDs(t *testing.T) {
	if err := EmbeddedTenantID.Validate(); err != nil {
		t.Errorf("EmbeddedTenantID invalid: %v", err)
	}
	if err := EmbeddedUserID.Validate(); err != nil {
		t.Errorf("EmbeddedUserID invalid: %v", err)
	}
}
