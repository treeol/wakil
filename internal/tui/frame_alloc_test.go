package tui

import (
	"runtime"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestKeypressFrameAllocs pins the Part B4 budget: a keypress
// (Update+View) at 2000 messages must stay well under the
// pre-cache ~1.07 MB/frame. The limit is generous (5x headroom
// over the measured ~0.18 MB) so slower machines do not flake.
func TestKeypressFrameAllocs(t *testing.T) {
	if testing.Short() {
		t.Skip("allocation budget is a CI gate, not a short run")
	}
	m := newBenchModel(benchCorpus(2000), 120, 40)
	msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")}
	for i := 0; i < 5; i++ {
		u, _ := m.Update(msg)
		m = u.(tuiModel)
		_ = m.View()
	}
	runtime.GC()
	var b, a runtime.MemStats
	runtime.ReadMemStats(&b)
	const n = 100
	for i := 0; i < n; i++ {
		u, _ := m.Update(msg)
		m = u.(tuiModel)
		_ = m.View()
	}
	runtime.ReadMemStats(&a)
	got := float64(a.TotalAlloc-b.TotalAlloc) / n
	const limit = 900_000 // ~5x the measured 175 KB; pre-fix was ~1.07 MB
	t.Logf("keypress frame: %.0f B/op (limit %d)", got, limit)
	if got > limit {
		t.Fatalf("keypress frame allocates %.0f B/op, limit %d", got, limit)
	}
	// Cached pane must be identical to a fresh render.
	fresh := m
	fresh.convCacheBox = &convPaneCache{}
	if m.View() != fresh.View() {
		t.Fatal("cached frame differs from fresh frame")
	}
}
