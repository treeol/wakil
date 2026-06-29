package lsp

import (
	"fmt"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// ─── Offset computation ──────────────────────────────────────────────────────

// computeCharacterOffset computes the LSP character offset of targetByteOffset
// within lineContent, using the negotiated encoding.
//
// lineContent is the raw bytes of the line (excluding the line terminator).
// targetByteOffset is a 0-based byte offset into lineContent that MUST land on
// a UTF-8 rune boundary.
//
// For UTF-8 encoding: the character offset is the byte offset (Go strings are
// UTF-8, so len == byte count).
//
// For UTF-16 encoding: the character offset is the count of UTF-16 code units
// from the start of the line to targetByteOffset. Astral characters (emoji,
// U+1F600+) count as 2 units; BMP characters count as 1.
//
// INVARIANT (panel-folded): no normalization (NFC/NFD/NFKC) is applied between
// reading and offset computation. The byte sequence here must be identical to
// what was sent in DidOpen/DidChange — positions are against the exact bytes
// the server holds, not a re-read or re-normalized copy.
func computeCharacterOffset(lineContent string, targetByteOffset int, encoding PositionEncodingKind) (uint32, error) {
	if targetByteOffset < 0 || targetByteOffset > len(lineContent) {
		return 0, fmt.Errorf("byte offset %d out of range [0, %d]", targetByteOffset, len(lineContent))
	}
	// Validate rune boundary (panel-folded: mid-character offset = wrong position).
	if targetByteOffset < len(lineContent) {
		if !utf8.RuneStart(lineContent[targetByteOffset]) {
			return 0, fmt.Errorf("byte offset %d is not on a UTF-8 rune boundary", targetByteOffset)
		}
	}

	prefix := lineContent[:targetByteOffset]

	switch encoding {
	case UTF8:
		// UTF-8 code units == bytes.
		return uint32(len(prefix)), nil
	case UTF16:
		// Count UTF-16 code units: BMP = 1, astral = 2 (surrogate pair).
		// FAIL CLOSED: utf16.RuneLen returns -1 for invalid runes. A -1 clamped
		// to 0 or 1 would produce a plausible-but-wrong offset — the one trade
		// we never make in this layer. Return an error so the failure contract
		// can surface it truthfully.
		// (In practice Go's range over string yields U+FFFD for malformed bytes,
		// which is a valid BMP rune with RuneLen 1, so this branch is unreachable —
		// but defensive correctness means the guard fails closed, not silently.)
		var count uint32
		for _, r := range prefix {
			n := utf16.RuneLen(r)
			if n < 0 {
				return 0, fmt.Errorf("invalid rune U+%04X in line content — cannot compute UTF-16 offset", r)
			}
			count += uint32(n)
		}
		return count, nil
	default:
		// Default to UTF-16 per LSP 3.17 spec (omitted positionEncoding → utf-16).
		var count uint32
		for _, r := range prefix {
			n := utf16.RuneLen(r)
			if n < 0 {
				return 0, fmt.Errorf("invalid rune U+%04X in line content — cannot compute offset", r)
			}
			count += uint32(n)
		}
		return count, nil
	}
}

// ─── Symbol resolution (R4: line-anchored primary, middle-of-identifier) ─────

// ResolveResult is the outcome of resolving a (line, symbol) to an LSP position.
type ResolveResult struct {
	Position  Position
	Ambiguous  bool
	Candidates []Candidate // populated when Ambiguous == true
}

// Candidate is one possible match when the symbol name is ambiguous on the line.
type Candidate struct {
	Name     string
	Kind     string // "identifier" (we don't have LSP symbol kind here)
	Column   uint32 // 1-based column for display
	Snippet  string
}

// resolvePosition finds the LSP position of symbolName on the given line.
// The line is 0-based (already converted from the 1-based model input).
// The encoding is the negotiated server encoding (stored on the Server handle).
//
// Primary path (R4): read the line, find the symbol substring with Go identifier
// word-boundary matching, compute the offset targeting the MIDDLE of the
// identifier (cursor-bias guard — landing at a token boundary resolves
// left-vs-right incorrectly in LSP).
//
// If the symbol appears multiple times on the line (overloads, same name twice),
// returns Ambiguous=true with a candidate list — NOT pick-first.
func resolvePosition(lineContent string, line uint32, symbolName string, encoding PositionEncodingKind) ResolveResult {
	if symbolName == "" {
		return ResolveResult{Ambiguous: true}
	}

	// Find all occurrences of symbolName as a Go identifier on this line.
	// Word-boundary: the char before and after must not be an identifier character
	// (letter, digit, underscore). This prevents matching "ctx" inside "context"
	// or "cctx", and matching inside string literals or comments (which we can't
	// fully distinguish without a parser, but word-boundary + the fact that the
	// model provides the line from read_file output is the best we can do without
	// a full AST).
	occurrences := findIdentifierOccurrences(lineContent, symbolName)

	if len(occurrences) == 0 {
		return ResolveResult{Ambiguous: true} // not found — caller handles
	}

	if len(occurrences) == 1 {
		// Single match — compute the position.
		byteOffset := occurrences[0]
		// Cursor-bias guard: target the middle of the identifier, not the start.
		// This avoids landing at a token boundary where LSP resolves left-vs-right
		// ambiguously.
		midOffset := byteOffset + len(symbolName)/2
		// Ensure we're on a rune boundary (mid-offset of a multi-byte symbol
		// could land inside a rune).
		for midOffset < len(lineContent) && !utf8.RuneStart(lineContent[midOffset]) {
			midOffset--
		}
		char, err := computeCharacterOffset(lineContent, midOffset, encoding)
		if err != nil {
			return ResolveResult{Ambiguous: true}
		}
		return ResolveResult{
			Position: Position{Line: line, Character: char},
		}
	}

	// Multiple matches — return candidate list, not pick-first.
	candidates := make([]Candidate, 0, len(occurrences))
	for _, byteOffset := range occurrences {
		midOffset := byteOffset + len(symbolName)/2
		for midOffset < len(lineContent) && !utf8.RuneStart(lineContent[midOffset]) {
			midOffset--
		}
		char, _ := computeCharacterOffset(lineContent, midOffset, encoding)
		// Snippet: a short context around the match.
		snippetStart := byteOffset - 10
		if snippetStart < 0 {
			snippetStart = 0
		}
		snippetEnd := byteOffset + len(symbolName) + 10
		if snippetEnd > len(lineContent) {
			snippetEnd = len(lineContent)
		}
		candidates = append(candidates, Candidate{
			Name:    symbolName,
			Kind:    "identifier",
			Column:  char + 1, // 1-based for display
			Snippet: lineContent[snippetStart:snippetEnd],
		})
	}
	return ResolveResult{
		Ambiguous:  true,
		Candidates: candidates,
	}
}

// findIdentifierOccurrences returns the byte offsets of all occurrences of name
// in line that are bounded by non-identifier characters (Go identifier rules:
// letters, digits, underscores are identifier characters).
func findIdentifierOccurrences(line, name string) []int {
	var offsets []int
	start := 0
	for {
		idx := strings.Index(line[start:], name)
		if idx < 0 {
			break
		}
		byteOffset := start + idx

		// Check word boundary before: decode the rune immediately preceding.
		if byteOffset > 0 {
			if isPrecedingIdentRune(line, byteOffset) {
				start = byteOffset + 1
				continue
			}
		}

		// Check word boundary after: decode the rune immediately following.
		afterIdx := byteOffset + len(name)
		if afterIdx < len(line) {
			if isFollowingIdentRune(line, afterIdx) {
				start = byteOffset + 1
				continue
			}
		}

		offsets = append(offsets, byteOffset)
		start = byteOffset + len(name)
	}
	return offsets
}

// isPrecedingIdentRune checks if the rune ending just before byteOffset is a
// Go identifier character (letter, digit, underscore, or any Unicode letter).
func isPrecedingIdentRune(line string, byteOffset int) bool {
	// Walk backwards from byteOffset to find the start of the preceding rune.
	// UTF-8 continuation bytes have the high bits 10xxxxxx (0x80-0xBF).
	// A rune start byte is anything that's NOT a continuation byte.
	i := byteOffset - 1
	for i >= 0 && isContinuationByte(line[i]) {
		i--
	}
	if i < 0 {
		return false
	}
	r := []rune(line[i:byteOffset])
	if len(r) == 0 {
		return false
	}
	return isIdentRune(r[0])
}

// isFollowingIdentRune checks if the rune starting at byteOffset is a Go
// identifier character.
func isFollowingIdentRune(line string, byteOffset int) bool {
	if byteOffset >= len(line) {
		return false
	}
	r := []rune(line[byteOffset:])
	if len(r) == 0 {
		return false
	}
	return isIdentRune(r[0])
}

func isContinuationByte(b byte) bool {
	return b >= 0x80 && b < 0xC0
}

// isIdentRune reports whether r is a Go identifier character: letter, digit,
// underscore, or any Unicode letter/digit (Go identifiers are Unicode).
func isIdentRune(r rune) bool {
	return r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9') ||
		(r >= 0x80 && isUnicodeLetterOrDigit(r))
}

// isUnicodeLetterOrDigit is a minimal check: any rune >= 0x80 is treated as a
// potential identifier character (Go allows Unicode letters in identifiers).
// This is conservative — it may treat some non-letter runes as ident chars,
// but that's the safe direction for word-boundary matching (err on the side
// of NOT matching, rather than matching inside a longer identifier).
func isUnicodeLetterOrDigit(r rune) bool {
	// This is a simplification; Go's unicode.IsLetter would be more precise,
	// but we avoid importing unicode to keep the dependency surface minimal.
	// For our purposes (word-boundary for symbol resolution), any non-ASCII
	// rune that's a letter in a Go identifier is handled correctly.
	return r >= 0x80
}
