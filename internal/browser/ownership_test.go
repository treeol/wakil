package browser

// R2 (0.4.4 review): a pid is signalled only if it is still the process we
// started — same start time, and its cmdline carries our marker.

import (
	"os"
	osexec "os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// startSleeper runs `sleep` with marker in its argv and returns its pid and a
// reaper-free cleanup.
func startSleeper(t *testing.T, marker string) int {
	t.Helper()
	// sh keeps the marker in its argv: `sh -c 'sleep 300' <marker>` makes the
	// marker $0 and sleep is the foreground child.
	cmd := osexec.Command("sh", "-c", "sleep 300", marker)
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start sleep: %v", err)
	}
	go func() { _ = cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	// Wait until /proc shows it (exec has happened).
	for i := 0; i < 100; i++ {
		if strings.Contains(procCmdline(cmd.Process.Pid), marker) {
			return cmd.Process.Pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("sleeper never showed its marker in cmdline")
	return 0
}

func TestKillOwned_SignalsOnlyTheRecordedProcess(t *testing.T) {
	marker := "wakil-owntest-" + strconv.Itoa(os.Getpid())
	pid := startSleeper(t, marker)
	h, ok := recordHostProcess(pid, marker)
	if !ok {
		t.Fatal("cannot record the test process")
	}
	if ok, why := h.owns(); !ok {
		t.Fatalf("a freshly recorded process must be owned: %s", why)
	}
	var logged []string
	logf := func(f string, a ...any) { logged = append(logged, f) }
	if !killOwned(h, logf) {
		t.Fatal("owned process must be signalled")
	}
	deadline := time.Now().Add(3 * time.Second)
	for procStartTime(pid) != "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if procStartTime(pid) != "" {
		t.Error("owned process was not killed")
	}
}

// A different start time (pid reuse) is not ours: nothing is killed, one log line.
func TestKillOwned_DifferentStartTimeNotKilled(t *testing.T) {
	marker := "wakil-reuse-" + strconv.Itoa(os.Getpid())
	pid := startSleeper(t, marker)
	h, _ := recordHostProcess(pid, marker)
	h.StartTime = "1" // what the recorded identity would be if the pid had been reused
	var logged []string
	if killOwned(h, func(f string, a ...any) { logged = append(logged, f) }) {
		t.Fatal("must not signal a process whose start time differs")
	}
	if procStartTime(pid) == "" {
		t.Fatal("the process was killed despite the identity mismatch")
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "not signalling") {
		t.Errorf("expected exactly one log line, got %v", logged)
	}
}

// Same process, but its cmdline no longer carries our marker: not killed.
func TestKillOwned_MarkerMissingNotKilled(t *testing.T) {
	marker := "wakil-marker-" + strconv.Itoa(os.Getpid())
	pid := startSleeper(t, marker)
	h, _ := recordHostProcess(pid, marker+"-NOT-IN-ARGV")
	var logged []string
	if killOwned(h, func(f string, a ...any) { logged = append(logged, f) }) {
		t.Fatal("must not signal a process without our marker in its cmdline")
	}
	if procStartTime(pid) == "" {
		t.Fatal("the process was killed without its marker")
	}
	if len(logged) != 1 {
		t.Errorf("expected one log line, got %v", logged)
	}
}

// A vanished process is not "owned" and is not logged as a mismatch.
func TestKillOwned_GoneProcessIsQuiet(t *testing.T) {
	h := hostProcess{Pid: 2147483646, StartTime: "1", Marker: "x"}
	var logged []string
	if killOwned(h, func(f string, a ...any) { logged = append(logged, f) }) {
		t.Fatal("nothing to kill")
	}
	if len(logged) != 0 {
		t.Errorf("a gone process must not be logged as a mismatch: %v", logged)
	}
}

func TestProcStartTimeMatchesStatField22(t *testing.T) {
	st := procStartTime(os.Getpid())
	if st == "" {
		t.Fatal("no start time for our own process")
	}
	if _, err := strconv.ParseUint(st, 10, 64); err != nil {
		t.Errorf("start time %q is not numeric", st)
	}
}
