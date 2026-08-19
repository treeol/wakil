package diag

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoDirectStdWritesInProduction guards the TUI-garble invariant: no
// production package under internal/ (except the startup-safe config and exec
// constructors, and this diag seam itself) may write directly to os.Stderr or
// os.Stdout. A raw write from any goroutine alive during prog.Run() would
// interleave with Bubble Tea's alt-screen renderer and garble the terminal —
// the bug this package exists to prevent.
//
// The scan is textual (not AST): it flags lines containing a write-shaped call
// (Fprint/Fprintf/Fprintln/Print/Printf/Println) whose first argument is
// os.Stderr or os.Stdout, and log.Print* calls. It skips _test.go files and
// the allowlisted directories below.
func TestNoDirectStdWritesInProduction(t *testing.T) {
	allowed := map[string]bool{
		"config": true, // startup config-file creation (pre-TUI)
		"exec":   true, // DockerExecutor constructor warnings (pre-TUI)
		"diag":   true, // the seam itself
		"wiring": true, // bootstrap package (chunk 7): BuildApp/NewExecutor
		// warnings are pre-TUI (same class as config/exec), and RunHeadless is
		// the headless CLI entry point — a separate process with no TUI loop, so
		// its stderr error messages ("executor error:", "policy:", …) cannot
		// garble an alt-screen. No wiring write can occur during prog.Run().
	}

	root := repoRoot(t)
	var violations []string
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != filepath.Join(root, "internal") && allowed[filepath.Base(path)] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(b), "\n") {
			if hasDirectStdWrite(line) {
				violations = append(violations, filepath.ToSlash(path)+":"+itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(violations) > 0 {
		t.Errorf("direct os.Stderr/os.Stdout writes in production internal/ code (route through diag):\n  %s",
			strings.Join(violations, "\n  "))
	}
}

// hasDirectStdWrite reports whether line contains a write call whose target is
// os.Stderr or os.Stdout, or a log package write (which defaults to stderr).
func hasDirectStdWrite(line string) bool {
	// log.Print/Printf/Println default to os.Stderr.
	for _, fn := range []string{"log.Print", "log.Printf", "log.Println"} {
		if strings.Contains(line, fn+"(") {
			return true
		}
	}
	// fmt.Fprintf(os.Stderr, ...) / fmt.Fprintln(os.Stderr, ...) etc.
	for _, target := range []string{"os.Stderr", "os.Stdout"} {
		for _, fn := range []string{"Fprint", "Fprintf", "Fprintln", "Print", "Printf", "Println"} {
			if strings.Contains(line, fn+"("+target) {
				return true
			}
		}
		// os.Stderr.Write(...) / os.Stdout.Write(...)
		if strings.Contains(line, target+".Write(") {
			return true
		}
	}
	return false
}

// repoRoot locates the module root by walking up until go.mod is found.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found walking up from cwd")
		}
		dir = parent
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
