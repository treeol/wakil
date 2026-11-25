package hisn

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// The open-mode kernel floor. Hisn 0.6.2+ enforces open-mode network policy at
// the supervisor (every listen and INET connect/bind goes through it) via
// Landlock TCP rules, which need ABI v4 — kernel 6.7+. Wakil checks the host
// kernel release at Preflight when the session selects network = "open" and
// names the requirement here, instead of letting the supervisor refuse the
// session at Start. HOST mode is never gated: only open needs ABI v4.

// openMinKernel is the lowest kernel release whose Landlock implements ABI v4.
var openMinKernel = [2]int{6, 7}

// hostKernelRelease returns the host kernel release string (the same text
// `uname -r` prints), or "" when it cannot be read.
func hostKernelRelease() string {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// parseKernelRelease parses a kernel release string ("7.3.4-300.fc45.x86_64",
// "6.7.0") into its leading major.minor. ok is false when the string does not
// start with "<major>.<minor>".
func parseKernelRelease(release string) (major, minor int, ok bool) {
	s := strings.TrimSpace(release)
	// Major is the digits before the first dot.
	dot := strings.IndexByte(s, '.')
	if dot < 0 {
		return 0, 0, false
	}
	majStr := s[:dot]
	if majStr == "" || !allDigits(majStr) {
		return 0, 0, false
	}
	rest := s[dot+1:]
	// Minor is the digits at the start of rest.
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, 0, false
	}
	major, _ = strconv.Atoi(majStr)
	minor, _ = strconv.Atoi(rest[:end])
	return major, minor, true
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// checkOpenKernelFloor gates network = "open" on the kernel floor. It returns
// nil for every non-open mode (including "") and for a host kernel >= 6.7. An
// unparseable or unreadable release fails CLOSED for open (Wakil never starts
// an open session it cannot prove satisfies the floor) and names what it
// found; the error always names the 6.7 / Landlock ABI v4 requirement.
func checkOpenKernelFloor(release, mode string) error {
	if mode != "open" {
		return nil
	}
	major, minor, ok := parseKernelRelease(release)
	if !ok {
		if strings.TrimSpace(release) == "" {
			return fmt.Errorf("sandbox backend hisn: host kernel release is unreadable; network = \"open\" needs kernel 6.7+ (Landlock ABI v4) — switch back with /network host or sandbox.hisn.network_mode = \"host\"")
		}
		return fmt.Errorf("sandbox backend hisn: cannot parse host kernel release %q; network = \"open\" needs kernel 6.7+ (Landlock ABI v4) — switch back with /network host or sandbox.hisn.network_mode = \"host\"", release)
	}
	if major > openMinKernel[0] || (major == openMinKernel[0] && minor >= openMinKernel[1]) {
		return nil
	}
	return fmt.Errorf("sandbox backend hisn: host kernel %s is too old for network = \"open\" (needs kernel 6.7+, Landlock ABI v4) — switch back with /network host or sandbox.hisn.network_mode = \"host\"", release)
}
