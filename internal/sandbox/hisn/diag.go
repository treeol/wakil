package hisn

import (
	"io"
	"strings"
	"sync"
	"sync/atomic"
)

// diagWriter forwards to w and records whether any line began with "hisn: ",
// the prefix hisn uses for its own diagnostics. It is how a "lost" exec (hisn
// exit 125 with no diagnostic) is told apart from a hisn error (125 with one).
type diagWriter struct {
	w      io.Writer
	prefix [6]byte
	n      int  // bytes of the current line's prefix collected
	skip   bool // current line already decided (matched or not a prefix)
	seen   atomic.Bool
}

const diagPrefix = "hisn: "

func (d *diagWriter) Write(p []byte) (int, error) {
	for _, c := range p {
		if c == '\n' {
			d.n, d.skip = 0, false
			continue
		}
		if d.skip {
			continue
		}
		d.prefix[d.n] = c
		d.n++
		if d.prefix[d.n-1] != diagPrefix[d.n-1] {
			d.skip = true
			continue
		}
		if d.n == len(diagPrefix) {
			d.seen.Store(true)
			d.skip = true
		}
	}
	if d.w == nil {
		return len(p), nil
	}
	return d.w.Write(p)
}

// Seen reports whether a hisn diagnostic line was observed.
func (d *diagWriter) Seen() bool { return d.seen.Load() }

// ringBuffer keeps the LAST max bytes written to it. It captures the hisn
// supervisor's stderr (hisn prints nothing unless run with -v) for diagnostics
// without growing with the session's lifetime. Safe for concurrent use.
type ringBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func newRingBuffer(max int) *ringBuffer { return &ringBuffer{max: max} }

func (r *ringBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if over := len(r.buf) - r.max; over > 0 {
		r.buf = append(r.buf[:0], r.buf[over:]...)
	}
	return len(p), nil
}

// String returns the captured tail, trimmed.
func (r *ringBuffer) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.TrimSpace(string(r.buf))
}
