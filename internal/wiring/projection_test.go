package wiring

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionhost"
)

// fakeSessionEmitter is a test SessionEmitter that records all Emit and
// Notify calls. It is safe for concurrent use.
type fakeSessionEmitter struct {
	mu       sync.Mutex
	emitted  []recordedEvent
	notified []recordedEvent
	closed   bool
}

type recordedEvent struct {
	kind    event.Kind
	payload any
}

func (f *fakeSessionEmitter) Emit(kind event.Kind, payload any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return sessionhost.ErrEmitterClosed
	}
	f.emitted = append(f.emitted, recordedEvent{kind: kind, payload: payload})
	return nil
}

func (f *fakeSessionEmitter) Notify(kind event.Kind, payload any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.notified = append(f.notified, recordedEvent{kind: kind, payload: payload})
}

func (f *fakeSessionEmitter) emitList() []recordedEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedEvent(nil), f.emitted...)
}

func (f *fakeSessionEmitter) notifyList() []recordedEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedEvent(nil), f.notified...)
}

func TestProjectNilEmitter(t *testing.T) {
	// nil emitter must not panic.
	projectAgentEvent(nil, agent.SysNoteMsg{Text: "hello"})
}

func TestProjectSubagentStart(t *testing.T) {
	emit := &fakeSessionEmitter{}
	projectAgentEvent(emit, agent.SubagentStartMsg{
		Task:       "find auth",
		ChatID:     "abc-123",
		Capability: "discovery",
	})
	got := emit.emitList()
	if len(got) != 1 {
		t.Fatalf("want 1 emit, got %d", len(got))
	}
	if got[0].kind != event.KindSubagentSpawned {
		t.Errorf("kind = %s, want %s", got[0].kind, event.KindSubagentSpawned)
	}
	p := got[0].payload.(event.SubagentSpawned)
	if p.Task != "find auth" {
		t.Errorf("Task = %q, want %q", p.Task, "find auth")
	}
	if p.Capability != "discovery" {
		t.Errorf("Capability = %q, want %q", p.Capability, "discovery")
	}
	if p.SubagentID != "sub_abc-123" {
		t.Errorf("SubagentID = %q, want %q", p.SubagentID, "sub_abc-123")
	}
}

func TestProjectSubagentStartDefaultCapability(t *testing.T) {
	emit := &fakeSessionEmitter{}
	projectAgentEvent(emit, agent.SubagentStartMsg{
		ChatID: "xyz",
	})
	p := emit.emitList()[0].payload.(event.SubagentSpawned)
	if p.Capability != "discovery" {
		t.Errorf("default Capability = %q, want %q", p.Capability, "discovery")
	}
}

func TestProjectSubagentActive(t *testing.T) {
	emit := &fakeSessionEmitter{}
	projectAgentEvent(emit, agent.SubagentActiveMsg{ChatID: "chat-1"})
	got := emit.notifyList()
	if len(got) != 1 {
		t.Fatalf("want 1 notify, got %d", len(got))
	}
	if got[0].kind != event.KindSubagentProgress {
		t.Errorf("kind = %s, want %s", got[0].kind, event.KindSubagentProgress)
	}
}

func TestProjectSubagentChunk(t *testing.T) {
	emit := &fakeSessionEmitter{}
	projectAgentEvent(emit, agent.SubagentChunkMsg{ChatID: "chat-1", Text: "found it"})
	got := emit.notifyList()
	if len(got) != 1 {
		t.Fatalf("want 1 notify, got %d", len(got))
	}
	p := got[0].payload.(event.SubagentProgress)
	if p.Text != "found it" {
		t.Errorf("Text = %q, want %q", p.Text, "found it")
	}
	if p.SubagentID != "sub_chat-1" {
		t.Errorf("SubagentID = %q, want %q", p.SubagentID, "sub_chat-1")
	}
}

func TestProjectSubagentFinished(t *testing.T) {
	emit := &fakeSessionEmitter{}
	projectAgentEvent(emit, agent.SubagentFinishedMsg{ChatID: "c1", Status: "ok"})
	got := emit.notifyList()
	if len(got) != 1 {
		t.Fatalf("want 1 notify, got %d", len(got))
	}
	if got[0].kind != event.KindSubagentProgress {
		t.Errorf("kind = %s, want %s", got[0].kind, event.KindSubagentProgress)
	}
}

func TestProjectSubagentDone(t *testing.T) {
	emit := &fakeSessionEmitter{}
	projectAgentEvent(emit, agent.SubagentDoneMsg{ChatID: "c1"})
	got := emit.emitList()
	if len(got) != 1 {
		t.Fatalf("want 1 emit, got %d", len(got))
	}
	if got[0].kind != event.KindSubagentCompleted {
		t.Errorf("kind = %s, want %s", got[0].kind, event.KindSubagentCompleted)
	}
	p := got[0].payload.(event.SubagentCompleted)
	if p.Status != "ok" {
		t.Errorf("Status = %q, want %q", p.Status, "ok")
	}
	if p.SubagentID != "sub_c1" {
		t.Errorf("SubagentID = %q, want %q", p.SubagentID, "sub_c1")
	}
}

func TestProjectSubagentDoneFailed(t *testing.T) {
	emit := &fakeSessionEmitter{}
	projectAgentEvent(emit, agent.SubagentDoneMsg{ChatID: "c1", Err: "timeout"})
	p := emit.emitList()[0].payload.(event.SubagentCompleted)
	if p.Status != "failed" {
		t.Errorf("Status = %q, want %q", p.Status, "failed")
	}
}

func TestProjectAsyncJobStart(t *testing.T) {
	emit := &fakeSessionEmitter{}
	projectAgentEvent(emit, agent.AsyncJobStartMsg{OpID: "op-1", Label: "panel A"})
	got := emit.emitList()
	if len(got) != 1 {
		t.Fatalf("want 1 emit, got %d", len(got))
	}
	if got[0].kind != event.KindAsyncJobStarted {
		t.Errorf("kind = %s, want %s", got[0].kind, event.KindAsyncJobStarted)
	}
	p := got[0].payload.(event.AsyncJobStarted)
	if p.Label != "panel A" {
		t.Errorf("Label = %q, want %q", p.Label, "panel A")
	}
	if p.OpID != "op_op-1" {
		t.Errorf("OpID = %q, want %q", p.OpID, "op_op-1")
	}
}

func TestProjectAsyncJobChunk(t *testing.T) {
	emit := &fakeSessionEmitter{}
	projectAgentEvent(emit, agent.AsyncJobChunkMsg{OpID: "op-1", Text: "calling"})
	got := emit.notifyList()
	if len(got) != 1 {
		t.Fatalf("want 1 notify, got %d", len(got))
	}
	p := got[0].payload.(event.AsyncJobProgress)
	if p.Text != "calling" {
		t.Errorf("Text = %q, want %q", p.Text, "calling")
	}
}

func TestProjectAsyncJobDone(t *testing.T) {
	emit := &fakeSessionEmitter{}
	projectAgentEvent(emit, agent.AsyncJobDoneMsg{OpID: "op-1", Result: "the answer"})
	got := emit.emitList()
	if len(got) != 1 {
		t.Fatalf("want 1 emit, got %d", len(got))
	}
	p := got[0].payload.(event.AsyncJobCompleted)
	if p.Status != "ok" {
		t.Errorf("Status = %q, want %q", p.Status, "ok")
	}
	if p.SummaryPreview != "the answer" {
		t.Errorf("SummaryPreview = %q, want %q", p.SummaryPreview, "the answer")
	}
}

func TestProjectAsyncJobDoneError(t *testing.T) {
	emit := &fakeSessionEmitter{}
	projectAgentEvent(emit, agent.AsyncJobDoneMsg{OpID: "op-1", Err: "boom"})
	p := emit.emitList()[0].payload.(event.AsyncJobCompleted)
	if p.Status != "error" {
		t.Errorf("Status = %q, want %q", p.Status, "error")
	}
}

func TestProjectSideQuestionChunk(t *testing.T) {
	emit := &fakeSessionEmitter{}
	projectAgentEvent(emit, agent.SideQuestionChunkMsg{ID: "sq-1", Text: "partial"})
	got := emit.notifyList()
	if len(got) != 1 {
		t.Fatalf("want 1 notify, got %d", len(got))
	}
	p := got[0].payload.(event.SideQuestionProgress)
	if p.Text != "partial" {
		t.Errorf("Text = %q, want %q", p.Text, "partial")
	}
	if p.OpID != "op_sq-1" {
		t.Errorf("OpID = %q, want %q", p.OpID, "op_sq-1")
	}
}

func TestProjectSideQuestionDone(t *testing.T) {
	emit := &fakeSessionEmitter{}
	projectAgentEvent(emit, agent.SideQuestionDoneMsg{ID: "sq-1"})
	got := emit.emitList()
	if len(got) != 1 {
		t.Fatalf("want 1 emit, got %d", len(got))
	}
	p := got[0].payload.(event.SideQuestionCompleted)
	if p.Status != "ok" {
		t.Errorf("Status = %q, want %q", p.Status, "ok")
	}
}

func TestProjectSideQuestionDoneError(t *testing.T) {
	emit := &fakeSessionEmitter{}
	projectAgentEvent(emit, agent.SideQuestionDoneMsg{ID: "sq-1", Err: errors.New("boom")})
	p := emit.emitList()[0].payload.(event.SideQuestionCompleted)
	if p.Status != "error" {
		t.Errorf("Status = %q, want %q", p.Status, "error")
	}
}

func TestProjectAgentDoneLearnNudge(t *testing.T) {
	emit := &fakeSessionEmitter{}
	projectAgentEvent(emit, agent.AgentDoneMsg{LearnNudge: "consider /learn"})
	got := emit.notifyList()
	if len(got) != 1 {
		t.Fatalf("want 1 notify, got %d", len(got))
	}
	if got[0].kind != event.KindLearnNudge {
		t.Errorf("kind = %s, want %s", got[0].kind, event.KindLearnNudge)
	}
	p := got[0].payload.(event.LearnNudge)
	if p.Text != "consider /learn" {
		t.Errorf("Text = %q, want %q", p.Text, "consider /learn")
	}
}

func TestProjectAgentDoneNoLearnNudge(t *testing.T) {
	emit := &fakeSessionEmitter{}
	projectAgentEvent(emit, agent.AgentDoneMsg{})
	if len(emit.emitList()) != 0 {
		t.Errorf("want 0 emits, got %d", len(emit.emitList()))
	}
	if len(emit.notifyList()) != 0 {
		t.Errorf("want 0 notifies, got %d", len(emit.notifyList()))
	}
}

func TestProjectDroppedMessages(t *testing.T) {
	// These message types produce no events.
	dropped := []any{
		agent.SysNoteMsg{Text: "hello"},
		agent.CompactedMsg{},
		agent.BackendCtxLimitMsg{},
		agent.ModelListUpdatedMsg{},
		agent.MCPReconnectedMsg{},
		agent.TokRateMsg{Tps: 42.5},
		agent.ToolStartMsg{},
		agent.ToolResultMsg{},
	}
	for _, msg := range dropped {
		emit := &fakeSessionEmitter{}
		projectAgentEvent(emit, msg)
		if len(emit.emitList()) != 0 || len(emit.notifyList()) != 0 {
			t.Errorf("message %T should produce no events", msg)
		}
	}
}

func TestProjectUnknownMessage(t *testing.T) {
	emit := &fakeSessionEmitter{}
	projectAgentEvent(emit, "some unknown type")
	if len(emit.emitList()) != 0 || len(emit.notifyList()) != 0 {
		t.Error("unknown message should produce no events")
	}
}

func TestProjectClosedEmitter(t *testing.T) {
	emit := &fakeSessionEmitter{closed: true}
	// Should not panic; Emit returns error, Notify drops.
	projectAgentEvent(emit, agent.SubagentStartMsg{ChatID: "c1"})
	if len(emit.emitList()) != 0 {
		t.Error("closed emitter should not record emits")
	}
}

// TestSubagentIDFromChatID verifies the ID mapping helper.
func TestSubagentIDFromChatID(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"abc-123", "sub_abc-123"},
		{"sub_already", "sub_already"},
		{"", ""},
		{"xyz", "sub_xyz"},
	}
	for _, tc := range tests {
		if got := subagentIDFromChatID(tc.in); string(got) != tc.want {
			t.Errorf("subagentIDFromChatID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestOpIDFromString(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"op-1", "op_op-1"},
		{"op_already", "op_already"},
		{"", ""},
		{"job-bg1", "op_job-bg1"},
	}
	for _, tc := range tests {
		if got := opIDFromString(tc.in); string(got) != tc.want {
			t.Errorf("opIDFromString(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestToolCallIDFromString(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"call_0", "tcl_call_0"},
		{"tcl_already", "tcl_already"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := toolCallIDFromString(tc.in); string(got) != tc.want {
			t.Errorf("toolCallIDFromString(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Compile-time: ensure we use context (for future integration tests).
var _ = context.Background
