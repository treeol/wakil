package agent

// Wiring the data-policy transcript notices.
//
// zdr auto falls back to an unenforced send (no provider ZDR fields) when
// the provider has no ZDR endpoint for the model; the user sees one notice
// per model per session. The transport fires the hook (no locks held); this
// file is the agent-side sink that writes it into the transcript.

import "fmt"

// WireNoZDRNotice registers the once-per-model transcript notice on the
// session proxy client. It is called during session construction (wiring)
// and whenever /zdr changes the switches, so a mid-session switch change
// still notifies.
func (a *App) WireNoZDRNotice() {
	if a.Client == nil {
		return
	}
	// The sink writes via sendEvent, which is goroutine-safe and does not
	// block on the TUI (a blocked sink drops the notice rather than
	// stalling a paid call — the notice is informational).
	a.Client.SetNoZDRNoticeHook(func(model string) {
		msg := fmt.Sprintf("zdr auto: no ZDR endpoint for %s, sent without ZDR.", model)
		a.SendEvent(SysNoteMsg{Text: msg})
	})
}
