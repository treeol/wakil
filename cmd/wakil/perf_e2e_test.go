package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/tui"
)

// TestTUIPerfSummaryRealProgram drives runTUI end to end (real
// config, bootstrap, real tuiModel through a real tea.Program)
// with WAKIL_TUI_PERF=1, sends a couple of keys, quits with
// /exit, and asserts:
//   - the three-line summary reached stderr (after the terminal is
//     restored) and names the diagnostic log file;
//   - that log file contains the per-frame tuiperf lines.
//
// Why not a subprocess + pty: the e2e harness runs the same main()
// path (LoadConfig → bootstrap → runTUI → postRunCleanup) with
// real stdio pipes, which preserves every property the user
// reported missing (no summary on stdout/stderr, no log lines)
// without a 40MB test binary. RefreshPerfFromEnv (called by
// runTUI) reads the switch per run, so the env var set here before
// startE2E is honored.
func TestTUIPerfSummaryRealProgram(t *testing.T) {
	t.Setenv("WAKIL_TUI_PERF", "1")
	t.Cleanup(tui.RefreshPerfFromEnv) // restore switch after this test process

	e := startE2E(t, e2eOpts{})
	e.waitScreen("type a task", 20*time.Second)
	e.submit("/exit")
	if !e.waitExit(10 * time.Second) {
		t.Fatalf("TUI did not exit on /exit\n%s", e.tail(1000))
	}
	_, stderr := e.outOfBand()
	for _, want := range []string{
		"tuiperf summary count=",
		"slowest=",
		"maxframe=",
		"log=",
		"tuiperf update median=",
		"tuiperf view median=",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr missing %q after quit:\n--- stderr ---\n%s\n--- tail ---\n%s", want, stderr, e.tail(500))
		}
	}
	// The log path named in the summary must exist and carry the
	// per-frame lines.
	logPath := ""
	if i := strings.Index(stderr, "log="); i >= 0 {
		rest := stderr[i+len("log="):]
		if j := strings.IndexByte(rest, '\n'); j >= 0 {
			logPath = strings.TrimSpace(rest[:j])
		}
	}
	if logPath == "" {
		t.Fatalf("no log= path in stderr:\n%s", stderr)
	}
	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("named log path %s unreadable: %v\nstderr:\n%s", logPath, err, stderr)
	}
	if !strings.Contains(string(body), "tuiperf WindowSizeMsg") && !strings.Contains(string(body), "tuiperf KeyMsg") && !strings.Contains(string(body), "tuiperf view") {
		t.Fatalf("log %s lacks per-frame tuiperf lines:\n%.500s", logPath, string(body))
	}
}
