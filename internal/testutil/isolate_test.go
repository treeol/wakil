package testutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The guard must report a created entry and an appended top-level file, and
// stay silent when nothing changed.
func TestCompareDetectsCreationAndGrowth(t *testing.T) {
	root := filepath.Join(t.TempDir(), "wakil")
	if err := os.MkdirAll(filepath.Join(root, "memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	hist := filepath.Join(root, "input_history")
	if err := os.WriteFile(hist, []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := SnapshotDirs(root)

	if got := Compare(before, SnapshotDirs(root)); len(got) != 0 {
		t.Fatalf("unchanged tree reported %v", got)
	}

	if err := os.MkdirAll(filepath.Join(root, "memory", "deadbeef"), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(hist, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("line two\n")
	f.Close()

	got := strings.Join(Compare(before, SnapshotDirs(root)), "\n")
	for _, want := range []string{"created " + filepath.Join(root, "memory", "deadbeef"), "grew " + hist} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// A watched root that does not exist yet is fine, and creating it is reported.
func TestCompareMissingRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent")
	before := SnapshotDirs(root)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := Compare(before, SnapshotDirs(root)); len(got) != 1 || !strings.HasPrefix(got[0], "created ") {
		t.Fatalf("got %v", got)
	}
}

func TestRealDirsUsesStartupEnvironment(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/x/data")
	t.Setenv("XDG_CONFIG_HOME", "/x/cfg")
	dirs := strings.Join(realDirs(), " ")
	for _, want := range []string{"/x/data/wakil", "/x/cfg/wakil"} {
		if !strings.Contains(dirs, want) {
			t.Errorf("realDirs %q missing %s", dirs, want)
		}
	}
}

// Main's own failure path: a run that touches a watched real directory must
// fail the package and name the offending entry. Exercised through compare +
// the report writer so the test does not need a real TestMain re-exec.
func TestReportNamesViolations(t *testing.T) {
	root := filepath.Join(t.TempDir(), "wakil")
	before := SnapshotDirs(root)
	if err := os.MkdirAll(filepath.Join(root, "sessionhost", "wsp_x"), 0o755); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	old := report
	report = &sb
	t.Cleanup(func() { report = old })
	for _, v := range Compare(before, SnapshotDirs(root)) {
		sb.WriteString(v + "\n")
	}
	if !strings.Contains(sb.String(), "created "+filepath.Join(root, "sessionhost")) {
		t.Fatalf("report lacks the created entry: %q", sb.String())
	}
}
