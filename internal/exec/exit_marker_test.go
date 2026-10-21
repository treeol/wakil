package exec

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ─── WrapExitMarker / ParseExitMarker (exit-code follow-up) ──────

func TestWrapExitMarkerShape(t *testing.T) {
	got, nonce := WrapExitMarker("echo hi", "/tmp/x.log")
	if len(nonce) != 16 {
		t.Errorf("nonce length = %d, want 16 hex chars", len(nonce))
	}
	// Subshell wrapper + code capture + marker append + original exit code.
	for _, want := range []string{"( echo hi", "__wakil_rc=$?", "[wakil-exit:", ">> '/tmp/x.log'", "exit $__wakil_rc", nonce} {
		if !strings.Contains(got, want) {
			t.Errorf("wrapper missing %q: %s", want, got)
		}
	}
}

func TestParseExitMarkerRoundTrip(t *testing.T) {
	nonce := "abcdef0123456789"
	mk := func(code int) string {
		return fmt.Sprintf("[wakil-exit:%d:%s]", code, nonce)
	}
	cases := []struct {
		name     string
		in       string
		wantOut  string
		wantCode int
		wantOK   bool
	}{
		{"success with output", "hello\n\n" + mk(0) + "\n", "hello\n", 0, true},
		{"non-zero", "boom\n" + mk(3) + "\n", "boom", 3, true},
		{"trailing whitespace after marker", "x\n" + mk(7) + "\n\n", "x", 7, true},
		{"empty output + zero", "\n" + mk(0) + "\n", "", 0, true},
		{"no marker", "just output\n", "just output\n", 0, false},
		{"marker mid-output only", "a" + mk(5) + "b\nreal output\n", "a" + mk(5) + "b\nreal output\n", 0, false},
		{"output AFTER marker (self-bg child)", "x\n" + mk(0) + "\nlate child output\n", "x\n" + mk(0) + "\nlate child output\n", 0, false},
		// Forged marker with wrong nonce must NOT parse.
		{"wrong nonce", "x\n[wakil-exit:0:deadbeefdeadbeef]\n", "x\n[wakil-exit:0:deadbeefdeadbeef]\n", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cleaned, code, ok := ParseExitMarker(tc.in, nonce)
			if ok != tc.wantOK || code != tc.wantCode || cleaned != tc.wantOut {
				t.Errorf("ParseExitMarker(%q, %q) = (%q, %d, %v), want (%q, %d, %v)",
					tc.in, nonce, cleaned, code, ok, tc.wantOut, tc.wantCode, tc.wantOK)
			}
		})
	}
}

// A spoofed marker that lands LAST in the log parses as genuine — the sentinel
// is not authenticatable by design (documented in exit_marker.go). The test
// pins the behavior so any future change is deliberate, and verifies that a
// mid-output spoof does NOT hide the real trailing marker.
func TestParseExitMarkerSpoofBoundaries(t *testing.T) {
	nonce := "abcdef0123456789"
	mk := func(code int) string { return fmt.Sprintf("[wakil-exit:%d:%s]", code, nonce) }
	// End-anchored: spoofed final marker with the RIGHT nonce parses (accepted limitation).
	_, code, ok := ParseExitMarker("real stuff\n"+mk(0)+"\n", nonce)
	if !ok || code != 0 {
		t.Fatalf("end-anchored marker must parse: (%d, %v)", code, ok)
	}
	// Mid-output spoof does not shadow the real marker.
	cleaned, code, ok := ParseExitMarker("lie"+mk(99)+"here\n"+mk(2)+"\n", nonce)
	if !ok || code != 2 {
		t.Fatalf("real trailing marker must win: (%d, %v)", code, ok)
	}
	if !strings.Contains(cleaned, "lie"+mk(99)+"here") {
		t.Fatalf("mid-output content must survive cleaning: %q", cleaned)
	}
	// A forged marker with a WRONG nonce at EOF must NOT parse — defense-in-depth.
	_, _, ok = ParseExitMarker("x\n[wakil-exit:0:ffffffffffffffff]\n", nonce)
	if ok {
		t.Fatal("forged marker with wrong nonce must not parse")
	}
}

// Trailing blank lines of REAL output must survive marker removal (only the
// wrapper's own newline is stripped).
func TestParseExitMarkerPreservesTrailingBlankLines(t *testing.T) {
	nonce := "abcdef0123456789"
	cleaned, _, ok := ParseExitMarker("a\n\n\n[wakil-exit:0:"+nonce+"]\n", nonce)
	if !ok {
		t.Fatal("marker must parse")
	}
	if cleaned != "a\n\n" {
		t.Errorf("trailing blank lines lost: %q", cleaned)
	}
}

// ACCEPTANCE (the live smoke-test gap): the exact command that previously
// returned "(no output)" with no error — `sleep 0.1; exit 3` — must now
// produce a parseable exit code when run through the REAL shell via
// StartBackground. Runs the wrapped command for real; no mocks.
func TestWrapExitMarkerEndToEnd(t *testing.T) {
	ex, _ := newDirectExec(t)
	ctx := context.Background()

	cases := []struct {
		command  string
		wantCode int
	}{
		{"sleep 0.1; exit 3", 3},         // the smoke-test failure case
		{"echo hello; exit 0", 0},        // success with output
		{"exit 5", 5},                    // bare exit in the command
		{"echo hi; echo there", 0},       // normal multi-line output
		{"grep -q nothere /dev/null", 1}, // informational non-zero (grep no match)
		{"echo hi \\", 0},                // trailing backslash (line continuation merges paren — verified safe)
	}
	for i, tc := range cases {
		logPath := filepath.Join(t.TempDir(), "bg.log")
		wrapped, nonce := WrapExitMarker(tc.command, logPath)
		pid, _, err := ex.StartBackground(ctx, wrapped, logPath)
		if err != nil {
			t.Fatalf("case %d (%q): StartBackground: %v", i, tc.command, err)
		}
		// Wait for the process group to finish (bounded).
		deadline := time.Now().Add(10 * time.Second)
		for ex.IsProcessGroupAlive(ctx, pid) && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		if ex.IsProcessGroupAlive(ctx, pid) {
			ex.KillPgid(ctx, pid, 9)
			t.Fatalf("case %d (%q): process did not finish in 10s", i, tc.command)
		}
		out, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatalf("case %d (%q): read log: %v", i, tc.command, err)
		}
		cleaned, code, ok := ParseExitMarker(string(out), nonce)
		if !ok {
			t.Fatalf("case %d (%q): no exit marker in log %q", i, tc.command, string(out))
		}
		if code != tc.wantCode {
			t.Errorf("case %d (%q): exit code = %d, want %d", i, tc.command, code, tc.wantCode)
		}
		// The marker itself must be stripped from what the model sees.
		if strings.Contains(cleaned, "[wakil-exit:") {
			t.Errorf("case %d (%q): marker leaked into cleaned output %q", i, tc.command, cleaned)
		}
	}
}

// A SIGKILLed process leaves no marker — ParseExitMarker must report
// ok=false so callers fail CLOSED (never silent success).
func TestWrapExitMarkerKilledProcessFailsClosed(t *testing.T) {
	ex, _ := newDirectExec(t)
	ctx := context.Background()
	logPath := filepath.Join(t.TempDir(), "bg.log")
	wrapped, nonce := WrapExitMarker("sleep 30", logPath)
	pid, _, err := ex.StartBackground(ctx, wrapped, logPath)
	if err != nil {
		t.Fatal(err)
	}
	// SIGKILL the group before the wrapper can write the marker.
	time.Sleep(200 * time.Millisecond)
	if err := ex.KillPgid(ctx, pid, 9); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for ex.IsProcessGroupAlive(ctx, pid) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	out, _ := os.ReadFile(logPath)
	if _, _, ok := ParseExitMarker(string(out), nonce); ok {
		t.Fatalf("killed process must leave no parseable marker, log: %q", string(out))
	}
}

// TestProcGroupAliveScriptHostSide exercises the EXACT script the docker
// executor uses (procGroupAliveScript) against the host's own /proc. This
// pins the field positions ($1=state, $3=pgrp after comm strip) under CI —
// the oracle-mandated coverage for the critical busybox-compat fix.
func TestProcGroupAliveScriptHostSide(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc on this host")
	}
	// Start a real process group: a backgrounded sleep whose pgid we read
	// from /proc/<pid>/stat (field 5, i.e. index 2 after the comm strip).
	// Setpgid gives it its OWN group — without it the child inherits the
	// test process's group and the probe can never report it gone.
	ctx := context.Background()
	cmd := exec.Command("sh", "-c", "sleep 2")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	defer func() { _ = cmd.Process.Kill() }()
	pgid := readPgrp(t, pid)
	if pgid != pid {
		t.Fatalf("expected fresh group (pgid==pid), got pgid=%d pid=%d", pgid, pid)
	}

	// While the process runs, the probe must say alive.
	out, err := exec.CommandContext(ctx, "sh", "-c", procGroupAliveScript(pgid)).CombinedOutput()
	if err != nil {
		t.Fatalf("probe failed: %v (%s)", err, out)
	}
	if strings.TrimSpace(string(out)) != "yes" {
		t.Fatalf("live group not detected: %q", out)
	}

	// Kill it; after reaping the group must be reported gone.
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out, err := exec.CommandContext(ctx, "sh", "-c", procGroupAliveScript(pgid)).CombinedOutput()
		if err == nil && strings.TrimSpace(string(out)) != "yes" {
			return // group gone — pass
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("group still reported alive 5s after kill (possible pgid reuse — unlikely)")
}

func readPgrp(t *testing.T, pid int) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("/proc", fmt.Sprintf("%d", pid), "stat"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	i := strings.LastIndex(s, ") ")
	if i < 0 {
		t.Fatalf("bad stat line: %q", s)
	}
	fields := strings.Fields(s[i+2:])
	if len(fields) < 3 {
		t.Fatalf("too few fields: %q", s)
	}
	pgrp, err := strconv.Atoi(fields[2])
	if err != nil {
		t.Fatal(err)
	}
	return pgrp
}
