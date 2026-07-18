package tui

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	agent "github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/proxy"
)

// TestContainsBinary_NULByte detects a NUL byte in pasted runes — the most
// common signal of binary content that was interpreted as text.
func TestContainsBinary_NULByte(t *testing.T) {
	runes := []rune("hello\x00world")
	if !containsBinary(runes) {
		t.Error("runes with NUL byte should be detected as binary")
	}
}

// TestContainsBinary_PNGMagic detects PNG magic bytes delivered as runes.
// A real PNG paste always includes NUL bytes in the IHDR chunk length right
// after the 8-byte signature — the NUL check catches it. This test uses the
// full signature + partial IHDR to exercise the magic-byte path too.
func TestContainsBinary_PNGMagic(t *testing.T) {
	// PNG signature + IHDR chunk length (contains NUL bytes)
	runes := []rune{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D}
	if !containsBinary(runes) {
		t.Error("PNG magic bytes should be detected as binary")
	}
}

// TestContainsBinary_JPEGMagic detects JPEG magic bytes delivered as runes.
func TestContainsBinary_JPEGMagic(t *testing.T) {
	// JPEG magic: \xff \xd8 \xff
	runes := []rune{0xFF, 0xD8, 0xFF, 0xE0, 0, 0, 0, 0, 0, 0, 0, 0}
	if !containsBinary(runes) {
		t.Error("JPEG magic bytes should be detected as binary")
	}
}

// TestContainsBinary_GIFMagic detects GIF magic bytes delivered as runes.
func TestContainsBinary_GIFMagic(t *testing.T) {
	runes := []rune("GIF89a000000")
	if !containsBinary(runes) {
		t.Error("GIF magic bytes should be detected as binary")
	}
}

// TestContainsBinary_WebPMagic detects WebP magic bytes delivered as runes.
func TestContainsBinary_WebPMagic(t *testing.T) {
	runes := []rune{'R', 'I', 'F', 'F', 0, 0, 0, 0, 'W', 'E', 'B', 'P'}
	if !containsBinary(runes) {
		t.Error("WebP magic bytes should be detected as binary")
	}
}

// TestContainsBinary_PlainText does not flag normal text pastes — including
// non-ASCII Unicode, which should never trigger interception.
func TestContainsBinary_PlainText(t *testing.T) {
	texts := []string{
		"hello world",
		"normal text paste with unicode: café ☕ 日本語",
		"function main() { return 42; }",
		"{\"type\":\"text\",\"content\":\"hello\"}",
		"multi\nline\ntext\npaste",
	}
	for _, text := range texts {
		if containsBinary([]rune(text)) {
			t.Errorf("normal text should not be detected as binary: %q", text)
		}
	}
}

// TestContainsBinary_EmptyString is not binary.
func TestContainsBinary_EmptyString(t *testing.T) {
	if containsBinary([]rune("")) {
		t.Error("empty string should not be detected as binary")
	}
}

// mangleAsBubbletea reproduces what bubbletea's detectBracketedPaste does to
// pasted bytes: UTF-8 decode, DROPPING utf8.RuneError runes (invalid bytes
// like PNG's 0x89 or JPEG's 0xFF vanish). See bubbletea key_sequences.go.
func mangleAsBubbletea(data []byte) []rune {
	var runes []rune
	for len(data) > 0 {
		r, w := utf8.DecodeRune(data)
		if r != utf8.RuneError {
			runes = append(runes, r)
		}
		data = data[w:]
	}
	return runes
}

// stripControls additionally removes NUL/control characters, as most
// terminals do to bracketed pastes (paste-injection protection). Keeps \n \r \t.
func stripControls(runes []rune) []rune {
	out := runes[:0:0]
	for _, r := range runes {
		if r < 0x20 && r != '\n' && r != '\r' && r != '\t' {
			continue
		}
		out = append(out, r)
	}
	return out
}

// TestContainsBinary_MangledPNGPaste is the real-world case: a PNG pasted
// through bubbletea loses its 0x89 lead byte (invalid UTF-8) so the intact
// magic check can never match — detection must work on the remnants.
func TestContainsBinary_MangledPNGPaste(t *testing.T) {
	runes := mangleAsBubbletea(makeTestPNG())
	if !containsBinary(runes) {
		t.Error("bubbletea-mangled PNG paste should be detected as binary")
	}
}

// TestContainsBinary_MangledPNGPasteControlsStripped goes further: the
// terminal also strips NUL/control bytes before bubbletea sees them (the
// common GNOME/VTE behavior). Only printable scaffolding like "PNG" and
// "IHDR" survives.
func TestContainsBinary_MangledPNGPasteControlsStripped(t *testing.T) {
	runes := stripControls(mangleAsBubbletea(makeTestPNG()))
	// Sanity: no NULs left, no intact magic — the old checks would both miss.
	for _, r := range runes {
		if r == 0 {
			t.Fatal("test setup wrong: NUL survived stripping")
		}
	}
	if !containsBinary(runes) {
		t.Error("control-stripped mangled PNG paste should be detected as binary")
	}
}

// TestContainsBinary_MangledJPEGPaste: JPEG loses 0xFF 0xD8 0xFF (invalid
// UTF-8) but the "JFIF" APP0 identifier survives near the start.
func TestContainsBinary_MangledJPEGPaste(t *testing.T) {
	// Minimal JPEG header: FF D8 FF E0 <len> "JFIF\0" ...
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01, 0x01, 0x00}
	runes := stripControls(mangleAsBubbletea(jpeg))
	if !containsBinary(runes) {
		t.Error("mangled JPEG paste should be detected as binary")
	}
}

// TestContainsBinary_MangledWebPPaste: "RIFF" and "WEBP" are printable ASCII
// and survive; the binary size bytes between them are stripped.
func TestContainsBinary_MangledWebPPaste(t *testing.T) {
	webp := []byte{'R', 'I', 'F', 'F', 0x1A, 0x00, 0x00, 0x00, 'W', 'E', 'B', 'P', 'V', 'P', '8', ' '}
	runes := stripControls(mangleAsBubbletea(webp))
	if !containsBinary(runes) {
		t.Error("mangled WebP paste should be detected as binary")
	}
}

// TestContainsBinary_TextMentioningPNG must NOT trigger: prose that merely
// mentions PNG or IHDR in a sentence doesn't put "PNG" at position 0-1.
func TestContainsBinary_TextMentioningPNG(t *testing.T) {
	texts := []string{
		"the PNG file has an IHDR chunk at the start",
		"convert image.PNG to JPEG using JFIF standard",
		"GIF images start with GIF89a usually",
		"see RIFF container docs for WEBP details",
		"PNG", // bare word, no IHDR anywhere
	}
	for _, text := range texts {
		if containsBinary([]rune(text)) {
			t.Errorf("prose mentioning image formats should not be detected as binary: %q", text)
		}
	}
}

// TestContainsBinary_PNGWordAtStartWithIHDRLater is the deliberate edge: text
// starting with "PNG" AND containing "IHDR" within 64 runes will false-
// positive. Document the tradeoff — this is pathological input for a chat
// prompt, and the failure mode is a clipboard read, not data loss.
func TestContainsBinary_PNGWordAtStartWithIHDRLater(t *testing.T) {
	s := "PNG files contain IHDR chunks"
	if !containsBinary([]rune(s)) {
		t.Skip("documented false-positive case no longer triggers — detection tightened")
	}
}

// TestBinaryPasteInterception verifies that a binary paste — with the runes
// mangled exactly as bubbletea + a control-stripping terminal would deliver
// them — triggers the clipboard read and does not insert into the textarea.
func TestBinaryPasteInterception(t *testing.T) {
	m := keyModel(t)
	runes := stripControls(mangleAsBubbletea(makeTestPNG()))
	msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: runes, Paste: true}

	m2 := step(m, msg)
	// The textarea should NOT contain the binary bytes.
	if m2.ta.Value() != "" {
		t.Errorf("textarea should be empty after binary paste; got %q", m2.ta.Value())
	}
}

// TestBinaryPasteInterception_NonBracketed covers terminals without bracketed
// paste: the burst arrives as a multi-rune KeyRunes with Paste=false.
func TestBinaryPasteInterception_NonBracketed(t *testing.T) {
	m := keyModel(t)
	runes := stripControls(mangleAsBubbletea(makeTestPNG()))
	msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: runes, Paste: false}

	m2 := step(m, msg)
	if m2.ta.Value() != "" {
		t.Errorf("textarea should be empty after non-bracketed binary paste; got %q", m2.ta.Value())
	}
}

// TestSendTimeSafetyNet verifies that if mangled image content somehow lands
// in the textarea anyway, Enter refuses to send it and reads the clipboard.
func TestSendTimeSafetyNet(t *testing.T) {
	m := keyModel(t)
	mangled := string(stripControls(mangleAsBubbletea(makeTestPNG())))
	m.ta.SetValue(mangled)

	m2, cmds, consumed := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if !consumed {
		t.Fatal("Enter should be consumed by the safety net")
	}
	if len(cmds) != 1 {
		t.Fatalf("want 1 cmd (clipboard read), got %d", len(cmds))
	}
	if m2.state != stateIdle {
		t.Error("safety net must not start an agent turn")
	}
	if m2.ta.Value() != "" {
		t.Errorf("textarea should be reset; got %q", m2.ta.Value())
	}
	// The garbage must not enter input history.
	for _, h := range m2.inputHistory {
		if h == mangled {
			t.Error("mangled paste must not be saved to input history")
		}
	}
}

// TestClipboardImageMsg_AttachImage verifies that a successful clipboard
// image read appends to PendingImages and inserts a compact placeholder chip
// into the text input (not the transcript) so the input stays readable.
func TestClipboardImageMsg_AttachImage(t *testing.T) {
	m := keyModel(t)
	pngData := makeTestPNG()
	img, _ := proxy.LoadImageFromBytes(pngData, "clipboard:png")

	m2 := step(m, clipboardImageMsg{Img: img})
	if len(m2.app.PendingImages) != 1 {
		t.Fatalf("expected 1 pending image, got %d", len(m2.app.PendingImages))
	}
	if m2.app.PendingImages[0].MIME != "image/png" {
		t.Errorf("pending image MIME=%q, want image/png", m2.app.PendingImages[0].MIME)
	}
	// The chip must appear in the text input, and be tracked for send-time
	// reconciliation.
	chip := img.Placeholder()
	if !strings.Contains(m2.ta.Value(), chip) {
		t.Errorf("text input should contain the chip %q; got %q", chip, m2.ta.Value())
	}
	if len(*m2.imageChips) != 1 || (*m2.imageChips)[0] != chip {
		t.Errorf("imageChips should track the chip; got %v", *m2.imageChips)
	}
}

// TestReconcileImageChips_StripsSurvivingChip: a chip left in the input is
// removed from the outgoing text; the pending image stays attached.
func TestReconcileImageChips_StripsSurvivingChip(t *testing.T) {
	img, _ := proxy.LoadImageFromBytes(makeTestPNG(), "clipboard:png")
	chip := img.Placeholder()
	input := "what is in this image? " + chip + " thanks"

	text, pending := reconcileImageChips(input, []string{chip}, []proxy.ImagePart{img})
	if strings.Contains(text, chip) {
		t.Errorf("chip should be stripped from outgoing text; got %q", text)
	}
	if text != "what is in this image? thanks" {
		t.Errorf("outgoing text = %q, want %q", text, "what is in this image? thanks")
	}
	if len(pending) != 1 {
		t.Errorf("image should stay attached; got %d pending", len(pending))
	}
}

// TestReconcileImageChips_DeletedChipDetaches: the user deleted the chip from
// the input before sending — the image must be detached.
func TestReconcileImageChips_DeletedChipDetaches(t *testing.T) {
	img, _ := proxy.LoadImageFromBytes(makeTestPNG(), "clipboard:png")
	chip := img.Placeholder()
	input := "just a text question, no image"

	text, pending := reconcileImageChips(input, []string{chip}, []proxy.ImagePart{img})
	if len(pending) != 0 {
		t.Errorf("deleted chip should detach the image; got %d pending", len(pending))
	}
	if text != input {
		t.Errorf("text should be unchanged; got %q", text)
	}
}

// TestReconcileImageChips_PathImagesUntouched: images queued via
// /image <path> have no chip and must never be detached by reconciliation.
func TestReconcileImageChips_PathImagesUntouched(t *testing.T) {
	clipImg, _ := proxy.LoadImageFromBytes(makeTestPNG(), "clipboard:png")
	pathImg, _ := proxy.LoadImageFromBytes(makeTestPNG(), "/home/user/shot.png")
	chip := clipImg.Placeholder()
	// Chip deleted from input; path-queued image has no chip.
	input := "describe the attached screenshot"

	_, pending := reconcileImageChips(input, []string{chip}, []proxy.ImagePart{clipImg, pathImg})
	if len(pending) != 1 {
		t.Fatalf("want 1 surviving image (the path one), got %d", len(pending))
	}
	if pending[0].Path != "/home/user/shot.png" {
		t.Errorf("surviving image should be the path-queued one; got %q", pending[0].Path)
	}
}

// TestSendWithChipOnlyInput: sending an input that is nothing but the chip is
// a legitimate image-only message — it must start a turn, with the chip
// stripped from the outgoing text.
func TestSendWithChipOnlyInput(t *testing.T) {
	m := keyModel(t)
	img, _ := proxy.LoadImageFromBytes(makeTestPNG(), "clipboard:png")
	m = step(m, clipboardImageMsg{Img: img})
	// Input now holds just the chip (plus trailing space from insertion).

	m2, cmds, consumed := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if !consumed {
		t.Fatal("Enter should be consumed (send)")
	}
	if m2.state != stateStreaming {
		t.Errorf("image-only send should start a turn; state=%v", m2.state)
	}
	if len(cmds) == 0 {
		t.Error("send should produce the turn cmd")
	}
	if len(m2.app.PendingImages) != 1 {
		t.Errorf("image should still be pending for Send to consume; got %d", len(m2.app.PendingImages))
	}
	if len(*m2.imageChips) != 0 {
		t.Errorf("chips should be cleared after send; got %v", *m2.imageChips)
	}
}

// fragment splits runes into chunks of at most n, mimicking how a
// non-bracketed paste is split into multiple KeyRunes events (bubbletea
// breaks the stream at control bytes and read-buffer boundaries).
func fragment(runes []rune, n int) [][]rune {
	var out [][]rune
	for len(runes) > 0 {
		k := n
		if k > len(runes) {
			k = len(runes)
		}
		out = append(out, runes[:k])
		runes = runes[k:]
	}
	return out
}

// TestFragmentedPasteCollapsesImmediately is the live-collapse case: the
// paste arrives as several small KeyRunes events, none individually
// recognizable. The post-insert textarea scan must detect the accumulated
// garbage and collapse it as soon as enough fragments have landed — without
// the user pressing Enter.
func TestFragmentedPasteCollapsesImmediately(t *testing.T) {
	m := keyModel(t)
	mangled := stripControls(mangleAsBubbletea(makeTestPNG()))

	// Feed fragments of 4 runes each — far below what any single-event
	// signature check could recognize.
	for _, frag := range fragment(mangled, 4) {
		m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: frag})
	}

	if idx := binaryPasteStart(m.ta.Value()); idx >= 0 {
		t.Errorf("garbage should have been collapsed out of the textarea; still present in %q", m.ta.Value())
	}
	if m.pasteSuppressUntil.IsZero() {
		t.Error("suppression window should be open after mid-stream collapse")
	}
}

// TestFragmentedPastePreservesTypedPrefix: text the user typed before pasting
// must survive the collapse; only the garbage is removed.
func TestFragmentedPastePreservesTypedPrefix(t *testing.T) {
	m := keyModel(t)
	m.ta.SetValue("describe this: ")
	m.ta.CursorEnd()

	mangled := stripControls(mangleAsBubbletea(makeTestPNG()))
	for _, frag := range fragment(mangled, 4) {
		m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: frag})
	}

	got := m.ta.Value()
	if !strings.HasPrefix(got, "describe this:") {
		t.Errorf("typed prefix should be preserved; got %q", got)
	}
	if idx := binaryPasteStart(got); idx >= 0 {
		t.Errorf("garbage should be gone from the textarea; got %q", got)
	}
}

// TestSuppressionWindowSwallowsPasteTail: while the window is open, further
// key events (the rest of the paste, including a control byte decoded as
// "enter") are swallowed — no send, no textarea pollution.
func TestSuppressionWindowSwallowsPasteTail(t *testing.T) {
	m := keyModel(t)
	m.pasteSuppressUntil = time.Now().Add(pasteSuppressWindow)

	// A tail fragment and a stray enter (0x0D decoded as a key).
	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("IDATx")})
	m = step(m, tea.KeyMsg{Type: tea.KeyEnter})

	if m.ta.Value() != "" {
		t.Errorf("paste tail should be swallowed; textarea has %q", m.ta.Value())
	}
	if m.state != stateIdle {
		t.Errorf("stray enter in paste tail must not send; state=%v", m.state)
	}
	if m.pasteSuppressUntil.IsZero() {
		t.Error("window should have been extended by the swallowed events")
	}
}

// TestSuppressionWindowExpires: after the window closes, typing works again.
func TestSuppressionWindowExpires(t *testing.T) {
	m := keyModel(t)
	m.pasteSuppressUntil = time.Now().Add(-time.Millisecond) // already expired

	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("h")})
	if m.ta.Value() != "h" {
		t.Errorf("typing after window expiry should reach the textarea; got %q", m.ta.Value())
	}
	if !m.pasteSuppressUntil.IsZero() {
		t.Error("expired window should be cleared")
	}
}

// TestPostInsertScanIgnoresSingleKeystrokes: hand-typing prose that mentions
// image formats must never trigger the collapse — signature hits require
// confirmation (NUL, PNG chunk train, or a symbol-dense ≥96-rune tail) that
// short typed prose can never satisfy.
func TestPostInsertScanIgnoresSingleKeystrokes(t *testing.T) {
	m := keyModel(t)
	for _, r := range "PNG IHDR JFIF GIF89a RIFF WEBP" {
		m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	want := "PNG IHDR JFIF GIF89a RIFF WEBP"
	if m.ta.Value() != want {
		t.Errorf("hand-typed text must never be collapsed; got %q, want %q", m.ta.Value(), want)
	}
}

// TestSingleRunePasteDeliveryCollapses covers terminals that deliver a paste
// as rapid SINGLE-rune events (the user's reported case): the accumulated
// scan must still fire once the garbage is recognizable, even though no
// event ever carries more than one rune.
func TestSingleRunePasteDeliveryCollapses(t *testing.T) {
	m := keyModel(t)
	m.ta.SetValue("this is how it looks like: ")
	m.ta.CursorEnd()

	mangled := stripControls(mangleAsBubbletea(makeTestPNG()))
	for _, r := range mangled {
		m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}

	got := m.ta.Value()
	if !strings.HasPrefix(got, "this is how it looks like:") {
		t.Errorf("typed prefix must be preserved; got %q", got)
	}
	if idx := binaryPasteStart(got); idx >= 0 {
		t.Errorf("garbage should be collapsed out; got %q", got)
	}
	if m.pasteCutStash == "" {
		t.Error("cut stash should hold the removed garbage for false-positive recovery")
	}
}

// TestSendTimeSafetyNet_WithTypedPrefix is the user's exact report: typed
// text in front of the garbage, then Enter. The position-independent scan
// must catch it, keep the prefix, and read the clipboard.
func TestSendTimeSafetyNet_WithTypedPrefix(t *testing.T) {
	m := keyModel(t)
	mangled := string(stripControls(mangleAsBubbletea(makeTestPNG())))
	m.ta.SetValue("this is how it looks like: " + mangled)

	m2, cmds, consumed := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if !consumed {
		t.Fatal("Enter should be consumed by the safety net")
	}
	if len(cmds) != 1 {
		t.Fatalf("want 1 cmd (clipboard read), got %d", len(cmds))
	}
	if m2.state != stateIdle {
		t.Error("safety net must not start an agent turn")
	}
	if !strings.HasPrefix(m2.ta.Value(), "this is how it looks like:") {
		t.Errorf("typed prefix should be restored to the input; got %q", m2.ta.Value())
	}
	if idx := binaryPasteStart(m2.ta.Value()); idx >= 0 {
		t.Errorf("garbage must not remain in the input; got %q", m2.ta.Value())
	}
}

// TestFalsePositiveRestoresCutText: when the collapse fires but the clipboard
// has no image, the cut text is restored — a false positive never loses data.
func TestFalsePositiveRestoresCutText(t *testing.T) {
	m := keyModel(t)
	m.ta.SetValue("kept prefix ")
	m.ta.CursorEnd()
	m.pasteCutStash = "JFIF and then a long hexdump analysis..."

	m2 := step(m, clipboardImageMsg{Err: "no clipboard backend available"})
	if !strings.Contains(m2.ta.Value(), "JFIF and then a long hexdump analysis...") {
		t.Errorf("cut text should be restored on clipboard failure; got %q", m2.ta.Value())
	}
	if m2.pasteCutStash != "" {
		t.Error("stash should be cleared after restore")
	}
	if len(m2.app.PendingImages) != 0 {
		t.Error("no image should be attached on failure")
	}
}

// TestSuccessDiscardsStash: when the clipboard read succeeds, the cut garbage
// stays gone and the chip replaces it.
func TestSuccessDiscardsStash(t *testing.T) {
	m := keyModel(t)
	m.pasteCutStash = "PNG IHDR <garbage>"
	img, _ := proxy.LoadImageFromBytes(makeTestPNG(), "clipboard:png")

	m2 := step(m, clipboardImageMsg{Img: img})
	if m2.pasteCutStash != "" {
		t.Error("stash should be cleared on success")
	}
	if strings.Contains(m2.ta.Value(), "garbage") {
		t.Errorf("garbage must not be restored on success; got %q", m2.ta.Value())
	}
	if !strings.Contains(m2.ta.Value(), img.Placeholder()) {
		t.Errorf("chip should be in the input; got %q", m2.ta.Value())
	}
}

// TestBinaryPasteStart_PlainProse: prose and code snippets that merely
// mention format names must return -1 (no confirmation possible).
func TestBinaryPasteStart_PlainProse(t *testing.T) {
	texts := []string{
		"this is how it looks like: a normal sentence",
		"the PNG file has an IHDR chunk at the start",
		"convert image.PNG to JPEG using JFIF standard",
		"if strings.Contains(s, \"JFIF\") { return true } // check for JPEG markers in the parser",
	}
	for _, s := range texts {
		if idx := binaryPasteStart(s); idx >= 0 {
			t.Errorf("prose should not be detected as binary (idx=%d): %q", idx, s)
		}
	}
}

// TestBinaryPasteStart_LongProseAfterSignature: a signature word followed by
// a LONG prose tail (>96 runes) must not confirm — prose fails both the
// symbol-density and the space-scarcity discriminators.
func TestBinaryPasteStart_LongProseAfterSignature(t *testing.T) {
	s := "JFIF is the JPEG File Interchange Format, a standard that defines " +
		"supplementary specifications for the container format holding compressed " +
		"image data, widely used across the web and in digital cameras since the " +
		"early nineties when it was first published as a de facto standard."
	if idx := binaryPasteStart(s); idx >= 0 {
		t.Errorf("long prose after a signature word must not confirm (idx=%d)", idx)
	}
}

// pasteAsTerminalEvents converts mangled paste runes into the event stream a
// terminal without bracketed paste actually produces: printable runs become
// KeyRunes bursts, spaces become separate KeySpace events (bubbletea emits
// space as KeySpace, never inside a KeyRunes run — see key.go detectOneMsg).
func pasteAsTerminalEvents(runes []rune) []tea.KeyMsg {
	var events []tea.KeyMsg
	var run []rune
	flush := func() {
		if len(run) > 0 {
			events = append(events, tea.KeyMsg{Type: tea.KeyRunes, Runes: run})
			run = nil
		}
	}
	for _, r := range runes {
		if r == ' ' {
			flush()
			events = append(events, tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}})
			continue
		}
		run = append(run, r)
	}
	flush()
	return events
}

// TestPasteEndingInSpaceEventCollapses is the "still have to press Enter"
// regression: a mangled paste whose confirmation threshold is crossed only
// at (or after) a KeySpace event. The old scan gated on KeyRunes and never
// re-checked after KeySpace — the garbage sat in the input until Enter.
// The scan must now run after every event type.
func TestPasteEndingInSpaceEventCollapses(t *testing.T) {
	m := keyModel(t)
	m.ta.SetValue("this is how it looks like: ")
	m.ta.CursorEnd()

	// Real mangled PNG bytes, delivered as the terminal would: KeyRunes
	// bursts split at spaces, spaces as KeySpace events.
	mangled := stripControls(mangleAsBubbletea(makeBigMangledPNG()))
	for _, ev := range pasteAsTerminalEvents(mangled) {
		m = step(m, ev)
	}

	got := m.ta.Value()
	if !strings.HasPrefix(got, "this is how it looks like:") {
		t.Errorf("typed prefix must be preserved; got %q", got)
	}
	if idx := binaryPasteStart(got); idx >= 0 {
		t.Errorf("garbage must be collapsed without Enter; input still holds %q", got)
	}
	if m.pasteCutStash == "" {
		t.Error("cut stash should hold the removed garbage")
	}
}

// makeBigMangledPNG builds a PNG-like byte stream with a realistic amount of
// compressed-looking (pseudo-random) data, so the mangled remnant crosses the
// 96-rune confirmation threshold the way a real screenshot does. Includes
// spaces (0x20) in the body — the byte that becomes KeySpace events.
func makeBigMangledPNG() []byte {
	data := makeTestPNG()
	// Splice pseudo-random IDAT-like payload before IEND (deterministic,
	// seeded LCG — no rand import needed).
	payload := make([]byte, 2048)
	state := uint32(0x12345678)
	for i := range payload {
		state = state*1664525 + 1013904223
		payload[i] = byte(state >> 24)
	}
	out := append([]byte{}, data[:len(data)-12]...) // keep through IDAT, drop IEND
	out = append(out, payload...)
	out = append(out, data[len(data)-12:]...) // re-append IEND
	return out
}

// TestClipboardImageMsg_Error verifies that a clipboard read failure shows an
// error note and does not attach any image.
func TestClipboardImageMsg_Error(t *testing.T) {
	m := keyModel(t)
	m2 := step(m, clipboardImageMsg{Err: "no clipboard backend available"})
	if len(m2.app.PendingImages) != 0 {
		t.Fatalf("expected 0 pending images on error, got %d", len(m2.app.PendingImages))
	}
}

// TestClipboardImageCmdSentinel verifies that the agent package's
// /image clipboard command produces the sentinel that the TUI adapter
// recognizes.
func TestClipboardImageCmdSentinel(t *testing.T) {
	// The adapter intercepts agent.ClipboardImageRequest and replaces it.
	// Verify AdaptCmd returns a non-nil tea.Cmd when given a Cmd that
	// returns the sentinel.
	c := func() agent.Msg { return agent.ClipboardImageRequest }
	adapted := AdaptCmd(c)
	if adapted == nil {
		t.Error("AdaptCmd should return a non-nil tea.Cmd for the clipboard sentinel")
	}
}

// makeTestPNG returns a minimal valid 1×1 red PNG file.
func makeTestPNG() []byte {
	return []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, // PNG signature
		0x00, 0x00, 0x00, 0x0D, // IHDR length
		0x49, 0x48, 0x44, 0x52, // "IHDR"
		0x00, 0x00, 0x00, 0x01, // width=1
		0x00, 0x00, 0x00, 0x01, // height=1
		0x08, 0x02, // bit depth=8, color type=RGB
		0x00, 0x00, 0x00, // compression, filter, interlace
		0x90, 0x77, 0x53, 0xDE, // CRC
		0x00, 0x00, 0x00, 0x0C, // IDAT length
		0x49, 0x44, 0x41, 0x54, // "IDAT"
		0x08, 0xD7, 0x63, 0xF8, 0xCF, 0xC0, 0x00, 0x00, // compressed data
		0x01, 0x01, 0x01, 0x00, // CRC tail
		0x18, 0xDD, 0x03, 0x4F, // CRC
		0x00, 0x00, 0x00, 0x00, // IEND length
		0x49, 0x45, 0x4E, 0x44, // "IEND"
		0xAE, 0x42, 0x60, 0x82, // CRC
	}
}
