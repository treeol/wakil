package tools

// TestToolsMdParity fails when a tool exists in code and is missing from the
// docs/tools.md table, or when the "asks first" column disagrees with
// GatedTool. The table is the user-facing catalogue; keeping it honest is
// the Group 10 contract.

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/proxy"
)

// toolCatalogue returns every tool the code can offer: DefaultTools (the
// built-in set), GitTools (parent + discovery), SearxngTools (when
// configured), LSP tools and Browser tools are covered by their own
// packages and not enumerated here; their docs rows are checked separately
// below.
func toolCatalogue() []proxy.Tool {
	t := DefaultTools("/work")
	t = append(t, GitTools()...)
	t = append(t, SearxngTools()...)
	return t
}

func TestToolsMdParity(t *testing.T) {
	data, err := os.ReadFile("../../docs/tools.md")
	if err != nil {
		t.Skipf("tools.md not found from package dir (running from module root?): %v; trying ./docs/tools.md", err)
		data, err = os.ReadFile("docs/tools.md")
		if err != nil {
			t.Fatalf("cannot read docs/tools.md: %v", err)
		}
	}
	text := string(data)

	// Parse the table: | `name` | gated | desc |
	rowRe := regexp.MustCompile("(?m)^\\| `([^`]+)` \\| (yes|no) \\|")
	documented := map[string]string{}
	for _, m := range rowRe.FindAllStringSubmatch(text, -1) {
		// A cell may carry `a` / `b` (grouped rows: `a` / `b` or
		// `a` / `b` / `c` — split on "` / `" and trim the backticks).
		parts := strings.Split(m[1], "` / `")
		// m[1] is the FIRST backticked name only (regex captures one);
		// the grouped /-separated names appear later in the cell.
		cell := m[0] // full row prefix up to the gated column
		_ = cell
		for _, part := range parts {
			documented[strings.TrimSpace(part)] = m[2]
		}
	}
	// Grouped rows (`a` / `b` / `c`) carry the rest after the first name:
	// expand by scanning the table lines for the `/` form.
	lineRe := regexp.MustCompile("(?m)^\\| ((?:`[^`]+`)(?: / `[^`]+`)*) \\| (yes|no) \\|")
	for _, m := range lineRe.FindAllStringSubmatch(text, -1) {
		for _, part := range strings.Split(m[1], " / ") {
			documented[strings.Trim(part, "`")] = m[2]
		}
	}

	offered := map[string]bool{}
	for _, tool := range toolCatalogue() {
		offered[tool.Function.Name] = true
	}

	// Every offered tool must be documented.
	for name := range offered {
		if _, ok := documented[name]; !ok {
			t.Errorf("tool %q exists in code but is missing from docs/tools.md", name)
		}
	}

	// The Gated column must agree with GatedTool for every documented row.
	for name, col := range documented {
		if name == "" || strings.Contains(name, " ") {
			continue
		}
		want := "no"
		if GatedTool(name) {
			want = "yes"
		}
		if col != want {
			t.Errorf("tools.md says %q gated=%q, GatedTool says gated=%v", name, col, GatedTool(name))
		}
	}

	// Removed Google tools must not be re-introduced in the docs as live.
	for _, gone := range []string{"google_search", "google_fetch_url"} {
		if _, ok := documented[gone]; ok {
			t.Errorf("removed tool %q is back in docs/tools.md", gone)
		}
		if offered[gone] {
			t.Errorf("removed tool %q is back in DefaultTools", gone)
		}
	}
}
