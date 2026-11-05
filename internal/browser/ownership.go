package browser

// Ownership checks before signalling a process (0.4.4, review R2).
//
// A pid alone identifies nothing after a reuse. Before we signal, we require
// the process to still be the one we started: its start time (field 22 of
// /proc/<pid>/stat) equals the value recorded at launch, and its cmdline
// carries this launch's profile dir (or, for the host-side `docker exec`
// client, the container name it was started with). On any mismatch we do not
// kill; we log one line and drop the record.

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// procStartTime returns field 22 of /proc/<pid>/stat (clock ticks since boot),
// or "" when the process does not exist. A reused pid has a different value.
func procStartTime(pid int) string {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return ""
	}
	s := string(b)
	i := strings.LastIndex(s, ") ")
	if i < 0 {
		return ""
	}
	fields := strings.Fields(s[i+2:])
	// fields[0] is state (field 3), so field 22 is fields[19].
	if len(fields) < 20 {
		return ""
	}
	return fields[19]
}

// procCmdline returns the NUL-separated argv of pid joined with spaces, or "".
func procCmdline(pid int) string {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return ""
	}
	return strings.ReplaceAll(strings.TrimRight(string(b), "\x00"), "\x00", " ")
}

// hostProcess is what a host-side pid was when it was started: its start
// time and a marker its cmdline must contain.
type hostProcess struct {
	Pid       int
	StartTime string // from procStartTime at launch
	Marker    string // must appear in procCmdline (e.g. the container name)
}

// recordHostProcess captures pid's identity right after it was started. It
// returns ok=false when the process is already gone (nothing to own).
func recordHostProcess(pid int, marker string) (hostProcess, bool) {
	st := procStartTime(pid)
	if pid <= 0 || st == "" {
		return hostProcess{}, false
	}
	return hostProcess{Pid: pid, StartTime: st, Marker: marker}, true
}

// owns reports whether the recorded process is still the same one.
func (h hostProcess) owns() (bool, string) {
	if h.Pid <= 0 {
		return false, "no pid recorded"
	}
	st := procStartTime(h.Pid)
	if st == "" {
		return false, "process already gone"
	}
	if st != h.StartTime {
		return false, fmt.Sprintf("pid %d was reused (start time %s, recorded %s)", h.Pid, st, h.StartTime)
	}
	if h.Marker != "" && !strings.Contains(procCmdline(h.Pid), h.Marker) {
		return false, fmt.Sprintf("pid %d no longer carries %q in its cmdline", h.Pid, h.Marker)
	}
	return true, ""
}

// killOwned signals pid only when the recorded identity still holds. A
// mismatch logs one line through logf and does not kill. It returns whether
// the signal was sent.
func killOwned(h hostProcess, logf func(format string, args ...any)) bool {
	ok, why := h.owns()
	if !ok {
		if logf != nil && why != "process already gone" {
			logf("browser: not signalling pid %d: %s", h.Pid, why)
		}
		return false
	}
	p, err := os.FindProcess(h.Pid)
	if err != nil {
		return false
	}
	return p.Kill() == nil
}
