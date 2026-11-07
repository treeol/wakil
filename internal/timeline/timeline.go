// Package timeline is Wakil's phase timeline: when enabled it records how long
// each phase of start, /new, /resume and exit takes, in milliseconds, and
// writes the result to a file.
//
// It is off unless WAKIL_TRACE=1 is set or --trace is given. When off, every
// call is a nil-receiver no-op that allocates nothing, so instrumented code
// pays one comparison. It never writes to stderr: the TUI owns the terminal in
// the alternate screen, and a stray line would garble it. The file path is
// announced by the caller before the TUI starts or after it has exited.
//
// Usage:
//
//	defer timeline.Phase("config load")()          // a phase of the current transition
//	ph := timeline.Begin("hisn start"); ...; ph()  // the same, split over statements
//	timeline.Event("first prompt ready")           // an instant, offset only
//	timeline.Enter("/new")                         // start a named transition
package timeline

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Schema names the JSON document format of the timeline file.
const Schema = "wakil-timeline/v1"

// Span is one recorded phase or instant.
type Span struct {
	Transition string  `json:"transition"`
	Name       string  `json:"name"`
	StartMS    float64 `json:"start_ms"`          // offset from the transition's start
	DurMS      float64 `json:"dur_ms"`            // 0 for an instant
	Instant    bool    `json:"instant,omitempty"` // an Event, not a Phase
	Depth      int     `json:"depth,omitempty"`   // nesting level inside the transition
	Detail     string  `json:"detail,omitempty"`  // short free text (a path, a count)
	Concurrent bool    `json:"concurrent,omitempty"`
	// Accumulated marks a span that sums many short calls (see Accum). Its
	// duration is a total, not a wall-clock interval, so it never counts as
	// the parent of another span.
	Accumulated bool `json:"accumulated,omitempty"`
}

// Transition is the ordered record of one start, /new, /resume or exit.
type Transition struct {
	Name string `json:"name"`
	// AtMS is when the transition began, in ms since the timeline started
	// (process start for "start"): "time to prompt" is AtMS of the transition
	// plus the offset of its "prompt ready" instant.
	AtMS    float64 `json:"at_ms"`
	TotalMS float64 `json:"total_ms"`
	Spans   []Span  `json:"spans"`
}

// Doc is the file's top-level document.
type Doc struct {
	Schema      string       `json:"schema"`
	Version     string       `json:"version,omitempty"`
	Repo        string       `json:"repo,omitempty"`
	Host        string       `json:"host,omitempty"`
	Started     time.Time    `json:"started"`
	Transitions []Transition `json:"transitions"`
}

// accum is the running total of an Accum phase inside one transition.
type accum struct {
	first float64 // offset of the first call
	sum   time.Duration
	n     int
}

type recorder struct {
	mu      sync.Mutex
	path    string
	doc     Doc           // header only; Transitions is filled at Flush
	all     []*Transition // stable pointers: a span closure outlives later Enter calls
	acc     map[*Transition]map[string]*accum
	cur     *Transition
	curT0   time.Time
	depth   int
	now     func() time.Time
	written bool
}

var (
	gmu sync.Mutex
	rec *recorder
)

// Enabled reports whether the timeline is recording.
func Enabled() bool {
	gmu.Lock()
	defer gmu.Unlock()
	return rec != nil
}

// EnvEnabled reports whether the environment turns the timeline on:
// WAKIL_TRACE set to 1, true, yes or on.
func EnvEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("WAKIL_TRACE"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Start enables recording and opens the first transition (usually "start") at
// t0. path is where Flush writes; "" picks DefaultPath. Calling Start twice
// keeps the first recorder. It returns the file path.
func Start(path, firstTransition string, t0 time.Time) string {
	gmu.Lock()
	defer gmu.Unlock()
	if rec != nil {
		return rec.path
	}
	if path == "" {
		path = DefaultPath("", t0)
	}
	rec = &recorder{path: path, now: time.Now}
	rec.doc = Doc{Schema: Schema, Started: t0}
	rec.beginLocked(firstTransition, t0)
	return path
}

// SetDir moves the output file into dir, keeping its name. It is for a
// --trace-dir that is only known after the config is loaded.
func SetDir(dir string) {
	gmu.Lock()
	r := rec
	gmu.Unlock()
	if r == nil || dir == "" {
		return
	}
	r.mu.Lock()
	r.path = filepath.Join(dir, filepath.Base(r.path))
	r.mu.Unlock()
}

// Path is where Flush writes ("" when the timeline is off).
func Path() string {
	gmu.Lock()
	r := rec
	gmu.Unlock()
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.path
}

// SetMeta records the version, repository and host named in the file header.
func SetMeta(version, repo, host string) {
	gmu.Lock()
	r := rec
	gmu.Unlock()
	if r == nil {
		return
	}
	r.mu.Lock()
	r.doc.Version, r.doc.Repo, r.doc.Host = version, repo, host
	r.mu.Unlock()
}

// DefaultPath is <dir>/timeline-<UTC stamp>-<pid>.json; dir "" is
// $XDG_DATA_HOME/wakil/traces or ~/.local/share/wakil/traces.
func DefaultPath(dir string, t time.Time) string {
	if dir == "" {
		dir = os.Getenv("WAKIL_TRACE_DIR")
	}
	if dir == "" {
		if x := os.Getenv("XDG_DATA_HOME"); x != "" {
			dir = filepath.Join(x, "wakil", "traces")
		} else if h, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(h, ".local", "share", "wakil", "traces")
		} else {
			dir = os.TempDir()
		}
	}
	return filepath.Join(dir, fmt.Sprintf("timeline-%s-%d.json", t.UTC().Format("20060102T150405Z"), os.Getpid()))
}

func (r *recorder) beginLocked(name string, t0 time.Time) {
	r.finishLocked()
	r.cur = &Transition{Name: name, AtMS: ms(t0.Sub(r.doc.Started))}
	r.all = append(r.all, r.cur)
	r.curT0 = t0
	r.depth = 0
}

func (r *recorder) finishLocked() {
	if r.cur == nil {
		return
	}
	r.total(r.cur)
	r.cur = nil
}

// total sets TotalMS to the end of the last span, so waiting time between
// transitions (the user typing) is never counted.
func (r *recorder) total(t *Transition) {
	var end float64
	for _, s := range t.Spans {
		if s.Accumulated {
			continue // a sum of calls, not an interval: it must not stretch the total
		}
		if e := s.StartMS + s.DurMS; e > end {
			end = e
		}
	}
	t.TotalMS = round(end)
}

func ms(d time.Duration) float64 { return round(float64(d) / float64(time.Millisecond)) }

func round(f float64) float64 { return float64(int64(f*100+0.5)) / 100 }

// Enter closes the current transition and opens a new one at now.
func Enter(name string) {
	gmu.Lock()
	r := rec
	gmu.Unlock()
	if r == nil {
		return
	}
	r.mu.Lock()
	r.beginLocked(name, r.now())
	r.mu.Unlock()
}

// EnterOnce is Enter unless the current transition already has that name (the
// several quit paths of the TUI all mark "exit"; only the first counts).
func EnterOnce(name string) {
	gmu.Lock()
	r := rec
	gmu.Unlock()
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.cur != nil && r.cur.Name == name {
		r.mu.Unlock()
		return
	}
	r.beginLocked(name, r.now())
	r.mu.Unlock()
}

// Begin starts a phase and returns the function that ends it. A nil-safe no-op
// when the timeline is off.
func Begin(name string) func() { return begin(name, "", false) }

// BeginConcurrent is Begin for a phase that runs in a goroutine alongside
// others: it is marked concurrent and does not nest.
func BeginConcurrent(name string) func() { return begin(name, "", true) }

// Phase is Begin for use with defer: defer timeline.Phase("x")().
func Phase(name string) func() { return begin(name, "", false) }

// Detail starts a phase carrying a short detail string.
func Detail(name, detail string) func() { return begin(name, detail, false) }

var noop = func() {}

func begin(name, detail string, concurrent bool) func() {
	gmu.Lock()
	r := rec
	gmu.Unlock()
	if r == nil {
		return noop
	}
	r.mu.Lock()
	if r.cur == nil {
		r.mu.Unlock()
		return noop
	}
	t0 := r.now()
	depth := r.depth
	if !concurrent {
		r.depth++
	}
	tr := r.cur
	off := ms(t0.Sub(r.curT0))
	r.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			t1 := r.now()
			r.mu.Lock()
			defer r.mu.Unlock()
			if !concurrent && r.depth > 0 {
				r.depth--
			}
			// The span belongs to the transition it began in, even if a later
			// Transition call has already opened another.
			tr.Spans = append(tr.Spans, Span{Transition: tr.Name, Name: name, StartMS: off,
				DurMS: ms(t1.Sub(t0)), Depth: depth, Detail: detail, Concurrent: concurrent})
		})
	}
}

// Accum is Begin for a phase that runs many times (a path canonicalisation, a
// hash): the calls are summed into ONE span per transition, reported with its
// call count, instead of flooding the file with tiny spans.
func Accum(name string) func() {
	gmu.Lock()
	r := rec
	gmu.Unlock()
	if r == nil {
		return noop
	}
	t0 := r.now()
	return func() {
		d := r.now().Sub(t0)
		r.mu.Lock()
		defer r.mu.Unlock()
		tr := r.cur
		if tr == nil {
			return
		}
		if r.acc == nil {
			r.acc = map[*Transition]map[string]*accum{}
		}
		m := r.acc[tr]
		if m == nil {
			m = map[string]*accum{}
			r.acc[tr] = m
		}
		a := m[name]
		if a == nil {
			a = &accum{first: ms(t0.Sub(r.curT0))}
			m[name] = a
		}
		a.sum += d
		a.n++
	}
}

// Record adds a phase that already happened, between from and to, to the
// current transition. A start before the transition began gives a negative
// offset, which is how time spent before the first phase is kept honest.
func Record(name string, from, to time.Time) {
	gmu.Lock()
	r := rec
	gmu.Unlock()
	if r == nil || to.Before(from) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur == nil {
		return
	}
	r.cur.Spans = append(r.cur.Spans, Span{Transition: r.cur.Name, Name: name,
		StartMS: ms(from.Sub(r.curT0)), DurMS: ms(to.Sub(from))})
}

// Event records an instant in the current transition (for example "first
// prompt ready").
func Event(name string) {
	gmu.Lock()
	r := rec
	gmu.Unlock()
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur == nil {
		return
	}
	r.cur.Spans = append(r.cur.Spans, Span{Transition: r.cur.Name, Name: name,
		StartMS: ms(r.now().Sub(r.curT0)), Instant: true, Depth: r.depth})
}

// Flush finishes the open transition and writes the file (atomically). It is
// safe to call more than once; each call rewrites the file with everything
// recorded so far. It reports the path, or "" when the timeline is off.
func Flush() (string, error) {
	gmu.Lock()
	r := rec
	gmu.Unlock()
	if r == nil {
		return "", nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// The open transition is totalled but not closed, so a later Flush still
	// sees spans added after this one.
	doc := r.doc
	doc.Transitions = make([]Transition, 0, len(r.all))
	for _, t := range r.all {
		r.total(t)
		cp := *t
		cp.Spans = append([]Span(nil), t.Spans...)
		names := make([]string, 0, len(r.acc[t]))
		for n := range r.acc[t] {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			a := r.acc[t][n]
			cp.Spans = append(cp.Spans, Span{Transition: t.Name, Name: n, StartMS: a.first, DurMS: ms(a.sum),
				Detail: fmt.Sprintf("%d calls, summed", a.n), Depth: 1, Accumulated: true})
		}
		doc.Transitions = append(doc.Transitions, cp)
	}
	b, err := json.MarshalIndent(&doc, "", "  ")
	if err != nil {
		return r.path, err
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return r.path, err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return r.path, err
	}
	r.written = true
	return r.path, os.Rename(tmp, r.path)
}

// Reset turns the timeline off and forgets everything (tests).
func Reset() {
	gmu.Lock()
	rec = nil
	gmu.Unlock()
}

// SetClock replaces the recorder's clock (tests); a no-op when off.
func SetClock(now func() time.Time) {
	gmu.Lock()
	r := rec
	gmu.Unlock()
	if r != nil {
		r.mu.Lock()
		r.now = now
		r.mu.Unlock()
	}
}

// Render writes a human table of every transition: phases indented by depth,
// with start offset and duration, and the five costliest phases listed last.
func Render(w io.Writer, d *Doc) {
	for _, t := range d.Transitions {
		fmt.Fprintf(w, "%s  at +%.1f ms  total %.1f ms\n", t.Name, t.AtMS, t.TotalMS)
		spans := append([]Span(nil), t.Spans...)
		sort.SliceStable(spans, func(i, j int) bool { return spans[i].StartMS < spans[j].StartMS })
		for _, s := range spans {
			ind := strings.Repeat("  ", s.Depth+1)
			mark := ""
			if s.Concurrent {
				mark = " (concurrent)"
			}
			if s.Instant {
				fmt.Fprintf(w, "%s@ %8.1f  %s\n", ind, s.StartMS, s.Name)
				continue
			}
			det := ""
			if s.Detail != "" {
				det = "  [" + s.Detail + "]"
			}
			fmt.Fprintf(w, "%s%-44s +%8.1f  %8.1f ms%s%s\n", ind, s.Name, s.StartMS, s.DurMS, mark, det)
		}
		fmt.Fprintf(w, "  top five by duration:\n")
		for _, s := range TopFive(t) {
			fmt.Fprintf(w, "    %8.1f ms  %s\n", s.DurMS, s.Name)
		}
		fmt.Fprintln(w)
	}
}

// TopFive returns the five longest leaf phases of t: a phase that contains
// another is skipped, so a parent never crowds out the costs inside it.
func TopFive(t Transition) []Span {
	var leaves []Span
	for i, s := range t.Spans {
		if s.Instant {
			continue
		}
		parent := false
		for j, o := range t.Spans {
			if i == j || o.Instant || o.Accumulated {
				continue
			}
			if o.Depth > s.Depth && o.StartMS >= s.StartMS && o.StartMS+o.DurMS <= s.StartMS+s.DurMS+0.01 {
				parent = true
				break
			}
		}
		if !parent {
			leaves = append(leaves, s)
		}
	}
	sort.SliceStable(leaves, func(i, j int) bool { return leaves[i].DurMS > leaves[j].DurMS })
	if len(leaves) > 5 {
		leaves = leaves[:5]
	}
	return leaves
}

// Load reads a timeline file.
func Load(path string) (*Doc, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var d Doc
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, err
	}
	if d.Schema != Schema {
		return nil, fmt.Errorf("timeline: unsupported schema %q", d.Schema)
	}
	return &d, nil
}

// ProcessStart returns when this process started, taken from the kernel, so it
// includes package initialisation that runs before main: a terminal-capability
// query made from a package init can block for seconds and would otherwise be
// invisible. The age is the difference between /proc/uptime and the process's
// start time in clock ticks, both on the boot clock, so it is exact to one
// tick (10 ms) and does not inherit the second-wide error of btime. It falls
// back to fallback (the time main began) when /proc cannot be read, and never
// returns a time after fallback.
func ProcessStart(fallback time.Time) time.Time {
	stat, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return fallback
	}
	// starttime is field 22 overall: the 20th after the closing parenthesis.
	i := strings.LastIndexByte(string(stat), ')')
	if i < 0 {
		return fallback
	}
	f := strings.Fields(string(stat[i+1:]))
	if len(f) < 20 {
		return fallback
	}
	ticks, err := strconv.ParseInt(f[19], 10, 64)
	if err != nil {
		return fallback
	}
	upb, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return fallback
	}
	fields := strings.Fields(string(upb))
	if len(fields) < 1 {
		return fallback
	}
	uptime, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return fallback
	}
	const hz = 100 // USER_HZ is 100 on every Linux Go supports
	age := time.Duration((uptime - float64(ticks)/hz) * float64(time.Second))
	if age < 0 || age > time.Hour {
		return fallback // a container with a foreign /proc
	}
	start := time.Now().Add(-age)
	if start.After(fallback) {
		return fallback
	}
	return start
}
