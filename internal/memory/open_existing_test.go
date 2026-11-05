package memory

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// A missing store is refused before anything is created: no directory, no
// database file, no schema. This is the guarantee read-only paths rely on.
func TestOpenExistingMissingCreatesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not-yet", "nested")
	path := filepath.Join(dir, "memory.db")

	store, err := OpenExisting(path, "/ws")
	if err == nil {
		store.Close()
		t.Fatal("OpenExisting on a missing store succeeded; want an error")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("error %v does not wrap fs.ErrNotExist", err)
	}
	if _, statErr := os.Stat(filepath.Dir(dir)); !os.IsNotExist(statErr) {
		t.Errorf("parent directory was created: stat err=%v", statErr)
	}
}

// An existing store opens and is usable.
func TestOpenExistingOpensExistingStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	created, err := Open(path, "/ws")
	if err != nil {
		t.Fatal(err)
	}
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenExisting(path, "/ws")
	if err != nil {
		t.Fatalf("OpenExisting on an existing store: %v", err)
	}
	defer store.Close()
}
