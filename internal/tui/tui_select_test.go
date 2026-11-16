package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/core/event"
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
	if len(m.pendingEscapeBytes()) == 0 {
		t.Fatal("precondition: pendingEscape set by osc52 copy")
	}
	m = step(m, copiedMsg{n: 5, via: copyViaNative, outcome: copyOK, detail: "wl-copy", id: 1})
	if len(m.pendingEscapeBytes()) != 0 {
		t.Fatalf("native success must clear pendingEscape, got %q", m.pendingEscapeBytes())
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

// TestSubTabStreamInvalidatesPaneCache verifies the convPaneKey
// invalidation for in-place sub-tab appends: sub-tab output is
// appended to tab.buf in place (SubagentProgress / AsyncJobProgress),
// which changes neither the buffer pointer nor any other key field —
// the cache key must carry the buffer LENGTH or view() keeps serving
// the stale pane while the watched tab streams.
func TestSubTabStreamInvalidatesPaneCache(t *testing.T) {
	m, _ := newTestTUI(t)
	m.ready = true
	m.width = 80
	m.height = 24
	buf := &strings.Builder{}
	buf.WriteString("first line")
	m.subTabs = []*subTab{{n: 1, task: "x", buf: buf, done: false}}
	m.subCur = 0

	first := ansi.Strip(m.view())
	if !strings.Contains(first, "first line") {
		t.Fatalf("precondition: first view missing sub-tab output:\n%s", first)
	}
	// In-place append, as the progress handlers do: same pointer.
	buf.WriteString("\nsecond line appended")
	second := ansi.Strip(m.view())
	if !strings.Contains(second, "second line appended") {
		t.Fatalf("view after in-place append served the stale pane:\n%s", second)
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

// TestCopiedMsgOSCEscapeEmission verifies that the OSC 52 escape is stored
// on the model as pendingEscape when the copiedMsg arrives and emitted
// through the first View() that follows — exactly once per copy action
// (Part 3, L5). The escape is cleared by that first View() (not by a
// KeyMsg), so a later frame never carries it again. Delivery is
// "queued, unverified", never claimed as verified.
func TestCopiedMsgOSCEscapeEmission(t *testing.T) {
	m, _ := newTestTUI(t)
	m.ready = true
	m.width = 80
	m.height = 24
	esc := []byte("\x1b]52;c;aGVsbG8=\x07")
	m = step(m, copiedMsg{n: 5, via: copyViaOSC52, outcome: copySentUnverified, escape: esc})

	if string(m.pendingEscapeBytes()) != string(esc) {
		t.Fatalf("pendingEscape=%q want %q", m.pendingEscapeBytes(), string(esc))
	}

	// View() must emit the escape prepended to the frame.
	view := m.View()
	if !strings.HasPrefix(view, "\x1b]52;c;") {
		t.Errorf("View() should emit the OSC 52 escape first; got %q...\"", view[:min(40, len(view))])
	}

	// Exactly once: the next View() must NOT carry it again.
	view2 := m.View()
	if strings.HasPrefix(view2, "\x1b]52;c;") {
		t.Errorf("View() re-emitted the OSC 52 escape on the second call; a copy action must schedule exactly one delivery")
	}
}

// TestCopiedMsgOSCHint verifies the flash wording per outcome: OSC 52 says
// "queued … unverified" (bytes sent is not delivery), native says "copied ✓
// via <backend>", failure says "copy failed: …".
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
	m.setPendingEscape([]byte("stale"))
	m = step(m, copiedMsg{n: 0, via: copyViaNative, outcome: copyFailed, detail: "no native clipboard tool available"})
	if !strings.Contains(m.flash, "copy failed") {
		t.Errorf("failure flash should say copy failed; got %q", m.flash)
	}
	if len(m.pendingEscapeBytes()) != 0 {
		t.Errorf("failure must clear pendingEscape; got %q", m.pendingEscapeBytes())
	}
}

// TestSelectedTextAcrossSoftWrap verifies that a selection spanning a
// soft-wrapped line copies the exact source text: no escape sequences
// (plain() is a no-op on the copied text), no border characters, no line
// breaks that came only from wrapping, and the original line breaks kept.
//
// Setup: an assistant answer whose first sentence wraps at 20 cols. The
// wrapped rows land in plainLines/plainLinesNoBox as the SAME line-split
// views of the same source; selecting from the tail of wrapped row 1 to
// the head of wrapped row 2 crosses a wrap break that must NOT become a
// newline of its own, plus the true source newline that the answer
// contains.
func TestSelectedTextAcrossSoftWrap(t *testing.T) {
	// Two mirrors of one wrapped content: bordered and no-box. The rows
	// are identical text except the leading "│ " border marker.
	src := "hello world this is\nwrapped content here"
	wrapped := []string{"hello world this is", "wrapped content here"}
	bordered := []string{"│ hello world this is", "│ wrapped content here"}
	_ = src
	_ = wrapped
	m := mdl(bordered...)
	m.plainLinesNoBox = wrapped

	// Select from the middle of the first content row's END (before the
	// wrap) to the start of the second row's content: plainLines cols
	// include the leading "│ " (2 cols), so col 1+2=3 is 'h', col
	// 13+2=15 is the 't' of "this". In plainLines space the selection is
	// (0,3)-(1,2): 'hello world ' ... wait 'hello world this is' ends at
	// col 17 (plainLines col 19 = 's', the last letter). We select the
	// second wrapped tail "is" through the wrap into the head of row 2.
	m.sel = selection{active: true, anchorRow: 0, anchorCol: 15, headRow: 1, headCol: 2}
	got := m.selectedText()
	plainGot := plain(got)
	// The selection is (0,15)-(1,2) in plainLines space: bordered cols
	// include "│ " (2 cols), so col 15 = 's' (the 's' of "this"),
	// col 16 = 's' wait 'this' is t-h-i-s: col 13='t', 14='h', 15='i'),
	// col 15='i', col 16='s', then ' is' tail, then "is" the last word.
	// In no-box space cols are shifted by -1 (border), so the wrapped
	// view slices "s is" off row 0's tail and "wra" off row 1's head:
	// "s is\nwra" — one newline (the TRUE break between the two source
	// lines), no escape codes, no borders.
	if plainGot != "s is\nwra" {
		t.Fatalf("selectedText across wrap = %q, want %q", plainGot, "s is\nwra")
	}
	if strings.Contains(got, "│") || strings.Contains(got, "\x1b") {
		t.Fatalf("copied text leaked border/escape: %q", got)
	}
	// Full-row selection across the same two rows: "hello world this is"
	// + "\n" (true break, from the source — the wrap joined it) +
	// "wrapped content here". No trailing newline, no missing break.
	m.sel = selection{active: true, anchorRow: 0, anchorCol: 0, headRow: 1, headCol: 99}
	gotAll := plain(m.selectedText())
	wantAll := "hello world this is\nwrapped content here"
	if gotAll != wantAll {
		t.Fatalf("full selection = %q, want %q", gotAll, wantAll)
	}
}

// TestSelectedTextWideCombiningRTL verifies that wide (CJK), combining
// and RTL (Arabic) text is copied as the same code points.
func TestSelectedTextWideCombiningRTL(t *testing.T) {
	src := "中文 тест مرحبا école"
	m := mdl(src)
	m.sel = selection{active: true, anchorRow: 0, anchorCol: 0, headRow: 0, headCol: 99}
	got := m.selectedText()
	if got != src {
		t.Fatalf("wide/combining/RTL copy = %q, want %q", got, src)
	}
}

// TestSelectedTextWhileStreaming verifies that a stream chunk arriving
// during an active drag does not move the anchor/head (the selection is
// pinned; the copied text on release is the selected source text).
func TestSelectedTextWhileStreaming(t *testing.T) {
	m, _ := newTestTUI(t)
	m.ready = true
	m.width, m.height = 80, 24
	// Feed the model a conversation through the real event path.
	m = step(m, evt(event.KindTurnStarted, event.TurnStarted{TurnID: "trn"}, benchPrincipalSID))
	m = step(m, evt(event.KindMessageDelta, event.MessageDelta{Text: "hello stream"}, benchPrincipalSID))
	m = step(m, evt(event.KindTurnCompleted, event.TurnCompleted{TurnID: "trn", Outcome: "complete"}, benchPrincipalSID))
	// A drag over the committed text.
	m.sel = selection{pressed: true, active: true, dragging: true, anchorRow: 0, anchorCol: 0, headRow: 0, headCol: 3}
	before := m.sel
	// A stream chunk arrives while dragging: it must not move the
	// anchor/head (follow is suppressed while active).
	m2 := step(m, evt(event.KindMessageDelta, event.MessageDelta{Text: "more"}, benchPrincipalSID))
	if m2.sel != before {
		t.Fatalf("streaming moved the selection: before %v after %v", before, m2.sel)
	}
}

// TestSelectionWhileScrollingReflow verifies wheel scroll + a same-width
// reflow keep the selection, while a WindowSizeMsg clears it (its
// coordinates would copy the wrong text after a re-wrap).
func TestSelectionWhileScrollingReflow(t *testing.T) {
	m, _ := newTestTUI(t)
	m.ready = true
	m.width, m.height = 80, 24
	m.sel = selection{pressed: true, active: true, dragging: true, anchorRow: 0, anchorCol: 0, headRow: 0, headCol: 5}
	// Wheel scroll: selection stays.
	m2 := step(m, tea.MouseMsg{X: 10, Y: 10, Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown})
	if m2.sel != m.sel {
		t.Fatalf("wheel scroll moved the selection: %v -> %v", m.sel, m2.sel)
	}
	// Same-width reflow: selection stays.
	m3 := m2.reflow()
	if m3.sel != m.sel {
		t.Fatalf("reflow moved the selection: %v -> %v", m.sel, m3.sel)
	}
	// WindowSizeMsg: selection clears (re-wrap invalidates coordinates).
	m4 := step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	if m4.sel != (selection{}) {
		t.Fatalf("resize must clear the selection: got %v, want zero", m4.sel)
	}
}

// TestTerminalSelectionBypassDocumented verifies the Shift/alt+t terminal-
// selection bypass is discoverable: `docs/configuration.md` documents holding
// Shift on most terminals (the terminal intercepts the drag), and wakil itself
// exposes alt+t (the status hint renders "select:terminal (alt+t restores
// mouse)" while in that mode — seen by TestNativeSelectionStatusIndicator).
func TestTerminalSelectionBypassDocumented(t *testing.T) {
	m, _ := newTestTUI(t)
	m.nativeSel.active = true
	joined := strings.Join(m.statusLines(), "\n")
	if !strings.Contains(joined, "alt+t") {
		t.Fatalf("status hint must mention alt+t for the terminal-selection bypass; got %q", joined)
	}
}

// TestCopyOncePerAction verifies one drag-release → exactly one copy
// command, with a monotonic id per dispatch (so two releases never share
// an id and the stale-result guard can tell them apart).
func TestCopyOncePerAction(t *testing.T) {
	m, _ := newTestTUI(t)
	cmdA := m.copyToClipboard("a")
	cmdB := m.copyToClipboard("b")
	if cmdA == nil || cmdB == nil {
		t.Fatal("copyToClipboard must always return a command")
	}
	msgA, ok := cmdA().(copiedMsg)
	if !ok {
		t.Fatalf("first command returned %T, want copiedMsg", cmdA())
	}
	msgB, ok := cmdB().(copiedMsg)
	if !ok {
		t.Fatalf("second command returned %T, want copiedMsg", cmdB())
	}
	if msgA.id == 0 || msgB.id == 0 || msgB.id <= msgA.id {
		t.Fatalf("ids must be non-zero and monotonic: A=%d B=%d", msgA.id, msgB.id)
	}
	// Exactly-once delivery of the OSC escape is pinned by
	// TestCopiedMsgOSCEscapeEmission (first View carries it, the second
	// does not).
}
