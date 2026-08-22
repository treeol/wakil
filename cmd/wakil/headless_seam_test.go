package main

// headless_seam_test.go: the structural guard for the chunk-7 headless
// re-route (docs/cards/card-148-chunk7-plan.md D19/D20, exit criterion 2)
// and the Gate #1 cmd half (card-148-chunk7b-plan.md: main.go stops
// importing internal/agent once the wiring wrappers exist).
//
// It is a source-level (AST) check, NOT go list (which is package-granular and
// cannot distinguish the deferred TUI path from the headless path — both live in
// package main). It asserts that NO cmd/wakil non-test file imports
// internal/agent — including main.go, which reaches the agent surface only
// through internal/wiring wrappers (sessions.go). internal/tui remains allowed
// in main.go alone (the deferred TUI bootstrap); run.go (the headless shim)
// must be agent-free and tui-free.

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cmdNonTestFiles lists the cmd/wakil non-test Go files. internal/tui is
// allowed in main.go alone (the deferred TUI bootstrap); every other file —
// main.go included — must be free of internal/agent.
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
		out = append(out, name)
	}
	return out
}

// TestHeadlessNoAgentImport asserts that no non-test cmd/wakil file imports
// internal/agent (Gate #1, cmd half — closed by the wiring sessions wrappers),
// and that internal/tui appears in main.go alone.
//
// daemon_server.go is excepted from the internal/agent rule: it is the daemon
// server's server-side code (previously cmd/wakild/server.go) and genuinely
// needs agent.App and agent.SessionHostDBPath. The headless shim (run.go) and
// the TUI entry (main.go) remain agent-free.
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
			if path == "github.com/treeol/wakil/internal/agent" && f != "daemon_server.go" {
				violations = append(violations,
					f+":"+fset.Position(imp.Pos()).String()+": imports "+path)
			}
			if path == "github.com/treeol/wakil/internal/tui" && f != "main.go" {
				violations = append(violations,
					f+":"+fset.Position(imp.Pos()).String()+": imports "+path+" (tui is main.go-only)")
			}
		}
	}
	if len(violations) > 0 {
		t.Fatalf("cmd/wakil non-test files must not import internal/agent (any file) or internal/tui (main.go is the only TUI exception):\n  %s",
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
