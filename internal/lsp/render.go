package lsp

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ─── Result rendering (Decision 7) ────────────────────────────────────────────
//
// lsp_references: cluster by file, cap 5/file + 50 total. When truncation
// happens, render an HONEST "showing N of M" count — silent truncation is a
// soft false-negative (the agent reasons on a partial caller set thinking
// it's complete).
//
// lsp_hover: strip markdown to plain text (reuse stripHTML from searxng.go
// pattern), truncate to ToolResultCap (8k).
//
// Tool-line summary: consistent with resultSummary format in app.go.

const (
	maxRefsPerFile = 5
	maxRefsTotal   = 50
	maxHoverChars  = 8000
)

// RenderReferences clusters references by file, caps at 5/file + 50 total,
// and renders with an honest truncation count when the cap bites.
func RenderReferences(locs []Location) string {
	if len(locs) == 0 {
		return "[lsp: no references found — the symbol exists but has no callers.]"
	}

	// Cluster by file (URI).
	byFile := make(map[string][]Location)
	for _, loc := range locs {
		byFile[loc.URI] = append(byFile[loc.URI], loc)
	}

	// Sort files for stable output.
	files := make([]string, 0, len(byFile))
	for f := range byFile {
		files = append(files, f)
	}
	sort.Strings(files)

	totalShown := 0
	truncated := false
	var b strings.Builder

	for _, file := range files {
		refs := byFile[file]
		hostPath := uriToHostPathDisplay(file)
		fmt.Fprintf(&b, "%s:\n", hostPath)

		shown := 0
		for _, ref := range refs {
			if totalShown >= maxRefsTotal {
				truncated = true
				break
			}
			if shown >= maxRefsPerFile {
				truncated = true
				break
			}
			line := ref.Range.Start.Line + 1
			col := ref.Range.Start.Character + 1
			fmt.Fprintf(&b, "  %d:%d\n", line, col)
			shown++
			totalShown++
		}
		if shown < len(refs) {
			fmt.Fprintf(&b, "  … %d more in this file (capped at %d/file)\n", len(refs)-shown, maxRefsPerFile)
		}
		b.WriteByte('\n')
		if totalShown >= maxRefsTotal {
			break
		}
	}

	// Honest truncation count.
	if truncated {
		fmt.Fprintf(&b, "[showing %d of %d references — cap: %d/file, %d total]\n",
			totalShown, len(locs), maxRefsPerFile, maxRefsTotal)
	} else {
		fmt.Fprintf(&b, "[%d references in %d files]\n", len(locs), len(files))
	}

	return strings.TrimRight(b.String(), "\n")
}

// RenderDefinition renders definition locations (cap 10, show first 5 + count).
func RenderDefinition(locs []Location) string {
	if len(locs) == 0 {
		return "[lsp: no definition found — the symbol may be built-in or external.]"
	}

	var b strings.Builder
	limit := 5
	if len(locs) <= 10 {
		limit = len(locs)
	}

	for i, loc := range locs {
		if i >= limit {
			break
		}
		hostPath := uriToHostPathDisplay(loc.URI)
		line := loc.Range.Start.Line + 1
		col := loc.Range.Start.Character + 1
		fmt.Fprintf(&b, "%s:%d:%d\n", hostPath, line, col)
	}

	if len(locs) > 10 {
		fmt.Fprintf(&b, "[showing %d of %d locations]\n", limit, len(locs))
	}
	return strings.TrimRight(b.String(), "\n")
}

// RenderHover strips markdown to plain text and truncates to ToolResultCap.
func RenderHover(hover *Hover) string {
	if hover == nil {
		return "[lsp: no hover information available for this symbol.]"
	}
	content := hover.Contents.Value
	if hover.Contents.Kind == "markdown" {
		content = stripMarkdown(content)
	}
	if len(content) > maxHoverChars {
		content = content[:maxHoverChars] + "\n… [truncated at " + fmt.Sprintf("%d", maxHoverChars) + " chars]"
	}
	return content
}

// RenderSymbols renders workspace/document symbols (cap 50, tree with kind + name + range).
func RenderSymbols(symbols []SymbolInfo) string {
	if len(symbols) == 0 {
		return "[lsp: no symbols found matching the query.]"
	}

	var b strings.Builder
	limit := 50
	truncated := false
	if len(symbols) < limit {
		limit = len(symbols)
	} else {
		truncated = true
	}

	for i, s := range symbols {
		if i >= limit {
			break
		}
		hostPath := uriToHostPathDisplay(s.Location.URI)
		line := s.Location.Range.Start.Line + 1
		fmt.Fprintf(&b, "%s: %s (%d) at line %d\n", hostPath, s.Name, int(s.Kind), line)
	}

	if truncated {
		fmt.Fprintf(&b, "[showing %d of %d symbols]\n", limit, len(symbols))
	}
	return strings.TrimRight(b.String(), "\n")
}

// ─── Decode helpers (item 1: defensive decode shapes) ────────────────────────
//
// definition: Location | Location[] | LocationLink[] — decode all arms.
// documentSymbol: DocumentSymbol[] | SymbolInformation[] — decode both.
// The unexpected arm decoded as the expected arm → empty result reading as
// "no definition/no symbols" — a silent false-negative. We decode defensively.

// DecodeDefinition decodes the raw result of textDocument/definition into
// a slice of Locations, handling all three spec shapes:
//   - Location (single object)
//   - Location[] (array)
//   - LocationLink[] (array of links — we don't advertise linkSupport, but
//     decode defensively)
//   - null (empty)
func DecodeDefinition(raw json.RawMessage) ([]Location, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}

	// Try array first — but verify it's actually Location[] (not LocationLink[],
	// which would partially decode as Location with empty URI).
	var locs []Location
	if err := json.Unmarshal(raw, &locs); err == nil && len(locs) > 0 && locs[0].URI != "" {
		return locs, nil
	}

	// Try LocationLink[] — extract TargetURI + TargetSelectionRange.
	var links []LocationLink
	if err := json.Unmarshal(raw, &links); err == nil && len(links) > 0 {
		out := make([]Location, 0, len(links))
		for _, l := range links {
			out = append(out, Location{URI: l.TargetURI, Range: l.TargetSelectionRange})
		}
		return out, nil
	}

	// Try bare single Location.
	var loc Location
	if err := json.Unmarshal(raw, &loc); err == nil && loc.URI != "" {
		return []Location{loc}, nil
	}

	// Empty array or unparseable — return nil (genuine empty).
	return nil, nil
}

// DecodeDocumentSymbol decodes the raw result of textDocument/documentSymbol,
// handling both hierarchical (DocumentSymbol[]) and flat (SymbolInformation[])
// shapes. We advertise hierarchicalDocumentSymbolSupport so gopls should return
// DocumentSymbol[], but decode the flat arm too.
func DecodeDocumentSymbol(raw json.RawMessage) ([]DocumentSymbol, []SymbolInformation, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil, nil
	}

	// Try hierarchical DocumentSymbol[]. A flat SymbolInformation[] would
	// partially decode as DocumentSymbol (both have Name+Kind), but
	// DocumentSymbol has required "range"+"selectionRange" while
	// SymbolInformation has "location". Check that Range is non-zero (a real
	// DocumentSymbol always has a Range).
	var docSyms []DocumentSymbol
	if err := json.Unmarshal(raw, &docSyms); err == nil && len(docSyms) > 0 {
		// Verify it's actually hierarchical (has selectionRange, which flat doesn't).
		if docSyms[0].SelectionRange.Start.Line != 0 || docSyms[0].SelectionRange.Start.Character != 0 ||
			docSyms[0].SelectionRange.End.Line != 0 || docSyms[0].SelectionRange.End.Character != 0 {
			return docSyms, nil, nil
		}
		// If SelectionRange is zero, it might be a genuine DocumentSymbol with
		// a symbol at 0:0. Check if the raw JSON has "selectionRange" key.
		var check []map[string]json.RawMessage
		if json.Unmarshal(raw, &check) == nil && len(check) > 0 {
			if _, ok := check[0]["selectionRange"]; ok {
				return docSyms, nil, nil
			}
		}
	}

	// Try flat SymbolInformation[].
	var symInfos []SymbolInformation
	if err := json.Unmarshal(raw, &symInfos); err == nil && len(symInfos) > 0 {
		return nil, symInfos, nil
	}

	return nil, nil, nil
}

// LocationLink is the LocationLink type (we don't advertise linkSupport, but
// decode defensively per item 1).
type LocationLink struct {
	OriginSelectionRange  *Range `json:"originSelectionRange,omitempty"`
	TargetURI             string `json:"targetUri"`
	TargetRange           Range  `json:"targetRange"`
	TargetSelectionRange  Range  `json:"targetSelectionRange"`
}

// SymbolInfo is a unified symbol representation for rendering.
type SymbolInfo struct {
	Name     string
	Kind     SymbolKind
	Location Location
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

// uriToHostPathDisplay extracts a readable path from a file:// URI for display.
func uriToHostPathDisplay(uri string) string {
	if !strings.HasPrefix(uri, "file://") {
		return uri
	}
	path := uri[len("file://"):]
	path = strings.ReplaceAll(path, "%20", " ")
	return path
}

// stripMarkdown converts markdown to plain text (reuses the stripHTML pattern
// from searxng.go — same logic, different package).
func stripMarkdown(s string) string {
	// Strip code fences.
	var out strings.Builder
	inFence := false
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			out.WriteString(line + "\n")
			continue
		}
		// Strip inline formatting: **bold**, *italic*, `code`, [text](url)
		line = stripInlineMarkdown(line)
		out.WriteString(line + "\n")
	}
	return strings.TrimRight(out.String(), "\n")
}

func stripInlineMarkdown(s string) string {
	// Simple inline markdown stripping.
	s = stripPair(s, "**")
	s = stripPair(s, "`")
	s = stripPair(s, "*")
	// Strip links: [text](url) → text
	for {
		idx := strings.Index(s, "](")
		if idx < 0 {
			break
		}
		start := strings.LastIndex(s[:idx], "[")
		if start < 0 {
			break
		}
		end := strings.Index(s[idx:], ")")
		if end < 0 {
			break
		}
		text := s[start+1 : idx]
		s = s[:start] + text + s[idx+end+1:]
	}
	return s
}

func stripPair(s, delim string) string {
	for {
		idx := strings.Index(s, delim)
		if idx < 0 {
			break
		}
		next := strings.Index(s[idx+len(delim):], delim)
		if next < 0 {
			break
		}
		inner := s[idx+len(delim) : idx+len(delim)+next]
		s = s[:idx] + inner + s[idx+len(delim)+next+len(delim):]
	}
	return s
}
