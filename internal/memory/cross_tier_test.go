package memory

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCrossTierSupersedeBlocked verifies that a mid-tier write cannot
// supersede a durable active entry. This prevents a subagent from
// displacing a promoted durable entry by writing a mid-tier entry to
// the same key — when the mid entry expires, the key would have no
// active entry and the durable entry stays superseded.
func TestCrossTierSupersedeBlocked(t *testing.T) {
	dir := t.TempDir()
	wsRoot := filepath.Join(dir, "workspace")
	os.MkdirAll(wsRoot, 0o700)
	dbPath := filepath.Join(dir, "memory", "test.db")

	now := time.Now().UnixMilli()
	s, err := Open(dbPath, wsRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.nowFunc = func() int64 { return now }

	c := ctx(t)

	// Write a durable active entry.
	_, err = s.PutActive(c, "key/x", "durable value", "note", TierDurable, "main", "s1", TaintUnknown, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	// Try to supersede with a mid-tier entry — should fail.
	expires := now + int64(time.Hour.Milliseconds())
	_, err = s.PutActive(c, "key/x", "mid value", "note", TierMid, "sub-a", "s1", TaintUnknown, &expires, nil, "")
	if err == nil {
		t.Fatal("expected error: cannot supersede durable active with mid-tier write")
	}

	// Verify the durable entry is still active.
	got, err := s.Get(c, "key/x")
	if err != nil {
		t.Fatal(err)
	}
	if got.Value != "durable value" {
		t.Fatalf("durable entry should still be active, got: %s", got.Value)
	}

	// Durable can supersede durable — should succeed.
	_, err = s.PutActive(c, "key/x", "new durable", "note", TierDurable, "main", "s1", TaintUnknown, nil, nil, "")
	if err != nil {
		t.Fatalf("durable should supersede durable: %v", err)
	}

	// Mid can supersede mid — should succeed.
	expires2 := now + 2*int64(time.Hour.Milliseconds())
	_, err = s.PutActive(c, "key/y", "mid1", "note", TierMid, "main", "s1", TaintUnknown, &expires2, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	expires3 := now + 3*int64(time.Hour.Milliseconds())
	_, err = s.PutActive(c, "key/y", "mid2", "note", TierMid, "main", "s1", TaintUnknown, &expires3, nil, "")
	if err != nil {
		t.Fatalf("mid should supersede mid: %v", err)
	}
}
