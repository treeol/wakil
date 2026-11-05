// Package testutil isolates test binaries from the user's real Wakil data and
// config directories and fails a package that touches them anyway.
//
// Use it from a package's TestMain:
//
//	func TestMain(m *testing.M) { os.Exit(testutil.Main(m)) }
package testutil

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// AllowRealDirChangesEnv disables the post-run guard when set to a non-empty
// value. The guard compares the real data dir before and after the run, so a
// real Wakil chat running on the same machine and creating new session or
// memory directories at that moment would be a false positive.
const AllowRealDirChangesEnv = "WAKIL_TEST_ALLOW_REAL_DIR_CHANGES"

// report is where Main writes its own diagnostics. A variable so the guard's
// failure output can be tested; production code never imports this package.
var report io.Writer = os.Stderr

// Main runs m with the process pointed at a private temp tree and returns the
// exit code (0 only if the tests passed AND the real directories were left
// alone). It sets HOME, XDG_DATA_HOME, XDG_CONFIG_HOME, XDG_CACHE_HOME and
// WAKIL_SESSIONS_DIR (see the comment at its creation) to private temp dirs. Tests that need their own location keep using t.Setenv, which
// overrides these for the test's duration.
func Main(m *testing.M) int {
	real := realDirs()
	before := snapshotAll(real)

	tmp, err := os.MkdirTemp("", "wakil-test-")
	if err != nil {
		fmt.Fprintf(report, "testutil: cannot create temp tree, tests run unisolated: %v\n", err)
		return m.Run()
	}
	home := filepath.Join(tmp, "home")
	data := filepath.Join(tmp, "data")
	// The sessions dir stays a direct child of the OS temp dir, exactly as the
	// per-package TestMains used to do it: its parent is where the toolcache
	// goes (<tmp>/toolcache), and some tests cap a result at a few hundred bytes
	// and depend on that spill path staying short. It is a private temp dir, not
	// a user data dir.
	sessions, serr := os.MkdirTemp("", "wakil-test-sessions")
	if serr != nil {
		fmt.Fprintf(report, "testutil: cannot create sessions dir, tests run with the default: %v\n", serr)
		sessions = filepath.Join(data, "sessions")
	}
	for _, d := range []string{home, data, filepath.Join(tmp, "config"), filepath.Join(tmp, "cache")} {
		_ = os.MkdirAll(d, 0o700)
	}
	for k, v := range map[string]string{
		"HOME":                 home,
		"XDG_DATA_HOME":        data,
		"XDG_CONFIG_HOME":      filepath.Join(tmp, "config"),
		"XDG_CACHE_HOME":       filepath.Join(tmp, "cache"),
		"WAKIL_SESSIONS_DIR":   sessions,
		"WAKIL_HISTORY_FILE":   "", // unset below: XDG_DATA_HOME already redirects it
		"WAKIL_REPO_STATE_DIR": "",
	} {
		if v == "" {
			_ = os.Unsetenv(k)
			continue
		}
		_ = os.Setenv(k, v)
	}

	code := m.Run()
	_ = os.RemoveAll(tmp)
	_ = os.RemoveAll(sessions)

	if os.Getenv(AllowRealDirChangesEnv) != "" {
		return code
	}
	if violations := Compare(before, snapshotAll(real)); len(violations) > 0 {
		fmt.Fprintf(report, "testutil: tests changed the real Wakil directories (set %s=1 to ignore, e.g. while a real chat runs):\n", AllowRealDirChangesEnv)
		for _, v := range violations {
			fmt.Fprintf(report, "  %s\n", v)
		}
		if code == 0 {
			code = 1
		}
	}
	return code
}

// realDirs returns the directories the guard watches, resolved from the
// environment the process started with (before Main redirects it).
func realDirs() []string {
	var dirs []string
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		dirs = append(dirs, filepath.Join(x, "wakil"))
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		dirs = append(dirs, filepath.Join(x, "wakil"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs,
			filepath.Join(home, ".local", "share", "wakil"),
			filepath.Join(home, ".config", "wakil"))
	}
	// De-duplicate while keeping order.
	seen := map[string]bool{}
	out := dirs[:0]
	for _, d := range dirs {
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out
}

// Snapshot is a listing of one or more directory trees: every path (relative
// to the watched root, prefixed with the root) and, for regular files directly
// under a root, its size.
type Snapshot struct {
	paths map[string]struct{}
	sizes map[string]int64
}

func snapshotAll(roots []string) Snapshot {
	s := Snapshot{paths: map[string]struct{}{}, sizes: map[string]int64{}}
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // unreadable or missing: not part of the listing
			}
			s.paths[p] = struct{}{}
			if d.Type().IsRegular() && filepath.Dir(p) == root {
				if info, ierr := d.Info(); ierr == nil {
					s.sizes[p] = info.Size()
				}
			}
			return nil
		})
	}
	return s
}

// Compare reports what the run changed: entries created under a watched root
// (a new file or directory), and growth of regular files directly under a root
// (an appended input_history). Removals are not reported.
func Compare(before, after Snapshot) []string {
	var out []string
	for p := range after.paths {
		if _, ok := before.paths[p]; !ok {
			out = append(out, "created "+p)
		}
	}
	for p, n := range after.sizes {
		if old, ok := before.sizes[p]; ok && n > old {
			out = append(out, fmt.Sprintf("grew %s (%d -> %d bytes)", p, old, n))
		}
	}
	sort.Strings(out)
	return out
}

// SnapshotDirs lists the given directories; exported for the guard's own test.
func SnapshotDirs(roots ...string) Snapshot { return snapshotAll(roots) }
