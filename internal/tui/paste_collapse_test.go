package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// TestPasteCollapseLargeText: a bracketed paste of 5+ lines should be collapsed
// into a "[Pasted text +N lines]" placeholder in the textarea. The full text
// is stashed and expanded back at send time.
func TestPasteCollapseLargeText(t *testing.T) {
	m, _ := keyModel(t)
	m.state = stateIdle

	pasted := "line one\nline two\nline three\nline four\nline five"
	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(pasted), Paste: true})

	got := m.ta.Value()
	if strings.Contains(got, "line one") {
		t.Errorf("textarea should not contain the full pasted text; got %q", got)
	}
	if !strings.Contains(got, "[Pasted text +") {
		t.Errorf("textarea should contain a placeholder; got %q", got)
	}
	if len(m.pasteStash) != 1 {
		t.Errorf("pasteStash should have 1 entry; got %d", len(m.pasteStash))
	}
}

// TestPasteCollapseShortTextNotCollapsed: a 1–2 line paste flows into the
// textarea unchanged — no placeholder, no stash.
func TestPasteCollapseShortTextNotCollapsed(t *testing.T) {
	m, _ := keyModel(t)
	m.state = stateIdle

	pasted := "short paste"
	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(pasted), Paste: true})

	got := m.ta.Value()
	if got != "short paste" {
		t.Errorf("short paste should flow into textarea unchanged; got %q", got)
	}
	if len(m.pasteStash) != 0 {
		t.Errorf("pasteStash should be empty for short paste; got %d", len(m.pasteStash))
	}
}

// TestPasteCollapseExpandAtSend: pressing Enter after a collapsed paste
// should submit the full expanded text, not the placeholder.
func TestPasteCollapseExpandAtSend(t *testing.T) {
	m, f := keyModel(t)
	m.state = stateIdle

	pasted := "line one\nline two\nline three\nline four\nline five"
	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(pasted), Paste: true})

	// Verify it was collapsed.
	if len(m.pasteStash) != 1 {
		t.Fatalf("pasteStash should have 1 entry; got %d", len(m.pasteStash))
	}

	// Press Enter to send.
	m = step(m, tea.KeyMsg{Type: tea.KeyEnter})

	if len(f.submitted) != 1 {
		t.Fatalf("should have 1 submitted prompt; got %d", len(f.submitted))
	}
	submitted := f.submitted[0].Text
	if !strings.Contains(submitted, "line one") {
		t.Errorf("submitted text should contain the full pasted text; got %q", submitted)
	}
	if strings.Contains(submitted, "[Pasted text +") {
		t.Errorf("submitted text should NOT contain the placeholder; got %q", submitted)
	}
	// Stash should be cleared after send.
	if len(m.pasteStash) != 0 {
		t.Errorf("pasteStash should be cleared after send; got %d", len(m.pasteStash))
	}
}

// TestPasteCollapseTypingAfterPlaceholder: the user can type after a collapsed
// paste placeholder and both the pasted text and typed text are submitted.
func TestPasteCollapseTypingAfterPlaceholder(t *testing.T) {
	m, f := keyModel(t)
	m.state = stateIdle

	pasted := "line one\nline two\nline three\nline four\nline five"
	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(pasted), Paste: true})

	// Type additional text after the placeholder.
	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" and that's it")})

	// Press Enter to send.
	m = step(m, tea.KeyMsg{Type: tea.KeyEnter})

	if len(f.submitted) != 1 {
		t.Fatalf("should have 1 submitted prompt; got %d", len(f.submitted))
	}
	submitted := f.submitted[0].Text
	if !strings.Contains(submitted, "line one") {
		t.Errorf("submitted text should contain the full pasted text; got %q", submitted)
	}
	if !strings.Contains(submitted, "and that's it") {
		t.Errorf("submitted text should contain typed suffix; got %q", submitted)
	}
}

// TestPasteCollapseMultiplePastes: two collapsed pastes should both be expanded
// at send time.
func TestPasteCollapseMultiplePastes(t *testing.T) {
	m, f := keyModel(t)
	m.state = stateIdle

	paste1 := "alpha\nbeta\ngamma\ndelta2\nepsilon2"
	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(paste1), Paste: true})

	// Type a separator.
	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" --- ")})

	paste2 := "delta\nepsilon\nzeta\neta\ntheta"
	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(paste2), Paste: true})

	if len(m.pasteStash) != 2 {
		t.Fatalf("pasteStash should have 2 entries; got %d", len(m.pasteStash))
	}

	// Send.
	m = step(m, tea.KeyMsg{Type: tea.KeyEnter})

	if len(f.submitted) != 1 {
		t.Fatalf("should have 1 submitted prompt; got %d", len(f.submitted))
	}
	submitted := f.submitted[0].Text
	if !strings.Contains(submitted, "alpha") || !strings.Contains(submitted, "delta") {
		t.Errorf("submitted text should contain both pasted texts; got %q", submitted)
	}
	if strings.Contains(submitted, "[Pasted text +") {
		t.Errorf("submitted text should NOT contain placeholders; got %q", submitted)
	}
}

// TestPasteCollapsePruneOrphanedStashEntry: if the user deletes a placeholder
// from the textarea before sending, the orphaned stash entry is pruned — not
// left to accumulate.
func TestPasteCollapsePruneOrphanedStashEntry(t *testing.T) {
	m, _ := keyModel(t)
	m.state = stateIdle

	// Paste and collapse.
	pasted := "line one\nline two\nline three\nline four\nline five"
	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(pasted), Paste: true})

	if len(m.pasteStash) != 1 {
		t.Fatalf("pasteStash should have 1 entry; got %d", len(m.pasteStash))
	}

	// Capture the placeholder.
	ph := ""
	for k := range m.pasteStash {
		ph = k
	}
	if ph == "" {
		t.Fatal("expected non-empty placeholder key")
	}

	// Simulate the user deleting the placeholder: clear the textarea.
	m.ta.SetValue("")
	m = m.prunePasteStash()

	if len(m.pasteStash) != 0 {
		t.Errorf("orphaned stash entry should be pruned; got %d entries", len(m.pasteStash))
	}
}

// TestPasteCollapsePruneKeepsLivePlaceholder: pruning must NOT remove the
// live burst placeholder that is still visible in the textarea.
func TestPasteCollapsePruneKeepsLivePlaceholder(t *testing.T) {
	m, _ := keyModel(t)
	m.state = stateIdle

	// Paste and collapse.
	pasted := "line one\nline two\nline three\nline four\nline five"
	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(pasted), Paste: true})

	if len(m.pasteStash) != 1 {
		t.Fatalf("pasteStash should have 1 entry; got %d", len(m.pasteStash))
	}

	// Prune — the placeholder is still in the textarea.
	m = m.prunePasteStash()

	if len(m.pasteStash) != 1 {
		t.Errorf("live stash entry should NOT be pruned; got %d entries", len(m.pasteStash))
	}
}

// TestPasteBurstCollapse: a fragmented (non-bracketed) paste — a rapid stream
// of KeyRunes with Paste=false — is collapsed into a placeholder once the
// burst goes quiet and the tick fires.
func TestPasteBurstCollapse(t *testing.T) {
	m, _ := keyModel(t)
	m.state = stateIdle

	// Simulate a fragmented paste: several rapid KeyRunes fragments.
	frags := []string{"line one\n", "line two\n", "line three\n", "line four\n", "line five"}
	for _, f := range frags {
		m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(f)})
	}

	// Burst should be tracked.
	if m.pasteBurstRunes == 0 {
		t.Fatal("burst should be accumulating runes")
	}

	// Simulate the burst going quiet, then fire the tick.
	m.pasteBurstLast = time.Now().Add(-time.Second)
	m = step(m, pasteBurstTickMsg{seq: m.pasteBurstSeq})

	got := m.ta.Value()
	if strings.Contains(got, "line one") {
		t.Errorf("textarea should not contain the full pasted text; got %q", got)
	}
	if !strings.Contains(got, "[Pasted text +") {
		t.Errorf("textarea should contain a placeholder; got %q", got)
	}
	if len(m.pasteStash) != 1 {
		t.Errorf("pasteStash should have 1 entry; got %d", len(m.pasteStash))
	}
}

// TestPasteBurstEagerCollapse: the burst collapses IMMEDIATELY once it
// crosses the threshold — the full text is never waiting for the quiet gap.
// Later fragments of the same burst fold into the same placeholder.
func TestPasteBurstEagerCollapse(t *testing.T) {
	m, _ := keyModel(t)
	m.state = stateIdle

	// First fragment already crosses the 40-rune minimum and 5-line threshold.
	frag1 := "line one\nline two\nline three\nline four\nline five\n"
	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(frag1)})

	got := m.ta.Value()
	if strings.Contains(got, "line one") {
		t.Fatalf("burst should have collapsed eagerly; got %q", got)
	}
	if !strings.Contains(got, "[Pasted text +") {
		t.Fatalf("expected placeholder after eager collapse; got %q", got)
	}
	ph1 := m.pasteBurstPh
	if ph1 == "" {
		t.Fatal("pasteBurstPh should be set after eager collapse")
	}

	// More fragments of the same burst arrive — they must fold into the
	// stash, not appear in the textarea.
	frag2 := "line six\nline seven\nline eight"
	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(frag2)})

	got = m.ta.Value()
	if strings.Contains(got, "line six") {
		t.Errorf("later fragments must not appear in textarea; got %q", got)
	}
	if !strings.Contains(got, "[Pasted text +") {
		t.Errorf("placeholder should still be present; got %q", got)
	}
	// Placeholder regenerated with updated line count.
	if m.pasteBurstPh == ph1 {
		t.Errorf("placeholder should have been regenerated after new fragment")
	}
	if len(m.pasteStash) != 1 {
		t.Errorf("stash should hold exactly 1 merged entry; got %d", len(m.pasteStash))
	}

	// End the burst and send — the full merged text must be submitted.
	m.pasteBurstLast = time.Now().Add(-time.Second)
	m = step(m, pasteBurstTickMsg{seq: m.pasteBurstSeq})
	m = step(m, tea.KeyMsg{Type: tea.KeyEnter})
	// submitted via fake — check expansion indirectly through stash/textarea.
	if len(m.pasteStash) != 0 {
		t.Errorf("stash should be cleared after send; got %d", len(m.pasteStash))
	}
}

// TestPasteBurstEagerCollapseMultiFragmentContent: a multi-fragment burst
// must NOT bake the old placeholder literal into the stash. The expanded
// text at send time must contain only the original fragments, not the
// placeholder string.
func TestPasteBurstEagerCollapseMultiFragmentContent(t *testing.T) {
	m, f := keyModel(t)
	m.state = stateIdle

	// First fragment crosses the threshold.
	frag1 := "line one\nline two\nline three\nline four\nline five\n"
	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(frag1)})

	if m.pasteBurstPh == "" {
		t.Fatal("expected placeholder after first fragment")
	}

	// Second fragment — must fold into stash without baking the placeholder.
	frag2 := "line six\nline seven\nline eight"
	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(frag2)})

	// Assert exactly one merged stash entry, old key is gone.
	if len(m.pasteStash) != 1 {
		t.Fatalf("expected 1 merged stash entry; got %d", len(m.pasteStash))
	}

	// Assert the stash value is exactly frag1+frag2, no placeholder literal.
	for _, stashed := range m.pasteStash {
		if strings.Contains(stashed, "[Pasted text +") {
			t.Fatalf("stash contains placeholder literal: %q", stashed)
		}
		want := frag1 + frag2
		if stashed != want {
			t.Fatalf("stashed text = %q; want %q", stashed, want)
		}
	}

	// Third fragment — second continuation collapse.
	frag3 := "\nline nine\nline ten"
	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(frag3)})

	if len(m.pasteStash) != 1 {
		t.Fatalf("expected 1 merged stash entry after frag3; got %d", len(m.pasteStash))
	}
	for _, stashed := range m.pasteStash {
		if strings.Contains(stashed, "[Pasted text +") {
			t.Fatalf("stash contains placeholder literal after frag3: %q", stashed)
		}
		want := frag1 + frag2 + frag3
		if stashed != want {
			t.Fatalf("stashed text after frag3 = %q; want %q", stashed, want)
		}
	}

	// End the burst and send.
	m.pasteBurstLast = time.Now().Add(-time.Second)
	m = step(m, pasteBurstTickMsg{seq: m.pasteBurstSeq})
	m = step(m, tea.KeyMsg{Type: tea.KeyEnter})

	if len(f.submitted) != 1 {
		t.Fatalf("should have 1 submitted prompt; got %d", len(f.submitted))
	}
	submitted := f.submitted[0].Text

	// The submitted text must contain the exact concatenated fragments.
	want := frag1 + frag2 + frag3
	if !strings.Contains(submitted, want) {
		t.Errorf("submitted text should contain %q; got %q", want, submitted)
	}

	// The submitted text must NOT contain the placeholder literal.
	if strings.Contains(submitted, "[Pasted text +") {
		t.Errorf("submitted text should NOT contain placeholder literal; got %q", submitted)
	}
}

// TestPasteBurstSlowTypingNotCollapsed: slow human typing (gaps larger than
// pasteBurstMinGap between keys) must never be collapsed.
func TestPasteBurstSlowTypingNotCollapsed(t *testing.T) {
	m, _ := keyModel(t)
	m.state = stateIdle

	for _, r := range "hello there this is a fairly long typed sentence with many runes" {
		m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		// Simulate a human-typing gap between keys.
		m.pasteBurstLast = time.Now().Add(-pasteBurstMinGap - time.Millisecond)
	}

	m.pasteBurstLast = time.Now().Add(-time.Second)
	m = step(m, pasteBurstTickMsg{seq: m.pasteBurstSeq})

	got := m.ta.Value()
	if strings.Contains(got, "[Pasted text +") {
		t.Errorf("slow typing must not be collapsed; got %q", got)
	}
	if len(m.pasteStash) != 0 {
		t.Errorf("pasteStash should be empty; got %d", len(m.pasteStash))
	}
}

// TestPasteBurstImageCutEarly: a fragmented image paste is cut by the
// post-insert scan as soon as the PNG chunk train confirms it (~13 runes
// in) — long before the 96-rune garbage-tail fallback would be needed —
// even while the burst tracker is active.
func TestPasteBurstImageCutEarly(t *testing.T) {
	m, _ := keyModel(t)
	m.state = stateIdle

	// Mangled PNG as the terminal delivers it: NULs stripped, "PNG" +
	// "IHDR" + "IDAT" survive in the first fragment.
	frag := "PNG\n\n\n\nIHDRxxxxIDATyyyy"
	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(frag)})

	got := m.ta.Value()
	if strings.Contains(got, "PNG") {
		t.Errorf("image garbage should have been cut at chunk-train confirmation; got %q", got)
	}
	if m.pasteCutStash == "" {
		t.Error("pasteCutStash should hold the cut garbage for restore")
	}
	if m.pasteSuppressUntil.IsZero() {
		t.Error("suppression window should be armed for the paste tail")
	}
}

// TestPasteBurstEndDetectsStashedImage: an image paste whose mangled bytes
// slip past the per-key scan (no NUL, no chunk train, symbol ratio below the
// confirmation threshold) gets eagerly collapsed as TEXT. The burst-end tick
// must then detect the binary content in the stash, cut it, and trigger the
// clipboard read — without waiting for Enter.
func TestPasteBurstEndDetectsStashedImage(t *testing.T) {
	m, _ := keyModel(t)
	m.state = stateIdle

	// Simulate a mangled JPEG whose "JFIF" signature was stripped by the
	// terminal: no signature, no NUL — only a long symbol-dense garbage
	// tail. The per-key scan can't confirm it (no signature to anchor the
	// tail check), so it gets eagerly collapsed as text; the burst-end tick
	// must catch it via the symbol-density heuristic.
	var sb strings.Builder
	for i := 0; i < 10; i++ {
		sb.WriteString("x%9$#@!&*()_+{}|<>?~^[]\\;':,./-=~\n")
	}
	frag := sb.String()
	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(frag)})

	// If the per-key scan didn't fire, the burst tracker eagerly collapsed
	// the content as text (or left it in the textarea). Either way, fire the
	// burst-end tick and require the binary cut to happen there.
	m.pasteBurstLast = time.Now().Add(-time.Second)
	m = step(m, pasteBurstTickMsg{seq: m.pasteBurstSeq})

	got := m.ta.Value()
	if strings.Contains(got, "PNG") || strings.Contains(got, "abcdefghijklmnop") {
		t.Errorf("burst-end tick should have cut the image garbage; got %q", got)
	}
	if m.pasteCutStash == "" {
		t.Error("pasteCutStash should hold the cut garbage for restore")
	}
	if m.pasteSuppressUntil.IsZero() {
		t.Error("suppression window should be armed")
	}
}

// TestCountLines verifies the line-counting helper.
func TestCountLines(t *testing.T) {
	for _, tc := range []struct {
		s    string
		want int
	}{
		{"", 0},
		{"one line", 1},
		{"two\nlines", 2},
		{"three\nlines\nhere", 3},
		{"trailing\nnewline\n", 2},
	} {
		got := countLines(tc.s)
		if got != tc.want {
			t.Errorf("countLines(%q) = %d, want %d", tc.s, got, tc.want)
		}
	}
}
