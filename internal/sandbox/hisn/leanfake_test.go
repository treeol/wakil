package hisn

import (
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"sync"
	"testing"
)

// The fake hisn is this package's test binary, re-executed once per hisn CLI
// call. toolpath_test.go links the agent (lipgloss, go-runewidth: ~180 ms of
// package init), which made EVERY fake call ~15x slower and pushed the
// bounded cleanup tests (stopWait 300 ms) past their limits on a loaded runner.
// So when the running binary carries those dependencies, the fake is a second,
// lean build of the same package's tests (-tags leanfake excludes
// toolpath_test.go), built once per test process.
var (
	leanOnce sync.Once
	leanDir  string
	leanBin  string
	leanErr  error
)

func fakeBinary(t *testing.T) string {
	t.Helper()
	if leanBuild {
		self, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		return self
	}
	leanOnce.Do(func() {
		leanDir, leanErr = os.MkdirTemp("", "hisn-lean")
		if leanErr != nil {
			return
		}
		leanBin = filepath.Join(leanDir, "hisn-fake.test")
		cmd := osexec.Command("go", "test", "-c", "-tags", "leanfake", "-o", leanBin, ".")
		if out, err := cmd.CombinedOutput(); err != nil {
			leanErr = fmt.Errorf("building the lean fake hisn: %w\n%s", err, out)
		}
	})
	if leanErr != nil {
		t.Fatal(leanErr)
	}
	return leanBin
}

func removeLeanFake() {
	if leanDir != "" {
		_ = os.RemoveAll(leanDir)
	}
}
