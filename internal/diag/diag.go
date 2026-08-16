// Package diag is the single diagnostic-output seam for Wakil.
//
// Any goroutine that needs to emit a diagnostic line (an error, warning, or
// debug note that is NOT part of the conversation transcript) writes through
// diag instead of os.Stderr. The default destination is os.Stderr, but the TUI
// path calls Redirect before prog.Run() so that a raw diagnostic can never
// interleave with Bubble Tea's alt-screen renderer and garble the terminal.
//
// This is deliberately a package-level writer, not per-struct injection: the
// bug it prevents is "some goroutine somewhere writes os.Stderr during the TUI
// loop", and that bug is only robustly fixed by giving every such site one
// shared, redirectable sink. A per-struct field would leave the next stray
// os.Stderr write to reintroduce the garble.
package diag

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	mu     sync.Mutex
	writer io.Writer = os.Stderr
)

// Redirect changes the diagnostic destination. It is called once at TUI startup
// (before any goroutine that might write) and in tests. Pass nil to restore the
// default (os.Stderr) — callers should do this before closing a file-backed
// destination so late writes don't hit a closed file.
func Redirect(w io.Writer) {
	mu.Lock()
	defer mu.Unlock()
	if w == nil {
		writer = os.Stderr
		return
	}
	writer = w
}

// Write writes p to the current diagnostic destination. Writes are serialized
// against each other (a non-thread-safe destination like a buffer or a
// non-file writer is safe) and against Redirect, so a concurrent Redirect can
// never tear a write in half. Uses a full write lock: concurrent Write calls
// must be mutually exclusive (an RWMutex read lock would let them interleave).
func Write(p []byte) (int, error) {
	mu.Lock()
	defer mu.Unlock()
	return writer.Write(p)
}

// Printf formats and writes a diagnostic line to the current destination, with
// a timestamp prefix (matching the stdlib log package's default), so append-only
// session logs remain correlatable. Formatting happens before the lock; the
// actual write goes through Write so concurrent Printf calls are serialized.
func Printf(format string, args ...any) {
	line := time.Now().Format("2006/01/02 15:04:05 ") + fmt.Sprintf(format, args...)
	_, _ = Write([]byte(line))
}

// OpenSessionLog opens (or appends to) a per-session diagnostic log file under
// the user's data dir (XDG_DATA_HOME/wakil, else ~/.local/share/wakil), and
// redirects the diagnostic sink to it. It returns the open file so the caller
// can Close it on exit. On any failure it returns nil and leaves the sink on
// os.Stderr — a missing log file must not break the session.
func OpenSessionLog(sessionID string) *os.File {
	dir := dataDir()
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil
	}
	path := filepath.Join(dir, "diag-"+sanitizeID(sessionID)+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil
	}
	Redirect(f)
	return f
}

// LogPath returns the diagnostic log path for a session ID without opening it,
// so a caller can surface the path to the user before entering the TUI.
func LogPath(sessionID string) string {
	dir := dataDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "diag-"+sanitizeID(sessionID)+".log")
}

// dataDir resolves the wakil data directory: XDG_DATA_HOME/wakil, else
// ~/.local/share/wakil. Empty when neither is resolvable.
func dataDir() string {
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "wakil")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "share", "wakil")
	}
	return ""
}

// sanitizeID strips path separators and ".." from a session ID so it can never
// escape the data directory. The caller's ShortID is already safe, but the
// exported function must not trust that.
func sanitizeID(id string) string {
	id = strings.ReplaceAll(id, "/", "_")
	id = strings.ReplaceAll(id, string(os.PathSeparator), "_")
	if id == "" || id == "." || id == ".." {
		return "unknown"
	}
	return id
}
