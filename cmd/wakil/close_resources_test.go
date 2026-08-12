package main

import (
	"testing"

	"github.com/treeol/wakil/internal/agent"
)

// TestCloseResources_NilSafe verifies closeResources is a no-op and does not
// panic on a freshly built App with no async ops / background processes and an
// empty appResources (all nil). This is the error-path contract: main.go calls
// closeResources immediately before os.Exit on post-build failures, where the
// app has just been constructed and no resources exist yet.
func TestCloseResources_NilSafe(t *testing.T) {
	// Zero-value App: both StopAllAsyncOps and StopAllBackgroundProcs
	// early-return on an empty registry (len 0), so this is safe and cheap.
	closeResources(&agent.App{}, appResources{})
	// Reaching here without a panic is the pass condition. Each field in the
	// zero appResources is nil, so every Close branch is skipped.
}
