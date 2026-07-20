package exec

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// confine_fuzz_test.go — Go native fuzz test for DirectExecutor.ConfinePath.
// CI runs the seed corpus (go test); longer fuzzing is manual.
//
// Invariants:
//   - panic-freedom on arbitrary path strings
//   - if ConfinePath returns no error, the result must be inside the workspace root
//     (the path confinement post-condition)
//
// Uses a t.TempDir() root so no real filesystem outside the temp dir is touched.
// Only the DirectExecutor variant is fuzzed (DockerExecutor shells out to
// readlink, which requires a running container).

// FuzzDirectExecutorConfinePath feeds random path strings to ConfinePath
// and asserts the confinement post-condition.
func FuzzDirectExecutorConfinePath(f *testing.F) {
	// Seed corpus — traversal patterns and edge cases.
	seeds := []string{
		"",                        // empty
		".",                       // current dir
		"..",                      // parent
		"../../etc/passwd",        // traversal
		"foo",                     // simple relative
		"foo/bar",                 // nested relative
		"./foo",                   // dot-prefixed
		"foo/../bar",              // traversal then back
		"foo/../../bar",           // escape then re-enter
		"./a/../b/../c",           // complex traversal
		"/",                       // root
		"/etc/passwd",             // absolute outside
		"//",                      // double slash
		"///",                     // triple slash
		"a//b",                    // internal double slash
		"\x00",                    // null byte
		"unicode/世界",              // unicode path
		"spaces/  /file",          // spaces in path
		strings.Repeat("../", 20), // deep traversal
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, path string) {
		// Create a fresh temp dir for each fuzz iteration.
		root := t.TempDir()
		e := &DirectExecutor{root: root}

		result, err := e.ConfinePath(context.Background(), path)

		if err != nil {
			// Error is fine — traversal detected, path outside workspace, etc.
			return
		}

		// Post-condition: if no error, result must be inside root.
		// Use the same isInsideWorkspace check the production code uses.
		// The root may have been resolved through symlinks (t.TempDir on
		// macOS is under /var → /private/var), so resolve both.
		resolvedRoot, _ := filepath.EvalSymlinks(root)
		if resolvedRoot == "" {
			resolvedRoot = root
		}
		if !isInsideWorkspace(result, resolvedRoot) {
			t.Errorf("path confinement violated: input=%q result=%q root=%q", path, result, resolvedRoot)
		}
	})
}
