package agent

import (
	"github.com/treeol/wakil/internal/proxy"
)

// appendConv appends msgs to a.Conv under convMu, stamping Turn=a.curTurn on
// any message that has Turn==0. All in-turn appends go through here so every
// message of a request carries the same turn number. Legacy messages (preamble,
// summary, handoff seed) keep Turn==0 and are never collapsed.
//
// The turn goroutine is the sole writer of curTurn, so the read here is
// race-free — no lock needed for curTurn. convMu protects the Conv slice
// append against concurrent readers (Snapshot, SaveSession, the TUI).
func (a *App) appendConv(msgs ...proxy.Message) {
	for i := range msgs {
		if msgs[i].Turn == 0 {
			msgs[i].Turn = a.curTurn
		}
	}
	a.convMu.Lock()
	a.Conv = append(a.Conv, msgs...)
	a.convMu.Unlock()
}
