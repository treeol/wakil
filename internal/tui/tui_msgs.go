package tui

import (
	"fmt"

	"github.com/treeol/wakil/internal/core/format"
	"github.com/treeol/wakil/internal/proxy"
)

// dotTickMsg fires every ~200 ms while the agent is busy.
type dotTickMsg struct{}

// armTickMsg fires armWindow after a quit/cancel arm is set, to clear it. seq
// is the arm's generation counter: the handler only clears if seq still matches
// m.armSeq, so a stale tick from a superseded arm can't clear a newer one.
type armTickMsg struct{ seq int }

// pasteBurstTickMsg fires pasteBurstGap after the last key event of a rapid
// input burst (a fragmented, non-bracketed paste). seq is the burst's
// generation counter: the handler only collapses if seq still matches
// m.pasteBurstSeq, so a stale tick from a superseded burst is a no-op.
type pasteBurstTickMsg struct{ seq int }

// pasteIdleTickMsg is the catch-all binary-paste scanner: fires 400ms after
// the last key event whenever the textarea is non-empty. It runs
// binaryPasteStart over the FULL textarea content regardless of how the
// paste arrived (bracketed, fragmented, or otherwise) — closing the gap
// where mangled image bytes sat in the input until the user pressed Enter.
type pasteIdleTickMsg struct{}

// restorePasteStashMsg fires after a failed clipboard read once the paste
// tail has drained, to restore the cut text into the textarea. Re-arms
// itself while the suppression window is still open (tail still arriving).
type restorePasteStashMsg struct{}

// subTabCloseMsg is fired 30s after a tab becomes done (via SubagentDoneMsg or
// AsyncJobDoneMsg), to auto-close it if the user is not currently viewing it.
// Fire-and-validate: if the tab was already pruned, manually closed, or is
// focused at fire time, the handler is a no-op. The timer is a one-shot tea.Cmd
// and cannot be cancelled — staleness is handled idempotently in the handler.
// Exactly one of ChatID (subagent) or OpID (async job) is set; the handler
// matches the tab by whichever identity its kind uses.
type subTabCloseMsg struct {
	ChatID string
	OpID   string
}

// copiedMsg reports that a text selection was copied to the system clipboard.
// via is copyViaNative when a local clipboard command (wl-copy/xclip/etc.)
// wrote it, or copyViaOSC52 when the OSC 52 terminal escape carried it
// (fallback or configured transport). The handler uses via + detail to hint
// the user how the copy was delivered — and at what verification level:
// native success means the tool exited 0; OSC 52 means the bytes were sent,
// NOT that the clipboard changed (many terminals/tmux block the sequence).
//
// id is the copy-request generation that produced this message. The handler
// drops stale results so an older, slow native write can't overwrite the
// status of a newer copy (ordering review finding). It cannot un-write an
// older native clipboard write — only its status.
//
// When via is copyViaOSC52, escape holds the raw OSC 52 terminal escape bytes.
// The handler stores them on the model as pendingEscape so View() can emit
// them through the renderer's synchronized output — writing them directly to
// os.Stdout from the Cmd goroutine would race with the renderer in alt-screen
// mode and garble the display. pendingEscape persists across View() calls
// (NOT cleared in View()) so the standard renderer's frame coalescing can't
// drop it; a KeyMsg clears it (alongside flash) as the lifecycle boundary, and
// a subsequent native success also clears it (a native write supersedes the
// older OSC payload — stale retransmission guard).
type copiedMsg struct {
	n       int
	via     string
	outcome copyOutcome // explicit transport outcome — free-text matching is fragile
	detail  string      // native backend name, or failure context in via=osc52 fallback
	id      uint64
	escape  []byte
}

// copyOutcome classifies a copiedMsg so the handler doesn't string-match free
// text (review finding A4).
type copyOutcome uint8

const (
	copyOK             copyOutcome = iota // native write succeeded (tool exited 0)
	copySentUnverified                    // OSC 52 bytes queued for emission — clipboard NOT verified
	copyFailed                            // no transport succeeded
)

const (
	copyViaNative = "native"
	copyViaOSC52  = "osc52"
)

// clipboardImageMsg carries an image read from the system clipboard. If Err is
// non-empty, the handler shows it and does not attach anything.
type clipboardImageMsg struct {
	Img proxy.ImagePart
	Err string
}

// fmtConfirmBlock builds the confirm-prompt display string.
func fmtConfirmBlock(headline, detail string, readAction bool) string {
	opts := "  [y] proceed   [n] decline"
	if readAction {
		opts = "  [y] proceed   [a] allow all reads   [n] decline"
	}
	return fmt.Sprintf("\n⟂ %s\n%s\n\n%s\n", headline, format.Indent(detail), opts)
}
