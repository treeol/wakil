package lsp

import (
	"encoding/json"
	"testing"
)

// TestDecodeDefinition_BareLocation verifies the unexpected arm: a single
// Location object (not wrapped in an array). Decoding as Location[] would
// produce empty → "no definition found" — a silent false-negative.
func TestDecodeDefinition_BareLocation(t *testing.T) {
	raw := json.RawMessage(`{"uri":"file:///work/foo.go","range":{"start":{"line":10,"character":5},"end":{"line":10,"character":15}}}`)
	locs, err := DecodeDefinition(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("expected 1 location from bare Location, got %d — this is the silent-empty bug", len(locs))
	}
	if locs[0].URI != "file:///work/foo.go" {
		t.Errorf("URI = %q", locs[0].URI)
	}
}

// TestDecodeDefinition_LocationArray verifies the expected arm.
func TestDecodeDefinition_LocationArray(t *testing.T) {
	raw := json.RawMessage(`[{"uri":"file:///work/a.go","range":{"start":{"line":1,"character":0},"end":{"line":1,"character":5}}}]`)
	locs, err := DecodeDefinition(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("expected 1 location, got %d", len(locs))
	}
}

// TestDecodeDefinition_LocationLinkArray verifies the link arm (we don't
// advertise linkSupport, but decode defensively).
func TestDecodeDefinition_LocationLinkArray(t *testing.T) {
	raw := json.RawMessage(`[{"targetUri":"file:///work/b.go","targetRange":{"start":{"line":2,"character":0},"end":{"line":2,"character":10}},"targetSelectionRange":{"start":{"line":2,"character":0},"end":{"line":2,"character":5}}}]`)
	locs, err := DecodeDefinition(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("expected 1 location from LocationLink, got %d", len(locs))
	}
	if locs[0].URI != "file:///work/b.go" {
		t.Errorf("URI = %q, want file:///work/b.go", locs[0].URI)
	}
}

// TestDecodeDefinition_Null verifies null returns nil (genuine empty).
func TestDecodeDefinition_Null(t *testing.T) {
	locs, err := DecodeDefinition(json.RawMessage("null"))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if locs != nil {
		t.Errorf("expected nil for null, got %v", locs)
	}
}

// TestDecodeDocumentSymbol_FlatSymbolInformation verifies the unexpected arm:
// flat SymbolInformation[] (not hierarchical DocumentSymbol[]). We advertise
// hierarchicalDocumentSymbolSupport so gopls should return hierarchical, but
// decode the flat arm too.
func TestDecodeDocumentSymbol_FlatSymbolInformation(t *testing.T) {
	raw := json.RawMessage(`[{"name":"Foo","kind":12,"location":{"uri":"file:///work/foo.go","range":{"start":{"line":5,"character":0},"end":{"line":5,"character":10}}}}]`)
	docSyms, symInfos, err := DecodeDocumentSymbol(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(docSyms) != 0 && len(symInfos) != 0 {
		t.Fatal("expected exactly one arm to decode")
	}
	if len(symInfos) != 1 {
		t.Fatalf("expected 1 SymbolInformation from flat arm, got %d — this is the silent-empty bug", len(symInfos))
	}
	if symInfos[0].Name != "Foo" {
		t.Errorf("Name = %q", symInfos[0].Name)
	}
}

// TestDecodeDocumentSymbol_Hierarchical verifies the expected arm.
func TestDecodeDocumentSymbol_Hierarchical(t *testing.T) {
	raw := json.RawMessage(`[{"name":"Foo","kind":12,"range":{"start":{"line":0,"character":0},"end":{"line":10,"character":0}},"selectionRange":{"start":{"line":0,"character":6},"end":{"line":0,"character":9}}}]`)
	docSyms, _, err := DecodeDocumentSymbol(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(docSyms) != 1 {
		t.Fatalf("expected 1 DocumentSymbol, got %d", len(docSyms))
	}
}

// TestRenderReferences_TruncationHonestCount verifies that an over-cap result
// renders an honest "showing N of M" count, not a silent drop to 50.
func TestRenderReferences_TruncationHonestCount(t *testing.T) {
	// 200 references across 10 files (20 each).
	locs := make([]Location, 0, 200)
	for f := 0; f < 10; f++ {
		uri := "file:///work/file" + string(rune('a'+f)) + ".go"
		for i := 0; i < 20; i++ {
			locs = append(locs, Location{
				URI:   uri,
				Range: Range{Start: Position{Line: uint32(i), Character: 0}},
			})
		}
	}

	result := RenderReferences(locs)

	// Must contain the honest truncation count.
	if !containsStr(result, "of 200") {
		t.Errorf("truncation result must contain 'of 200', got:\n%s", result)
	}
	// Must NOT look like 50 is the total.
	if containsStr(result, "[50 references") && !containsStr(result, "showing") {
		t.Errorf("result looks like 50 is the total — silent truncation:\n%s", result)
	}
}

// TestRenderReferences_GenuineEmpty verifies the genuine-empty contract:
// zero references renders a distinct message (not an error, not silent empty).
func TestRenderReferences_GenuineEmpty(t *testing.T) {
	result := RenderReferences(nil)
	if !containsStr(result, "no references found") {
		t.Errorf("genuine empty must say 'no references found', got: %s", result)
	}
	if containsStr(result, "Fell back to text search") {
		t.Errorf("genuine empty must NOT say 'fell back' — it's a definitive answer, not an error: %s", result)
	}
}

// TestRenderDefinition_Empty verifies the genuine-empty contract for definition.
func TestRenderDefinition_Empty(t *testing.T) {
	result := RenderDefinition(nil)
	if !containsStr(result, "no definition found") {
		t.Errorf("genuine empty def must say 'no definition found', got: %s", result)
	}
}

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || indexOfStr(s, substr) >= 0)
}

func indexOfStr(s, sub string) int {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
