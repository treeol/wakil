package tui

import (
	"fmt"

	agent "github.com/treeol/wakil/internal/agent"
)

// dotTickMsg fires every ~200 ms while the agent is busy.
type dotTickMsg struct{}

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
