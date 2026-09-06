package tui

import (
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// TestTurnBoxCachePersistence verifies that rendering through index ranges
// (not value copies) persists the item cache in *m.items, and that a
// same-width rebuild reuses the cache (the cache string is unchanged).
func TestTurnBoxCachePersistence(t *testing.T) {
	m := newTabModel()
	m.width, m.height = 80, 30
	m = m.reflow()

	m.addItem(iUser, "hello")
	m.addItem(iAsst, "response with **bold**")

	// First refresh — renders and caches.
	m.prefixDirty = true
	m.refreshViewport()

	innerW := m.vp.Width - turnBoxBorderW
	if innerW < 1 {
		innerW = 1
	}

	// Capture cache values after first render.
	caches := make([]string, len(*m.items))
	for i := range *m.items {
		item := &(*m.items)[i]
		if item.cacheW != innerW {
			t.Errorf("item %d cacheW = %d, want %d (cache not persisted)", i, item.cacheW, innerW)
		}
		if item.cache == "" {
			t.Errorf("item %d cache is empty (cache not persisted)", i)
		}
		caches[i] = item.cache
	}

	// Second refresh at same width with prefixDirty=true (force rebuild).
	// The cache should be REUSED — item.cache should be unchanged.
	m.prefixDirty = true
	m.refreshViewport()
	for i := range *m.items {
		if (*m.items)[i].cache != caches[i] {
			t.Errorf("item %d cache changed after same-width rebuild (cache not reused)", i)
		}
	}

	// Resize: cache should be invalidated and recomputed.
	m.width = 100
	m = m.reflow()
	m.prefixDirty = true
	m.refreshViewport()
	newInnerW := m.vp.Width - turnBoxBorderW
	for i := range *m.items {
		if (*m.items)[i].cacheW != newInnerW {
			t.Errorf("item %d cacheW = %d after resize, want %d (cache not invalidated)", i, (*m.items)[i].cacheW, newInnerW)
		}
	}
}

// TestTurnBoxIDiagGrouping verifies iDiag items are grouped with the preceding turn.
func TestTurnBoxIDiagGrouping(t *testing.T) {
	m := newTabModel()
	m.width, m.height = 80, 30
	m = m.reflow()

	m.addItem(iUser, "question")
	m.addItem(iDiag, "· thought: analyzing")
	m.addItem(iAsst, "answer")

	m.prefixDirty = true
	m.refreshViewport()

	plain := ansi.Strip(m.prefixStyled)
	borderCount := 0
	for _, line := range strings.Split(plain, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "╭") || strings.HasPrefix(strings.TrimSpace(line), "┌") {
			borderCount++
		}
	}
	if borderCount != 1 {
		t.Errorf("expected 1 box (iDiag grouped with turn), found %d", borderCount)
	}
}

// TestTurnBoxConsecutiveIUser verifies consecutive user messages (no assistant
// between them) each get their own box.
func TestTurnBoxConsecutiveIUser(t *testing.T) {
	m := newTabModel()
	m.width, m.height = 80, 30
	m = m.reflow()

	m.addItem(iUser, "first")
	m.addItem(iUser, "second")
	m.addItem(iUser, "third")

	m.prefixDirty = true
	m.refreshViewport()

	plain := ansi.Strip(m.prefixStyled)
	borderCount := 0
	for _, line := range strings.Split(plain, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "╭") || strings.HasPrefix(strings.TrimSpace(line), "┌") {
			borderCount++
		}
	}
	if borderCount != 3 {
		t.Errorf("expected 3 boxes for 3 consecutive users, found %d", borderCount)
	}
}

// TestTurnBoxEmptyItems verifies no panic and no content with empty items.
func TestTurnBoxEmptyItems(t *testing.T) {
	m := newTabModel()
	m.width, m.height = 80, 30
	m = m.reflow()

	m.prefixDirty = true
	m.refreshViewport()

	if m.prefixStyled != "" {
		t.Errorf("expected empty prefixStyled for no items, got %q", m.prefixStyled)
	}
}

// TestTurnBoxSingleItem verifies a single item gets one box.
func TestTurnBoxSingleItem(t *testing.T) {
	m := newTabModel()
	m.width, m.height = 80, 30
	m = m.reflow()

	m.addItem(iAsst, "just an assistant message")

	m.prefixDirty = true
	m.refreshViewport()

	plain := ansi.Strip(m.prefixStyled)
	borderCount := 0
	for _, line := range strings.Split(plain, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "╭") || strings.HasPrefix(strings.TrimSpace(line), "┌") {
			borderCount++
		}
	}
	if borderCount != 1 {
		t.Errorf("expected 1 box for single item, found %d", borderCount)
	}
}

// TestTurnBoxSelectedTextNoBorderGlyphs verifies that selectedText does not
// include box border characters when selecting across content.
func TestTurnBoxSelectedTextNoBorderGlyphs(t *testing.T) {
	m := newTabModel()
	m.width, m.height = 80, 30
	m = m.reflow()

	m.addItem(iUser, "hello world")
	m.addItem(iAsst, "this is the response")

	m.prefixDirty = true
	m.refreshViewport()

	// Simulate a selection spanning content lines (not border rows).
	// plainLines includes border lines; select only content lines.
	// Find the first content line (skip border rows).
	contentStart := -1
	for i, line := range m.plainLines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !isBoxBorderLine(trimmed) {
			contentStart = i
			break
		}
	}
	if contentStart < 0 {
		t.Fatal("no content line found in plainLines")
	}

	// Select a single content line.
	m.sel = selection{
		active:    true,
		anchorRow: contentStart,
		anchorCol: 0,
		headRow:   contentStart,
		headCol:   10,
	}
	text := m.selectedText()

	// Must not contain border glyphs.
	borderChars := []string{"│", "╭", "╮", "╰", "╯", "─"}
	for _, bc := range borderChars {
		if strings.Contains(text, bc) {
			t.Errorf("selectedText contains border glyph %q: %q", bc, text)
		}
	}
	if text == "" {
		t.Error("selectedText should not be empty for a content selection")
	}
}

// TestTurnBoxSelectedTextExactContent verifies that selectedText returns the
// exact conversation text (not border glyphs) when selecting through the
// refreshViewport path with boxes rendered.
func TestTurnBoxSelectedTextExactContent(t *testing.T) {
	m := newTabModel()
	m.width, m.height = 80, 30
	m = m.reflow()

	m.addItem(iUser, "hello world")

	m.prefixDirty = true
	m.refreshViewport()

	// Find content row in plainLines (skip border rows).
	contentRow := -1
	for i, line := range m.plainLines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !isBoxBorderLine(trimmed) {
			contentRow = i
			break
		}
	}
	if contentRow < 0 {
		t.Fatal("no content line found")
	}

	// Select the entire content line.
	m.sel = selection{
		active:    true,
		anchorRow: contentRow,
		anchorCol: 1, // past the left border
		headRow:   contentRow,
		headCol:   11, // "hello world" = 11 chars
	}
	text := m.selectedText()

	// Should contain "hello world" or a prefix of it, without border chars.
	for _, bc := range []string{"│", "╭", "╮", "╰", "╯", "─"} {
		if strings.Contains(text, bc) {
			t.Errorf("selectedText contains border glyph %q: %q", bc, text)
		}
	}
	if !strings.Contains(text, "hello") {
		t.Errorf("selectedText should contain 'hello', got %q", text)
	}
}

// TestTurnBoxNarrowWidth verifies no panic at very narrow widths.
func TestTurnBoxNarrowWidth(t *testing.T) {
	widths := []int{0, 1, 2, 3, 4, 5}
	for _, w := range widths {
		t.Run("width_"+strconv.Itoa(w), func(t *testing.T) {
			m := newTabModel()
			m.width, m.height = w, 10
			m = m.reflow()

			m.addItem(iUser, "hello")
			m.addItem(iAsst, "response")

			m.prefixDirty = true
			m.refreshViewport()

			// Must not panic and must produce some output (or empty at width 0).
			_ = m.prefixStyled
		})
	}
}

// TestTurnBoxPlainLinesMatchesViewport verifies that plainLines length
// matches the viewport content after refresh.
func TestTurnBoxPlainLinesMatchesViewport(t *testing.T) {
	m := newTabModel()
	m.width, m.height = 80, 30
	m = m.reflow()

	m.addItem(iUser, "test")
	m.addItem(iAsst, "response")

	m.prefixDirty = true
	m.refreshViewport()

	if len(m.plainLines) != m.vp.TotalLineCount() {
		t.Errorf("plainLines len = %d, vp.TotalLineCount = %d (must match for selection correctness)",
			len(m.plainLines), m.vp.TotalLineCount())
	}
}
