package exec

import (
	"io"
	osexec "os/exec"
	"sync"
	"time"
)

// boundedTailBuffer captures a subprocess's combined output while retaining
// only the LAST cap bytes — retained memory is O(cap) regardless of output
// volume. Writes never fail: once the cap is reached the buffer evicts the
// oldest bytes, so the child's pipe always drains (no deadlock from a full
// pipe) and the command's exit behavior is unchanged by truncation. This is
// a shifting buffer, not a ring: over-cap writes memmove the retained window
// (write-amplified but bounded — acceptable at the 1 MB default with os/exec's
// 32 KiB copy chunks). Writes block only on mutex acquisition, never on the
// consumer.
//
// Semantics chosen per H4 plan review: TAIL retention (errors, test
// summaries, and exit diagnostics live at the end of output).
type boundedTailBuffer struct {
	mu      sync.Mutex
	cap     int64
	buf     []byte // retained tail, ≤ cap
	dropped int64  // exact count of bytes ever written (for omission math)
}

func newBoundedTailBuffer(capBytes int64) *boundedTailBuffer {
	if capBytes < 1 {
		capBytes = 1
	}
	return &boundedTailBuffer{cap: capBytes}
}

func (b *boundedTailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.dropped += int64(len(p))
	if int64(len(p)) >= b.cap {
		// A single write ≥ cap: keep only the tail of p.
		b.buf = append(b.buf[:0], p[int64(len(p))-b.cap:]...)
		return len(p), nil
	}
	if int64(len(b.buf))+int64(len(p)) > b.cap {
		drop := int64(len(b.buf)) + int64(len(p)) - b.cap
		b.buf = append(b.buf[:0], b.buf[drop:]...)
	}
	b.buf = append(b.buf, p...)
	return len(p), nil
}

// Snapshot returns the retained tail and the exact number of bytes discarded
// before the retained window (0 when everything fit).
func (b *boundedTailBuffer) Snapshot() (tail string, omitted int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf), b.dropped - int64(len(b.buf))
}

// runShellCapped runs cmd capturing combined stdout+stderr into a
// boundedTailBuffer of capBytes. It returns the retained tail, the exact
// number of omitted head bytes, and cmd.Run's error (*exec.ExitError
// preserved). cmd.Stdout and cmd.Stderr are set to the SAME writer value so
// os/exec serialises writes and interleaving is preserved (single dup'd pipe
// + one copy goroutine per GOROOT childStderr). Both streams drain to EOF
// regardless of the cap (Write never fails), so a chatty child cannot
// deadlock on a full pipe.
//
// WaitDelay guarantees Wait returns even if a grandchild inherits the pipe
// and outlives the leader. NOTE: this is a visible behavior change vs
// CombinedOutput — a leader that exits successfully while a descendant holds
// the pipes now returns after WaitDelay with err == osexec.ErrWaitDelay
// instead of blocking forever; callers may treat ErrWaitDelay as success.
func runShellCapped(cmd *osexec.Cmd, capBytes int64) (tail string, omitted int64, err error) {
	b := newBoundedTailBuffer(capBytes)
	cmd.Stdout = b
	cmd.Stderr = b // same value as Stdout: os/exec serialises, interleaving kept
	cmd.WaitDelay = 5 * time.Second
	err = cmd.Run()
	tail, omitted = b.Snapshot()
	return tail, omitted, err
}

var _ io.Writer = (*boundedTailBuffer)(nil)
