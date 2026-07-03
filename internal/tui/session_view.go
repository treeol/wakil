package tui

import (
	"strings"

	agent "github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/proxy"
)

// convItemsFrom reconstructs the TUI conversation view from a stored transcript.
func convItemsFrom(conv []proxy.Message) []convItem {
	items := make([]convItem, 0, len(conv))
	for _, m := range conv {
		switch m.Role {
		case "user":
			items = append(items, convItem{kind: iUser, text: agent.DerefStr(m.Content)})
		case "assistant":
			if strings.TrimSpace(agent.DerefStr(m.Content)) != "" {
				items = append(items, convItem{kind: iAsst, text: agent.DerefStr(m.Content)})
			}
		case "tool":
			items = append(items, convItem{kind: iSys, text: dim2("· " + m.Name + "\n" + agent.Indent(agent.Truncate(agent.DerefStr(m.Content), 800)))})
		case "system":
			items = append(items, convItem{kind: iSys, text: dim2(agent.DerefStr(m.Content))})
		}
	}
	return items
}
