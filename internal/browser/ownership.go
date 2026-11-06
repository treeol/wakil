package browser

// Ownership checks before signalling a process (0.4.4, review R2; pidfd added
// after the first host run).
//
// A pid alone identifies nothing after a reuse. Where the kernel has pidfds
// (Linux >= 5.3) the identity is a pidfd taken at spawn: it names one process
// for its whole life and the signal goes THROUGH it, so a reused pid can never
// be hit. The recorded start time (field 22 of /proc/<pid>/stat) is checked as
// well, and the cmdline (the profile dir, or for the host-side `docker exec`
// client the container name) is an extra guard where it can be read. On any
// mismatch we do not kill; we log one line and drop the record.

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

	fd    int  // pidfd taken at record time; valid only when hasFD
	hasFD bool // false where the kernel offers no pidfd
}

// recordHostProcess captures pid's identity right after it was started. It
// returns ok=false when the process is already gone (nothing to own).
//
// The pidfd is opened BEFORE the start time is read and the start time is read
// again afterwards: if the pid changed hands in between, the two reads differ
// and nothing is recorded. The caller must end the record with killOwned or
// release, or the descriptor stays open.
func recordHostProcess(pid int, marker string) (hostProcess, bool) {
	if pid <= 0 {
		return hostProcess{}, false
	}
	fd := pidfdOpen(pid)
	st := procStartTime(pid)
	if st == "" || (fd >= 0 && procStartTime(pid) != st) {
		if fd >= 0 {
			pidfdClose(fd)
		}
		return hostProcess{}, false
	}
	return hostProcess{Pid: pid, StartTime: st, Marker: marker, fd: fd, hasFD: fd >= 0}, true
}

// release closes the pidfd without signalling. Safe on a record without one.
func (h hostProcess) release() {
	if h.hasFD {
		pidfdClose(h.fd)
	}
}

// owns reports whether the recorded process is still the same one.
func (h hostProcess) owns() (bool, string) {
	if h.Pid <= 0 {
		return false, "no pid recorded"
	}
	if h.hasFD {
		// Signal 0 through the pidfd: ESRCH means the process we recorded has
		// exited (whatever the pid was reused for since is not ours).
		if err := pidfdSignal(h.fd, 0); err != nil {
			return false, "process already gone"
		}
	}
	st := procStartTime(h.Pid)
	if st == "" {
		return false, "process already gone"
	}
	if st != h.StartTime {
		return false, fmt.Sprintf("pid %d was reused (start time %s, recorded %s)", h.Pid, st, h.StartTime)
	}
	// Extra guard, where the cmdline can be read: an unreadable or empty
	// cmdline (a zombie, or no /proc) does not veto what the pidfd and the
	// start time already proved.
	if cl := procCmdline(h.Pid); h.Marker != "" && cl != "" && !strings.Contains(cl, h.Marker) {
		return false, fmt.Sprintf("pid %d no longer carries %q in its cmdline", h.Pid, h.Marker)
	}
	return true, ""
}

// killOwned signals pid only when the recorded identity still holds, through
// the pidfd when there is one. A mismatch logs one line through logf and does
// not kill. It ends the record (closes the pidfd) and returns whether the
// signal was sent.
func killOwned(h hostProcess, logf func(format string, args ...any)) bool {
	defer h.release()
	ok, why := h.owns()
	if !ok {
		if logf != nil && why != "process already gone" {
			logf("browser: not signalling pid %d: %s", h.Pid, why)
		}
		return false
	}
	if h.hasFD {
		return pidfdSignal(h.fd, syscall.SIGKILL) == nil
	}
	p, err := os.FindProcess(h.Pid)
	if err != nil {
		return false
	}
	return p.Kill() == nil
}
