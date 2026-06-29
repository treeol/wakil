package lsp

import (
	"testing"
	"unicode/utf16"
)

// TestComputeCharacterOffset_Encodings is the panel-mandated test that catches
// ALL THREE confusion classes: byte-vs-UTF16, rune-vs-UTF16, and byte-vs-rune.
//
// Fixture: "ab🙂جTARGET" — ASCII + astral emoji + Arabic before the target.
// The target symbol starts after this mixed prefix.
//
//	char       UTF-8 bytes  UTF-16 units  runes
//	'a'        1            1             1
//	'b'        1            1             1
//	'🙂' (U+1F600) 4            2             1
//	'ج' (Arabic) 2          1             1
//
// Offset before TARGET:
//	UTF-8: 1+1+4+2 = 8
//	UTF-16: 1+1+2+1 = 5
//	runes: 4
//
// Arabic alone (BMP) would pass a rune-vs-UTF16 bug (both=1). The astral emoji
// is the ONLY input that separates rune count from UTF-16 units. This fixture
// is the minimum bar.
func TestComputeCharacterOffset_Encodings(t *testing.T) {
	line := "ab🙂جTARGET"
	targetByteOffset := 8 // byte offset of 'T' in TARGET

	// UTF-8 encoding: offset == byte offset.
	got, err := computeCharacterOffset(line, targetByteOffset, UTF8)
	if err != nil {
		t.Fatalf("utf-8: %v", err)
	}
	if got != 8 {
		t.Errorf("utf-8 offset = %d, want 8 (byte count)", got)
	}

	// UTF-16 encoding: offset == UTF-16 code-unit count.
	got, err = computeCharacterOffset(line, targetByteOffset, UTF16)
	if err != nil {
		t.Fatalf("utf-16: %v", err)
	}
	if got != 5 {
		t.Errorf("utf-16 offset = %d, want 5 (code-unit count, not runes=4 or bytes=8)", got)
	}
}

// TestComputeCharacterOffset_ArabicOnly proves Arabic alone is insufficient:
// a rune-count implementation would pass this test for UTF-16 (both = 1 per char).
// This test documents the trap; the mixed fixture above catches the bug.
func TestComputeCharacterOffset_ArabicOnly(t *testing.T) {
	line := "سلامTARGET" // 4 Arabic letters, each BMP
	targetByteOffset := 8 // 4 Arabic letters × 2 bytes each = 8

	// UTF-8: 8 bytes.
	got, _ := computeCharacterOffset(line, targetByteOffset, UTF8)
	if got != 8 {
		t.Errorf("utf-8 arabic: got %d, want 8", got)
	}

	// UTF-16: 4 code units. A rune-count implementation would ALSO get 4 here
	// (both = 1 for BMP). This test passes with the wrong implementation —
	// proving Arabic alone is a false-confidence trap.
	got, _ = computeCharacterOffset(line, targetByteOffset, UTF16)
	if got != 4 {
		t.Errorf("utf-16 arabic: got %d, want 4", got)
	}
}

// TestComputeCharacterOffset_RuneBoundaryValidation verifies that a mid-character
// byte offset is rejected (panel-folded: mid-character = plausible-but-wrong).
func TestComputeCharacterOffset_RuneBoundaryValidation(t *testing.T) {
	line := "ab🙂ج"
	// Byte offset 3 is inside the 4-byte emoji 🙂 (bytes 2-5).
	_, err := computeCharacterOffset(line, 3, UTF8)
	if err == nil {
		t.Error("expected error for mid-character byte offset, got nil")
	}
}

// TestResolvePosition_MiddleOfIdentifier exercises the cursor-bias guard (R4):
// the offset targets the middle of the identifier, not the start, to avoid
// landing at a token boundary where LSP resolves left-vs-right ambiguously.
func TestResolvePosition_MiddleOfIdentifier(t *testing.T) {
	line := "result := someFunc()"
	symbol := "someFunc"
	// someFunc starts at byte 10, length 8, middle = 10+4 = 14.
	// At byte 14 (the 'u' in someFunc), the offset is well inside the identifier.
	res := resolvePosition(line, 0, symbol, UTF8)
	if res.Ambiguous {
		t.Fatal("expected unambiguous match")
	}
	// Middle of "someFunc" (10..18) → byte 14 → UTF-8 char 14.
	if res.Position.Character != 14 {
		t.Errorf("character = %d, want 14 (middle of identifier, not 10 at start)", res.Position.Character)
	}
}

// TestResolvePosition_WordBoundary verifies that "ctx" does not match inside
// "context" or "cctx".
func TestResolvePosition_WordBoundary(t *testing.T) {
	line := "context := ctx"
	symbol := "ctx"
	res := resolvePosition(line, 0, symbol, UTF8)
	if res.Ambiguous {
		t.Fatal("expected single match for 'ctx' (not 'context')")
	}
	// "ctx" at the end starts at byte 11.
	if res.Position.Character < 10 {
		t.Errorf("matched 'ctx' inside 'context' — word boundary failed: character = %d", res.Position.Character)
	}
}

// TestResolvePosition_MultiMatchReturnsCandidates verifies that multiple
// occurrences of the symbol on the same line return a candidate list, not
// pick-first.
func TestResolvePosition_MultiMatchReturnsCandidates(t *testing.T) {
	line := "foo(foo)"
	symbol := "foo"
	res := resolvePosition(line, 0, symbol, UTF8)
	if !res.Ambiguous {
		t.Fatal("expected ambiguous result for multiple 'foo' on line")
	}
	if len(res.Candidates) < 2 {
		t.Errorf("expected at least 2 candidates, got %d", len(res.Candidates))
	}
}

// TestResolvePosition_NotFound verifies that a missing symbol returns ambiguous
// (caller handles the "not found" failure contract).
func TestResolvePosition_NotFound(t *testing.T) {
	line := "someFunc()"
	symbol := "missingFunc"
	res := resolvePosition(line, 0, symbol, UTF8)
	if !res.Ambiguous {
		t.Fatal("expected ambiguous (not found) for missing symbol")
	}
	if len(res.Candidates) != 0 {
		t.Errorf("expected 0 candidates for not-found, got %d", len(res.Candidates))
	}
}

// TestResolvePosition_MultiLine verifies position is per-line, not per-file
// (whole-file-offset-as-character is a classic silent bug).
func TestResolvePosition_MultiLine(t *testing.T) {
	content := "line one\nsomeFunc()\nline three"
	lines := splitLines(content)
	if len(lines) < 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}
	// Symbol is on line 1 (0-based), "someFunc" starts at byte 0 of that line.
	res := resolvePosition(lines[1], 1, "someFunc", UTF8)
	if res.Ambiguous {
		t.Fatal("expected match")
	}
	if res.Position.Line != 1 {
		t.Errorf("line = %d, want 1", res.Position.Line)
	}
	if res.Position.Character != 4 { // middle of "someFunc" (8 chars, middle=4)
		t.Errorf("character = %d, want 4", res.Position.Character)
	}
}

// TestResolvePosition_MixedNonASCII verifies resolution on a line with mixed
// non-ASCII content (Arabic + emoji), using the negotiated encoding.
func TestResolvePosition_MixedNonASCII(t *testing.T) {
	line := "ab 🙂 ج TARGET"
	res := resolvePosition(line, 0, "TARGET", UTF16)
	if res.Ambiguous {
		t.Fatal("expected match")
	}
	// TARGET starts at byte offset of "TARGET" in "ab 🙂 ج TARGET".
	// ab(2) + space(1) + 🙂(4) + space(1) + ج(2) + space(1) = 11.
	// Middle of "TARGET" (6 chars): 11 + 3 = 14.
	// UTF-16 offset at byte 14: a(1) b(1) space(1) 🙂(2) space(1) ج(1) space(1) T(1) A(1) R(1) = 11.
	// Actually let's compute: prefix = line[:14]
	// "ab 🙂 ج T" = a,b,space,🙂,space,ج,space,T = 1+1+1+2+1+1+1+1 = 9 UTF-16 units.
	// Wait, middle of TARGET (6 chars) = 11 + 3 = 14 bytes.
	// line[:14] = "ab 🙂 ج T" → runes: a b ' ' 🙂 ' ' ج ' ' T
	// UTF-16: 1+1+1+2+1+1+1+1 = 9.
	// Hmm, let me just compute it properly:
	prefix := line[:14]
	var want uint32 = 0
	for _, r := range prefix {
		want += uint32(utf16.RuneLen(r))
	}
	if res.Position.Character != want {
		t.Errorf("utf-16 character = %d, want %d", res.Position.Character, want)
	}
}

// splitLines is defined in filesync.go (splitOnNewlines wrapper).
// This test uses it directly.
