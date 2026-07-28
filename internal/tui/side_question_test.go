package tui

import (
	"errors"
	"strings"
	"testing"

	agent "github.com/treeol/wakil/internal/agent"
)

// TestSideQuestionChunkAccumulates verifies that when m.sideQuestion is set,
// a SideQuestionChunkMsg appends its Text to the sideQuestion buffer.
func TestSideQuestionChunkAccumulates(t *testing.T) {
	m := newTestTUI(t)
	m.sideQuestion = &sideQuestionState{
		id:  agent.SideQuestionID("sq-1"),
		buf: &strings.Builder{},
	}

	m = step(m, agent.SideQuestionChunkMsg{ID: "sq-1", Text: "hello "})
	m = step(m, agent.SideQuestionChunkMsg{ID: "sq-1", Text: "world"})

	if got := m.sideQuestion.buf.String(); got != "hello world" {
		t.Errorf("sideQuestion.buf = %q, want %q", got, "hello world")
	}
}

// TestSideQuestionChunkNilSideQuestion verifies that when m.sideQuestion is nil,
// a SideQuestionChunkMsg is silently dropped (no panic).
func TestSideQuestionChunkNilSideQuestion(t *testing.T) {
	m := newTestTUI(t)
	// m.sideQuestion is nil by default — this should not panic.
	m = step(m, agent.SideQuestionChunkMsg{ID: "sq-1", Text: "hello"})
	// No assertion needed beyond not panicking.
}

// TestSideQuestionDoneWithOutput verifies that when a side question completes
// with non-empty output, an iSys item is added with "≫ " + output,
// and sideQuestion is set to nil.
func TestSideQuestionDoneWithOutput(t *testing.T) {
	m := newTestTUI(t)
	m.sideQuestion = &sideQuestionState{
		id:  agent.SideQuestionID("sq-1"),
		buf: &strings.Builder{},
	}
	m.sideQuestion.buf.WriteString("some useful info")

	m = step(m, agent.SideQuestionDoneMsg{ID: "sq-1"})

	if m.sideQuestion != nil {
		t.Fatal("sideQuestion should be nil after SideQuestionDoneMsg")
	}
	last := lastItemText(m)
	if !strings.Contains(last, "some useful info") {
		t.Errorf("expected item with side-question output, got: %q", last)
	}
}

// TestSideQuestionDoneWithErr verifies that when side question completes
// with a non-nil error, the error is rendered as an iSys item.
func TestSideQuestionDoneWithErr(t *testing.T) {
	m := newTestTUI(t)
	m.sideQuestion = &sideQuestionState{
		id:  agent.SideQuestionID("sq-1"),
		buf: &strings.Builder{},
	}
	m.sideQuestion.buf.WriteString("partial output")

	m = step(m, agent.SideQuestionDoneMsg{ID: "sq-1", Err: errors.New("something went wrong")})

	if m.sideQuestion != nil {
		t.Fatal("sideQuestion should be nil after SideQuestionDoneMsg with Err")
	}
	last := lastItemText(m)
	if !strings.Contains(last, "side question error") {
		t.Errorf("expected item with error text, got: %q", last)
	}
	if !strings.Contains(last, "something went wrong") {
		t.Errorf("expected error message in item, got: %q", last)
	}
}

// TestSideQuestionDoneEmptyOutput verifies that when side question completes
// with no output and no error, the placeholder text is rendered.
func TestSideQuestionDoneEmptyOutput(t *testing.T) {
	m := newTestTUI(t)
	m.sideQuestion = &sideQuestionState{
		id:  agent.SideQuestionID("sq-1"),
		buf: &strings.Builder{},
	}
	// buf is empty — no output written.

	m = step(m, agent.SideQuestionDoneMsg{ID: "sq-1"})

	if m.sideQuestion != nil {
		t.Fatal("sideQuestion should be nil after SideQuestionDoneMsg")
	}
	last := lastItemText(m)
	if !strings.Contains(last, "side question returned no output") {
		t.Errorf("expected empty-output placeholder, got: %q", last)
	}
}

// TestSideQuestionDoneNilSideQuestion verifies that when m.sideQuestion is nil,
// SideQuestionDoneMsg is silently dropped (no panic).
func TestSideQuestionDoneNilSideQuestion(t *testing.T) {
	m := newTestTUI(t)
	// m.sideQuestion is nil by default — this should not panic.
	m = step(m, agent.SideQuestionDoneMsg{ID: "sq-1", Err: errors.New("should be dropped")})
	// No crash means success.
	if len(*m.items) != 0 {
		t.Errorf("expected no items added when sideQuestion is nil, got %d", len(*m.items))
	}
}
