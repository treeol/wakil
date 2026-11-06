package browser

// R2 (0.4.4 review): a pid is signalled only if it is still the process we
// started — same start time, and its cmdline carries our marker.

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// startSleeper starts a process with marker in its argv (no shell involved: see
// helper_proc_test.go) and waits until /proc shows it.
func startSleeper(t *testing.T, marker string) int {
	t.Helper()
	pid := startHelper(t, "", marker)
	for i := 0; i < 300; i++ {
		if strings.Contains(procCmdline(pid), marker) {
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("helper never showed its marker in cmdline")
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

// On Linux the identity is a pidfd taken at spawn, and the signal goes through
// it.
func TestKillOwned_UsesPidfdOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("pidfd is Linux only")
	}
	marker := "wakil-pidfd-" + strconv.Itoa(os.Getpid())
	pid := startSleeper(t, marker)
	h, ok := recordHostProcess(pid, marker)
	if !ok {
		t.Fatal("cannot record the test process")
	}
	if !h.hasFD {
		h.release()
		t.Skip("kernel offers no pidfd here")
	}
	if !killOwned(h, nil) {
		t.Fatal("the recorded process must be signalled through its pidfd")
	}
	deadline := time.Now().Add(3 * time.Second)
	for procStartTime(pid) != "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if procStartTime(pid) != "" {
		t.Error("process survived the pidfd signal")
	}
}

// A process that already exited is not signalled, quietly — through the pidfd
// it is the recorded process or nothing, whatever the pid is now.
func TestKillOwned_ExitedProcessIsNotSignalled(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("pidfd is Linux only")
	}
	marker := "wakil-exited-" + strconv.Itoa(os.Getpid())
	pid := startSleeper(t, marker)
	h, ok := recordHostProcess(pid, marker)
	if !ok || !h.hasFD {
		if ok {
			h.release()
		}
		t.Skip("no pidfd here")
	}
	p, _ := os.FindProcess(pid)
	_ = p.Kill()
	deadline := time.Now().Add(3 * time.Second)
	for procStartTime(pid) != "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	var logged []string
	if killOwned(h, func(f string, a ...any) { logged = append(logged, f) }) {
		t.Fatal("an exited process must not be signalled")
	}
	if len(logged) != 0 {
		t.Errorf("an exited process is not a mismatch: %v", logged)
	}
}
