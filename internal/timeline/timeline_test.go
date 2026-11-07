package timeline

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock advances only when told to, so durations are exact.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func start(t *testing.T) (*fakeClock, string) {
	t.Helper()
	Reset()
	t.Cleanup(Reset)
	c := &fakeClock{t: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	p := filepath.Join(t.TempDir(), "tl.json")
	Start(p, "start", c.now())
	SetClock(c.now)
	return c, p
}

func TestOffIsANoOp(t *testing.T) {
	Reset()
	if Enabled() {
		t.Fatal("must be off by default")
	}
	end := Begin("x")
	end()
	Event("y")
	Enter("z")
	defer Phase("p")()
	if p, err := Flush(); p != "" || err != nil {
		t.Fatalf("Flush when off = %q, %v", p, err)
	}
}

func TestOffDoesNotAllocate(t *testing.T) {
	Reset()
	if n := testing.AllocsPerRun(100, func() {
		Begin("x")()
		Event("y")
	}); n != 0 {
		t.Fatalf("a disabled timeline allocated %v times per call", n)
	}
}

func TestEnvEnabled(t *testing.T) {
	for v, want := range map[string]bool{"1": true, "true": true, "TRUE": true, "yes": true, "on": true, " 1 ": true,
		"": false, "0": false, "false": false, "no": false, "2": false} {
		t.Setenv("WAKIL_TRACE", v)
		if got := EnvEnabled(); got != want {
			t.Errorf("WAKIL_TRACE=%q: EnvEnabled = %v, want %v", v, got, want)
		}
	}
}

func TestNestedPhasesAndOffsets(t *testing.T) {
	c, p := start(t)
	c.add(10 * time.Millisecond)
	endOuter := Begin("outer")
	c.add(5 * time.Millisecond)
	endInner := Begin("inner")
	c.add(20 * time.Millisecond)
	endInner()
	c.add(5 * time.Millisecond)
	endOuter()
	Event("ready")
	if _, err := Flush(); err != nil {
		t.Fatal(err)
	}
	d, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Transitions) != 1 || d.Transitions[0].Name != "start" {
		t.Fatalf("transitions = %+v", d.Transitions)
	}
	got := map[string]Span{}
	for _, s := range d.Transitions[0].Spans {
		got[s.Name] = s
	}
	if o := got["outer"]; o.StartMS != 10 || o.DurMS != 30 || o.Depth != 0 {
		t.Errorf("outer = %+v", o)
	}
	if i := got["inner"]; i.StartMS != 15 || i.DurMS != 20 || i.Depth != 1 {
		t.Errorf("inner = %+v", i)
	}
	if r := got["ready"]; !r.Instant || r.StartMS != 40 {
		t.Errorf("ready = %+v", r)
	}
	if d.Transitions[0].TotalMS != 40 {
		t.Errorf("total = %v, want 40", d.Transitions[0].TotalMS)
	}
}

// Idle time between transitions is not part of either: the user typing
// between /new and the next command must never inflate a total.
func TestTransitionsExcludeIdleTime(t *testing.T) {
	c, p := start(t)
	end := Begin("work")
	c.add(100 * time.Millisecond)
	end()
	c.add(60 * time.Second) // the user is thinking
	Enter("/new")
	end = Begin("rotate")
	c.add(50 * time.Millisecond)
	end()
	if _, err := Flush(); err != nil {
		t.Fatal(err)
	}
	d, _ := Load(p)
	if len(d.Transitions) != 2 || d.Transitions[0].TotalMS != 100 || d.Transitions[1].TotalMS != 50 {
		t.Fatalf("totals = %+v", d.Transitions)
	}
	if d.Transitions[1].Spans[0].StartMS != 0 {
		t.Errorf("a new transition's offsets restart at 0: %+v", d.Transitions[1].Spans[0])
	}
}

// A phase that ends after the next transition began still lands in the
// transition it started in (a slow close finishing during the next /new).
func TestSpanBelongsToItsOwnTransition(t *testing.T) {
	c, p := start(t)
	end := Begin("late")
	Enter("/new")
	c.add(30 * time.Millisecond)
	end()
	if _, err := Flush(); err != nil {
		t.Fatal(err)
	}
	d, _ := Load(p)
	if len(d.Transitions[0].Spans) != 1 || d.Transitions[0].Spans[0].Name != "late" || len(d.Transitions[1].Spans) != 0 {
		t.Fatalf("spans = %+v / %+v", d.Transitions[0].Spans, d.Transitions[1].Spans)
	}
}

func TestEndIsIdempotent(t *testing.T) {
	c, p := start(t)
	end := Begin("once")
	c.add(time.Millisecond)
	end()
	end()
	end()
	_, _ = Flush()
	d, _ := Load(p)
	if n := len(d.Transitions[0].Spans); n != 1 {
		t.Fatalf("%d spans after three ends, want 1", n)
	}
}

func TestConcurrentPhasesAreMarkedAndDoNotNest(t *testing.T) {
	c, p := start(t)
	var wg sync.WaitGroup
	for _, n := range []string{"a", "b", "c"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer BeginConcurrent("probe " + n)()
		}()
	}
	wg.Wait()
	c.add(time.Millisecond)
	end := Begin("after")
	end()
	_, _ = Flush()
	d, _ := Load(p)
	for _, s := range d.Transitions[0].Spans {
		if strings.HasPrefix(s.Name, "probe") && (!s.Concurrent || s.Depth != 0) {
			t.Errorf("%+v", s)
		}
		if s.Name == "after" && s.Depth != 0 {
			t.Errorf("a finished concurrent phase left nesting behind: %+v", s)
		}
	}
}

func TestFlushTwiceKeepsAllSpans(t *testing.T) {
	c, p := start(t)
	end := Begin("one")
	c.add(time.Millisecond)
	end()
	_, _ = Flush()
	end = Begin("two")
	c.add(time.Millisecond)
	end()
	_, _ = Flush()
	d, _ := Load(p)
	if len(d.Transitions[0].Spans) != 2 {
		t.Fatalf("spans = %+v", d.Transitions[0].Spans)
	}
	if _, err := os.Stat(p + ".tmp"); !os.IsNotExist(err) {
		t.Error("the temp file must not remain")
	}
}

func TestRaceSafe(t *testing.T) {
	_, _ = start(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				Begin("x")()
				BeginConcurrent("y")()
				Event("z")
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 20; j++ {
			_, _ = Flush()
		}
	}()
	wg.Wait()
}

func TestTopFiveSkipsParents(t *testing.T) {
	tr := Transition{Name: "start", Spans: []Span{
		{Name: "parent", StartMS: 0, DurMS: 100, Depth: 0},
		{Name: "child-a", StartMS: 0, DurMS: 60, Depth: 1},
		{Name: "child-b", StartMS: 60, DurMS: 30, Depth: 1},
		{Name: "solo", StartMS: 100, DurMS: 40, Depth: 0},
		{Name: "tiny1", StartMS: 140, DurMS: 1},
		{Name: "tiny2", StartMS: 141, DurMS: 2},
		{Name: "tiny3", StartMS: 143, DurMS: 3},
		{Name: "mark", StartMS: 5, Instant: true},
	}}
	got := TopFive(tr)
	if len(got) != 5 || got[0].Name != "child-a" || got[1].Name != "solo" || got[2].Name != "child-b" {
		t.Fatalf("TopFive = %+v", got)
	}
	for _, s := range got {
		if s.Name == "parent" || s.Instant {
			t.Errorf("a parent or an instant crowded the list: %+v", s)
		}
	}
}

func TestRenderAndDefaultPath(t *testing.T) {
	c, p := start(t)
	SetMeta("v", "repo", "host")
	end := Detail("config load", "/etc/x")
	c.add(12 * time.Millisecond)
	end()
	_, _ = Flush()
	d, _ := Load(p)
	var b bytes.Buffer
	Render(&b, d)
	for _, want := range []string{"start  at +0.0 ms  total 12.0 ms", "config load", "12.0 ms", "[/etc/x]", "top five"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("render lacks %q:\n%s", want, b.String())
		}
	}
	t.Setenv("XDG_DATA_HOME", "/xdg")
	got := DefaultPath("", time.Date(2026, 11, 8, 1, 2, 3, 0, time.UTC))
	if !strings.HasPrefix(got, "/xdg/wakil/traces/timeline-20261108T010203Z-") || !strings.HasSuffix(got, ".json") {
		t.Errorf("DefaultPath = %q", got)
	}
}

func TestLoadRefusesAnUnknownSchema(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.json")
	_ = os.WriteFile(p, []byte(`{"schema":"other/v1"}`), 0o600)
	if _, err := Load(p); err == nil {
		t.Fatal("an unknown schema must be refused")
	}
}

func TestAccumSumsManyCallsIntoOneSpan(t *testing.T) {
	c, p := start(t)
	for i := 0; i < 40; i++ {
		end := Accum("canonicalise")
		c.add(250 * time.Microsecond)
		end()
	}
	end := Begin("real work")
	c.add(5 * time.Millisecond)
	end()
	_, _ = Flush()
	d, _ := Load(p)
	var acc, work int
	for _, s := range d.Transitions[0].Spans {
		switch s.Name {
		case "canonicalise":
			acc++
			if s.DurMS != 10 || !s.Accumulated || !strings.Contains(s.Detail, "40 calls") {
				t.Errorf("accumulated span = %+v", s)
			}
		case "real work":
			work++
		}
	}
	if acc != 1 || work != 1 {
		t.Fatalf("accumulated spans %d, work spans %d, want 1 and 1", acc, work)
	}
	// The sum of calls must not stretch the total past the wall-clock end.
	if d.Transitions[0].TotalMS != 15 {
		t.Errorf("total = %v, want 15 (10 ms of calls then 5 ms of work)", d.Transitions[0].TotalMS)
	}
}

func TestEnterOnceDoesNotReopen(t *testing.T) {
	c, p := start(t)
	EnterOnce("exit")
	end := Begin("a")
	c.add(time.Millisecond)
	end()
	EnterOnce("exit")
	end = Begin("b")
	c.add(time.Millisecond)
	end()
	_, _ = Flush()
	d, _ := Load(p)
	if len(d.Transitions) != 2 || d.Transitions[1].Name != "exit" || len(d.Transitions[1].Spans) != 2 {
		t.Fatalf("transitions = %+v", d.Transitions)
	}
}

func TestSetDirKeepsTheFileName(t *testing.T) {
	_, p := start(t)
	dir := t.TempDir()
	SetDir(dir)
	if got := Path(); got != filepath.Join(dir, filepath.Base(p)) {
		t.Fatalf("Path = %q", got)
	}
	if _, err := Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.Base(p))); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultPathHonoursTraceDirEnv(t *testing.T) {
	t.Setenv("WAKIL_TRACE_DIR", "/custom/traces")
	if got := DefaultPath("", time.Unix(0, 0)); !strings.HasPrefix(got, "/custom/traces/timeline-") {
		t.Fatalf("DefaultPath = %q", got)
	}
}

func TestTransitionsCarryTheirAbsoluteStart(t *testing.T) {
	c, p := start(t)
	c.add(40 * time.Millisecond)
	Enter("/new")
	c.add(5 * time.Millisecond)
	Event("x")
	_, _ = Flush()
	d, _ := Load(p)
	if d.Transitions[0].AtMS != 0 || d.Transitions[1].AtMS != 40 {
		t.Fatalf("at_ms = %v, %v want 0, 40", d.Transitions[0].AtMS, d.Transitions[1].AtMS)
	}
}

func TestProcessStartIsNotAfterTheFallbackAndIsRecent(t *testing.T) {
	now := time.Now()
	got := ProcessStart(now)
	if got.After(now) {
		t.Fatalf("ProcessStart %v is after the fallback %v", got, now)
	}
	if now.Sub(got) > time.Hour {
		t.Fatalf("ProcessStart %v is implausibly old", got)
	}
	// A test binary has been running for a moment at least: the kernel start
	// time is at or before the first line of the test.
	if now.Sub(got) < 0 {
		t.Fatal("negative age")
	}
}

func TestProcessStartFallsBackWhenSkewed(t *testing.T) {
	future := time.Now().Add(-2 * time.Hour) // a fallback older than the process
	if got := ProcessStart(future); !got.Equal(future) {
		t.Fatalf("a process cannot predate its own fallback by hours: %v", got)
	}
}

func BenchmarkAccumOff(b *testing.B) {
	Reset()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		Accum("x")()
	}
}

func TestEnabledIsCheap(t *testing.T) {
	Reset()
	if n := testing.AllocsPerRun(100, func() { _ = Enabled() }); n != 0 {
		t.Fatalf("Enabled allocated %v times", n)
	}
	if n := testing.AllocsPerRun(100, func() { Accum("tui view")() }); n != 0 {
		t.Fatalf("a disabled Accum allocated %v times: it sits on the per-frame path", n)
	}
}
