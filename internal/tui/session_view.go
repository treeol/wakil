package tui

import (
	"strings"

	"github.com/treeol/wakil/internal/core/format"
	"github.com/treeol/wakil/internal/proxy"
)

// convItemsFrom reconstructs the TUI conversation view from a stored transcript.
func convItemsFrom(conv []proxy.Message) []convItem {
	items := make([]convItem, 0, len(conv))
	for _, m := range conv {
		switch m.Role {
		case "user":
			items = append(items, convItem{kind: iUser, text: format.DerefStr(m.Content)})
		case "assistant":
			if strings.TrimSpace(format.DerefStr(m.Content)) != "" {
				items = append(items, convItem{kind: iAsst, text: format.DerefStr(m.Content)})
			}
		case "tool":
			items = append(items, convItem{kind: iSys, text: dim2("· " + m.Name + "\n" + format.Indent(format.Truncate(format.DerefStr(m.Content), 800)))})
		case "system":
			items = append(items, convItem{kind: iSys, text: dim2(format.DerefStr(m.Content))})
		}
	}
	return items
}
