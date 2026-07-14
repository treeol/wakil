package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// newTestStore opens a store in a temp directory with a controllable clock.
// Returns the store, a cleanup function, and a pointer to the now-offset
// (in milliseconds) that tests can advance.
func newTestStore(t *testing.T) (*Store, func()) {
	t.Helper()
	dir := t.TempDir()
	wsRoot := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(wsRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "memory", "test.db")

	var nowMu sync.Mutex
	now := time.Now().UnixMilli()
	nowFunc := func() int64 {
		nowMu.Lock()
		defer nowMu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		nowMu.Lock()
		defer nowMu.Unlock()
		now += d.Milliseconds()
	}

	s, err := Open(dbPath, wsRoot)
	if err != nil {
		t.Fatal(err)
	}
	s.nowFunc = nowFunc

	cleanup := func() {
		s.Close()
	}
	// Expose advance via t context for tests that need it.
	t.Cleanup(cleanup)
	_ = advance // available if needed
	return s, cleanup
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// ─── FTS5 smoke test ───────────────────────────────────────────────────────

func TestFTS5Available(t *testing.T) {
	s, _ := newTestStore(t)
	c := ctx(t)

	// Insert an entry and search for it — if FTS5 is unavailable, the
	// schema creation in Open would have failed. This test also verifies
	// the trigger sync works.
	_, err := s.PutActive(c, "test/fts", "full text search content", "note", TierMid, "main", "s1", TaintUnknown, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	results, err := s.Search(c, "text", "", false)
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Key != "test/fts" {
		t.Fatalf("expected key test/fts, got %s", results[0].Key)
	}
}

// ─── Supersede chains ──────────────────────────────────────────────────────

func TestSupersedeChain(t *testing.T) {
	s, _ := newTestStore(t)
	c := ctx(t)

	// Write three active entries for the same key in sequence.
	e1, err := s.PutActive(c, "arch/flow", "v1", "note", TierMid, "main", "s1", TaintUnknown, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	e2, err := s.PutActive(c, "arch/flow", "v2", "note", TierMid, "main", "s1", TaintUnknown, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	e3, err := s.PutActive(c, "arch/flow", "v3", "note", TierMid, "main", "s1", TaintUnknown, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	// e1 should be superseded by e2, e2 by e3. Reload by ID since the Go
	// structs returned from PutActive are snapshots from creation time.
	e1Full, err := s.GetByID(c, e1.ID)
	if err != nil {
		t.Fatal(err)
	}
	e2Full, err := s.GetByID(c, e2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if e1Full.SupersededBy == nil || *e1Full.SupersededBy != e2.ID {
		t.Fatalf("e1 should be superseded by e2 (id=%d), got SupersededBy=%v", e2.ID, e1Full.SupersededBy)
	}
	if e2Full.SupersededBy == nil || *e2Full.SupersededBy != e3.ID {
		t.Fatalf("e2 should be superseded by e3 (id=%d), got SupersededBy=%v", e3.ID, e2Full.SupersededBy)
	}

	// e3 should be the current active entry.
	got, err := s.Get(c, "arch/flow")
	if err != nil {
		t.Fatal(err)
	}
	if got.Value != "v3" {
		t.Fatalf("expected v3, got %s", got.Value)
	}

	// Verify e1 and e2 are superseded by loading them by ID.
	e1Full, err = s.GetByID(c, e1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if e1Full.Status != StatusSuperseded {
		t.Fatalf("e1 should be superseded, got status=%s", e1Full.Status)
	}
	e2Full, err = s.GetByID(c, e2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if e2Full.Status != StatusSuperseded {
		t.Fatalf("e2 should be superseded, got status=%s", e2Full.Status)
	}

	// Verify the supersede chain: e3.Supersedes → e2, e2.Supersedes → e1.
	if e3.Supersedes == nil || *e3.Supersedes != e2.ID {
		t.Fatalf("e3 should supersede e2 (id=%d), got Supersedes=%v", e2.ID, e3.Supersedes)
	}
	if e2Full.Supersedes == nil || *e2Full.Supersedes != e1.ID {
		t.Fatalf("e2 should supersede e1 (id=%d), got Supersedes=%v", e1.ID, e2Full.Supersedes)
	}
}

func TestSupersedeWithEdit(t *testing.T) {
	s, _ := newTestStore(t)
	c := ctx(t)

	// Write a proposed entry.
	proposed, err := s.PutProposed(c, "decision/x", "original", "decision", "sub-abc", "s1", TaintUnknown, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	// Promote with an edited value.
	edited := "edited value"
	active, err := s.Promote(c, proposed.ID, &edited, "main")
	if err != nil {
		t.Fatal(err)
	}
	if active.Value != "edited value" {
		t.Fatalf("expected edited value, got %s", active.Value)
	}
	if active.PromotedBy != "main" {
		t.Fatalf("expected promoted_by=main, got %s", active.PromotedBy)
	}

	// The proposed entry should be superseded, original value preserved.
	proposedAfter, err := s.GetByID(c, proposed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if proposedAfter.Status != StatusSuperseded {
		t.Fatalf("proposed should be superseded, got %s", proposedAfter.Status)
	}
	if proposedAfter.Value != "original" {
		t.Fatalf("original value should be preserved, got %s", proposedAfter.Value)
	}

	// The active entry should supersede the proposed entry.
	if active.Supersedes == nil || *active.Supersedes != proposed.ID {
		t.Fatalf("active should supersede proposed (id=%d), got %v", proposed.ID, active.Supersedes)
	}
}

// ─── One-active-per-key under concurrent writes ───────────────────────────

func TestOneActivePerKeyConcurrent(t *testing.T) {
	s, _ := newTestStore(t)
	c := ctx(t)

	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	errs := make(chan error, n)

	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_, err := s.PutActive(c, "concurrent/key", fmt.Sprintf("v%d", i), "note", TierMid, "main", "s1", TaintUnknown, nil, nil, "")
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent write failed: %v", err)
	}

	// There must be exactly one active entry.
	got, err := s.Get(c, "concurrent/key")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusActive {
		t.Fatalf("expected active, got %s", got.Status)
	}

	// The other 19 should be superseded.
	entries, err := s.List(c, "concurrent/", "", StatusSuperseded)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != n-1 {
		t.Fatalf("expected %d superseded entries, got %d", n-1, len(entries))
	}
}

// ─── Expiry boundary ───────────────────────────────────────────────────────

func TestExpiryBoundary(t *testing.T) {
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

	// Write an entry that expires in 1 hour.
	expires := now + int64(time.Hour.Milliseconds())
	_, err = s.PutActive(c, "temp/data", "ephemeral", "note", TierMid, "main", "s1", TaintUnknown, &expires, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	// Before expiry: should be retrievable.
	got, err := s.Get(c, "temp/data")
	if err != nil {
		t.Fatalf("expected entry before expiry, got error: %v", err)
	}
	if got.Value != "ephemeral" {
		t.Fatalf("expected ephemeral, got %s", got.Value)
	}

	// Advance past expiry.
	now += 2 * time.Hour.Milliseconds()

	// After expiry: should be not found (lazy expiry on read).
	_, err = s.Get(c, "temp/data")
	if err != ErrNotFound {
		t.Fatalf("expected ErrNotFound after expiry, got %v", err)
	}

	// Sweep should mark it expired.
	if err := s.Sweep(c); err != nil {
		t.Fatal(err)
	}

	// The entry should now have status=expired.
	entries, err := s.List(c, "temp/", "", StatusExpired)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 expired entry, got %d", len(entries))
	}
}

func TestExpirySweepHardDelete(t *testing.T) {
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

	// Write an entry that expires in 1 hour.
	expires := now + int64(time.Hour.Milliseconds())
	e, err := s.PutActive(c, "temp/old", "data", "note", TierMid, "main", "s1", TaintUnknown, &expires, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	// Advance 31 days past expiry — beyond the 30-day auditability window.
	now += 31 * 24 * time.Hour.Milliseconds()

	// Sweep should mark it expired AND hard-delete it (past audit window).
	if err := s.Sweep(c); err != nil {
		t.Fatal(err)
	}

	// The entry should be gone (hard-deleted).
	_, err = s.GetByID(c, e.ID)
	if err != ErrNotFound {
		t.Fatalf("expected ErrNotFound after hard-delete, got %v", err)
	}
}

// ─── Stale-anchor detection ────────────────────────────────────────────────

func TestStaleAnchorDetection(t *testing.T) {
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

	// Create a file to anchor.
	anchorFile := filepath.Join(wsRoot, "arch.go")
	os.WriteFile(anchorFile, []byte("package main"), 0o600)

	// Write an entry anchored to the file.
	_, err = s.PutActive(c, "arch/entry", "architecture note", "note", TierDurable, "main", "s1", TaintUnknown, nil, []string{"arch.go"}, "")
	if err != nil {
		t.Fatal(err)
	}

	// Read: should not be stale.
	got, err := s.Get(c, "arch/entry")
	if err != nil {
		t.Fatal(err)
	}
	if got.StaleAnchors != 0 || got.TotalAnchors != 1 {
		t.Fatalf("expected 0 stale of 1, got %d stale of %d", got.StaleAnchors, got.TotalAnchors)
	}

	// Modify the file.
	os.WriteFile(anchorFile, []byte("package main // changed"), 0o600)

	// Read: should be stale (flag-not-filter — still returned).
	got, err = s.Get(c, "arch/entry")
	if err != nil {
		t.Fatal(err)
	}
	if got.StaleAnchors != 1 || got.TotalAnchors != 1 {
		t.Fatalf("expected 1 stale of 1, got %d stale of %d", got.StaleAnchors, got.TotalAnchors)
	}
	// A2: stale entries are still returned, flagged — never silently dropped.
	if got.Value != "architecture note" {
		t.Fatalf("stale entry value should still be returned, got %s", got.Value)
	}

	// Delete the file.
	os.Remove(anchorFile)

	// Read: still stale (missing file = stale), still returned.
	got, err = s.Get(c, "arch/entry")
	if err != nil {
		t.Fatal(err)
	}
	if got.StaleAnchors != 1 || got.TotalAnchors != 1 {
		t.Fatalf("expected 1 stale of 1 (missing file), got %d stale of %d", got.StaleAnchors, got.TotalAnchors)
	}
}

func TestStaleAnchorInSearch(t *testing.T) {
	// A2: stale entries are returned with the flag in search, not filtered.
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

	anchorFile := filepath.Join(wsRoot, "doc.go")
	os.WriteFile(anchorFile, []byte("v1"), 0o600)

	_, err = s.PutActive(c, "doc/entry", "documentation note", "note", TierDurable, "main", "s1", TaintUnknown, nil, []string{"doc.go"}, "")
	if err != nil {
		t.Fatal(err)
	}

	// Make it stale.
	os.WriteFile(anchorFile, []byte("v2"), 0o600)

	// Search should still return it, flagged.
	results, err := s.Search(c, "documentation", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result (stale, not filtered), got %d", len(results))
	}
	if results[0].StaleAnchors != 1 {
		t.Fatalf("expected 1 stale anchor, got %d", results[0].StaleAnchors)
	}
}

// ─── FTS consistency after supersede ───────────────────────────────────────

func TestFTSConsistencyAfterSupersede(t *testing.T) {
	s, _ := newTestStore(t)
	c := ctx(t)

	// Write an entry with a searchable value.
	_, err := s.PutActive(c, "data/x", "alpha content", "note", TierMid, "main", "s1", TaintUnknown, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	// Search should find it.
	results, err := s.Search(c, "alpha", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for 'alpha', got %d", len(results))
	}

	// Supersede with a new value.
	_, err = s.PutActive(c, "data/x", "beta content", "note", TierMid, "main", "s1", TaintUnknown, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	// Search for "alpha" should find nothing (the old active entry is now
	// superseded — FTS should not return it for active-only search).
	// Note: the superseded entry's FTS row still exists (triggers fire on
	// UPDATE), but the Search query filters status='active', so it's excluded.
	results, err = s.Search(c, "alpha", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results for 'alpha' after supersede, got %d", len(results))
	}

	// Search for "beta" should find the new active entry.
	results, err = s.Search(c, "beta", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for 'beta', got %d", len(results))
	}
	if results[0].Value != "beta content" {
		t.Fatalf("expected 'beta content', got %s", results[0].Value)
	}
}

// ─── Multiple proposed entries per key ────────────────────────────────────

func TestMultipleProposedPerKey(t *testing.T) {
	s, _ := newTestStore(t)
	c := ctx(t)

	// Write two proposed entries for the same key — both should succeed.
	p1, err := s.PutProposed(c, "idea/x", "proposal 1", "decision", "sub-a", "s1", TaintUnknown, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	p2, err := s.PutProposed(c, "idea/x", "proposal 2", "decision", "sub-b", "s1", TaintUnknown, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	if p1.ID == p2.ID {
		t.Fatal("proposed entries should have different IDs")
	}

	// List proposed entries for the key.
	entries, err := s.List(c, "idea/", "", StatusProposed)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 proposed entries, got %d", len(entries))
	}

	// Promote one.
	active, err := s.Promote(c, p1.ID, nil, "main")
	if err != nil {
		t.Fatal(err)
	}
	if active.Status != StatusActive {
		t.Fatalf("expected active, got %s", active.Status)
	}

	// The other should still be proposed.
	p2After, err := s.GetByID(c, p2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p2After.Status != StatusProposed {
		t.Fatalf("p2 should still be proposed, got %s", p2After.Status)
	}
}

// ─── Reject ───────────────────────────────────────────────────────────────

func TestReject(t *testing.T) {
	s, _ := newTestStore(t)
	c := ctx(t)

	proposed, err := s.PutProposed(c, "reject/x", "bad idea", "decision", "sub-a", "s1", TaintUnknown, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Reject(c, proposed.ID, "not viable"); err != nil {
		t.Fatal(err)
	}

	e, err := s.GetByID(c, proposed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if e.Status != StatusRejected {
		t.Fatalf("expected rejected, got %s", e.Status)
	}
	// A3: reject reason stored in the note column, not anchors.
	if e.Note != "not viable" {
		t.Fatalf("expected note 'not viable', got %q", e.Note)
	}
	if len(e.Anchors) != 0 {
		t.Fatalf("anchors should be empty, got %d", len(e.Anchors))
	}

	// Cannot reject a non-proposed entry.
	if err := s.Reject(c, proposed.ID, "again"); err == nil {
		t.Fatal("expected error rejecting non-proposed entry")
	}
}

// ─── Forget ───────────────────────────────────────────────────────────────

func TestForget(t *testing.T) {
	s, _ := newTestStore(t)
	c := ctx(t)

	_, err := s.PutActive(c, "forget/x", "data", "note", TierDurable, "main", "s1", TaintUnknown, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Forget(c, "forget/x"); err != nil {
		t.Fatal(err)
	}

	// Should be gone from Get (no active entry).
	_, err = s.Get(c, "forget/x")
	if err != ErrNotFound {
		t.Fatalf("expected ErrNotFound after forget, got %v", err)
	}

	// Should still exist as superseded with a tombstone note (A3: note column).
	entries, err := s.List(c, "forget/", "", StatusSuperseded)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 superseded entry, got %d", len(entries))
	}
	if entries[0].Note == "" {
		t.Fatal("expected tombstone note in note column")
	}
}

// ─── Dangling supersedes ──────────────────────────────────────────────────

func TestDanglingSupersedes(t *testing.T) {
	// Test that rendering degrades gracefully when a superseded_by reference
	// points at a hard-deleted entry (past the 30-day auditability window).
	//
	// Scenario: entry A (durable, active) is superseded by entry B (mid, TTL).
	// B expires and is hard-deleted past the audit window. A.superseded_by
	// now dangles. Loading A should still work; loading B by ID should
	// return ErrNotFound, which the render path handles as "history unavailable".
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

	// Write a mid-tier active entry A with a 2-hour TTL.
	expiresA := now + 2*int64(time.Hour.Milliseconds())
	a, err := s.PutActive(c, "chain/x", "original", "note", TierMid, "main", "s1", TaintUnknown, &expiresA, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	// Supersede A with a mid-tier entry B that has a 1-hour TTL.
	expiresB := now + int64(time.Hour.Milliseconds())
	b, err := s.PutActive(c, "chain/x", "replacement", "note", TierMid, "main", "s1", TaintUnknown, &expiresB, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	// A should now be superseded, with superseded_by pointing at B.
	aAfter, err := s.GetByID(c, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if aAfter.Status != StatusSuperseded {
		t.Fatalf("A should be superseded, got %s", aAfter.Status)
	}
	if aAfter.SupersededBy == nil || *aAfter.SupersededBy != b.ID {
		t.Fatalf("A.superseded_by should point at B (id=%d), got %v", b.ID, aAfter.SupersededBy)
	}

	// Advance 31 days — B expires and is past the 30-day audit window.
	now += 31 * 24 * time.Hour.Milliseconds()

	// Sweep: B (expired, past audit window) is hard-deleted.
	if err := s.Sweep(c); err != nil {
		t.Fatal(err)
	}

	// Loading B by ID should return ErrNotFound — the reference dangles.
	_, err = s.GetByID(c, b.ID)
	if err != ErrNotFound {
		t.Fatalf("expected ErrNotFound for hard-deleted B, got %v", err)
	}

	// Loading A by ID should still work — A is superseded (not expired),
	// so it's not hard-deleted. Its superseded_by reference dangles but
	// the load should not error.
	aReload, err := s.GetByID(c, a.ID)
	if err != nil {
		t.Fatalf("loading A with dangling superseded_by should succeed, got %v", err)
	}
	if aReload.SupersededBy != nil && *aReload.SupersededBy == b.ID {
		// The reference is still set in the DB (we don't null it on hard-delete),
		// but GetByID for that ID returns ErrNotFound. The tool layer's rendering
		// function must handle this gracefully ("history unavailable") — tested
		// at the tool layer (M2).
	}
}

// ─── Validation ───────────────────────────────────────────────────────────

func TestValidation(t *testing.T) {
	s, _ := newTestStore(t)
	c := ctx(t)

	// Empty key.
	_, err := s.PutActive(c, "", "v", "note", TierMid, "main", "s1", TaintUnknown, nil, nil, "")
	if err != ErrEmptyKey {
		t.Fatalf("expected ErrEmptyKey, got %v", err)
	}

	// Key too long.
	longKey := make([]byte, maxKeyLen+1)
	for i := range longKey {
		longKey[i] = 'a'
	}
	_, err = s.PutActive(c, string(longKey), "v", "note", TierMid, "main", "s1", TaintUnknown, nil, nil, "")
	if err != ErrKeyTooLong {
		t.Fatalf("expected ErrKeyTooLong, got %v", err)
	}

	// Value too big.
	bigValue := make([]byte, maxValueLen+1)
	for i := range bigValue {
		bigValue[i] = 'x'
	}
	_, err = s.PutActive(c, "k", string(bigValue), "note", TierMid, "main", "s1", TaintUnknown, nil, nil, "")
	if err != ErrValueTooBig {
		t.Fatalf("expected ErrValueTooBig, got %v", err)
	}
}

// ─── Stats / digest ───────────────────────────────────────────────────────

func TestStats(t *testing.T) {
	s, _ := newTestStore(t)
	c := ctx(t)

	// Write some entries.
	s.PutActive(c, "durable/1", "v1", "note", TierDurable, "main", "s1", TaintUnknown, nil, nil, "")
	s.PutActive(c, "durable/2", "v2", "note", TierDurable, "main", "s1", TaintUnknown, nil, nil, "")
	expires := s.now() + int64(time.Hour.Milliseconds())
	s.PutActive(c, "mid/1", "v3", "note", TierMid, "main", "s1", TaintUnknown, &expires, nil, "")
	s.PutProposed(c, "durable/1", "proposal", "note", "sub-a", "s1", TaintUnknown, nil, "")

	stats, err := s.Stats(c, 5)
	if err != nil {
		t.Fatal(err)
	}
	if stats.ActiveDurable != 2 {
		t.Fatalf("expected 2 active durable, got %d", stats.ActiveDurable)
	}
	if stats.ActiveMid != 1 {
		t.Fatalf("expected 1 active mid, got %d", stats.ActiveMid)
	}
	if stats.PendingProposed != 1 {
		t.Fatalf("expected 1 pending proposed, got %d", stats.PendingProposed)
	}
	if len(stats.RecentKeys) != 2 {
		t.Fatalf("expected 2 recent keys, got %d", len(stats.RecentKeys))
	}
}
