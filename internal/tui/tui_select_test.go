package tui

import (
	"bytes"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func mdl(lines ...string) tuiModel {
	return tuiModel{plainLines: lines}
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

func TestSelectedTextMultiLineTrimsPadding(t *testing.T) {
	// glamour pads lines with trailing spaces; selection should drop them.
	m := mdl("first line   ", "second line  ", "third")
	m.sel = selection{active: true, anchorRow: 0, anchorCol: 6, headRow: 2, headCol: 4}
	want := "line\nsecond line\nthird"
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
	if cmd := copyToClipboard("   "); cmd == nil {
		t.Fatal("expected a command")
	} else if msg := cmd(); msg != nil {
		t.Fatalf("blank selection should produce no copiedMsg, got %#v", msg)
	}
}

// withClipboardOut swaps clipboardOut for the duration of the test and restores
// it on cleanup. Returns a bytes.Buffer the caller can inspect.
func withClipboardOut(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := new(bytes.Buffer)
	orig := clipboardOut
	clipboardOut = buf
	t.Cleanup(func() { clipboardOut = orig })
	return buf
}

// TestCopyToClipboardOSCFallback verifies that when no native clipboard writer
// is available (the common SSH scenario), copyToClipboard falls back to OSC 52
// and reports via=copyViaOSC52 so the UI can hint the user.
func TestCopyToClipboardOSCFallback(t *testing.T) {
	orig := clipboardCmds
	clipboardCmds = nil
	defer func() { clipboardCmds = orig }()

	buf := withClipboardOut(t)

	cmd := copyToClipboard("hello")
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
	// The OSC 52 escape sequence must have been written to the injected writer.
	if !strings.Contains(buf.String(), "\x1b]52;c;") {
		t.Errorf("expected OSC 52 escape in output, got %q", buf.String())
	}
}

// TestCopyToClipboardUnicode verifies the rune count is correct for non-ASCII.
func TestCopyToClipboardUnicode(t *testing.T) {
	orig := clipboardCmds
	clipboardCmds = nil
	defer func() { clipboardCmds = orig }()

	withClipboardOut(t)

	cmd := copyToClipboard("å🙂")
	msg, ok := cmd().(copiedMsg)
	if !ok {
		t.Fatalf("expected copiedMsg, got %T", cmd())
	}
	if msg.n != 2 {
		t.Errorf("n=%d want 2 (runes), got %d", msg.n, msg.n)
	}
}

// TestCopiedMsgOSCHint verifies the flash message includes the OSC 52 hint when
// via is copyViaOSC52, and shows the normal copy confirmation for native.
func TestCopiedMsgOSCHint(t *testing.T) {
	// OSC 52 fallback path — hint should appear, no checkmark.
	m := newTestTUI(t)
	m = step(m, copiedMsg{n: 10, via: copyViaOSC52})
	if !strings.Contains(m.flash, "OSC 52") {
		t.Errorf("osc52 flash should mention OSC 52; got %q", m.flash)
	}
	if !strings.Contains(m.flash, "10") {
		t.Errorf("flash should include char count; got %q", m.flash)
	}
	if strings.Contains(m.flash, "✓") {
		t.Errorf("osc52 flash should NOT have checkmark; got %q", m.flash)
	}

	// Native clipboard path — normal confirmation, no OSC 52 hint.
	m = newTestTUI(t)
	m = step(m, copiedMsg{n: 20, via: copyViaNative})
	if strings.Contains(m.flash, "OSC 52") {
		t.Errorf("native flash should NOT mention OSC 52; got %q", m.flash)
	}
	if !strings.Contains(m.flash, "20") {
		t.Errorf("flash should include char count; got %q", m.flash)
	}
	if !strings.Contains(m.flash, "✓") {
		t.Errorf("native flash should have checkmark; got %q", m.flash)
	}

	// Legacy/empty via (backward compat) — no hint, no OSC 52 mention.
	m = newTestTUI(t)
	m = step(m, copiedMsg{n: 30})
	if strings.Contains(m.flash, "OSC 52") {
		t.Errorf("empty-via flash should NOT mention OSC 52; got %q", m.flash)
	}
}
