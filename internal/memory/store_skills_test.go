package memory

// store_skills_test.go tests the skill-specific Store methods: the atomic
// create/update invariant (expectExists), the store-level 256 KiB cap, and
// HistoryForKey.

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func openSkillStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "skills.db")
	s, err := Open(dbPath, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestPutSkillActive_CreateOnlyEnforcedInTx verifies that PutSkillActive with
// expectExists=false FAILS when an active row already exists — the create-only
// invariant is enforced atomically in the transaction, not just in a handler
// pre-check. This is the race-safe backstop against silent supersede.
func TestPutSkillActive_CreateOnlyEnforcedInTx(t *testing.T) {
	s := openSkillStore(t)
	ctx := context.Background()

	if _, err := s.PutSkillActive(ctx, "k", "v1", "skill", "main", "sess", TaintFalse, false, ""); err != nil {
		t.Fatalf("first create: %v", err)
	}
	// Second create of the same key must fail with ErrSkillExists — NOT
	// silently supersede (which is what would happen without the invariant).
	_, err := s.PutSkillActive(ctx, "k", "v2", "skill", "main", "sess", TaintFalse, false, "")
	if !errors.Is(err, ErrSkillExists) {
		t.Errorf("second create should fail with ErrSkillExists, got: %v", err)
	}
	// The original value must be intact (not clobbered).
	got, err := s.Get(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	if got.Value != "v1" {
		t.Errorf("value = %q, want v1 (must not be clobbered by failed create)", got.Value)
	}
}

// TestPutSkillActive_UpdateRequiresExistingInTx verifies expectExists=true
// fails when no active row exists — update cannot silently become a create.
func TestPutSkillActive_UpdateRequiresExistingInTx(t *testing.T) {
	s := openSkillStore(t)
	ctx := context.Background()

	_, err := s.PutSkillActive(ctx, "ghost", "v", "skill", "main", "sess", TaintFalse, true, "")
	if !errors.Is(err, ErrSkillNotFound) {
		t.Errorf("update of missing key should fail with ErrSkillNotFound, got: %v", err)
	}

	// Create then update succeeds.
	if _, err := s.PutSkillActive(ctx, "k", "v1", "skill", "main", "sess", TaintFalse, false, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutSkillActive(ctx, "k", "v2", "skill", "main", "sess", TaintFalse, true, ""); err != nil {
		t.Errorf("update of existing key should succeed: %v", err)
	}
}

// TestPutSkillActive_StoreLevelCap verifies the 256 KiB ceiling holds at the
// store boundary (not just the wrapper).
func TestPutSkillActive_StoreLevelCap(t *testing.T) {
	s := openSkillStore(t)
	ctx := context.Background()

	// Over 64 KiB succeeds (skills raise the memory cap).
	if _, err := s.PutSkillActive(ctx, "big", strings.Repeat("x", 100*1024), "skill", "main", "sess", TaintFalse, false, ""); err != nil {
		t.Errorf("100 KiB should succeed: %v", err)
	}
	// Over 256 KiB fails at the store.
	if _, err := s.PutSkillActive(ctx, "huge", strings.Repeat("x", skillStoreValueMaxBytes+1), "skill", "main", "sess", TaintFalse, false, ""); err == nil {
		t.Error(">256 KiB should fail at the store boundary")
	}
	// Empty value fails.
	if _, err := s.PutSkillActive(ctx, "empty", "", "skill", "main", "sess", TaintFalse, false, ""); err == nil {
		t.Error("empty value should fail")
	}
}

// TestHistoryForKey verifies the full version chain including tombstones.
func TestHistoryForKey(t *testing.T) {
	s := openSkillStore(t)
	ctx := context.Background()

	s.PutSkillActive(ctx, "k", "v1", "skill", "main", "sess", TaintFalse, false, "")
	s.PutSkillActive(ctx, "k", "v2", "skill", "main", "sess", TaintFalse, true, "")
	s.Forget(ctx, "k")

	hist, err := s.HistoryForKey(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 2 {
		t.Fatalf("history len = %d, want 2 (v2 superseded by forget, v1 superseded by update)", len(hist))
	}
	// Newest first.
	if hist[0].Value != "v2" {
		t.Errorf("history[0] = %q, want v2 (newest first)", hist[0].Value)
	}
	if hist[0].Status != StatusSuperseded {
		t.Errorf("forgotten version status = %q, want superseded", hist[0].Status)
	}
}
