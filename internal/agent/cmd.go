package agent

// This file defines the agent-internal command and message types that replace
// the direct use of github.com/charmbracelet/bubbletea in the agent package.
// The TUI adapter (internal/tui/adapter.go) bridges between these types and
// tea.Cmd/tea.Msg, keeping the bubbletea dependency in the TUI layer only.
//
// Cmd is `func() any` — structurally compatible with tea.Cmd (`func()
// tea.Msg`, where tea.Msg is `interface{}`). The adapter wraps a Cmd into a
// tea.Cmd; BatchMsg (below) is expanded into tea.Batch so concurrent
// execution semantics are bubbletea's responsibility, not the agent's.

// Msg is the agent-internal message type. It is an alias of any because agent
// code produces plain struct messages (SysNoteMsg, AgentDoneMsg, …) that the
// TUI adapter passes through to bubbletea's Update verbatim. The TUI already
// type-switches on concrete message types, so the alias is transparent.
type Msg = any

// Cmd is the agent-internal command type: a function that returns a Msg.
// It is structurally compatible with tea.Cmd (`func() tea.Msg`). The TUI
// adapter (AdaptCmd) wraps a Cmd into a tea.Cmd. Using an alias rather than
// a named type means `func() any` is assignable both ways — but the TUI
// always wraps through AdaptCmd, which is the only path that expands
// BatchMsg into tea.Batch.
type Cmd = func() Msg

// BatchMsg wraps a slice of Cmds that the TUI adapter expands into a
// tea.Batch. It is the agent-side representation of "run these concurrently".
// The agent's Batch() constructor returns a Cmd that produces a BatchMsg;
// AdaptCmd intercepts it and delegates to tea.Batch for execution.
//
// IMPORTANT: BatchMsg must only travel through AdaptCmd (i.e., be returned
// from HandleTUICommand/HandlePlanCommand, which the TUI always wraps).
// If it were sent through sendEvent (which bypasses the adapter), it would
// arrive as an unhandled struct and its sub-Cmds would silently never run.
type BatchMsg struct {
	Cmds []Cmd
}

// Batch returns a Cmd that, when processed by AdaptCmd, produces a BatchMsg
// containing the given Cmds. The TUI adapter then delegates to tea.Batch,
// so concurrent execution, nil filtering, and empty/single compaction are
// bubbletea's responsibility — the agent package does not run anything
// concurrently itself.
func Batch(cmds ...Cmd) Cmd {
	return func() Msg {
		return BatchMsg{Cmds: cmds}
	}
}

// NoteCmd is a convenience constructor for a Cmd that produces a SysNoteMsg.
// It replaces the repeated `func() Msg { return SysNoteMsg{Text: text} }`
// pattern that appeared throughout commands.go.
func NoteCmd(text string) Cmd {
	return func() Msg {
		return SysNoteMsg{Text: text}
	}
}
