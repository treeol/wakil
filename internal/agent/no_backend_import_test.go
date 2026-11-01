package agent

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The tool layer must never learn which sandbox backend runs. Production code
// in these packages may use exec.Executor and the optional capability
// interfaces, but must not import a concrete backend package or name a
// concrete executor type/constructor.
var toolLayerDirs = []string{
	".",          // internal/agent
	"../tools",   // internal/tools
	"../lsp",     // internal/lsp
	"../browser", // internal/browser
	"../tui",     // internal/tui
	"../core/sessionclient",
}

var forbiddenImports = []string{
	"github.com/treeol/wakil/internal/sandbox/hisn",
}

var forbiddenExecSelectors = map[string]bool{
	"DockerExecutor": true, "DirectExecutor": true, "BackendExecutor": true,
	"NewDockerExecutor": true, "NewDirectExecutor": true, "NewBackendExecutor": true,
	"DockerOpts": true, "NewDockerWorktreeExecutor": true,
}

// execImportNames returns the local names under which a file imports
// github.com/treeol/wakil/internal/exec.
func execImportNames(f *ast.File) map[string]bool {
	names := map[string]bool{}
	for _, imp := range f.Imports {
		if strings.Trim(imp.Path.Value, `"`) != "github.com/treeol/wakil/internal/exec" {
			continue
		}
		n := "exec"
		if imp.Name != nil {
			n = imp.Name.Name
		}
		names[n] = true
	}
	return names
}

// parseNonTestFiles parses the non-test .go files of dir. (parser.ParseDir is
// deprecated; build tags are irrelevant to these import/selector checks.)
func parseNonTestFiles(fset *token.FileSet, dir string) (map[string]*ast.File, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	files := map[string]*ast.File{}
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, n), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		files[filepath.Join(dir, n)] = f
	}
	return files, nil
}

func TestToolLayerIsBackendAgnostic(t *testing.T) {
	for _, dir := range toolLayerDirs {
		fset := token.NewFileSet()
		files, err := parseNonTestFiles(fset, dir)
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		{
			for path, f := range files {
				for _, imp := range f.Imports {
					p := strings.Trim(imp.Path.Value, `"`)
					for _, bad := range forbiddenImports {
						if p == bad || strings.HasPrefix(p, bad+"/") {
							t.Errorf("%s imports %s: the tool layer must not know the backend", filepath.ToSlash(path), p)
						}
					}
				}
				names := execImportNames(f)
				if len(names) == 0 {
					continue
				}
				ast.Inspect(f, func(n ast.Node) bool {
					sel, ok := n.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					id, ok := sel.X.(*ast.Ident)
					if ok && names[id.Name] && forbiddenExecSelectors[sel.Sel.Name] {
						t.Errorf("%s: references concrete exec.%s: use exec.Executor or an optional capability interface",
							filepath.ToSlash(path), sel.Sel.Name)
					}
					return true
				})
			}
		}
	}
}
