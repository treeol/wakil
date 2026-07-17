package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/treeol/wakil/internal/agent"
)

// TestAdaptCmdNil verifies that a nil agent.Cmd adapts to a nil tea.Cmd.
func TestAdaptCmdNil(t *testing.T) {
	if got := AdaptCmd(nil); got != nil {
		t.Fatalf("AdaptCmd(nil) = %v, want nil", got)
	}
}

// TestAdaptCmdPassthrough verifies that a non-batch agent.Cmd's message
// passes through to tea.Msg unchanged. The concrete message types (here
// SysNoteMsg) are the same structs the TUI type-switches on.
func TestAdaptCmdPassthrough(t *testing.T) {
	cmd := agent.NoteCmd("hello")
	teaCmd := AdaptCmd(cmd)
	if teaCmd == nil {
		t.Fatal("AdaptCmd returned nil for non-nil cmd")
	}
	msg := teaCmd()
	note, ok := msg.(agent.SysNoteMsg)
	if !ok {
		t.Fatalf("expected agent.SysNoteMsg, got %T", msg)
	}
	if note.Text != "hello" {
		t.Fatalf("expected 'hello', got %q", note.Text)
	}
}

// TestAdaptCmdBatch verifies that agent.Batch is expanded through the adapter
// into a tea.BatchMsg — i.e., the adapter delegates to tea.Batch so concurrent
// execution semantics are bubbletea's responsibility. Two sub-Cmds should
// produce a tea.BatchMsg with two members.
func TestAdaptCmdBatch(t *testing.T) {
	cmd := agent.Batch(
		agent.NoteCmd("first"),
		agent.NoteCmd("second"),
	)
	teaCmd := AdaptCmd(cmd)
	if teaCmd == nil {
		t.Fatal("AdaptCmd returned nil for batch cmd")
	}
	msg := teaCmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		t.Fatalf("expected tea.BatchMsg, got %T", msg)
	}
	if len(batch) != 2 {
		t.Fatalf("expected 2 sub-cmds in batch, got %d", len(batch))
	}
}

// TestAdaptCmdBatchNilMsg verifies that a sub-Cmd returning nil produces a
// nil entry in the tea.BatchMsg (not a panic). The nil filtering / compaction
// is delegated to tea.Batch.
func TestAdaptCmdBatchNilSubCmd(t *testing.T) {
	nilCmd := func() agent.Msg { return nil }
	cmd := agent.Batch(
		nilCmd,
		agent.NoteCmd("real"),
	)
	teaCmd := AdaptCmd(cmd)
	msg := teaCmd()
	// tea.Batch may compact nil entries; we just verify no panic and
	// that the batch contains at most 2 entries.
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		// tea.Batch may return a single cmd if it compacts; that's fine.
		return
	}
	if len(batch) > 2 {
		t.Fatalf("expected at most 2 sub-cmds, got %d", len(batch))
	}
}

// TestAdaptCmds verifies the slice adapter.
func TestAdaptCmds(t *testing.T) {
	cmds := []agent.Cmd{
		agent.NoteCmd("a"),
		agent.NoteCmd("b"),
	}
	teaCmds := AdaptCmds(cmds)
	if len(teaCmds) != 2 {
		t.Fatalf("expected 2 tea.Cmds, got %d", len(teaCmds))
	}
	for i, tc := range teaCmds {
		if tc == nil {
			t.Fatalf("teaCmds[%d] is nil", i)
		}
	}
}
