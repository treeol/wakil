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

// TestSubagentStartDoesNotStealFocus verifies that dispatching subagents keeps
// the user's current view: main stays main, and a focused tab stays focused.
func TestSubagentStartDoesNotStealFocus(t *testing.T) {
	m := newTabModel()
	if m.subCur != -1 {
		t.Fatalf("precondition: subCur = %d, want -1 (main)", m.subCur)
	}
	m = step(m, agent.SubagentStartMsg{Task: "task A", ChatID: "chat-a"})
	if m.subCur != -1 {
		t.Errorf("after first Start: subCur = %d, want -1 (stay on main)", m.subCur)
	}
	// Simulate the user focusing tab A, then a new dispatch arriving.
	m.subCur = tabIndexByN(m.subTabs, 1)
	m = step(m, agent.SubagentStartMsg{Task: "task B", ChatID: "chat-b"})
	if got := m.subTabs[m.subCur].chatID; got != "chat-a" {
		t.Errorf("focus moved to %q, want to stay on chat-a", got)
	}
}

// TestSubagentActiveMarksTab verifies the queued→running transition: only the
// tab whose worker acquired a slot becomes active.
func TestSubagentActiveMarksTab(t *testing.T) {
	m := newTabModel()
	m = step(m, agent.SubagentStartMsg{Task: "task A", ChatID: "chat-a"})
	m = step(m, agent.SubagentStartMsg{Task: "task B", ChatID: "chat-b"})

	for _, tab := range m.subTabs {
		if tab.active {
			t.Errorf("tab %s active before SubagentActiveMsg", tab.chatID)
		}
	}
	m = step(m, agent.SubagentActiveMsg{ChatID: "chat-a"})
	for _, tab := range m.subTabs {
		switch tab.chatID {
		case "chat-a":
			if !tab.active {
				t.Error("chat-a should be active")
			}
		case "chat-b":
			if tab.active {
				t.Error("chat-b should still be queued")
			}
		}
	}
}

// TestRenderSubTabDotStates verifies the three dot states: queued = static
// gray, active = pulsing yellow (phase-dependent), done = green check.
func TestRenderSubTabDotStates(t *testing.T) {
	queued := &subTab{}
	active := &subTab{active: true}
	done := &subTab{active: true, done: true}

	// Compare glyph+color specs, not rendered strings: lipgloss strips escape
	// codes in non-TTY test environments, making all renders look identical.
	if g, c := subTabDotSpec(done, 0); g != "✓" || c != "2" {
		t.Errorf("done dot = %q/%v, want ✓/2", g, c)
	}
	if g, c := subTabDotSpec(queued, 0); g != "●" || c != "240" {
		t.Errorf("queued dot = %q/%v, want ●/240", g, c)
	}
	// Queued is static: identical across phases.
	_, q0 := subTabDotSpec(queued, 0)
	_, q1 := subTabDotSpec(queued, 1)
	if q0 != q1 {
		t.Error("queued dot must not pulse")
	}
	// Active pulses: shade differs across phases.
	_, a0 := subTabDotSpec(active, 0)
	_, a1 := subTabDotSpec(active, 1)
	if a0 == a1 {
		t.Error("active dot should pulse (different shades per phase)")
	}
	// Active differs from queued at every phase (yellow family vs gray).
	for phase := 0; phase < len(subTabPulseShades); phase++ {
		if _, ac := subTabDotSpec(active, phase); ac == q0 {
			t.Errorf("phase %d: active dot color identical to queued", phase)
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
