package tui

// WAKIL_TUI_PERF=1 frame-timing instrumentation.
//
// When enabled, every Update and View call is timed and appended to the
// per-session diagnostic log (internal/diag). The record format is one line:
//
//	tuiperf <event-type> <update-ms> <view-ms> <frame-bytes>
//
// Event type is the Go type name of the Update message (tea.KeyMsg,
// tea.MouseMsg, tea.WindowSizeMsg, timer messages, domain events, …).
// update-ms is the duration of the Update call, view-ms the duration of
// the View call that follows it (measured in this wrapper, i.e. the
// renderer pays it), frame-bytes the length of the string View returned
// (the renderer may write fewer bytes after coalescing, but View's
// output is the frame it was offered).
//
// On exit (runTUI's postRunCleanup, cmd/wakil/main.go) perfPrintSummary
// prints a three-line summary to the same diagnostic log:
//
//	tuiperf summary count=N slowest=<type> maxframe=Nbytes
//	tuiperf update median=… p99=…
//	tuiperf view median=… p99=…
//
// The update leg is timed in updateTimed (never by calling View: a
// measurement View would consume the pending OSC 52 escape — Part
// B5/B3). The drawing leg is timed inside View() itself (tui_view.go),
// which also records the frame's byte size.
//
// Off by default. With WAKIL_TUI_PERF unset, checkPerf returns false
// (a single nil/atomic load) and the timed path is never taken, so there
// is no cost when off. The switch is read once at process start into a
// package-level atomic, so tests can flip it without re-exec'ing and so
// no per-event getenv cost is paid.

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/treeol/wakil/internal/diag"
)

var perfEnabled atomic.Bool

func init() {
	if v := os.Getenv("WAKIL_TUI_PERF"); v == "1" || v == "true" || v == "yes" || v == "on" {
		perfEnabled.Store(true)
	}
}

// checkPerf reports whether frame timing is recording. It is the only
// cost paid when the switch is off: one atomic load per Update/View.
func checkPerf() bool { return perfEnabled.Load() }

// setPerfForTest flips the switch for tests and returns a restore
// function. Test-only affordance; production code never calls it.
func setPerfForTest(on bool) func() {
	prev := perfEnabled.Load()
	perfEnabled.Store(on)
	return func() { perfEnabled.Store(prev) }
}

// perfSample is one timed Update+View pair.
type perfSample struct {
	typ       string
	updateDur time.Duration
	viewDur   time.Duration
	bytes     int
}

// perfCount is the number of samples appended so far, used only to
// decide whether the buffer needs a (rare) reallocation. The slice
// itself is built with exponential growth: appending one sample costs
// O(1) amortized, not O(N) — the previous implementation copied the
// whole slice on every append (O(N²) over a session with the switch on,
// Mashūra 0.8.4 review finding).
var perfMu sync.Mutex
var perfBuf []perfSample

func perfReset() {
	perfMu.Lock()
	defer perfMu.Unlock()
	perfBuf = perfBuf[:0]
}

func perfLoad() []perfSample {
	perfMu.Lock()
	defer perfMu.Unlock()
	return append([]perfSample(nil), perfBuf...)
}

// perfRecord appends one sample. It is called only when checkPerf is
// true, i.e. only while the user opted in.
func perfRecord(s perfSample) {
	perfMu.Lock()
	defer perfMu.Unlock()
	perfBuf = append(perfBuf, s)
}

// perfSummary aggregates the recorded samples: count, median and 99th
// percentile (ns) of update and view durations, the largest frame in
// bytes, and the slowest event type by median update duration.
// Update legs are the samples updateTimed records (typ = msg type);
// view legs are the samples View records (typ "View"). ok is false
// when no samples exist.
func perfSummary() (count int, upMed, upP99, viewMed, viewP99 int64, slowest string, maxBytes int, ok bool) {
	samples := perfLoad()
	if len(samples) == 0 {
		return 0, 0, 0, 0, 0, "", 0, false
	}
	var ups, views []int64
	byType := map[string][]int64{}
	for _, s := range samples {
		if s.bytes > maxBytes {
			maxBytes = s.bytes
		}
		if s.viewDur > 0 || s.typ == "View" {
			views = append(views, s.viewDur.Nanoseconds())
		}
		if (s.updateDur > 0 || s.typ != "View") && s.typ != "" {
			ups = append(ups, s.updateDur.Nanoseconds())
			byType[s.typ] = append(byType[s.typ], s.updateDur.Nanoseconds())
		}
	}
	med := func(a []int64) int64 {
		s := append([]int64(nil), a...)
		sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
		n := len(s)
		if n%2 == 1 {
			return s[n/2]
		}
		return (s[n/2-1] + s[n/2]) / 2
	}
	p99 := func(a []int64) int64 {
		s := append([]int64(nil), a...)
		sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
		idx := (99*len(s)+99)/100 - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(s) {
			idx = len(s) - 1
		}
		return s[idx]
	}
	slowestMed := int64(-1)
	for typ, ds := range byType {
		if m := med(ds); m > slowestMed {
			slowestMed = m
			slowest = typ
		}
	}
	medOr0 := func(a []int64) int64 {
		if len(a) == 0 {
			return 0
		}
		return med(a)
	}
	p99Or0 := func(a []int64) int64 {
		if len(a) == 0 {
			return 0
		}
		return p99(a)
	}
	// count = number of frames: max(frames drawn, events handled)
	// — one sample per View and one per Update are recorded, while
	// a directly-recorded sample carries both legs.
	count = max(len(views), len(ups))
	return count, medOr0(ups), p99Or0(ups), medOr0(views), p99Or0(views), slowest, maxBytes, true
}

// perfPrintSummary writes the three-line exit summary to the diagnostic
// log. It is a no-op when recording was off or no samples were taken.
func perfPrintSummary() {
	count, upMed, upP99, viewMed, viewP99, slowest, maxBytes, ok := perfSummary()
	if !ok {
		return
	}
	diag.Printf("tuiperf summary count=%d slowest=%s maxframe=%dbytes\n", count, slowest, maxBytes)
	diag.Printf("tuiperf update median=%dns p99=%dns\n", upMed, upP99)
	diag.Printf("tuiperf view median=%dns p99=%dns\n", viewMed, viewP99)
}

// PrintPerfSummary is the exported exit hook (cmd/wakil/main.go invokes it
// from postRunCleanup, before the session log closes). No-op when
// WAKIL_TUI_PERF was unset or no samples were taken.
func PrintPerfSummary() { perfPrintSummary() }

// perfMsgType names an Update message for the diagnostic log. It is a
// short type name (no package path) so the log stays greppable.
func perfMsgType(msg any) string {
	if msg == nil {
		return "View"
	}
	name := fmt.Sprintf("%T", msg)
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// updateTimed is Update's perf-enabled path: it times the inner update
// and the View that follows, and appends one sample to the record.
// When the switch is off Update calls m.update directly (see tui.go)
// and this function is never entered, so off costs one atomic load.
//
// The extra View() this function makes is a REAL View(): View() has a
// side effect (it is the only place pendingEscape is emitted onto the
// frame, and it clears the escape after that one frame — Part 3, L5).
// Timing a frame therefore cannot call View() on a copy to measure it,
// or the measurement itself would consume the escape and the renderer's
// real frame would never carry it (the Mashūra 0.8.4 review caught this).
// To keep the measured frame identical to the rendered one, this
// function returns the model and the frame, and the renderer writes the
// frame the model produced rather than asking View() again.
func (m tuiModel) updateTimed(msg tea.Msg) (tea.Model, tea.Cmd) {
	start := time.Now()
	out, cmd := m.update(msg)
	updateDur := time.Since(start)
	// The perf switch must NOT call View() here. View() prepends
	// pendingEscape onto the returned frame and immediately CLEARS it
	// (Part 3, L5: one copy action, exactly one delivery). A measurement
	// call would consume the escape into a frame that is then thrown
	// away, and Bubbletea's real render of the same model would never
	// carry it — the copy would be silently lost whenever the perf
	// switch is on (Mashūra 0.8.4 review, finding 1).
	//
	// Instead: measure View() duration where it's already measured —
	// the View() wrapper at the top of tui_view.go records its own
	// sample on the one View() Bubbletea actually calls per real frame.
	// updateTimed therefore records only the update leg; the view leg
	// comes from View()'s own perfRecord call, so the summary's
	// update+view breakdown is unchanged and one copy action now
	// schedules exactly one delivery whether the switch is on or off.
	perfRecord(perfSample{typ: perfMsgType(msg), updateDur: updateDur})
	diag.Printf("tuiperf %s %d\n", perfMsgType(msg), updateDur.Nanoseconds())
	return out, cmd
}
