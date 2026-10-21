package tools

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func mkChatDir(t *testing.T, root, name string, age time.Duration, now time.Time) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(dir, "read_file-1.txt")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ts := now.Add(-age)
	if err := os.Chtimes(f, ts, ts); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(dir, ts, ts); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSweepToolCache(t *testing.T) {
	now := time.Now()
	root := t.TempDir()
	old := mkChatDir(t, root, "old-chat", 40*24*time.Hour, now)
	fresh := mkChatDir(t, root, "fresh-chat", 2*24*time.Hour, now)
	active := mkChatDir(t, root, "active-chat", 90*24*time.Hour, now)

	// An old dir that received a NEW file recently must be kept (newest-file
	// mtime, not the directory mtime).
	touched := mkChatDir(t, root, "touched-chat", 60*24*time.Hour, now)
	if err := os.WriteFile(filepath.Join(touched, "new.txt"), []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	old60 := now.Add(-60 * 24 * time.Hour)
	_ = os.Chtimes(touched, old60, old60)

	// A symlink to an old directory outside root must never be followed.
	outside := mkChatDir(t, t.TempDir(), "outside", 90*24*time.Hour, now)
	if err := os.Symlink(outside, filepath.Join(root, "link-chat")); err != nil {
		t.Fatal(err)
	}

	n, err := SweepToolCache(root, 30*24*time.Hour, now, map[string]bool{"active-chat": true})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("removed = %d, want 1", n)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("old chat dir should be removed")
	}
	for _, keep := range []string{fresh, active, touched, outside, filepath.Join(outside, "read_file-1.txt")} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("%s should be kept: %v", keep, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(root, "link-chat")); err != nil {
		t.Error("symlink entry should be left alone")
	}
}

// TestSweepToolCacheKeepsNestedLayouts: a chat dir with a subdirectory is an
// unknown layout — kept even if its direct files are old, because RemoveAll
// would also delete possibly-fresh nested content.
func TestSweepToolCacheKeepsNestedLayouts(t *testing.T) {
	now := time.Now()
	root := t.TempDir()
	dir := mkChatDir(t, root, "nested-chat", 90*24*time.Hour, now)
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "fresh.txt"), []byte("z"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-90 * 24 * time.Hour)
	_ = os.Chtimes(dir, old, old)
	n, err := SweepToolCache(root, 30*24*time.Hour, now, nil)
	if err != nil || n != 0 {
		t.Fatalf("nested layout must be kept: n=%d err=%v", n, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sub", "fresh.txt")); err != nil {
		t.Fatal("nested fresh content was deleted")
	}
}

// TestSweepToolCacheReportsRemovalErrors: a directory that cannot be removed
// is reported (and the sweep continues with the others).
func TestSweepToolCacheReportsRemovalErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	now := time.Now()
	root := t.TempDir()
	locked := mkChatDir(t, root, "locked-chat", 90*24*time.Hour, now)
	mkChatDir(t, root, "old-chat", 90*24*time.Hour, now)
	// Removing locked-chat's file needs write permission on locked-chat.
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	old := now.Add(-90 * 24 * time.Hour)
	_ = os.Chtimes(locked, old, old)

	n, err := SweepToolCache(root, 30*24*time.Hour, now, nil)
	if n != 1 {
		t.Errorf("removed = %d, want 1 (the unlocked dir)", n)
	}
	if err == nil {
		t.Fatal("removal failure must be reported")
	}
}

func TestSweepToolCacheDisabled(t *testing.T) {
	now := time.Now()
	root := t.TempDir()
	old := mkChatDir(t, root, "old-chat", 400*24*time.Hour, now)
	if n, err := SweepToolCache(root, 0, now, nil); err != nil || n != 0 {
		t.Fatalf("retention 0 must be a no-op: n=%d err=%v", n, err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal("retention 0 must not delete anything")
	}
	if n, err := SweepToolCache(filepath.Join(root, "missing"), time.Hour, now, nil); err != nil || n != 0 {
		t.Fatalf("missing root must be a no-op: n=%d err=%v", n, err)
	}
}
