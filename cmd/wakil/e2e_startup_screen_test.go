package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/treeol/wakil/internal/testutil"
)

func e2eLogoRows(frame string) int {
	n := 0
	for _, row := range strings.Split(frame, "\n") {
		if strings.Contains(row, "ctx") {
			continue
		}
		if strings.IndexFunc(row, func(r rune) bool { return r >= 0x2800 && r <= 0x28FF }) >= 0 {
			n++
		}
	}
	return n
}

// The real runTUI path with a slow bootstrap: the logo is whole in every frame
// from the first painted one until the first key, one placeholder line in each,
// and the whole logo area is empty after the first key. Also checks that
// writing to os.Stderr after the swap does not reach the alt-screen terminal
// (a post-swap stderr write used to scroll the alt-screen by one row and leave
// the top braille row, stale status, and a duplicate placeholder behind).
func TestE2EStartupLogoUntilFirstKey(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(termenv.Ascii) })
	vs := testutil.NewVScreen(95, 30)
	e := startE2E(t, e2eOpts{width: 95, height: 30, tee: vs, bootDelay: 700 * time.Millisecond, earlySize: true})
	e.waitScreen("type a task", 30*time.Second)
	e2eReady(t, e)
	e.waitProbes(30 * time.Second)
	time.Sleep(300 * time.Millisecond)

	// Confirm os.Stderr stays redirected away from the terminal while the TUI
	// is still running after the swap (e.g. deferred hisn reconcile errors).
	_, _ = fmt.Fprintln(os.Stderr, "simulated late stderr line during tui")
	time.Sleep(50 * time.Millisecond)
	if _, stderr := e.outOfBand(); strings.Contains(stderr, "simulated late stderr line") {
		t.Fatalf("post-swap os.Stderr write leaked to the terminal:\n%s", stderr)
	}

	painted := 0
	for i, f := range vs.AllFrames() {
		if !strings.Contains(f, "type a task") && e2eLogoRows(f) == 0 {
			continue // before the first window size
		}
		painted++
		if n := e2eLogoRows(f); n != 20 {
			t.Fatalf("frame %d: logo has %d rows, want 20:\n%s", i, n, f)
		}
		if n := strings.Count(f, "type a task"); n != 1 {
			t.Fatalf("frame %d: %d placeholder lines, want 1:\n%s", i, n, f)
		}
	}
	if painted < 2 {
		t.Fatalf("only %d painted frames; the start-up view was not on screen while booting", painted)
	}

	before := vs.NFrames()
	e.typeText("x")
	deadline := time.Now().Add(5 * time.Second)
	for e2eLogoRows(vs.Screen()) != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	for i, f := range vs.AllFrames()[before:] {
		if n := e2eLogoRows(f); n != 0 && n != 20 {
			t.Fatalf("frame %d after the key: partial logo (%d rows):\n%s", i, n, f)
		}
	}
	if n := e2eLogoRows(vs.Screen()); n != 0 {
		t.Fatalf("logo not removed after the first key (%d rows):\n%s", n, vs.Screen())
	}
}
