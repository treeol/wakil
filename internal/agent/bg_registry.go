package agent

import "sync"

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
}
