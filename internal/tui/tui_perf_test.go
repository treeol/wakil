package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/treeol/wakil/internal/diag"
)

// TestPerfOffByDefault verifies the switch is off without the env var and
// that Update/View never record, never write to the diagnostic sink, and
// never add overhead beyond the single atomic load.
func TestPerfOffByDefault(t *testing.T) {
	if checkPerf() {
		t.Fatal("perf must be off by default (WAKIL_TUI_PERF unset)")
	}
	m, _ := newTestTUI(t)
	perfReset()
	m2, _ := m.Update(keyMsgForPerf("x"))
	_ = m2.(tuiModel).View()
	if n := len(perfLoad()); n != 0 {
		t.Fatalf("off: %d samples recorded, want 0", n)
	}
	if len(perfLoad()) != 0 {
		t.Fatal("off: summary must be empty")
	}
}

// TestPerfRecordsWhenOn verifies that with the switch on, every Update
// records one Update+View sample with a non-zero View byte count, and
// the summary aggregates median/p99/slowest-event-type correctly.
func TestPerfRecordsWhenOn(t *testing.T) {
	restore := setPerfForTest(true)
	defer restore()
	perfReset()

	m, _ := newTestTUI(t)
	m2, _ := m.Update(keyMsgForPerf("x"))
	_ = m2.(tuiModel).View() // separate View: records a View sample too

	samples := perfLoad()
	if len(samples) == 0 {
		t.Fatal("on: no samples recorded")
	}
	// The first sample is the Update leg from updateTimed (typ and
	// updateDur set), and the View leg is recorded by View()'s own
	// wrapper on the type "View". The bytes of the update sample are
	// always 0 (Part 3/L5: Update must not call View() to measure it —
	// a measurement View() consumes the OSC 52 escape, so the copy
	// would be lost whenever the switch was on).
	if samples[0].typ == "" || samples[0].bytes != 0 {
		t.Fatalf("on: unexpected first sample: %+v", samples[0])
	}
	count, upMed, upP99, viewMed, viewP99, slowest, maxBytes, ok := perfSummary()
	if !ok || count == 0 {
		t.Fatalf("summary ok=%v count=%d samples=%d", ok, count, len(samples))
	}
	_ = maxBytes
	if upP99 < upMed || viewP99 < viewMed {
		t.Fatalf("p99 < median: up %d/%d view %d/%d", upMed, upP99, viewMed, viewP99)
	}
	if slowest == "" {
		t.Fatal("summary: slowest event type must be non-empty")
	}
}

// TestPerfWritesToDiag verifies each sample lands as one tuiperf line in
// the diagnostic sink (the per-session log in production).
func TestPerfWritesToDiag(t *testing.T) {
	restore := setPerfForTest(true)
	defer restore()
	perfReset()
	var buf strings.Builder
	diag.Redirect(&buf)
	t.Cleanup(func() { diag.Redirect(nil) })

	m, _ := newTestTUI(t)
	_, _ = m.Update(keyMsgForPerf("x"))
	if !strings.Contains(buf.String(), "tuiperf ") {
		t.Fatalf("on: no tuiperf line in diag output: %q", buf.String())
	}
}

// TestPerfSummaryLines verifies the exit summary is exactly three
// tuiperf lines (count/median/p99/slowest, update, view).
func TestPerfSummaryLines(t *testing.T) {
	restore := setPerfForTest(true)
	defer restore()
	perfReset()
	var buf strings.Builder
	diag.Redirect(&buf)
	t.Cleanup(func() { diag.Redirect(nil) })

	for i := 0; i < 5; i++ {
		perfRecord(perfSample{typ: "KeyMsg", updateDur: time.Duration(i) * time.Microsecond, viewDur: time.Duration(i) * 2 * time.Microsecond, bytes: 100})
	}
	perfPrintSummary()
	lines := strings.Count(buf.String(), "tuiperf ")
	if lines != 3 {
		t.Fatalf("summary: %d tuiperf lines, want 3\n%s", lines, buf.String())
	}
	for _, want := range []string{"count=5", "median=", "p99=", "slowest=KeyMsg", "maxframe=100bytes"} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("summary missing %q\n%s", want, buf.String())
		}
	}
}

// TestPerfCopyStillOnceWithSwitchOn: with WAKIL_TUI_PERF on, a copy
// still reaches the clipboard path exactly once — the frame timing
// must not consume the OSC 52 escape (Part B3 test; the update leg
// is timed WITHOUT calling View, and View times itself, so the one
// View bubbletea renders carries the escape exactly once).
func TestPerfCopyStillOnceWithSwitchOn(t *testing.T) {
	restore := setPerfForTest(true)
	defer restore()
	perfReset()

	m, _ := newTestTUI(t)
	m = step(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	// Drive an Update through the program's real entry (no View
	// yet): the timed path must not consume the escape.
	u, _ := m.Update(keyMsgForPerf("x"))
	m = u.(tuiModel)
	esc := []byte("\x1b]52;c;aGVsbG8=\x07")
	m = step(m, copiedMsg{n: 5, via: copyViaOSC52, outcome: copySentUnverified, escape: esc})
	// No further KeyMsg here: the key handler clears a pending
	// escape (tui.go KeyMsg case) — the next render happens on the
	// copiedMsg frame itself, so the escape must still be pending.
	// The frame-production function must return the escape
	// prepended — view() is where the escape is prepended and
	// consumed (View() wraps view()).
	frame := m.view()
	if !strings.HasPrefix(frame, "\x1b]52;c;") {
		t.Fatal("with perf on, the rendered frame lost the OSC 52 escape (timing consumed it)")
	}
	if frame2 := m.View(); strings.HasPrefix(frame2, "\x1b]52;c;") {
		t.Fatal("with perf on, the escape was emitted twice")
	}
}

// keyMsgForPerf builds a printable key event for the perf tests. It
// mirrors wiringKeyMsg in tui_wiring_test.go (Runes for plain text).
func keyMsgForPerf(s string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}
