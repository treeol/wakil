package tui

import "testing"

// pruneSubTabs must never drop the running or currently-viewed tab, and should
// shed the oldest finished tabs first.
func TestPruneSubTabsKeepsRunningAndFocus(t *testing.T) {
	mk := func(n int, done bool) *subTab { return &subTab{n: n, done: done} }
	tabs := []*subTab{mk(1, true), mk(2, true), mk(3, true), mk(4, false)}

	got := pruneSubTabs(tabs, 1 /*focus*/, 2 /*max*/)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	has := map[int]bool{}
	for _, x := range got {
		has[x.n] = true
	}
	if !has[4] {
		t.Error("running tab (n=4) was pruned")
	}
	if !has[1] {
		t.Error("focused tab (n=1) was pruned")
	}
}

func TestPruneSubTabsNoOpUnderCap(t *testing.T) {
	tabs := []*subTab{{n: 1}, {n: 2}}
	if got := pruneSubTabs(tabs, 1, 12); len(got) != 2 {
		t.Fatalf("len = %d, want 2 (no prune under cap)", len(got))
	}
}

// TestPruneSubTabsPass2DropsFinishedNotDone tests the second pass of
// pruneSubTabs: when there aren't enough done tabs to reach the cap,
// finished-but-not-done tabs are dropped. The pass-2 iteration must
// operate on the survivors from pass 1, not the original tabs —
// re-iterating the original slice re-encounters done tabs already
// dropped, wasting drop slots and re-adding them (exceeding max).
func TestPruneSubTabsPass2DropsFinishedNotDone(t *testing.T) {
	mk := func(n int, done, finished bool) *subTab {
		return &subTab{n: n, done: done, finished: finished}
	}
	// 6 tabs, max=3, focus on running tab (n=6):
	//   n=1: done — dropped in pass 1 (drop 3→2)
	//   n=2: done — dropped in pass 1 (drop 2→1)
	//   n=3: finished, not done — dropped in pass 2 (drop 1→0)
	//   n=4: finished, not done — kept (drop exhausted)
	//   n=5: running — protected (never dropped)
	//   n=6: running, focused — protected (never dropped)
	// Expected: 3 tabs (n=4, n=5, n=6)
	tabs := []*subTab{
		mk(1, true, true),    // done — pass-1 drop
		mk(2, true, true),    // done — pass-1 drop
		mk(3, false, true),   // finished, not done — pass-2 drop
		mk(4, false, true),   // finished, not done — kept
		mk(5, false, false),  // running — protected
		mk(6, false, false),  // running, focused — protected
	}
	got := pruneSubTabs(tabs, 6, 3)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3 (max), got tabs: %v", len(got), tabNs(got))
	}
	has := map[int]bool{}
	for _, x := range got {
		has[x.n] = true
	}
	for _, n := range []int{4, 5, 6} {
		if !has[n] {
			t.Errorf("tab n=%d should be retained, got tabs: %v", n, tabNs(got))
		}
	}
	for _, n := range []int{1, 2, 3} {
		if has[n] {
			t.Errorf("tab n=%d should have been pruned", n)
		}
	}
}

// tabNs returns the n-values of a slice of subTabs for debug output.
func tabNs(tabs []*subTab) []int {
	ns := make([]int, len(tabs))
	for i, t := range tabs {
		ns[i] = t.n
	}
	return ns
}

func TestTabIndexByN(t *testing.T) {
	tabs := []*subTab{{n: 3}, {n: 7}, {n: 9}}
	if i := tabIndexByN(tabs, 7); i != 1 {
		t.Errorf("index of n=7 = %d, want 1", i)
	}
	if i := tabIndexByN(tabs, 0); i != -1 {
		t.Errorf("n=0 (main) = %d, want -1", i)
	}
	if i := tabIndexByN(tabs, 99); i != -1 {
		t.Errorf("missing n = %d, want -1", i)
	}
}

// The tab bar windows to the terminal width, always keeping the newest tab
// visible, and the slot-start helper stays consistent with the window offset.
func TestVisibleSubTabsWindowing(t *testing.T) {
	m := tuiModel{width: 200}
	for i := 0; i < 5; i++ {
		m.subTabs = append(m.subTabs, &subTab{n: i + 1})
	}
	if start, count := m.visibleSubTabs(); start != 0 || count != 5 {
		t.Fatalf("5 tabs at width 200: start=%d count=%d, want 0,5", start, count)
	}

	m.subTabs = nil
	for i := 0; i < 15; i++ {
		m.subTabs = append(m.subTabs, &subTab{n: i + 1})
	}
	start, count := m.visibleSubTabs()
	if start == 0 {
		t.Fatal("15 tabs should window (start > 0)")
	}
	if start+count != 15 {
		t.Fatalf("newest tab not visible: start=%d count=%d (want start+count==15)", start, count)
	}
	// With older tabs hidden, slot 0 leaves room for the "‹N" indicator.
	if got := m.subTabSlotStart(0); got != tabMainW+tabGap+tabMoreW {
		t.Fatalf("slot 0 start = %d, want %d", got, tabMainW+tabGap+tabMoreW)
	}
}
