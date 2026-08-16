// Package safe provides a goroutine launcher with panic recovery.
//
// A panic in an unrecovered goroutine crashes the entire process. safe.Go
// wraps the goroutine in a recover() that writes the panic value and stack
// trace to the diagnostic sink (internal/diag), so a panicking goroutine is
// caught instead of taking down Wakil — and never writes raw os.Stderr that
// could garble the TUI.
//
// The function does NOT swallow the panic silently: every recovery produces a
// visible diagnostic line. Callers that need to know about the panic (e.g. to
// return an error) should use their own channel/errgroup pattern and check
// for the error downstream.
//
// Usage:
//
//	safe.Go("oracle-panel", func() {
//	    // ... code that might panic ...
//	})
//
// The name is included in the output so panics can be traced to their origin
// without reading the stack trace.
package safe

import (
	"runtime/debug"

	"github.com/treeol/wakil/internal/diag"
)

// Go launches fn as a goroutine with panic recovery. If fn panics, the
// panic value and stack trace are written to the diagnostic sink and the
// process continues. The name is a short identifier for the goroutine's
// purpose (e.g. "oracle-panel", "bg-reaper") used in the log line.
func Go(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				diag.Printf("panic in goroutine %q: %v\n%s", name, r, debug.Stack())
			}
		}()
		fn()
	}()
}
