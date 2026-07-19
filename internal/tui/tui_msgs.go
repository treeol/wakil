package tui

import (
	"fmt"

	agent "github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/proxy"
)

// dotTickMsg fires every ~200 ms while the agent is busy.
type dotTickMsg struct{}

// armTickMsg fires armWindow after a quit/cancel arm is set, to clear it. seq
// is the arm's generation counter: the handler only clears if seq still matches
// m.armSeq, so a stale tick from a superseded arm can't clear a newer one.
type armTickMsg struct{ seq int }

// subTabCloseMsg is fired 30s after a subagent tab becomes done (via
// SubagentDoneMsg), to auto-close it if the user is not currently viewing it.
// Fire-and-validate: if the tab was already pruned, manually closed, or is
// focused at fire time, the handler is a no-op. The timer is a one-shot tea.Cmd
// and cannot be cancelled — staleness is handled idempotently in the handler.
type subTabCloseMsg struct{ ChatID string }

// copiedMsg reports that a text selection was copied to the system clipboard.
type copiedMsg struct{ n int }

// clipboardImageMsg carries an image read from the system clipboard. If Err is
// non-empty, the handler shows it and does not attach anything.
type clipboardImageMsg struct {
	Img proxy.ImagePart
	Err string
}

// fmtConfirmBlock builds the confirm-prompt display string.
func fmtConfirmBlock(headline, detail string, readAction bool) string {
	opts := "  [y] proceed   [n] decline"
	if readAction {
		opts = "  [y] proceed   [a] allow all reads   [n] decline"
	}
	return fmt.Sprintf("\n⟂ %s\n%s\n\n%s\n", headline, agent.Indent(detail), opts)
}
