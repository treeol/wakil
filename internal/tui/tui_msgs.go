package tui

import (
	"fmt"

	agent "github.com/treeol/wakil/internal/agent"
)

// dotTickMsg fires every ~200 ms while the agent is busy.
type dotTickMsg struct{}

// subTabCloseMsg is fired 30s after a subagent tab becomes done (via
// SubagentDoneMsg), to auto-close it if the user is not currently viewing it.
// Fire-and-validate: if the tab was already pruned, manually closed, or is
// focused at fire time, the handler is a no-op. The timer is a one-shot tea.Cmd
// and cannot be cancelled — staleness is handled idempotently in the handler.
type subTabCloseMsg struct{ ChatID string }

// copiedMsg reports that a text selection was copied to the system clipboard.
type copiedMsg struct{ n int }

// fmtConfirmBlock builds the confirm-prompt display string.
func fmtConfirmBlock(headline, detail string, readAction bool) string {
	opts := "  [y] proceed   [n] decline"
	if readAction {
		opts = "  [y] proceed   [a] allow all reads   [n] decline"
	}
	return fmt.Sprintf("\n⟂ %s\n%s\n\n%s\n", headline, agent.Indent(detail), opts)
}
