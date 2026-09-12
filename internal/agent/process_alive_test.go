//go:build !windows

package agent

import (
	"os"
	"testing"
)

func TestProcessAliveImpl_aliveSelf(t *testing.T) {
	// Our own process is alive and we have permission to signal it.
	if !processAliveImpl(os.Getpid()) {
		t.Fatal("processAliveImpl should return true for our own pid")
	}
}

func TestProcessAliveImpl_deadPid(t *testing.T) {
	// PID 1 is typically init (alive on most systems, and we may or may not
	// have permission to signal it). Use a very high PID that is almost
	// certainly not running.
	if processAliveImpl(999999) {
		// This could be alive on some systems, but it's very unlikely.
		t.Skip("pid 999999 appears to be alive — skipping on this system")
	}
}
