package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/treeol/wakil/internal/config"
)

func mdl(lines ...string) tuiModel {
	return tuiModel{plainLines: lines, plainLinesNoBox: lines}
}

// TestCopyModeNativeNoOSCFallback verifies clipboard_mode=native never emits
// OSC 52 and reports failure honestly when no backend works.
func TestCopyModeNativeNoOSCFallback(t *testing.T) {
	orig := clipboardCmds
	clipboardCmds = nil
	defer func() { clipboardCmds = orig }()

	m, _ := newTestTUI(t)
	m.clipboardMode = config.ClipboardModeNative
	msg, ok := m.copyToClipboard("hello")().(copiedMsg)
	if !ok {
		t.Fatalf("expected copiedMsg, got %T", msg)
	}
	if msg.outcome != copyFailed {
		t.Errorf("outcome=%d want copyFailed (no OSC fallback)", msg.outcome)
	}
	if msg.n != 0 || msg.detail == "" {
		t.Errorf("expected honest failure report, got n=%d detail=%q", msg.n, msg.detail)
	}
	if msg.escape != nil {
		t.Errorf("native mode must not emit OSC 52 escape")
	}
}

// TestCopyModeOSC52Only verifies clipboard_mode=osc52 returns the OSC
// transport without running any native backend (observable contract:
// via=osc52 with escape bytes regardless of what's on PATH).
func TestCopyModeOSC52Only(t *testing.T) {
	m, _ := newTestTUI(t)
	m.clipboardMode = config.ClipboardModeOSC52
	msg, ok := m.copyToClipboard("hello")().(copiedMsg)
	if !ok {
		t.Fatalf("expected copiedMsg, got %T", msg)
	}
	if msg.via != copyViaOSC52 || !strings.Contains(string(msg.escape), "\x1b]52;c;") {
		t.Errorf("osc52 mode: via=%q escape=%q", msg.via, string(msg.escape))
	}
}

// TestTryNativeClipboardFirstFailsSecondSucceeds verifies backends are tried
// in order until one SUCCEEDS (presence on PATH is not enough) and failures
// are bounded.
func TestTryNativeClipboardFirstFailsSecondSucceeds(t *testing.T) {
	orig := clipboardCmds
	clipboardCmds = [][]string{{"false"}, {"true", "arg"}}
	defer func() { clipboardCmds = orig }()

	res, ok := tryNativeClipboard("payload")
	if !ok {
		t.Fatal("expected eventual success")
	}
	if res.backend != "true" {
		t.Fatalf("backend=%q want true (second candidate)", res.backend)
	}
	if len(res.errs) != 1 || !strings.Contains(res.errs[0], "false") {
		t.Fatalf("errs=%v want exactly the first failure", res.errs)
	}
}

// TestTryNativeClipboardAllFail verifies honest failure when every backend
// exits non-zero.
func TestTryNativeClipboardAllFail(t *testing.T) {
	orig := clipboardCmds
	clipboardCmds = [][]string{{"false"}}
	defer func() { clipboardCmds = orig }()

	if _, ok := tryNativeClipboard("payload"); ok {
		t.Fatal("expected failure when all backends fail")
	}
}

// TestCopyIDsMonotonicPerDispatch verifies the request id is minted at
// dispatch time (in copyToClipboard, before the Cmd runs) so two commands
// constructed in order carry increasing ids regardless of execution order —
// this is what makes the stale-result guard track user intent.
func TestCopyIDsMonotonicPerDispatch(t *testing.T) {
	m, _ := newTestTUI(t)
	cmdA := m.copyToClipboard("a")
	cmdB := m.copyToClipboard("b")
	msgA := cmdA().(copiedMsg)
	msgB := cmdB().(copiedMsg)
	if msgB.id <= msgA.id {
		t.Fatalf("dispatch-order ids inverted: A=%d B=%d (id must be minted at dispatch, not in the Cmd goroutine)", msgA.id, msgB.id)
	}
}

// TestCopiedMsgStaleIDRejected verifies the handler drops copiedMsg results
// older than the most recently accepted copy (ordering guard).
func TestCopiedMsgStaleIDRejected(t *testing.T) {
	m, _ := newTestTUI(t)
	m = step(m, copiedMsg{n: 5, via: copyViaNative, detail: "wl-copy", id: 2})
	first := m.flash
	if !strings.Contains(first, "5") {
		t.Fatalf("first copy flash=%q", first)
	}
	// A stale result (id 1 < 2) must not overwrite it.
	m = step(m, copiedMsg{n: 1, via: copyViaNative, detail: "xclip", id: 1})
	if m.flash != first {
		t.Fatalf("stale copy overwrote status: %q != %q", m.flash, first)
	}
	// A newer result (id 3) is accepted.
	m = step(m, copiedMsg{n: 7, via: copyViaNative, outcome: copyOK, detail: "pbcopy", id: 3})
	if !strings.Contains(m.flash, "7") {
		t.Fatalf("newer copy rejected: flash=%q", m.flash)
	}
	// id=0 (legacy callers/tests) bypasses the guard.
	m = step(m, copiedMsg{n: 9, via: copyViaNative, id: 0})
	if !strings.Contains(m.flash, "9") {
		t.Fatalf("legacy copiedMsg (id 0) rejected: flash=%q", m.flash)
	}
}

// TestNativeSuccessClearsPendingEscape verifies a native copy success clears a
// previously queued OSC 52 payload so a later redraw can't retransmit the old
// clipboard contents.
func TestNativeSuccessClearsPendingEscape(t *testing.T) {
	m, _ := newTestTUI(t)
	m = step(m, copiedMsg{n: 5, via: copyViaOSC52, outcome: copySentUnverified, escape: []byte("\x1b]52;c;old=\x07")})
	if len(m.pendingEscape) == 0 {
		t.Fatal("precondition: pendingEscape set by osc52 copy")
	}
	m = step(m, copiedMsg{n: 5, via: copyViaNative, outcome: copyOK, detail: "wl-copy", id: 1})
	if m.pendingEscape != nil {
		t.Fatalf("native success must clear pendingEscape, got %q", m.pendingEscape)
	}
}

// TestSubTabDragDoesNotCopyMainPane verifies the sub-tab guard: with a
// sub-tab active, mouse selection events must not produce a copy command or
// map into the hidden main-pane content.
func TestSubTabDragDoesNotCopyMainPane(t *testing.T) {
	m, _ := newTestTUI(t)
	m.ready = true
	m.width = 80
	m.height = 24
	buf := &strings.Builder{}
	buf.WriteString("subagent output")
	m.subTabs = []*subTab{{n: 1, task: "x", buf: buf, done: true}}
	m.subCur = 0
	// A stale main-pane selection highlight must be cleared by mouse traffic.
	m.sel = selection{active: true, anchorRow: 0, anchorCol: 0, headRow: 0, headCol: 3}

	m2, handled, cmd := m.handleMouse(tea.MouseMsg{X: 5, Y: 5, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	if !handled {
		t.Fatal("press with active sub-tab should be consumed")
	}
	if cmd != nil {
		t.Fatal("no copy command may be produced while a sub-tab is active")
	}
	if m2.sel.active || m2.sel.pressed {
		t.Fatal("stale selection must be cleared while a sub-tab is active")
	}
	if m2.subCur != 0 {
		t.Fatalf("guard must not change the active tab, subCur=%d", m2.subCur)
	}
}

// TestCopyLastResponseSource verifies ctrl+y's source lookup: returns the
// most recent committed assistant text verbatim (pre-glamour, blank lines
// kept), ignores user/system items, trims only leading/trailing newlines, and
// reports honestly when nothing exists.
func TestCopyLastResponseSource(t *testing.T) {
	m, _ := newTestTUI(t)
	if m.lastAssistantSource() != "" {
		t.Fatal("empty history should yield empty source")
	}
	m2, cmd := m.copyLastResponseCmd()
	if cmd != nil || !strings.Contains(m2.flash, "nothing to copy") {
		t.Fatalf("empty history: flash=%q cmd=%v", m2.flash, cmd)
	}

	items := []convItem{
		{kind: iUser, text: "question"},
		{kind: iAsst, text: "first answer"},
		{kind: iSys, text: "note"},
		{kind: iAsst, text: "\nfinal **answer**\nwith blank line\n\n"},
	}
	m.items = &items
	if got := m.lastAssistantSource(); got != "final **answer**\nwith blank line" {
		t.Fatalf("got %q", got)
	}
}

// TestToggleNativeSelection verifies the alt+t toggle clears in-app selection
// state and flips between DisableMouse and EnableMouseCellMotion.
func TestToggleNativeSelection(t *testing.T) {
	m, _ := newTestTUI(t)
	m.sel = selection{active: true}

	m2, cmd := m.toggleNativeSelection()
	if !m2.nativeSel.active {
		t.Fatal("first toggle should activate")
	}
	if cmd == nil {
		t.Fatal("expected a mouse Cmd")
	}
	if m2.sel.active {
		t.Fatal("toggle must clear in-app selection")
	}
	_ = cmd() // must be callable without panicking (disableMouseMsg)
	// Toggle back: must request mouse re-enable.
	m3, cmd2 := m2.toggleNativeSelection()
	if m3.nativeSel.active {
		t.Fatal("second toggle should deactivate")
	}
	if cmd2 == nil {
		t.Fatal("expected a re-enable Cmd")
	}
}

// TestNativeSelectionStatusIndicator verifies the persistent status segment
// appears while terminal-selection mode is on and not when off.
func TestNativeSelectionStatusIndicator(t *testing.T) {
	m, _ := newTestTUI(t)
	m.ready = true
	m.width = 100
	m.height = 24
	m.hadTurn = true

	m2, _ := m.toggleNativeSelection()
	out := m2.statusLines() // must not drop the indicator
	joined := strings.Join(out, "\n")
	if !strings.Contains(joined, "select:terminal") {
		t.Fatalf("status should contain select:terminal indicator; got %q", joined)
	}
	m3, _ := m2.toggleNativeSelection()
	if strings.Contains(strings.Join(m3.statusLines(), "\n"), "select:terminal") {
		t.Fatal("indicator must disappear when mode is off")
	}
}

func TestSelectedTextSingleLine(t *testing.T) {
	m := mdl("hello world")
	m.sel = selection{active: true, anchorRow: 0, anchorCol: 0, headRow: 0, headCol: 4}
	if got := m.selectedText(); got != "hello" {
		t.Fatalf("got %q want %q", got, "hello")
	}
}

func TestSelectedTextReversedDrag(t *testing.T) {
	// Dragging right-to-left must yield the same text as left-to-right.
	m := mdl("hello world")
	m.sel = selection{active: true, anchorRow: 0, anchorCol: 4, headRow: 0, headCol: 0}
	if got := m.selectedText(); got != "hello" {
		t.Fatalf("got %q want %q", got, "hello")
	}
}

func TestSelectedTextPreservesBlankLinesAndInteriorTrailingSpaces(t *testing.T) {
	// Blank lines inside the selection are preserved (previously dropped by
	// the empty-segment filter), and trailing spaces WITHIN the selected range
	// are kept verbatim (previously TrimRight'd). Layout padding beyond the
	// mirror line's end is excluded by the slice clamp, not by trimming.
	m := mdl("first line   ", "", "  indented  ", "third")
	m.sel = selection{active: true, anchorRow: 0, anchorCol: 0, headRow: 3, headCol: 5}
	want := "first line   \n\n  indented  \nthird"
	if got := m.selectedText(); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestSelectedTextClampsBeyondLine(t *testing.T) {
	// headCol past the end of a short line must clamp, not panic.
	m := mdl("ab")
	m.sel = selection{active: true, anchorRow: 0, anchorCol: 0, headRow: 0, headCol: 99}
	if got := m.selectedText(); got != "ab" {
		t.Fatalf("got %q want %q", got, "ab")
	}
}

func TestHighlightedContentWrapsSelection(t *testing.T) {
	m := mdl("hello world")
	m.sel = selection{active: true, anchorRow: 0, anchorCol: 0, headRow: 0, headCol: 4}
	out := m.highlightedContent()
	// The visible text is unchanged; only ANSI styling is added around "hello".
	if ansi.Strip(out) != "hello world" {
		t.Fatalf("stripped mismatch: %q", ansi.Strip(out))
	}
	if !strings.Contains(out, "\x1b[7m") { // reverse video
		t.Fatalf("expected reverse-video escape in %q", out)
	}
}

func TestEmptySelectionCopiesNothing(t *testing.T) {
	m, _ := newTestTUI(t)
	if cmd := m.copyToClipboard("   "); cmd == nil {
		t.Fatal("expected a command")
	} else if msg := cmd(); msg != nil {
		t.Fatalf("blank selection should produce no copiedMsg, got %#v", msg)
	}
}

// TestCopyToClipboardOSCFallback verifies that when no native clipboard writer
// is available (the common SSH scenario), copyToClipboard falls back to OSC 52
// and reports via=copyViaOSC52 so the UI can hint the user. The escape bytes
// are carried in msg.escape (not written to os.Stdout) so View() can emit them
// through the renderer's synchronized output.
func TestCopyToClipboardOSCFallback(t *testing.T) {
	orig := clipboardCmds
	clipboardCmds = nil
	defer func() { clipboardCmds = orig }()

	m, _ := newTestTUI(t)
	cmd := m.copyToClipboard("hello")
	if cmd == nil {
		t.Fatal("expected a command")
	}
	msg, ok := cmd().(copiedMsg)
	if !ok {
		t.Fatalf("expected copiedMsg, got %T", cmd())
	}
	if msg.n != 5 {
		t.Errorf("n=%d want 5", msg.n)
	}
	if msg.via != copyViaOSC52 {
		t.Errorf("via=%q want %q", msg.via, copyViaOSC52)
	}
	// The OSC 52 escape sequence must be carried in msg.escape for View() to emit.
	if !strings.Contains(string(msg.escape), "\x1b]52;c;") {
		t.Errorf("expected OSC 52 escape in msg.escape, got %q", string(msg.escape))
	}
	// The escape must contain the base64-encoded "hello".
	if !strings.Contains(string(msg.escape), "aGVsbG8=") {
		t.Errorf("expected base64('hello') in escape, got %q", string(msg.escape))
	}
}

// TestCopyToClipboardUnicode verifies the rune count is correct for non-ASCII.
func TestCopyToClipboardUnicode(t *testing.T) {
	orig := clipboardCmds
	clipboardCmds = nil
	defer func() { clipboardCmds = orig }()

	m, _ := newTestTUI(t)
	cmd := m.copyToClipboard("å🙂")
	msg, ok := cmd().(copiedMsg)
	if !ok {
		t.Fatalf("expected copiedMsg, got %T", cmd())
	}
	if msg.n != 2 {
		t.Errorf("n=%d want 2 (runes), got %d", msg.n, msg.n)
	}
}

// TestCopiedMsgOSCEscapeEmission verifies that the OSC 52 escape is stored on
// the model as pendingEscape when the copiedMsg arrives, emitted through
// View(), and persists across View() calls (NOT one-shot) so the standard
// renderer's frame coalescing can't drop it. The escape is cleared by a
// KeyMsg, not by View(). NOTE: this persistence is a known best-effort
// trade-off — delivery is "queued, unverified", never claimed as verified.
func TestCopiedMsgOSCEscapeEmission(t *testing.T) {
	m, _ := newTestTUI(t)
	m.ready = true
	m.width = 80
	m.height = 24
	esc := []byte("\x1b]52;c;aGVsbG8=\x07")
	m = step(m, copiedMsg{n: 5, via: copyViaOSC52, outcome: copySentUnverified, escape: esc})

	if string(m.pendingEscape) != string(esc) {
		t.Fatalf("pendingEscape=%q want %q", m.pendingEscape, string(esc))
	}

	// View() must emit the escape prepended to the frame.
	view := m.View()
	if !strings.HasPrefix(view, "\x1b]52;c;") {
		t.Errorf("View() should emit the OSC 52 escape first; got %q...\"", view[:min(40, len(view))])
	}

	// The escape must PERSIST across View() calls — the standard renderer
	// coalesces frames at 60fps, so View() may be called multiple times before
	// a frame is actually written. Clearing in View() would drop the escape.
	view2 := m.View()
	if !strings.HasPrefix(view2, "\x1b]52;c;") {
		t.Errorf("View() should still emit the escape on second call; the renderer may not have flushed the first frame yet")
	}
}

// TestCopiedMsgOSCHint verifies the flash wording per outcome: OSC 52 says
// "queued … unverified" (bytes sent is not delivery), native says "copied ✓
// via <backend>", failure says "copy failed: …".
func TestCopiedMsgOSCHint(t *testing.T) {
	// OSC 52 path — "queued"/"unverified" wording, no checkmark.
	m, _ := newTestTUI(t)
	m = step(m, copiedMsg{n: 10, via: copyViaOSC52, outcome: copySentUnverified})
	if !strings.Contains(m.flash, "OSC 52") || !strings.Contains(m.flash, "unverified") {
		t.Errorf("osc52 flash should say queued+unverified; got %q", m.flash)
	}
	if !strings.Contains(m.flash, "10") {
		t.Errorf("flash should include char count; got %q", m.flash)
	}
	if strings.Contains(m.flash, "✓") || strings.Contains(m.flash, "copied") {
		t.Errorf("osc52 flash must NOT claim 'copied'; got %q", m.flash)
	}
	// With native-failure context (auto-mode fallthrough).
	m, _ = newTestTUI(t)
	m = step(m, copiedMsg{n: 10, via: copyViaOSC52, outcome: copySentUnverified, detail: "native failed (xclip: exit 1)"})
	if !strings.Contains(m.flash, "native failed") || !strings.Contains(m.flash, "unverified") {
		t.Errorf("auto-fallback flash should include failure context; got %q", m.flash)
	}

	// Native success — normal confirmation with backend name, no OSC 52 hint.
	m, _ = newTestTUI(t)
	m = step(m, copiedMsg{n: 20, via: copyViaNative, outcome: copyOK, detail: "wl-copy"})
	if strings.Contains(m.flash, "OSC 52") {
		t.Errorf("native flash should NOT mention OSC 52; got %q", m.flash)
	}
	if !strings.Contains(m.flash, "20") || !strings.Contains(m.flash, "wl-copy") {
		t.Errorf("flash should include count + backend; got %q", m.flash)
	}
	if !strings.Contains(m.flash, "✓") {
		t.Errorf("native flash should have checkmark; got %q", m.flash)
	}

	// Failure — honest report, escape cleared.
	m, _ = newTestTUI(t)
	m.pendingEscape = []byte("stale")
	m = step(m, copiedMsg{n: 0, via: copyViaNative, outcome: copyFailed, detail: "no native clipboard tool available"})
	if !strings.Contains(m.flash, "copy failed") {
		t.Errorf("failure flash should say copy failed; got %q", m.flash)
	}
	if m.pendingEscape != nil {
		t.Errorf("failure must clear pendingEscape; got %q", m.pendingEscape)
	}
}
