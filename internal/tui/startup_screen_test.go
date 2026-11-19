package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/testutil"
)

// slowFacade is a fake backend that comes up slowly: its deferred start-up
// work (provider probes, sandbox session) finishes only when the test says so
// and then reports a start-up note, as a real session does when memory has
// pending proposals.
type slowFacade struct {
	*fakeFacade
	deferredGo chan struct{}
	note       string
}

func (f *slowFacade) RunDeferredStartup(done func(string)) bool {
	go func() {
		<-f.deferredGo
		done(f.note)
	}()
	return true
}

// startupRig runs the real tea.Program over the real start-up path
// (NewLoadingModel, the swap to NewTUIModelWithFacade, deferred start-up) with
// a terminal emulator as output. Bootstrap and deferred start-up are held
// until the test releases them.
type startupRig struct {
	t      *testing.T
	p      *tea.Program
	vs     *testutil.VScreen
	in     *lockedBuffer
	bootGo chan struct{}
	facade *slowFacade
	done   chan struct{}
	w, h   int
}

func newStartupRig(t *testing.T, w, h int) *startupRig {
	t.Helper()
	old := splashTruecolor
	splashTruecolor = func() bool { return true }
	t.Cleanup(func() { splashTruecolor = old })

	r := &startupRig{
		t: t, w: w, h: h,
		vs:     testutil.NewVScreen(w, h),
		in:     newLockedBuffer(),
		bootGo: make(chan struct{}),
		done:   make(chan struct{}),
		facade: &slowFacade{
			fakeFacade: &fakeFacade{sid: "s", chatID: "c"},
			deferredGo: make(chan struct{}),
			note:       "memory: 251 proposals pending",
		},
	}
	boot := func() tea.Msg { <-r.bootGo; return bootDone{} }
	swap := func(tea.Msg) (tea.Model, tea.Cmd) {
		return NewTUIModelWithFacade(r.facade, nil,
			core.Principal{TenantID: event.EmbeddedTenantID, UserID: event.EmbeddedUserID, Role: core.RoleOwner}), nil
	}
	r.p = tea.NewProgram(NewLoadingModel("creating session…", boot, swap),
		tea.WithInput(r.in), tea.WithOutput(r.vs), tea.WithAltScreen())
	old2 := programSend
	programSend = r.p.Send
	t.Cleanup(func() { programSend = old2 })
	go func() { _, _ = r.p.Run(); close(r.done) }()
	t.Cleanup(func() { r.p.Kill(); <-r.done })
	r.size(w, h)
	r.settle()
	return r
}

func (r *startupRig) size(w, h int) {
	r.w, r.h = w, h
	r.vs.Resize(w, h)
	r.p.Send(tea.WindowSizeMsg{Width: w, Height: h})
	r.settle()
}

// settle waits until the renderer has written nothing for a while.
func (r *startupRig) settle() {
	r.t.Helper()
	last := -1
	for i := 0; i < 100; i++ {
		time.Sleep(40 * time.Millisecond)
		n := r.vs.NFrames()
		if n == last {
			return
		}
		last = n
	}
	r.t.Fatal("renderer did not settle")
}

func (r *startupRig) key(s string) {
	r.p.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)})
	r.settle()
}

func (r *startupRig) finishBoot()     { close(r.bootGo); r.settle() }
func (r *startupRig) finishDeferred() { close(r.facade.deferredGo); r.settle() }

func isBraille(c rune) bool { return c >= 0x2800 && c <= 0x28FF }

// logoBlock returns the rows of a frame that carry logo braille (the status
// gauge also uses braille, so rows with "ctx" are not logo rows) and the
// index of the first such row.
func logoBlock(frame string) (rows []string, top int) {
	top = -1
	for y, row := range strings.Split(frame, "\n") {
		if strings.Contains(row, "ctx") {
			continue
		}
		if strings.IndexFunc(row, isBraille) >= 0 {
			if top < 0 {
				top = y
			}
			rows = append(rows, row)
		}
	}
	return rows, top
}

func placeholderLines(frame string) int {
	n := 0
	for _, row := range strings.Split(frame, "\n") {
		if strings.Contains(row, "type a task") {
			n++
		}
	}
	return n
}

// paintedFrames returns the frames from the first one that shows the input
// box on, so the "initializing" frame before the first window size is skipped.
func (r *startupRig) paintedFrames() []string {
	var out []string
	for _, f := range r.vs.AllFrames() {
		if len(out) == 0 && placeholderLines(f) == 0 {
			continue
		}
		out = append(out, f)
	}
	return out
}

// checkLogoStable fails on any frame whose logo differs from ref (partial,
// moved, missing) and on any frame without exactly one placeholder line.
func checkLogoStable(t *testing.T, frames []string, ref []string, refTop int, what string) {
	t.Helper()
	for i, f := range frames {
		rows, top := logoBlock(f)
		if len(rows) != len(ref) || top != refTop || strings.Join(rows, "\n") != strings.Join(ref, "\n") {
			t.Fatalf("%s: frame %d of %d: logo is not the reference (rows %d want %d, top %d want %d):\n%s",
				what, i, len(frames), len(rows), len(ref), top, refTop, f)
		}
		if n := placeholderLines(f); n != 1 {
			t.Fatalf("%s: frame %d of %d has %d placeholder lines, want 1:\n%s", what, i, len(frames), n, f)
		}
	}
}

// The logo stays, unchanged, through the swap to the real model and through
// the session becoming ready (start-up note arriving), until the first key.
func TestStartupLogoStaysUntilFirstKey(t *testing.T) {
	r := newStartupRig(t, 95, 30)
	first := r.paintedFrames()
	if len(first) == 0 {
		t.Fatal("no frame painted")
	}
	ref, refTop := logoBlock(first[len(first)-1])
	if len(ref) != 20 {
		t.Fatalf("reference logo has %d rows, want 20:\n%s", len(ref), first[len(first)-1])
	}
	r.p.Send(loadingProgressMsg{text: "setting up…"})
	r.settle()
	r.finishBoot()     // swap to the real model
	r.finishDeferred() // session ready, start-up note arrives
	checkLogoStable(t, r.paintedFrames(), ref, refTop, "before the first key")
	if strings.Contains(r.vs.Screen(), "creating session") {
		t.Errorf("status still reads creating session…:\n%s", r.vs.Screen())
	}

	// The first key removes the logo, whole, in one frame.
	nBefore := r.vs.NFrames()
	r.key("a")
	frames := r.vs.AllFrames()
	last := frames[len(frames)-1]
	if rows, _ := logoBlock(last); len(rows) != 0 {
		t.Fatalf("logo not removed after the first key (%d rows left):\n%s", len(rows), last)
	}
	if n := placeholderLines(last); n != 0 {
		t.Errorf("placeholder still shown after typing: %d", n)
	}
	// No frame between the last full-logo frame and the empty one is partial.
	seenEmpty := false
	for _, f := range frames[nBefore-1:] {
		rows, _ := logoBlock(f)
		if len(rows) == 0 {
			seenEmpty = true
		} else if seenEmpty {
			t.Fatalf("logo came back after it was removed:\n%s", f)
		} else if len(rows) != len(ref) {
			t.Fatalf("partial logo (%d of %d rows):\n%s", len(rows), len(ref), f)
		}
	}
	// The note held back behind the logo is shown once the logo is gone.
	if !strings.Contains(last, "251 proposals pending") {
		t.Errorf("start-up note not shown after the logo was removed:\n%s", last)
	}

	// Also check at height 29 where the old 1-row boot status shifted vpH from
	// 21 to 22 and moved the logo down one row on swap.
	r2 := newStartupRig(t, 95, 29)
	first2 := r2.paintedFrames()
	ref2, refTop2 := logoBlock(first2[len(first2)-1])
	if len(ref2) != 20 {
		t.Fatalf("95x29 reference logo has %d rows, want 20", len(ref2))
	}
	r2.finishBoot()
	r2.finishDeferred()
	checkLogoStable(t, r2.paintedFrames(), ref2, refTop2, "95x29 before first key")
}

// Typing before the session is ready removes the logo, and the swap and the
// session becoming ready do not bring it back or leave cells behind.
func TestStartupTypingBeforeReady(t *testing.T) {
	r := newStartupRig(t, 95, 30)
	r.key("h")
	if rows, _ := logoBlock(r.vs.Screen()); len(rows) != 0 {
		t.Fatalf("logo still shown after the first key:\n%s", r.vs.Screen())
	}
	r.finishBoot()
	r.finishDeferred()
	for i, f := range r.vs.AllFrames()[len(r.vs.AllFrames())-3:] {
		if rows, _ := logoBlock(f); len(rows) != 0 {
			t.Fatalf("logo back after the swap (frame -%d):\n%s", 3-i, f)
		}
	}
	if !strings.Contains(r.vs.Screen(), "h") || placeholderLines(r.vs.Screen()) != 0 {
		t.Errorf("typed text lost or placeholder shown:\n%s", r.vs.Screen())
	}
}

// A resize during start-up (before and after the swap) keeps one logo, one
// placeholder, and nothing left over from the old size.
func TestStartupResize(t *testing.T) {
	r := newStartupRig(t, 95, 30)
	r.size(110, 34)
	f := r.vs.Screen()
	ref, top := logoBlock(f)
	if len(ref) != 20 {
		t.Fatalf("after resize: logo has %d rows, want 20:\n%s", len(ref), f)
	}
	r.finishBoot()
	r.finishDeferred()
	checkLogoStable(t, r.vs.AllFrames()[len(r.vs.AllFrames())-1:], ref, top, "after swap at the new size")
	r.size(100, 32)
	rows, _ := logoBlock(r.vs.Screen())
	if len(rows) != 20 || placeholderLines(r.vs.Screen()) != 1 {
		t.Fatalf("after a resize post-swap: logo rows %d placeholders %d:\n%s", len(rows), placeholderLines(r.vs.Screen()), r.vs.Screen())
	}
}
