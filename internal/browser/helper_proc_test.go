package browser

// Test helper: a process whose argv carries a marker WITHOUT any shell. The
// first host run of 0.4.4 (Fedora, /bin/sh = bash) showed that
// `sh -c 'sleep 300' marker` is not a stable way to get a marker into argv:
// bash execs a single command in place, so the marker disappears, while dash
// keeps the shell. The helper is this test binary re-executed, so its argv is
// exactly what the test passes, on every shell.

import (
	"os"
	osexec "os/exec"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

const helperEnv = "WAKIL_BROWSER_TEST_HELPER"

// TestHelperProcess is not a test: when helperEnv is set it becomes the
// sleeper. It exits when its parent goes away, so a crashed test run leaves
// nothing behind, and after at most two minutes regardless.
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv(helperEnv)
	if mode == "" {
		t.Skip("helper process, not a test")
	}
	if mode == "ignore-term" {
		signal.Ignore(syscall.SIGTERM)
	}
	parent := os.Getppid()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if os.Getppid() != parent {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	os.Exit(0)
}

// startHelper starts the helper with the given extra argv (after "--") and
// returns its pid. mode is "" for a plain sleeper, "ignore-term" for one that
// ignores SIGTERM. The process is killed and reaped on cleanup.
func startHelper(t *testing.T, mode string, argv ...string) int {
	t.Helper()
	args := append([]string{"-test.run=^TestHelperProcess$", "--"}, argv...)
	cmd := osexec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), helperEnv+"="+modeOrPlain(mode))
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start helper: %v", err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-done
	})
	return cmd.Process.Pid
}

func modeOrPlain(m string) string {
	if m == "" {
		return "plain"
	}
	return m
}

// fixtureSleep is the sleep argument the stop-script fixtures use. It is not a
// round number on purpose: another test (the hisn integration test greps the
// host for "sleep 300") must never see this package's fixtures as its own.
const fixtureSleep = "2718"

// reapFixtureSleepers kills fixture sleepers left behind when a stop-script
// test ends (the script kills what carries the profile dir; a bare `sleep`
// child of a killed shell does not carry it and would run out its 45 minutes).
func reapFixtureSleepers(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { _ = osexec.Command("pkill", "-KILL", "-f", "sleep "+fixtureSleep).Run() })
}
