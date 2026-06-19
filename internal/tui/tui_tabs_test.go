package tui

import "testing"

// pruneSubTabs must never drop the running or currently-viewed tab, and should
// shed the oldest finished tabs first.
func TestPruneSubTabsKeepsRunningAndFocus(t *testing.T) {
	mk := func(n int, done bool) *subTab { return &subTab{n: n, done: done} }
	tabs := []*subTab{mk(1, true), mk(2, true), mk(3, true), mk(4, false)}

	got := pruneSubTabs(tabs, 4 /*running*/, 1 /*focus*/, 2 /*max*/)
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
	if got := pruneSubTabs(tabs, 2, 1, 12); len(got) != 2 {
		t.Fatalf("len = %d, want 2 (no prune under cap)", len(got))
	}
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
