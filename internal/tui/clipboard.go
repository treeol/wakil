package tui

import (
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/treeol/wakil/internal/proxy"
)

// readClipboardImageBytes runs a clipboard backend command on the host and
// returns its stdout as raw bytes. The command must output clipboard image
// data to stdout when an image is available, and exit non-zero (or produce
// non-image output) when the clipboard holds no image.
//
// Backends are tried in order: wl-paste (Wayland), xclip (X11), pbpaste (macOS).
func readClipboardImageBytes() ([]byte, error) {
	backends := []clipboardBackend{
		{"wl-paste", []string{"-t", "image/png"}},
		{"xclip", []string{"-selection", "clipboard", "-t", "image/png", "-o"}},
		{"pbpaste", []string{"-Prefer", "png"}},
	}
	for _, b := range backends {
		data, err := b.run()
		if err == nil && len(data) > 0 {
			return data, nil
		}
	}
	return nil, errNoClipboardBackend
}

// clipboardBackend is one clipboard provider command + its arguments.
type clipboardBackend struct {
	cmd  string
	args []string
}

func (b clipboardBackend) run() ([]byte, error) {
	c := exec.Command(b.cmd, b.args...)
	return c.Output()
}

// errNoClipboardBackend is returned when none of the clipboard backends
// (wl-paste, xclip, pbpaste) are available or produced output.
var errNoClipboardBackend = &clipboardError{"no clipboard backend available (install wl-paste, xclip, or run on macOS)"}

type clipboardError struct{ msg string }

func (e *clipboardError) Error() string { return e.msg }

// readClipboardCmd constructs a tea.Cmd that reads the host clipboard and
// returns a clipboardImageMsg. Runs off the event loop so the exec doesn't
// block keystroke processing.
func readClipboardCmd() tea.Cmd {
	return func() tea.Msg {
		data, err := readClipboardImageBytes()
		if err != nil {
			return clipboardImageMsg{Err: err.Error()}
		}
		// Validate the clipboard data is actually an image before loading.
		mime, ok := proxy.DetectMIME(data)
		if !ok {
			return clipboardImageMsg{Err: "clipboard does not contain a recognizable image (png, jpeg, gif, webp)"}
		}
		img, loadErr := proxy.LoadImageFromBytes(data, "clipboard:"+strings.Split(mime, "/")[1])
		if loadErr != nil {
			return clipboardImageMsg{Err: loadErr.Error()}
		}
		return clipboardImageMsg{Img: img}
	}
}

// looksLikeBinaryPaste reports whether pasted runes look like the mangled
// remains of a binary image paste. This is used to detect binary pastes
// (e.g. a screenshot pasted into the terminal) so they can be routed through
// the image pipeline instead of corrupting the text input.
//
// Detection cannot rely on intact magic bytes: bubbletea's bracketed-paste
// decoder drops invalid-UTF-8 bytes (PNG's leading 0x89, JPEG's 0xFF 0xD8 —
// see detectBracketedPaste in bubbletea/key_sequences.go, which discards
// utf8.RuneError), and most terminals strip NUL/control bytes from pastes as
// paste-injection protection. What reliably survives is the printable-ASCII
// scaffolding of the format: "PNG" + "IHDR" for PNG, "JFIF"/"Exif" for JPEG,
// "GIF87a"/"GIF89a" for GIF, "RIFF"+"WEBP" for WebP.
//
// Checks, in order:
//  1. NUL anywhere → binary (kept for terminals that do pass NULs through).
//  2. Intact magic bytes at the start (terminals that pass everything).
//  3. Mangled signature remnants near the start:
//     - "PNG" within the first 2 runes AND "IHDR" within the first 64
//     (a PNG's IHDR chunk always sits within ~30 bytes of the header)
//     - "JFIF" or "Exif" within the first 16 runes
//     - "GIF87a" or "GIF89a" at position 0
//     - "RIFF" at position 0 AND "WEBP" within the first 16 runes
//
// A normal text paste never matches: it has no NULs, no image magic, and
// does not open with these format markers in these exact positions.
func looksLikeBinaryPaste(runes []rune) bool {
	if len(runes) == 0 {
		return false
	}

	// 1. NUL — the universal binary marker, when the terminal passes it.
	for _, r := range runes {
		if r == 0 {
			return true
		}
	}

	// 2. Intact magic bytes (nothing was stripped). Collect a Latin-1 byte
	// prefix for sniffing; stop at the first rune that can't be a raw byte.
	bs := make([]byte, 0, 16)
	for _, r := range runes {
		if r > 0xFF {
			break
		}
		bs = append(bs, byte(r))
		if len(bs) >= 16 {
			break
		}
	}
	if _, ok := proxy.DetectMIME(bs); ok {
		return true
	}

	// 3. Mangled signature remnants. Work on a bounded prefix — signatures
	// live at the start of the file, so scanning further only adds noise.
	head := runes
	if len(head) > 64 {
		head = head[:64]
	}
	s := string(head)

	// PNG: 0x89 dropped → "PNG" lands at position 0 or 1; IHDR follows
	// within the first chunk (~30 bytes in, well inside 64 runes).
	if idx := strings.Index(s, "PNG"); idx >= 0 && idx <= 1 && strings.Contains(s, "IHDR") {
		return true
	}
	// JPEG: 0xFF 0xD8 0xFF 0xE0 dropped → "JFIF"/"Exif" appears near the start.
	first16 := s
	if len(first16) > 16 {
		first16 = first16[:16]
	}
	if strings.Contains(first16, "JFIF") || strings.Contains(first16, "Exif") {
		return true
	}
	// GIF: magic is fully printable ASCII and survives intact at position 0.
	if strings.HasPrefix(s, "GIF87a") || strings.HasPrefix(s, "GIF89a") {
		return true
	}
	// WebP: "RIFF" survives at position 0; the 4 binary size bytes between
	// RIFF and WEBP may be dropped/kept, so allow WEBP anywhere in the
	// first 16 runes.
	if strings.HasPrefix(s, "RIFF") && strings.Contains(first16, "WEBP") {
		return true
	}
	return false
}

// containsBinary is the legacy name for looksLikeBinaryPaste, kept so the
// call site in tui.go and existing tests read naturally.
func containsBinary(runes []rune) bool {
	return looksLikeBinaryPaste(runes)
}

// binaryPasteStart scans s for the start of mangled binary-image content and
// returns its rune index, or -1 when none is found. Unlike looksLikeBinaryPaste
// (which is position-anchored for whole-paste checks), this searches anywhere
// in the string — it runs against the accumulated TEXTAREA content, where the
// user may have typed real text before pasting ("describe this: <garbage>").
// The prefix before the returned index is the user's text and is preserved.
//
// Because this scans content that may include hand-typed prose, a signature
// hit alone is not enough — it must be CONFIRMED by one of:
//   - a NUL rune anywhere (never valid text),
//   - PNG chunk train: "IHDR" plus a second chunk marker (IDAT/IEND/tEXt/…)
//     after the signature — prose mentions IHDR, real remnants carry the
//     chunk sequence,
//   - a garbage tail: ≥ binaryTailMinRunes runes after the signature with a
//     symbol ratio ≥ binaryTailSymbolRatio — mangled compressed image data
//     is symbol-dense; prose and even code are not, at that length.
//
// False positives are still possible (pasting a hexdump analysis that lists
// PNG chunk names). The caller keeps the cut text in a stash and restores it
// if the clipboard read yields no image — a false positive costs a visible
// note, never data.
func binaryPasteStart(s string) int {
	runes := []rune(s)

	// NUL anywhere confirms binary unconditionally; the cut starts at the
	// earlier of the NUL and any signature.
	nulIdx := -1
	for i, r := range runes {
		if r == 0 {
			nulIdx = i
			break
		}
	}

	sig := signatureStart(s)
	if sig < 0 {
		return nulIdx // possibly -1
	}
	if nulIdx >= 0 {
		if nulIdx < sig {
			return nulIdx
		}
		return sig
	}

	tail := string(runes[sig:])
	// PNG chunk train: IHDR plus at least one more chunk type marker.
	if strings.Contains(tail, "IHDR") {
		for _, chunk := range []string{"IDAT", "IEND", "tEXt", "PLTE", "pHYs", "sRGB", "gAMA", "bKGD", "tIME"} {
			if strings.Contains(tail, chunk) {
				return sig
			}
		}
	}
	// Garbage tail: long enough to sample AND statistically non-prose.
	// Two independent discriminators, either confirms:
	//   - symbol density: mangled compressed data is ~12-13% non-prose
	//     symbols (uniform printable ASCII); prose is ~2-5%. Threshold
	//     sits between them.
	//   - space scarcity: prose/code has a space every ~5-8 runes (≥10%);
	//     binary garbage has space at the uniform rate (~1%). This is the
	//     stronger signal and catches garbage whose symbols happen to
	//     cluster inside the prose-punctuation allowlist.
	tailRunes := runes[sig:]
	if len(tailRunes) >= binaryTailMinRunes {
		if symbolRatio(tailRunes) >= binaryTailSymbolRatio || spaceRatio(tailRunes) <= binaryTailMaxSpaceRatio {
			return sig
		}
	}
	return -1
}

// binaryTailMinRunes is the minimum content length after a signature hit for
// the garbage-tail confirmation to apply. Real image pastes are KBs even
// after mangling; a sentence mentioning "JFIF" is not.
const binaryTailMinRunes = 96

// binaryTailSymbolRatio is the minimum fraction of non-prose runes in the
// tail sample. Mangled compressed data lands ~12-13% symbols among surviving
// printable ASCII; English prose is ~2-5%. 0.09 sits between prose and
// garbage while staying below the garbage floor.
const binaryTailSymbolRatio = 0.09

// binaryTailMaxSpaceRatio is the maximum fraction of space runes for a tail
// to count as binary garbage. Prose and code carry ≥10% spaces; uniform
// printable garbage carries ~1%.
const binaryTailMaxSpaceRatio = 0.04

// spaceRatio returns the fraction of space runes in a sample of up to 256.
func spaceRatio(runes []rune) float64 {
	sample := runes
	if len(sample) > 256 {
		sample = sample[:256]
	}
	if len(sample) == 0 {
		return 0
	}
	spaces := 0
	for _, r := range sample {
		if r == ' ' {
			spaces++
		}
	}
	return float64(spaces) / float64(len(sample))
}

// symbolRatio returns the fraction of runes (sampled up to 256) that are not
// typical of prose or code: not letters, digits, whitespace, or common
// punctuation.
func symbolRatio(runes []rune) float64 {
	sample := runes
	if len(sample) > 256 {
		sample = sample[:256]
	}
	if len(sample) == 0 {
		return 0
	}
	symbols := 0
	for _, r := range sample {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == ' ', r == '\n', r == '\r', r == '\t':
		case strings.ContainsRune(`.,:;!?'"()-_/=<>{}[]`, r):
		default:
			symbols++
		}
	}
	return float64(symbols) / float64(len(sample))
}

// signatureStart returns the rune index of the earliest image-signature
// remnant in s, or -1. Position-independent companion to the anchored checks
// in looksLikeBinaryPaste.
func signatureStart(s string) int {
	best := -1
	upd := func(byteIdx int) {
		if byteIdx < 0 {
			return
		}
		ri := len([]rune(s[:byteIdx]))
		if best == -1 || ri < best {
			best = ri
		}
	}
	// PNG: "IHDR" within ~64 runes after "PNG" (byte window is a safe
	// overestimate of the rune window).
	if i := strings.Index(s, "PNG"); i >= 0 {
		window := s[i:]
		if len(window) > 64*4 {
			window = window[:64*4]
		}
		if strings.Contains(window, "IHDR") {
			upd(i)
		}
	}
	upd(strings.Index(s, "JFIF"))
	upd(strings.Index(s, "Exif"))
	upd(strings.Index(s, "GIF87a"))
	upd(strings.Index(s, "GIF89a"))
	// WebP: "WEBP" within ~16 runes after "RIFF".
	if i := strings.Index(s, "RIFF"); i >= 0 {
		window := s[i:]
		if len(window) > 16*4 {
			window = window[:16*4]
		}
		if strings.Contains(window, "WEBP") {
			upd(i)
		}
	}
	return best
}

// reconcileImageChips reconciles the image chips inserted into the text input
// with the pending images at send time. For each tracked chip:
//   - chip still present in the input → strip it from the outgoing text
//     (the image itself travels via PendingImages, not as text);
//   - chip deleted by the user → detach the matching pending image
//     (deleting the chip is how you un-attach an image before sending).
//
// Only chip-tracked images are ever detached — images queued via
// /image <path> or --attach-image have no chip and pass through untouched.
// Returns the cleaned input text and the surviving pending images.
func reconcileImageChips(input string, chips []string, pending []proxy.ImagePart) (string, []proxy.ImagePart) {
	for _, chip := range chips {
		if idx := strings.Index(input, chip); idx >= 0 {
			// Chip present: remove it (and one trailing space, which the
			// insertion added) from the outgoing text.
			end := idx + len(chip)
			if end < len(input) && input[end] == ' ' {
				end++
			}
			input = input[:idx] + input[end:]
			continue
		}
		// Chip deleted: detach the first pending image with this placeholder.
		for i, img := range pending {
			if img.Placeholder() == chip {
				pending = append(pending[:i:i], pending[i+1:]...)
				break
			}
		}
	}
	return strings.TrimSpace(input), pending
}
