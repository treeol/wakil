package memory

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
)

func fileSum(t *testing.T, p string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

// C2 invariant: a legacy store is read, never written. The file bytes do not
// change across reads, a write fails, and nothing is created on a miss.
func TestOpenReadOnlyNeverWrites(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "legacy.db")
	w, err := Open(p, dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := w.PutActive(ctx, "legacy/k", "legacy value", "note", "durable", "test", "s", 0, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	w.Close()
	before := fileSum(t, p)

	r, err := OpenReadOnly(p, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !r.ReadOnly() {
		t.Fatal("store not marked read-only")
	}
	e, err := r.Get(ctx, "legacy/k")
	if err != nil || e.Value != "legacy value" {
		t.Fatalf("read-only Get: %v %+v", err, e)
	}
	if _, err := r.db.Exec("DELETE FROM entries"); err == nil {
		t.Fatal("write succeeded on read-only store")
	}
	r.Close()

	if after := fileSum(t, p); after != before {
		t.Fatal("read-only open changed the legacy file")
	}
	if _, err := OpenReadOnly(filepath.Join(dir, "missing.db"), dir); err == nil {
		t.Fatal("missing store must be an error")
	}
	if _, err := os.Stat(filepath.Join(dir, "missing.db")); !os.IsNotExist(err) {
		t.Fatal("OpenReadOnly created a file for a missing store")
	}
}
