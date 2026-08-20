package event

import (
	"fmt"
	"reflect"
)

// payloadTypes maps each event kind to the exact concrete Go type its payload
// must be. Using reflect.Type (not name strings) makes the check unsound-proof:
// a struct named "TurnStarted" from another package, or a generated proto type
// with the same name, will NOT match — only the exact domain type in this
// package does.
//
// Built from TypeOf at init so a payload struct rename breaks compilation at
// this map, not silently at runtime. This map is also the single registry that
// drives three things:
//
//  1. Validate's kind/payload pairing check,
//  2. P1's deserialization factory (Kind → concrete type to decode a stored
//     payload column),
//  3. table-driven completeness tests (every Kind has an entry, and every entry
//     has a matching Kind).
var payloadTypes = map[Kind]reflect.Type{
	KindSessionCreated:          reflect.TypeOf(SessionCreated{}),
	KindTurnStarted:             reflect.TypeOf(TurnStarted{}),
	KindMessageDelta:            reflect.TypeOf(MessageDelta{}),
	KindMessageCommitted:        reflect.TypeOf(MessageCommitted{}),
	KindReasoningDelta:          reflect.TypeOf(ReasoningDelta{}),
	KindToolCallStarted:         reflect.TypeOf(ToolCallStarted{}),
	KindToolCallCompleted:       reflect.TypeOf(ToolCallCompleted{}),
	KindApprovalRequested:       reflect.TypeOf(ApprovalRequested{}),
	KindApprovalResolved:        reflect.TypeOf(ApprovalResolved{}),
	KindSubagentSpawned:         reflect.TypeOf(SubagentSpawned{}),
	KindSubagentProgress:        reflect.TypeOf(SubagentProgress{}),
	KindSubagentCompleted:       reflect.TypeOf(SubagentCompleted{}),
	KindMemoryProposed:          reflect.TypeOf(MemoryProposed{}),
	KindGuardTriggered:          reflect.TypeOf(GuardTriggered{}),
	KindContextWarning:          reflect.TypeOf(ContextWarning{}),
	KindTurnCompleted:           reflect.TypeOf(TurnCompleted{}),
	KindSessionError:            reflect.TypeOf(SessionError{}),
	KindSessionClosed:           reflect.TypeOf(SessionClosed{}),
	KindUserMessageCommitted:     reflect.TypeOf(UserMessageCommitted{}),
	KindConversationCompacted:    reflect.TypeOf(ConversationCompacted{}),
	KindWorkflowTurnStarted:      reflect.TypeOf(WorkflowTurnStarted{}),
	KindWorkflowFinalReview:      reflect.TypeOf(WorkflowFinalReview{}),
	KindAsyncJobStarted:          reflect.TypeOf(AsyncJobStarted{}),
	KindAsyncJobCompleted:        reflect.TypeOf(AsyncJobCompleted{}),
	KindSideQuestionCompleted:    reflect.TypeOf(SideQuestionCompleted{}),
	KindTokRate:                  reflect.TypeOf(TokRate{}),
	KindAsyncJobProgress:         reflect.TypeOf(AsyncJobProgress{}),
	KindSideQuestionProgress:     reflect.TypeOf(SideQuestionProgress{}),
	KindLearnNudge:               reflect.TypeOf(LearnNudge{}),
	KindSessionNote:              reflect.TypeOf(SessionNote{}),
}

// payloadType returns the concrete Go type of p's payload, canonicalized to a
// non-pointer struct type. It returns (nil, false) when p is a typed-nil
// pointer (e.g. (*TurnStarted)(nil)) — a payload that is a nil pointer is not a
// valid payload and must be rejected, not dereferenced into a "matching" name.
func payloadType(p any) (reflect.Type, bool) {
	if p == nil {
		return nil, false
	}
	t := reflect.TypeOf(p)
	for t.Kind() == reflect.Ptr {
		// A typed nil pointer (interface non-nil, pointer nil) must be rejected.
		if reflect.ValueOf(p).IsNil() {
			return nil, false
		}
		t = t.Elem()
		p = reflect.ValueOf(p).Elem().Interface()
	}
	if t.Kind() != reflect.Struct {
		return nil, false
	}
	return t, true
}

// typeName returns a human-readable name for a payload type, for error messages.
func typeName(t reflect.Type) string {
	if t == nil {
		return "<nil>"
	}
	return t.Name()
}

// validatePayloadType reports whether p's concrete type is exactly the domain
// type registered for kind. It rejects nil, typed-nil pointers, non-structs,
// and same-named types from other packages.
func validatePayloadType(kind Kind, p any) error {
	want, ok := payloadTypes[kind]
	if !ok {
		return fmt.Errorf("event: unknown kind %q", kind)
	}
	got, ok := payloadType(p)
	if !ok {
		return fmt.Errorf("event %s: payload is nil or not a struct value", kind)
	}
	if got != want {
		return fmt.Errorf("event %s: payload type %s does not match expected %s", kind, got, want)
	}
	return nil
}
