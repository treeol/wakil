package tui

import (
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/core/event"
)

func sqModel(t *testing.T) (tuiModel, *fakeFacade) {
	t.Helper()
	f := &fakeFacade{sid: "sess_tui_test", chatID: "chat123"}
	return newWiringModel(f), f
}

func sqChunk(text string, f *fakeFacade) event.Event {
	return evt(event.KindSideQuestionProgress, event.SideQuestionProgress{OpID: "op_sq1", Text: text}, f.sid)
}

func sqDone(status, preview string, f *fakeFacade) event.Event {
	return evt(event.KindSideQuestionCompleted, event.SideQuestionCompleted{OpID: "op_sq1", Status: status, AnswerPreview: preview}, f.sid)
}

// TestSideQuestionChunkAccumulates verifies that when m.sideQuestion is set,
// a SideQuestionProgress event appends its Text to the sideQuestion buffer.
func TestSideQuestionChunkAccumulates(t *testing.T) {
	m, f := sqModel(t)
	m.sideQuestion = &sideQuestionState{buf: &strings.Builder{}, opID: "op_sq1"}

	m = step(m, sqChunk("hello ", f))
	m = step(m, sqChunk("world", f))

	if got := m.sideQuestion.buf.String(); got != "hello world" {
		t.Errorf("sideQuestion.buf = %q, want %q", got, "hello world")
	}
}

// TestSideQuestionChunkNilSideQuestion verifies that when m.sideQuestion is
// nil, a progress event is silently dropped (no panic).
func TestSideQuestionChunkNilSideQuestion(t *testing.T) {
	m, f := sqModel(t)
	step(m, sqChunk("hello", f))
}

// TestSideQuestionDoneWithOutput verifies the completed-with-output render:
// "≫ " + output, and sideQuestion set to nil.
func TestSideQuestionDoneWithOutput(t *testing.T) {
	m, f := sqModel(t)
	m.sideQuestion = &sideQuestionState{buf: &strings.Builder{}, opID: "op_sq1"}
	m.sideQuestion.buf.WriteString("some useful info")

	m = step(m, sqDone("ok", "", f))

	if m.sideQuestion != nil {
		t.Fatal("sideQuestion should be nil after completion")
	}
	last := lastItemText(m)
	if !strings.Contains(last, "some useful info") {
		t.Errorf("expected item with side-question output, got: %q", last)
	}
}

// TestSideQuestionDoneWithErr verifies the error render (the error text
// travels via AnswerPreview on the event path).
func TestSideQuestionDoneWithErr(t *testing.T) {
	m, f := sqModel(t)
	m.sideQuestion = &sideQuestionState{buf: &strings.Builder{}, opID: "op_sq1"}

	m = step(m, sqDone("error", "something went wrong", f))

	if m.sideQuestion != nil {
		t.Fatal("sideQuestion should be nil after errored completion")
	}
	last := lastItemText(m)
	if !strings.Contains(last, "side question error") {
		t.Errorf("expected item with error text, got: %q", last)
	}
	if !strings.Contains(last, "something went wrong") {
		t.Errorf("expected error message in item, got: %q", last)
	}
}

// TestSideQuestionDoneEmptyOutput verifies the no-output placeholder.
func TestSideQuestionDoneEmptyOutput(t *testing.T) {
	m, f := sqModel(t)
	m.sideQuestion = &sideQuestionState{buf: &strings.Builder{}, opID: "op_sq1"}

	m = step(m, sqDone("ok", "", f))

	if m.sideQuestion != nil {
		t.Fatal("sideQuestion should be nil after completion")
	}
	last := lastItemText(m)
	if !strings.Contains(last, "side question returned no output") {
		t.Errorf("expected empty-output placeholder, got: %q", last)
	}
}

// TestSideQuestionDoneNilSideQuestion verifies a completion with no active
// side question is silently dropped (no panic).
func TestSideQuestionDoneNilSideQuestion(t *testing.T) {
	m, f := sqModel(t)
	m = step(m, sqDone("error", "should be dropped", f))
	if len(*m.items) != 0 {
		t.Errorf("expected no items added when sideQuestion is nil, got %d", len(*m.items))
	}
}

// TestSideQuestionStaleCompletionIgnored verifies that a late completion from
// a cancelled question (OpID mismatch) is ignored — not attributed to the
// newly-started question.
func TestSideQuestionStaleCompletionIgnored(t *testing.T) {
	m, f := sqModel(t)
	// Start question A (op_sq1), then cancel it.
	m.sideQuestion = &sideQuestionState{buf: &strings.Builder{}, opID: "op_sq1"}
	m.sideQuestion = nil // cancelled

	// Start question B (op_sq2).
	m.sideQuestion = &sideQuestionState{buf: &strings.Builder{}, opID: "op_sq2"}
	m.sideQuestion.buf.WriteString("answer from B")

	// Late completion from A arrives (op_sq1) — should be ignored.
	m = step(m, sqDone("ok", "stale answer from A", f))

	// B should still be active (not nil'd by the stale completion).
	if m.sideQuestion == nil {
		t.Fatal("sideQuestion should still be active (B) — stale completion from A should not clear it")
	}
	if m.sideQuestion.opID != "op_sq2" {
		t.Errorf("active sideQuestion opID = %q, want %q", m.sideQuestion.opID, "op_sq2")
	}
	if got := m.sideQuestion.buf.String(); got != "answer from B" {
		t.Errorf("sideQuestion.buf = %q, want %q (B's buffer should be intact)", got, "answer from B")
	}
}
