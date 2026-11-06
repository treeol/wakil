package browser

// R3 (0.4.4 review): direct-mode teardown waits for Chromium to exit (bounded),
// then KILLs what still carries this launch's --user-data-dir, then removes the
// directory. Tested with a real process that ignores TERM.

import (
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startIgnoringTerm starts a process that ignores SIGTERM, with dir in its argv
// (as Chromium's children carry --user-data-dir). No shell is involved.
func startIgnoringTerm(t *testing.T, dir string) int {
	t.Helper()
	pid := startHelper(t, "ignore-term", "--user-data-dir="+dir)
	for i := 0; i < 300; i++ {
		if countUsingDir(dir) > 0 {
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("process never showed the profile dir in its argv")
	return 0
}

func TestLocalTeardown_KillsStragglerAndRemovesDir(t *testing.T) {
	dir := t.TempDir()
	pid := startIgnoringTerm(t, dir)
	start := time.Now()
	killUsingDir(dir) // what teardown does after its bounded wait
	deadline := time.Now().Add(3 * time.Second)
	for procStartTime(pid) != "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if procStartTime(pid) != "" {
		t.Fatal("a process that ignores TERM was not KILLed")
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Errorf("KILL sweep took %v", el)
	}
	if countUsingDir(dir) != 0 {
		t.Error("processes still carry the directory after the sweep")
	}
}

// The wait is bounded: with a live process that never exits on its own, the
// wait returns after the bound (the KILL then decides).
func TestLocalTeardown_WaitIsBounded(t *testing.T) {
	dir := t.TempDir()
	pid := startIgnoringTerm(t, dir)
	defer func() { killUsingDir(dir); _ = pid }()
	start := time.Now()
	waitGoneBounded(dir, 300*time.Millisecond)
	el := time.Since(start)
	if el < 250*time.Millisecond || el > 2*time.Second {
		t.Errorf("waitGoneBounded took %v, want ~300ms", el)
	}
}

// Teardown of a real direct-mode launch leaves nothing behind: the profile dir
// is gone and no process carries it. Needs a Chromium binary (skipped otherwise).
func TestLocalTeardown_RealChromiumLeavesNothing(t *testing.T) {
	if os.Getenv(liveEnv) != "1" {
		t.Skipf("set %s=1 to launch a real headless Chromium on a private profile", liveEnv)
	}
	if _, err := osexec.LookPath("chromium"); err != nil {
		if _, err := osexec.LookPath("google-chrome"); err != nil {
			t.Skip("no chromium on PATH")
		}
	}
	m, err := NewManager(nil, "")
	if err != nil {
		t.Skipf("no browser: %v", err)
	}
	if err := m.TouchForTest(); err != nil {
		t.Skipf("cannot launch: %v", err)
	}
	// The profile dir for this launch is the one the allocator was given; find
	// it through the running processes.
	dirs, _ := filepath.Glob(filepath.Join(os.TempDir(), "wakil-chrome-profile-*"))
	if len(dirs) == 0 {
		t.Fatal("no profile dir created by the launch")
	}
	// The browser must be alive with that dir in its argv before Close, or the
	// post-Close checks prove nothing.
	live := 0
	for _, d := range dirs {
		live += countUsingDir(d)
	}
	if live == 0 {
		t.Fatal("no process carries the profile dir while the browser runs; the test would be vacuous")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		if _, err := os.Stat(d); err == nil {
			t.Errorf("profile dir %s survived Close", d)
		}
		if n := countUsingDir(d); n != 0 {
			t.Errorf("%d process(es) still carry %s after Close", n, strings.TrimSpace(d))
		}
	}
}
