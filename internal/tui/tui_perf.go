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
//	tuiperf summary count=N median=… p99=… slowest=<type> <ms>
//	tuiperf update median=… p99=…
//	tuiperf view median=… p99=…
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

var perfSamples atomic.Value // []perfSample, reset by perfReset

func perfReset() { perfSamples.Store([]perfSample{}) }

func perfLoad() []perfSample {
	v := perfSamples.Load()
	if v == nil {
		return nil
	}
	return v.([]perfSample)
}

// perfRecord appends one sample. It is called only when checkPerf is
// true, i.e. only while the user opted in.
func perfRecord(s perfSample) {
	cur := perfLoad()
	next := make([]perfSample, len(cur)+1)
	copy(next, cur)
	next[len(cur)] = s
	perfSamples.Store(next)
}

// perfSummary aggregates the recorded samples: count, median and 99th
// percentile (ns) of update and view durations, and the slowest event
// type by median update duration. ok is false when no samples exist.
func perfSummary() (count int, upMed, upP99, viewMed, viewP99 int64, slowest string, ok bool) {
	samples := perfLoad()
	if len(samples) == 0 {
		return 0, 0, 0, 0, 0, "", false
	}
	ups := make([]int64, len(samples))
	views := make([]int64, len(samples))
	byType := map[string][]int64{}
	for i, s := range samples {
		ups[i] = s.updateDur.Nanoseconds()
		views[i] = s.viewDur.Nanoseconds()
		byType[s.typ] = append(byType[s.typ], s.updateDur.Nanoseconds())
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
	return len(samples), med(ups), p99(ups), med(views), p99(views), slowest, true
}

// perfPrintSummary writes the three-line exit summary to the diagnostic
// log. It is a no-op when recording was off or no samples were taken.
func perfPrintSummary() {
	count, upMed, upP99, viewMed, viewP99, slowest, ok := perfSummary()
	if !ok {
		return
	}
	diag.Printf("tuiperf summary count=%d median=%dms p99=%dms slowest=%s\n",
		count, upMed/1e6, upP99/1e6, slowest)
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
func (m tuiModel) updateTimed(msg tea.Msg) (tea.Model, tea.Cmd) {
	start := time.Now()
	out, cmd := m.update(msg)
	updateDur := time.Since(start)
	m2 := out.(tuiModel)
	vStart := time.Now()
	frame := m2.View()
	viewDur := time.Since(vStart)
	perfRecord(perfSample{typ: perfMsgType(msg), updateDur: updateDur, viewDur: viewDur, bytes: len(frame)})
	diag.Printf("tuiperf %s %d %d %d\n", perfMsgType(msg), updateDur.Nanoseconds(), viewDur.Nanoseconds(), len(frame))
	return out, cmd
}
