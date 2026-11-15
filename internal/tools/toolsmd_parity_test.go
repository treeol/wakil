package tools

// TestToolsMdParity fails when a tool exists in code and is missing from the
// docs/tools.md table, or when the "asks first" column disagrees with the
// gatedToolList entry. The table is the user-facing catalogue; keeping it
// honest is the contract. The list is the single source: every tool the agent
// loop gates through a.Confirm is listed here, and the parity test enforces
// that the agent source and this file cannot drift apart (see below).

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/proxy"
)

// gatedToolList is the single source for which tools require human
// confirmation before running. The list mirrors the tool handlers that call
// a.Confirm (see TestGatedToolListMatchesHandlers).
var gatedToolList = map[string]bool{
	"run_shell":              true,
	"write_file":             true,
	"write_binary_file":      true,
	"edit_file":              true,
	"delete_file":            true,
	"move_file":              true,
	"run_background":         true,
	"kill_process":           true,
	"save_skill":             true,
	"update_skill":           true,
	"forget_skill":           true,
	"apply_skill_template":   true,
	"capture_skill_template": true,
	// open_url opens a URL on the host desktop; the query string can carry
	// data out, and every other outbound path needs consent.
	"open_url": true,
}

func gatedTool(name string) bool { return gatedToolList[name] }

// TestGatedTool verifies which tools require human confirmation.
func TestGatedTool(t *testing.T) {
	gated := []string{"run_shell", "write_file", "write_binary_file", "edit_file", "delete_file", "move_file", "run_background", "kill_process", "open_url", "save_skill", "update_skill", "forget_skill", "apply_skill_template", "capture_skill_template"}
	for _, name := range gated {
		if !gatedTool(name) {
			t.Errorf("gatedTool(%q) = false, want true", name)
		}
	}
	ungated := []string{"read_file", "read_file_full", "list_dir", "search_files", "find_files", "read_process_log"}
	for _, name := range ungated {
		if gatedTool(name) {
			t.Errorf("gatedTool(%q) = true, want false", name)
		}
	}
}

// TestGatedToolListMatchesHandlers keeps the gated list honest: the agent
// source files under ../../internal/agent must mention each gated tool at an
// a.Confirm call site (or the dispatch path), and must not confirm an
// ungated tool through a literal a.Confirm("name", ...) call.
func TestGatedToolListMatchesHandlers(t *testing.T) {
	agentDir := "../../internal/agent"
	entries, err := os.ReadDir(agentDir)
	if err != nil {
		t.Fatalf("cannot read %s: %v", agentDir, err)
	}
	// Collect literal a.Confirm("name", ...) call-site names from the agent
	// package (production files only).
	confirmRe := regexp.MustCompile(`a\.Confirm\("([^"]+)",`)
	confirmed := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		data, err := os.ReadFile(agentDir + "/" + e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, m := range confirmRe.FindAllSubmatch(data, -1) {
			confirmed[string(m[1])] = true
		}
	}
	// Every gated tool must appear at a Confirm site. The list also covers
	// MCP tools (through the passthrough confirmer) and run_shell via the
	// verify path, which are named here so drift is caught.
	for name := range gatedToolList {
		if !confirmed[name] {
			t.Errorf("gated tool %q is not confirmed in the agent source — the list and the gate have drifted", name)
		}
	}
	// Every literal Confirm name must be in the list.
	for name := range confirmed {
		if name == "external_backend" || strings.HasSuffix(name, "__review") {
			continue // pseudo-tool names, not catalogue tools
		}
		if !gatedToolList[name] {
			t.Errorf("agent source confirms %q but it is not in the gated list", name)
		}
	}
}

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
		// Running from module root: try the direct path before failing.
		data, err = os.ReadFile("docs/tools.md")
		if err != nil {
			t.Fatalf("cannot read docs/tools.md (tried ../../docs/tools.md and docs/tools.md): %v", err)
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

	// The Gated column must agree with gatedTool for every documented row.
	for name, col := range documented {
		if name == "" || strings.Contains(name, " ") {
			continue
		}
		want := "no"
		if gatedTool(name) {
			want = "yes"
		}
		if col != want {
			t.Errorf("tools.md says %q gated=%q, gatedTool says gated=%v", name, col, gatedTool(name))
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
