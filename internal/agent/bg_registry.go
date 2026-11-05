package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
)

// bgRegistry is the B3 background-process registry, extracted from the App god
// struct (WP-6.3). It groups the four fields that together track run_background
// processes for the session. Embedded in App, so selector access is unchanged
// (a.bgProcs, a.bgMu, ...). Composite literals that set these fields must use the
// nested form, e.g. App{bgRegistry: bgRegistry{bgProcs: ...}}.
//
// bgMu protects bgProcs and bgCounter. Written in turn handlers (run_background,
// kill_process, read_process_log), read in shutdown (StopAllBackgroundProcs).
// Do NOT hold the lock while waiting on process exit — copy references under
// lock, then signal/wait outside.
type bgRegistry struct {
	bgMu      sync.RWMutex
	bgProcs   map[string]*bgEntry
	bgCounter int
	bgLogDir  string // per-session temp dir for bg process logs; cleaned up in StopAllBackgroundProcs

	// bgFinished holds jobs that left bgProcs (natural exit, kill, abandon),
	// oldest first, so read_process_log keeps working for the rest of the chat.
	// bgEvicted records ids whose retained log was evicted, so the error can say
	// "evicted" instead of "no background process" (bgCounter cannot tell: it
	// also advances for declined and failed launches). Both guarded by bgMu.
	bgFinished []*bgEntry
	bgEvicted  map[string]struct{}
}

// Retention bounds for finished jobs (T2, 0.4.4). Variables so tests can shrink
// them.
var (
	bgFinishedMaxJobs        = 32
	bgFinishedMaxBytes int64 = 64 << 20
)

// retireBgLocked removes id from bgProcs and, if it was live there, appends the
// entry to bgFinished. Idempotent: an id that is not in bgProcs (already
// retired, evicted, or never present) is a no-op, so the several paths that
// delete a finished job can never double-insert. Caller holds bgMu (write).
// Callers invoke trimBgFinished after releasing the lock.
func (a *App) retireBgLocked(id string) {
	e := a.bgProcs[id]
	delete(a.bgProcs, id)
	if e == nil || e.retired {
		return
	}
	e.retired = true
	a.bgFinished = append(a.bgFinished, e)
}

// finishBgLocked is the reaper-side exit: jobs the model can only reach through
// read_process_log (run_background, or a run_shell detached at its deadline) are
// retained; a run_shell that finished inside its deadline already returned its
// output inline and is simply dropped. Caller holds bgMu (write).
func (a *App) finishBgLocked(e *bgEntry) {
	if e.detached || e.toolName == "run_background" {
		a.retireBgLocked(e.id)
		return
	}
	delete(a.bgProcs, e.id)
}

// finishedBgLocked returns the retained entry for id, or nil. Caller holds bgMu.
func (a *App) finishedBgLocked(id string) *bgEntry {
	for _, e := range a.bgFinished {
		if e.id == id {
			return e
		}
	}
	return nil
}

// finishedBg is finishedBgLocked taking the lock itself.
func (a *App) finishedBg(id string) *bgEntry {
	a.bgMu.RLock()
	defer a.bgMu.RUnlock()
	return a.finishedBgLocked(id)
}

// trimBgFinished enforces the retention bounds: oldest-first eviction by job
// count and by total retained log bytes. An entry with active readers is never
// evicted (its log may be mid-read): eviction waits for it and happens at the
// next trim, so the order stays strictly oldest-first.
// A single retained log is never evicted by the byte bound alone. Log sizes are
// statted outside bgMu (the stat is an executor round-trip).
func (a *App) trimBgFinished() {
	ctx := context.Background()
	a.bgMu.RLock()
	var unknown []*bgEntry
	for _, e := range a.bgFinished {
		if !e.logSizeKnown {
			unknown = append(unknown, e)
		}
	}
	a.bgMu.RUnlock()
	sizes := make(map[*bgEntry]int64, len(unknown))
	for _, e := range unknown {
		if n, err := a.Exec.StatFile(ctx, e.logPath); err == nil {
			sizes[e] = n
		}
	}

	a.bgMu.Lock()
	for e, n := range sizes {
		e.logSize, e.logSizeKnown = n, true
	}
	var victims []*bgEntry
	for {
		var total int64
		for _, e := range a.bgFinished {
			total += e.logSize
		}
		if len(a.bgFinished) <= bgFinishedMaxJobs && (total <= bgFinishedMaxBytes || len(a.bgFinished) <= 1) {
			break
		}
		// Strictly oldest-first: if the oldest entry is mid-read, wait (the
		// bound is exceeded briefly) rather than evict a newer job instead.
		if a.bgFinished[0].readers > 0 {
			break
		}
		v := a.bgFinished[0]
		a.bgFinished = a.bgFinished[1:]
		if a.bgEvicted == nil {
			a.bgEvicted = make(map[string]struct{})
		}
		a.bgEvicted[v.id] = struct{}{}
		victims = append(victims, v)
	}
	a.bgMu.Unlock()
	for _, v := range victims {
		a.removeBgLog(v.logPath)
	}
}

// removeBgLog deletes a retained job log, best effort.
func (a *App) removeBgLog(path string) {
	if path == "" || a.Exec == nil {
		return
	}
	_, _ = a.Exec.RunShell(context.Background(), "rm -f -- "+shellQuote(path))
}

// removeFinishedBgLogs drops every retained log (shutdown).
func (a *App) removeFinishedBgLogs() {
	a.bgMu.Lock()
	logs := make([]string, 0, len(a.bgFinished))
	for _, e := range a.bgFinished {
		logs = append(logs, e.logPath)
	}
	a.bgFinished = nil
	a.bgMu.Unlock()
	for _, l := range logs {
		a.removeBgLog(l)
	}
}

// bgLogPath generates a collision-resistant log path for a background process.
// Format: /tmp/wakil-bg-<pid>-<12hex>.log — the PID disambiguates concurrent
// sessions on the same host, and the 12 hex chars (6 random bytes) make
// same-PID collisions astronomically unlikely. This replaces the old
// /tmp/wakil-bg-%d.log scheme that used only a per-App counter, allowing two
// concurrent direct-mode sessions to clobber each other's logs and exit
// markers. On multi-user hosts the predictable name was also a symlink target.
//
// Docker mode is unaffected: /tmp is per-container tmpfs, so the PID+random
// suffix is sufficient (the shell redirect uses ">" which can't do O_EXCL).
func (a *App) bgLogPath(n int) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return fmt.Sprintf("/tmp/wakil-bg-%d-%s-%d.log", os.Getpid(), hex.EncodeToString(b), n)
}
