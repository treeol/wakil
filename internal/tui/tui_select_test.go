package tui

import (
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
