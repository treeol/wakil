package tui

// Tests for per-ChatID subagent tab routing (step 3 of the parallel-subagents
// plan): interleaved chunks from concurrent subagents land in the right tabs,
// and a Done for one subagent closes only its own tab.

import (
	"strings"
	"testing"

	agent "github.com/treeol/wakil/internal/agent"
)

func newTabModel() tuiModel {
	// Use the real constructor: reflow() runs when the first tab appears and
	// walks app, textarea, viewport, items, and streaming builders.
	m := NewTUIModel(newTestApp("http://unused", newFakeExecutor(), nil))
	m.width, m.height = 200, 50
	return m
}

// TestSubagentChunksRouteByChatID verifies that interleaved chunks from two
// concurrently running subagents are appended to their own tab buffers.
func TestSubagentChunksRouteByChatID(t *testing.T) {
	m := newTabModel()
	m = step(m, agent.SubagentStartMsg{Task: "task A", ChatID: "chat-a"})
	m = step(m, agent.SubagentStartMsg{Task: "task B", ChatID: "chat-b"})

	m = step(m, agent.SubagentChunkMsg{ChatID: "chat-a", Text: "alpha1 "})
	m = step(m, agent.SubagentChunkMsg{ChatID: "chat-b", Text: "beta1 "})
	m = step(m, agent.SubagentChunkMsg{ChatID: "chat-a", Text: "alpha2"})
	m = step(m, agent.SubagentChunkMsg{ChatID: "chat-b", Text: "beta2"})

	var bufA, bufB string
	for _, tab := range m.subTabs {
		switch tab.chatID {
		case "chat-a":
			bufA = tab.buf.String()
		case "chat-b":
			bufB = tab.buf.String()
		}
	}
	if bufA != "alpha1 alpha2" {
		t.Errorf("tab A buffer = %q, want %q", bufA, "alpha1 alpha2")
	}
	if bufB != "beta1 beta2" {
		t.Errorf("tab B buffer = %q, want %q", bufB, "beta1 beta2")
	}
	if strings.Contains(bufA, "beta") || strings.Contains(bufB, "alpha") {
		t.Error("cross-tab contamination detected")
	}
}

// TestSubagentDoneClosesOnlyItsTab verifies that Done for subagent A marks
// only A's tab done while B keeps streaming.
func TestSubagentDoneClosesOnlyItsTab(t *testing.T) {
	m := newTabModel()
	m = step(m, agent.SubagentStartMsg{Task: "task A", ChatID: "chat-a"})
	m = step(m, agent.SubagentStartMsg{Task: "task B", ChatID: "chat-b"})

	m = step(m, agent.SubagentDoneMsg{ChatID: "chat-a", UsedBackend: "llama"})

	// B still accepts chunks after A's Done.
	m = step(m, agent.SubagentChunkMsg{ChatID: "chat-b", Text: "still going"})

	for _, tab := range m.subTabs {
		switch tab.chatID {
		case "chat-a":
			if !tab.done {
				t.Error("tab A should be done")
			}
			if tab.usedBackend != "llama" {
				t.Errorf("tab A usedBackend = %q, want llama", tab.usedBackend)
			}
		case "chat-b":
			if tab.done {
				t.Error("tab B must not be done")
			}
			if tab.buf.String() != "still going" {
				t.Errorf("tab B buffer = %q, want %q", tab.buf.String(), "still going")
			}
		}
	}
}

// TestPruneNeverDropsAnyRunningTab verifies the new prune contract: with
// several tabs running concurrently, none of them may be pruned.
func TestPruneNeverDropsAnyRunningTab(t *testing.T) {
	mk := func(n int, done bool) *subTab { return &subTab{n: n, done: done} }
	// Three running tabs + three finished, cap 3: only finished ones drop.
	tabs := []*subTab{
		mk(1, true), mk(2, false), mk(3, true), mk(4, false), mk(5, true), mk(6, false),
	}
	got := pruneSubTabs(tabs, 6 /*focus*/, 3 /*max*/)
	for _, tab := range got {
		if tab.n == 2 || tab.n == 4 || tab.n == 6 {
			continue
		}
	}
	has := map[int]bool{}
	for _, x := range got {
		has[x.n] = true
	}
	for _, n := range []int{2, 4, 6} {
		if !has[n] {
			t.Errorf("running tab n=%d was pruned", n)
		}
	}
}
