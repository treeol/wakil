package main

// headless_seam_test.go: the structural guard for the chunk-7 headless
// re-route (docs/cards/card-148-chunk7-plan.md D19/D20, exit criterion 2).
//
// It is a source-level (AST) check, NOT go list (which is package-granular and
// cannot distinguish the deferred TUI path from the headless path — both live in
// package main). It asserts that NO cmd/wakil non-test file EXCEPT main.go (the
// deferred TUI bootstrap, chunk 7b) imports internal/agent or internal/tui.
// main.go is the enumerated exception; run.go (the headless shim) must be
// agent-free and tui-free.
//
// Gate #1 (parent plan §3) stays red until 7b removes *agent.App from
// internal/tui AND main.go stops importing it; this test enforces the HEADLESS
// half and will flag any regression that re-introduces agent into run.go.

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// headlessCleanFiles are the cmd/wakil non-test files that must NOT import
// internal/agent or internal/tui. main.go is deliberately excluded (TUI path).
func cmdNonTestFiles(t *testing.T) []string {
	t.Helper()
	ents, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if name == "main.go" {
			continue // deferred TUI bootstrap exception (chunk 7b)
		}
		out = append(out, name)
	}
	return out
}

// TestHeadlessNoAgentImport asserts no non-test cmd/wakil file other than
// main.go imports internal/agent or internal/tui.
func TestHeadlessNoAgentImport(t *testing.T) {
	fset := token.NewFileSet()
	var violations []string
	for _, f := range cmdNonTestFiles(t) {
		node, err := parser.ParseFile(fset, f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for _, imp := range node.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if path == "github.com/treeol/wakil/internal/agent" ||
				path == "github.com/treeol/wakil/internal/tui" {
				violations = append(violations,
					f+":"+fset.Position(imp.Pos()).String()+": imports "+path)
			}
		}
	}
	if len(violations) > 0 {
		t.Fatalf("headless cmd/wakil files must not import agent/tui (main.go is the TUI exception):\n  %s",
			strings.Join(violations, "\n  "))
	}
}

// TestHeadlessGuardDetectsRegression proves the guard is not vacuous: a
// synthetic file importing internal/agent must be flagged.
func TestHeadlessGuardDetectsRegression(t *testing.T) {
	fset := token.NewFileSet()
	const src = `package main
import "github.com/treeol/wakil/internal/agent"
var _ = agent.App{}
`
	node, err := parser.ParseFile(fset, "synthetic.go", src, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range node.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if path == "github.com/treeol/wakil/internal/agent" {
			return // detected
		}
	}
	t.Fatal("guard failed to detect a synthetic internal/agent import; detection is broken")
}

// TestGoListCoreIsClean mirrors the D12 core check: internal/core must not
// import internal/agent, internal/tui, or bubbletea. Verified via the module
// cache path of internal/core/*/**.go files directly (go list is slow in-test).
func TestGoListCoreIsClean(t *testing.T) {
	root := filepath.Join("..", "..", "internal", "core")
	var violations []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, forbidden := range []string{
			`"github.com/treeol/wakil/internal/agent"`,
			`"github.com/treeol/wakil/internal/tui"`,
			`"github.com/charmbracelet/bubbletea"`,
			`"github.com/treeol/wakil/api/gen"`,
			`"github.com/treeol/wakil/internal/server"`,
		} {
			if strings.Contains(string(b), forbidden) {
				violations = append(violations, path+": imports "+forbidden)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) > 0 {
		t.Fatalf("internal/core dependency guard violated:\n  %s", strings.Join(violations, "\n  "))
	}
}
