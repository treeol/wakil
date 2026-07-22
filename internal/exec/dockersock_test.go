package exec

import (
	"os"
	"path/filepath"
	"testing"
)

// dockersock_test.go — tests for fileGid, the Docker socket GID helper.

func TestFileGid_ExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "testfile")
	if err := os.WriteFile(path, []byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}
	gid, ok := fileGid(path)
	if !ok {
		t.Fatal("expected ok=true for existing file")
	}
	// The GID should match the current process's GID (the file was created
	// by this test process).
	expected := os.Getgid()
	if int(gid) != expected {
		t.Errorf("gid = %d, want %d (process gid)", gid, expected)
	}
}

func TestFileGid_NonExistentFile(t *testing.T) {
	gid, ok := fileGid("/nonexistent/path/that/does/not/exist")
	if ok {
		t.Error("expected ok=false for non-existent file")
	}
	if gid != 0 {
		t.Errorf("expected gid=0 for non-existent file, got %d", gid)
	}
}

func TestFileGid_Directory(t *testing.T) {
	dir := t.TempDir()
	gid, ok := fileGid(dir)
	if !ok {
		t.Fatal("expected ok=true for existing directory")
	}
	if gid == 0 {
		t.Error("expected non-zero gid for directory (should be process gid)")
	}
}
