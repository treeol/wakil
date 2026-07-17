package tui

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/treeol/wakil/internal/agent"
)

// This file bridges agent-internal command/message types (agent.Cmd, agent.Msg,
// agent.BatchMsg) to bubbletea's types (tea.Cmd, tea.Msg, tea.BatchMsg).
// The agent package no longer imports bubbletea; this adapter is the only
// place where the two type systems meet.
//
// The bridging is trivial — agent.Cmd is `func() any` and tea.Cmd is
// `func() tea.Msg` (where tea.Msg is `interface{}`). The adapter calls the
// agent Cmd and returns the result as a tea.Msg. BatchMsg is expanded into
// tea.Batch so concurrent execution semantics are preserved.

// AdaptCmd converts an agent.Cmd into a tea.Cmd. The returned tea.Cmd calls
// the agent Cmd and:
//   - if the result is an agent.BatchMsg, delegates to tea.Batch (the public
//     constructor) to produce a tea.BatchMsg, so concurrent execution semantics
//     and nil/empty handling are bubbletea's responsibility, not ours;
//   - otherwise returns the result directly as a tea.Msg (the concrete message
//     types like SysNoteMsg, AgentDoneMsg, etc. are the same structs the TUI
//     already type-switches on, so they pass through unchanged).
//
// Note: agent.BatchMsg must never be sent through sendEvent (which bypasses
// this adapter) — it would arrive as an unhandled struct and its sub-Cmds
// would silently never run. BatchMsg is only produced by agent.Batch(), which
// is only returned from HandleTUICommand/HandlePlanCommand, which the TUI
// always wraps with AdaptCmd.
func AdaptCmd(cmd agent.Cmd) tea.Cmd {
	if cmd == nil {
		return nil
	}
	return func() tea.Msg {
		msg := cmd()
		if msg == nil {
			return nil
		}
		if batch, ok := msg.(agent.BatchMsg); ok {
			teaCmds := make([]tea.Cmd, 0, len(batch.Cmds))
			for _, c := range batch.Cmds {
				teaCmds = append(teaCmds, AdaptCmd(c))
			}
			// Delegate to tea.Batch — the public constructor handles nil
			// filtering, empty batches, and single-cmd compaction per
			// bubbletea's own semantics. We don't construct tea.BatchMsg
			// directly to avoid coupling to its internal representation.
			return tea.Batch(teaCmds...)()
		}
		return msg
	}
}

// AdaptCmds converts a slice of agent.Cmd into a slice of tea.Cmd.
func AdaptCmds(cmds []agent.Cmd) []tea.Cmd {
	out := make([]tea.Cmd, 0, len(cmds))
	for _, c := range cmds {
		out = append(out, AdaptCmd(c))
	}
	return out
}
