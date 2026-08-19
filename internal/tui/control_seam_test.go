package tui

// control_seam_test.go: the structural guard for the chunk-6 TUI→App mutation
// seam (docs/cards/card-148-chunk6-plan.md §5, exit criteria 2–3).
//
// This is a HEURISTIC source scan, not a semantic proof. It detects:
//
//	(a) assignment / compound-assignment / inc-dec statements whose left-hand
//	    side is syntactically rooted at `m.app` (the `*agent.App` field) —
//	    i.e. raw field writes;
//	(b) method calls through `m.app` whose method name is in the reflected
//	    method set of agent.Control ∪ agent.StateApply (the mutation seams) —
//	    i.e. mutations must go through `m.control` / `m.apply`, never `m.app`;
//	(c) call sites that pass `m.app` (or a local `app` captured from it) into an
//	    agent free function NOT in the fixed enumerated set {RunTurn,
//	    RunFinalReview, HandleTUICommand, ResumeSessionMsg}. The first three are
//	    documented deferred exceptions (command/turn dispatch, deliverable 7);
//	    ResumeSessionMsg is allowed only because its Conv write was fixed to
//	    take convMu (chunk 6).
//
// It does NOT and cannot catch (documented gaps, not covered):
//	- aliasing: `a := m.app; a.X = ...` (a local `app` capture passed to an
//	  enumerated function IS tolerated by (c); a capture used for a direct field
//	  write is not detected)
//	- method values: `f := m.app.SetWorkflow; f(nil)`
//	- mutation inside any agent function the TUI calls with *App (besides the
//	  enumerated set, which is name-checked)
//
// The real structural enforcement is removing *agent.App from tuiModel
// entirely (the turn-driving chunk). Until then this test is a regression
// heuristic, intentionally narrow, that fails the build if a future TUI write
// bypasses the seam the way it did before.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	agent "github.com/treeol/wakil/internal/agent"
)

// passAppCalls is the enumerated set of agent free functions the TUI directly
// calls with *App. These are command/turn DISPATCH entry points that mutate App
// internally and are deferred to the turn-driving chunk (deliverable 7):
// RunTurn, RunFinalReview, HandleTUICommand. ResumeSessionMsg is session-resume
// (not turn-driving) and is allowed only because its Conv write was fixed to
// take convMu (chunk 6). (HandlePlanCommand mutates App too, but is reached
// only from inside HandleTUICommand — not a direct TUI call site.)
var passAppCalls = map[string]bool{
	"RunTurn":          true,
	"RunFinalReview":   true,
	"HandleTUICommand": true,
	"ResumeSessionMsg": true,
}

// mutationMethods is the reflected method set of the two mutation seams. A
// call through `m.app` to any of these is a seam bypass.
func mutationMethods() map[string]bool {
	out := map[string]bool{}
	add := func(i interface{}) {
		t := reflect.TypeOf(i).Elem()
		for i := 0; i < t.NumMethod(); i++ {
			out[t.Method(i).Name] = true
		}
	}
	var c agent.Control
	add(&c)
	var a agent.StateApply
	add(&a)
	return out
}

// tuiSourceFiles returns the non-test .go files of the tui package.
func tuiSourceFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range files {
		if !strings.HasSuffix(f, "_test.go") {
			out = append(out, f)
		}
	}
	return out
}

func pos(fset *token.FileSet, f string, n ast.Node) string {
	return f + ":" + fset.Position(n.Pos()).String()
}

// rootedAtAppField reports whether e IS the `m.app` selector or a
// selector/index/star/paren expression whose base is rooted at `m.app`.
func rootedAtAppField(e ast.Expr) bool {
	if isAppField(e) {
		return true
	}
	switch e := e.(type) {
	case *ast.SelectorExpr:
		return rootedAtAppField(e.X)
	case *ast.IndexExpr:
		return rootedAtAppField(e.X)
	case *ast.StarExpr:
		return rootedAtAppField(e.X)
	case *ast.ParenExpr:
		return rootedAtAppField(e.X)
	}
	return false
}

// TestControlSeamGuardDetectsBypass proves the guard is not vacuous: parsing a
// synthetic source fragment containing a raw field write and a seam-method call
// through m.app must produce violations. If this ever fails, the guard's
// detection logic is broken and the main test is passing for the wrong reason.
func TestControlSeamGuardDetectsBypass(t *testing.T) {
	muts := mutationMethods()
	fset := token.NewFileSet()
	const src = `package tui
func bypass(m tuiModel) {
	m.app.CtxLimit = msgLimit
	m.app.SetAutoApprove(true)
}
`
	node, err := parser.ParseFile(fset, "synthetic.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var violations []string
	ast.Inspect(node, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			for _, lhs := range n.Lhs {
				if rootedAtAppField(lhs) {
					violations = append(violations, "assign")
				}
			}
		case *ast.CallExpr:
			if se, ok := n.Fun.(*ast.SelectorExpr); ok && rootedAtAppField(se.X) && muts[se.Sel.Name] {
				violations = append(violations, "seam-call")
			}
		}
		return true
	})
	if len(violations) != 2 {
		t.Fatalf("guard failed to detect both bypass patterns (got %d: %v); detection logic is broken", len(violations), violations)
	}
}

// isAppField reports whether e is exactly the `m.app` selector.
func isAppField(e ast.Expr) bool {
	se, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := se.X.(*ast.Ident)
	return ok && id.Name == "m" && se.Sel.Name == "app"
}

// TestControlSeamNoDirectAppFieldWrites is exit criterion 2: no direct field
// write rooted at m.app, and no seam-method call through m.app, in TUI
// non-test source.
func TestControlSeamNoDirectAppFieldWrites(t *testing.T) {
	muts := mutationMethods()
	fset := token.NewFileSet()
	var violations []string

	for _, f := range tuiSourceFiles(t) {
		node, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		ast.Inspect(node, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.AssignStmt:
				for _, lhs := range n.Lhs {
					if rootedAtAppField(lhs) {
						violations = append(violations,
							pos(fset, f, lhs)+": assignment to m.app field bypasses the Control/StateApply seam")
					}
				}
			case *ast.IncDecStmt:
				if rootedAtAppField(n.X) {
					violations = append(violations,
						pos(fset, f, n)+": inc/dec of m.app field bypasses the seam")
				}
			case *ast.CallExpr:
				se, ok := n.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				// A call whose receiver is rooted at m.app and whose selected
				// method is a seam mutation → bypass. Reads (Consent, ContextLimit,
				// EffectiveModel, SessionWorkspace, …) are not in the set and pass.
				if rootedAtAppField(se.X) && muts[se.Sel.Name] {
					violations = append(violations,
						pos(fset, f, n)+": call to seam method "+se.Sel.Name+" through m.app (use m.control/m.apply)")
				}
			}
			return true
		})
	}

	if len(violations) > 0 {
		t.Fatalf("TUI→App mutation seam violated (%d):\n  %s",
			len(violations), strings.Join(violations, "\n  "))
	}
}

// TestControlSeamNoPassAppBeyondEnumSet is exit criterion 3: the set of agent
// free functions the TUI calls with *App (or a capture of it) is exactly the
// enumerated deferred set.
func TestControlSeamNoPassAppBeyondEnumSet(t *testing.T) {
	fset := token.NewFileSet()
	var violations []string

	for _, f := range tuiSourceFiles(t) {
		node, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		ast.Inspect(node, func(n ast.Node) bool {
			ce, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			// Only agent-package free functions can host hidden mutation. A
			// selector `agent.X` where X takes the App pointer is the risk class.
			se, ok := ce.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := se.X.(*ast.Ident)
			if !ok || id.Name != "agent" {
				return true
			}
			name := se.Sel.Name
			if passAppCalls[name] {
				return true
			}
			// Does any argument pass the App pointer ITSELF (exactly `m.app`, or
			// a local `app` capture)? Passing a field read (m.app.Conv) is a
			// read, not a pointer hand-off, and is out of scope.
			for _, arg := range ce.Args {
				if isAppField(arg) {
					violations = append(violations,
						pos(fset, f, ce)+": passes *App to unenumerated agent function "+name)
					return true
				}
				if a, ok := arg.(*ast.Ident); ok && a.Name == "app" {
					violations = append(violations,
						pos(fset, f, ce)+": passes captured *App to unenumerated agent function "+name)
					return true
				}
			}
			return true
		})
	}

	if len(violations) > 0 {
		t.Fatalf("TUI passes *App to unenumerated agent function (%d):\n  %s",
			len(violations), strings.Join(violations, "\n  "))
	}
}
