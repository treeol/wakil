package agent

import "github.com/treeol/wakil/internal/proxy"

// danglingToolResultText is the synthetic result for a tool call whose result
// was never recorded (a session saved by v0.8.10 after a second token-limit
// cut, or a crash between the assistant message and its results).
const danglingToolResultText = "ERROR: no result was recorded for this call; the turn ended before it ran."

// repairDanglingToolCalls returns a copy of conv in which every assistant
// tool_call id that has no tool-result message gets a synthetic one, placed
// directly after the assistant message's existing results. Backends reject a
// request whose tool_calls have no matching result, so a resumed session that
// ends that way would fail on its next message.
//
// The input slice is never modified, so the saved session file stays
// byte-identical until the next normal save (which stores the repaired
// transcript). When nothing is missing it returns an unmodified copy.
func repairDanglingToolCalls(conv []proxy.Message) []proxy.Message {
	answered := make(map[string]bool)
	for _, m := range conv {
		if m.Role == "tool" && m.ToolCallID != "" {
			answered[m.ToolCallID] = true
		}
	}
	out := make([]proxy.Message, 0, len(conv))
	for i := 0; i < len(conv); i++ {
		m := conv[i]
		out = append(out, m)
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}
		var missing []proxy.Message
		for _, tc := range m.ToolCalls {
			if answered[tc.ID] {
				continue
			}
			answered[tc.ID] = true
			missing = append(missing, proxy.Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content:    StrPtr(danglingToolResultText),
				Turn:       m.Turn,
			})
		}
		if len(missing) == 0 {
			continue
		}
		// Keep the existing results of this message contiguous with the
		// synthetic ones: copy the run of tool messages first.
		for i+1 < len(conv) && conv[i+1].Role == "tool" {
			i++
			out = append(out, conv[i])
		}
		out = append(out, missing...)
	}
	return out
}
