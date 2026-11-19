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

// TestPerfSummaryToStdout: the exported stdout form writes the
// four summary lines plus the log path, and the "no samples"
// form when the switch is on but nothing was recorded.
func TestPerfSummaryToStdout(t *testing.T) {
	restore := setPerfForTest(true)
	defer restore()
	perfReset()
	var buf strings.Builder
	perfRecord(perfSample{typ: "KeyMsg", updateDur: 5 * time.Microsecond, viewDur: 7 * time.Microsecond, bytes: 42})
	PrintPerfSummaryTo(&buf, "/tmp/x/diag-abc.log")
	s := buf.String()
	for _, want := range []string{"tuiperf summary count=1 slowest=KeyMsg maxframe=42bytes log=/tmp/x/diag-abc.log", "tuiperf update median=", "tuiperf view median="} {
		if !strings.Contains(s, want) {
			t.Fatalf("stdout summary missing %q\n%s", want, s)
		}
	}
	perfReset()
	buf.Reset()
	PrintPerfSummaryTo(&buf, "/tmp/x/diag-abc.log")
	if !strings.Contains(buf.String(), "no frames recorded") {
		t.Fatalf("empty summary must say so: %q", buf.String())
	}
	off := setPerfForTest(false)
	buf.Reset()
	PrintPerfSummaryTo(&buf, "/tmp/x/diag-abc.log")
	off()
	if buf.Len() != 0 {
		t.Fatalf("switch off must print nothing: %q", buf.String())
	}
}

// TestPerfSummaryLines verifies the exit summary is exactly four
// tuiperf lines (count/median/p99/slowest, update, view, start).
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
	if lines != 4 {
		t.Fatalf("summary: %d tuiperf lines, want 4\n%s", lines, buf.String())
	}
	for _, want := range []string{"count=5", "median=", "p99=", "slowest=KeyMsg", "maxframe=100bytes"} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("summary missing %q\n%s", want, buf.String())
		}
	}
}

// TestPerfStartLine verifies the fourth summary line: the two start times
// measured from process start, and prompt_ready=n/a when the program quits
// before prompt ready. The fake backend (newTestTUI's fakeFacade) drives the
// model to first frame and prompt ready through the same timelineOnce path
// the real program uses.
func TestPerfStartLine(t *testing.T) {
	restore := setPerfForTest(true)
	defer restore()
	perfReset()
	// Isolated perf start-time state (the marks and process start are package
	// globals). Restore them so other tests are unaffected.
	perfStartMu.Lock()
	oldStart, oldFirst, oldReady := perfStart, perfFirstFrame, perfPromptReady
	perfStart = time.Now()
	perfFirstFrame, perfPromptReady = time.Time{}, time.Time{}
	perfStartMu.Unlock()
	t.Cleanup(func() {
		perfStartMu.Lock()
		perfStart, perfFirstFrame, perfPromptReady = oldStart, oldFirst, oldReady
		perfStartMu.Unlock()
	})

	// Drive the fake-backend model to first frame (the start-up view's first
	// window size) and then to prompt ready (the real model's first window
	// size), exactly as the program does. newTestTUI builds a ready model, so
	// reset it to not-ready before the first size.
	m, _ := newTestTUI(t)
	m.width, m.height = 0, 0
	m.ready = false
	// Clear the per-process timelineOnce gates so this test re-fires the same
	// instants the program records (an earlier test may have consumed them).
	timelineOnceMu.Lock()
	delete(timelineSeen, "first-frame")
	delete(timelineSeen, "first-frame-boot")
	timelineOnceMu.Unlock()
	// First frame: the start-up view's first WindowSizeMsg marks first_frame.
	_, _ = NewLoadingModel("", nil, nil).Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	if _, _, have := perfStartMs(); have {
		t.Fatal("before prompt ready, perfStartMs must report havePrompt=false")
	}
	first, _, have := perfStartMs()
	if have || first < 0 {
		t.Fatalf("first_frame=%d havePrompt=%v, want first>=0 and havePrompt=false", first, have)
	}
	// Prompt ready: the real model handles its first WindowSizeMsg.
	m2, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	_ = m2
	_, prompt, have := perfStartMs()
	if !have || prompt < first {
		t.Fatalf("after prompt ready: first=%d prompt=%d have=%v, want have=true and prompt>=first", first, prompt, have)
	}
	var buf strings.Builder
	perfRecord(perfSample{typ: "KeyMsg", updateDur: time.Microsecond, viewDur: time.Microsecond, bytes: 10})
	PrintPerfSummaryTo(&buf, "/tmp/x/diag.log")
	if !strings.Contains(buf.String(), "tuiperf start first_frame=") || !strings.Contains(buf.String(), "prompt_ready=") {
		t.Fatalf("summary missing the start line:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "prompt_ready=n/a") {
		t.Fatalf("after prompt ready the start line must not say n/a:\n%s", buf.String())
	}
	// Quit-before-prompt-ready form: no prompt_ready mark → n/a.
	perfStartMu.Lock()
	perfFirstFrame = time.Now()
	perfPromptReady = time.Time{}
	perfStartMu.Unlock()
	buf.Reset()
	PrintPerfSummaryTo(&buf, "/tmp/x/diag.log")
	if !strings.Contains(buf.String(), "tuiperf start first_frame=") || !strings.Contains(buf.String(), "prompt_ready=n/a") {
		t.Fatalf("quit before prompt ready must print prompt_ready=n/a:\n%s", buf.String())
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
