package main

import "github.com/treeol/wakil/internal/wiring"

// bootstrapDoneMsg carries the result of the async bootstrap (NewExecutor +
// BootstrapTUI). It implements tui.BootstrapDone so the loading model can
// distinguish it from mouse/key events and other messages.
type bootstrapDoneMsg struct {
	rt      *wiring.TUIRuntime
	cleanup func()
	exe     interface{ Close() error }
	err     error
}

func (bootstrapDoneMsg) IsBootstrapDone() {}
