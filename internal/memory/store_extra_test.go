package memory

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPromoteEditOverExistingActive tests the fix for the promote-with-edit
// ordering bug: when an active entry already exists for the key, promoting
// a proposed entry with an edited value must supersede the old active FIRST,
// then INSERT the new active — not the other way around.
func TestPromoteEditOverExistingActive(t *testing.T) {
	s, _ := newTestStore(t)
	c := ctx(t)

	// Create an active entry for the key.
	_, err := s.PutActive(c, "key/x", "original active", "note", TierDurable, "main", "s1", TaintUnknown, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	// Create a proposed entry for the same key.
	proposed, err := s.PutProposed(c, "key/x", "proposed value", "decision", "sub-a", "s1", TaintUnknown, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	// Promote with an edited value — this must not fail with a unique
	// constraint violation.
	edited := "edited promoted value"
	active, err := s.Promote(c, proposed.ID, &edited, "main")
	if err != nil {
		t.Fatalf("promote with edit over existing active failed: %v", err)
	}
	if active.Value != "edited promoted value" {
		t.Fatalf("expected edited value, got %s", active.Value)
	}
	if active.Status != StatusActive {
		t.Fatalf("expected active, got %s", active.Status)
	}

	// Verify exactly one active entry.
	got, err := s.Get(c, "key/x")
	if err != nil {
		t.Fatal(err)
	}
	if got.Value != "edited promoted value" {
		t.Fatalf("expected edited promoted value, got %s", got.Value)
	}

	// Verify the proposed entry is superseded.
	proposedAfter, err := s.GetByID(c, proposed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if proposedAfter.Status != StatusSuperseded {
		t.Fatalf("proposed should be superseded, got %s", proposedAfter.Status)
	}
}

// TestPromoteInPlaceOverExistingActive tests the in-place promote path when
// an active entry already exists.
func TestPromoteInPlaceOverExistingActive(t *testing.T) {
	s, _ := newTestStore(t)
	c := ctx(t)

	// Create an active entry.
	_, err := s.PutActive(c, "key/y", "active v1", "note", TierDurable, "main", "s1", TaintUnknown, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	// Create a proposed entry for the same key.
	proposed, err := s.PutProposed(c, "key/y", "proposed", "decision", "sub-a", "s1", TaintUnknown, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	// Promote in place (no edit).
	active, err := s.Promote(c, proposed.ID, nil, "main")
	if err != nil {
		t.Fatalf("promote in place over existing active failed: %v", err)
	}
	if active.Status != StatusActive {
		t.Fatalf("expected active, got %s", active.Status)
	}
	if active.Value != "proposed" {
		t.Fatalf("expected 'proposed', got %s", active.Value)
	}

	// Verify exactly one active.
	got, err := s.Get(c, "key/y")
	if err != nil {
		t.Fatal(err)
	}
	if got.Value != "proposed" {
		t.Fatalf("expected 'proposed', got %s", got.Value)
	}
}

// TestAUTOINCREMENTNoReuse verifies that AUTOINCREMENT prevents ID reuse after
// hard-deletion — the core rationale for using AUTOINCREMENT over plain
// INTEGER PRIMARY KEY.
func TestAUTOINCREMENTNoReuse(t *testing.T) {
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

	// Create an entry with a short TTL.
	expires := now + int64(time.Hour.Milliseconds())
	e1, err := s.PutActive(c, "k", "v1", "note", TierMid, "main", "s1", TaintUnknown, &expires, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	// Advance past expiry + audit window, then sweep to hard-delete.
	now += 31 * 24 * time.Hour.Milliseconds()
	if err := s.Sweep(c); err != nil {
		t.Fatal(err)
	}

	// Create a new entry — its ID must be greater than e1's.
	e2, err := s.PutActive(c, "k2", "v2", "note", TierMid, "main", "s1", TaintUnknown, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if e2.ID <= e1.ID {
		t.Fatalf("AUTOINCREMENT should prevent ID reuse: e2.ID=%d <= e1.ID=%d", e2.ID, e1.ID)
	}

	// The deleted entry should be gone.
	_, err = s.GetByID(c, e1.ID)
	if err != ErrNotFound {
		t.Fatalf("expected ErrNotFound for hard-deleted entry, got %v", err)
	}
}

// TestAnchorPathTraversal verifies that anchor paths escaping the workspace
// are treated as missing (empty hash), not read from the host filesystem.
func TestAnchorPathTraversal(t *testing.T) {
	dir := t.TempDir()
	wsRoot := filepath.Join(dir, "workspace")
	os.MkdirAll(wsRoot, 0o700)
	dbPath := filepath.Join(dir, "memory", "test.db")

	s, err := Open(dbPath, wsRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	c := ctx(t)

	// Create a file outside the workspace.
	outsideFile := filepath.Join(dir, "secret.txt")
	os.WriteFile(outsideFile, []byte("secret"), 0o600)

	// Try to anchor a path that traverses outside the workspace.
	e, err := s.PutActive(c, "traversal/x", "note", "note", TierDurable, "main", "s1", TaintUnknown, nil, []string{"../secret.txt"}, "")
	if err != nil {
		t.Fatal(err)
	}

	// The anchor should have an empty hash (treated as missing/stale).
	if len(e.Anchors) != 1 {
		t.Fatalf("expected 1 anchor, got %d", len(e.Anchors))
	}
	if e.Anchors[0].Hash != "" {
		t.Fatalf("traversal anchor should have empty hash, got %s", e.Anchors[0].Hash)
	}

	// At read time, it should be stale.
	got, err := s.Get(c, "traversal/x")
	if err != nil {
		t.Fatal(err)
	}
	if got.StaleAnchors != 1 || got.TotalAnchors != 1 {
		t.Fatalf("expected 1 stale of 1 (traversal blocked), got %d stale of %d", got.StaleAnchors, got.TotalAnchors)
	}
}

// TestExactExpiryBoundary tests the exact boundary: an entry with
// expires_at == now is still active (inclusive: >= now means not expired).
func TestExactExpiryBoundary(t *testing.T) {
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

	// Entry expires exactly at now.
	expires := now
	_, err = s.PutActive(c, "boundary/x", "data", "note", TierMid, "main", "s1", TaintUnknown, &expires, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	// At now == expires_at: still active (inclusive, >= now).
	got, err := s.Get(c, "boundary/x")
	if err != nil {
		t.Fatalf("entry at exact boundary should be active, got: %v", err)
	}
	if got.Value != "data" {
		t.Fatalf("expected 'data', got %s", got.Value)
	}

	// Advance 1ms past expiry: now expired.
	now += 1
	_, err = s.Get(c, "boundary/x")
	if err != ErrNotFound {
		t.Fatalf("entry 1ms past boundary should be expired, got: %v", err)
	}
}

// TestPutActiveNoteRoundTrip verifies the note parameter on PutActive.
func TestPutActiveNoteRoundTrip(t *testing.T) {
	s, _ := newTestStore(t)
	c := ctx(t)

	_, err := s.PutActive(c, "note/x", "value", "note", TierDurable, "main", "s1", TaintUnknown, nil, nil, "custom note")
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.Get(c, "note/x")
	if err != nil {
		t.Fatal(err)
	}
	if got.Note != "custom note" {
		t.Fatalf("expected 'custom note', got %q", got.Note)
	}
}
