package agent

import (
	"fmt"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/lsp"
)

// budgetFile returns a fileSymbols whose rendered section is exactly `size`
// bytes or more, built from one long symbol name.
func budgetFile(path string, size int) fileSymbols {
	name := strings.Repeat("x", size)
	return fileSymbols{
		path:    path,
		symbols: []lsp.DocumentSymbol{{Name: name, Kind: lsp.SymFunction, Range: lspRange(0, 0, 1, 0)}},
	}
}

// TestRenderOutlineBudget_CountsDroppedFiles checks that the dropped count is
// the number of files that did not fit, not the total number of candidates.
func TestRenderOutlineBudget_CountsDroppedFiles(t *testing.T) {
	const budget = 32 * 1024
	// Each section is about 6 KiB, so the 32 KiB budget (minus the marker
	// reserve) fits some of the 10 files but not all of them.
	var results []fileSymbols
	for i := 0; i < 10; i++ {
		results = append(results, budgetFile(fmt.Sprintf("f%d.go", i), 6*1024))
	}

	outline, dropped, truncated := renderOutlineBudget(results, budget)

	rendered := 0
	for i := range results {
		if strings.Contains(outline, fmt.Sprintf("f%d.go:", i)) {
			rendered++
		}
	}
	if rendered == 0 || rendered == len(results) {
		t.Fatalf("test setup: want a partial render, rendered=%d of %d", rendered, len(results))
	}
	if want := len(results) - rendered; dropped != want {
		t.Errorf("dropped = %d, want %d (total %d minus rendered %d)", dropped, want, len(results), rendered)
	}
	if truncated {
		t.Errorf("truncated = true for a multi-file cut; want false")
	}
}

// TestRenderOutlineBudget_SingleOversizedFile checks that one file larger than
// the budget is included truncated, is marked as truncated, and yields no
// "files omitted" count, because no file was dropped.
func TestRenderOutlineBudget_SingleOversizedFile(t *testing.T) {
	const budget = 1000
	results := []fileSymbols{budgetFile("big.go", 5000)}

	outline, dropped, truncated := renderOutlineBudget(results, budget)

	if dropped != 0 {
		t.Errorf("dropped = %d, want 0 (the only file is included, truncated)", dropped)
	}
	if !truncated {
		t.Errorf("truncated = false, want true")
	}
	if !strings.Contains(outline, "big.go:") {
		t.Errorf("outline must still name the included file: %.120q", outline)
	}
	if !strings.Contains(outline, "(truncated)") {
		t.Errorf("outline should carry the truncation note: %.120q", outline)
	}
	if len(outline) > budget-100+len("\n  (truncated)\n") {
		t.Errorf("outline %d bytes exceeds the budget", len(outline))
	}
}

// TestRenderOutlineBudget_AllFit checks the no-cut case: nothing dropped, not truncated.
func TestRenderOutlineBudget_AllFit(t *testing.T) {
	results := []fileSymbols{budgetFile("a.go", 20), budgetFile("b.go", 20)}

	outline, dropped, truncated := renderOutlineBudget(results, 32*1024)

	if dropped != 0 || truncated {
		t.Errorf("dropped=%d truncated=%v, want 0 and false", dropped, truncated)
	}
	for _, name := range []string{"a.go:", "b.go:"} {
		if !strings.Contains(outline, name) {
			t.Errorf("outline missing %q", name)
		}
	}
}
