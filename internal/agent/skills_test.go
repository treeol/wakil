package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/memory"
)

// openTestSkillProfile opens an in-memory-backed skills profile in a temp dir.
func openTestSkillProfile(t *testing.T) *skillsProfile {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "skills.db")
	store, err := memory.Open(dbPath, "")
	if err != nil {
		t.Fatalf("open skill store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return newSkillsProfile(store)
}

func TestSkillDBPath_UsesStagingDataRoot(t *testing.T) {
	// With XDG_DATA_HOME set, SkillDBPath must resolve under it (not a
	// hardcoded ~/.local/share). Save/restore env.
	old := os.Getenv("XDG_DATA_HOME")
	defer os.Setenv("XDG_DATA_HOME", old)
	os.Setenv("XDG_DATA_HOME", t.TempDir())

	p := SkillDBPath()
	if p == "" {
		t.Fatal("SkillDBPath returned empty")
	}
	if filepath.Base(p) != "skills.db" {
		t.Errorf("SkillDBPath base = %q, want skills.db", filepath.Base(p))
	}
	if filepath.Base(filepath.Dir(p)) != "skills" {
		t.Errorf("SkillDBPath dir = %q, want .../skills/skills.db", p)
	}
	// Must NOT be workspace-keyed: no 16-hex-char subdirectory between
	// "skills" and "skills.db".
	if strings.Count(p, string(filepath.Separator)) < 2 {
		t.Errorf("SkillDBPath too shallow: %q", p)
	}
}

func TestSkillsProfile_SaveAndGet(t *testing.T) {
	sp := openTestSkillProfile(t)
	ctx := context.Background()

	e, err := sp.putActiveSkill(ctx, "commit-conventions", "Use conventional commits.", "main", "sess1", memory.TaintFalse, false, "")
	if err != nil {
		t.Fatalf("putActiveSkill: %v", err)
	}
	if e.Status != memory.StatusActive {
		t.Errorf("status = %q, want active", e.Status)
	}
	if e.Tier != memory.TierDurable {
		t.Errorf("tier = %q, want durable", e.Tier)
	}

	got, err := sp.getActiveSkill(ctx, "commit-conventions")
	if err != nil {
		t.Fatalf("getActiveSkill: %v", err)
	}
	if got.Value != "Use conventional commits." {
		t.Errorf("value = %q", got.Value)
	}
	if got.ExpiresAt != nil {
		t.Errorf("skill should have no expiry, got %v", got.ExpiresAt)
	}
	if got.TotalAnchors != 0 {
		t.Errorf("skill should have no anchors, got %d", got.TotalAnchors)
	}
}

func TestSkillsProfile_UpdateSupersedes(t *testing.T) {
	sp := openTestSkillProfile(t)
	ctx := context.Background()

	if _, err := sp.putActiveSkill(ctx, "k", "v1", "main", "s", memory.TaintFalse, false, ""); err != nil {
		t.Fatal(err)
	}
	e2, err := sp.putActiveSkill(ctx, "k", "v2", "main", "s", memory.TaintFalse, true, "")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if e2.Supersedes == nil {
		t.Error("update should record supersedes link to prior active")
	}

	// Only one active version.
	got, _ := sp.getActiveSkill(ctx, "k")
	if got.Value != "v2" {
		t.Errorf("active value = %q, want v2", got.Value)
	}

	// History shows both versions.
	hist, err := sp.historyForKey(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 2 {
		t.Fatalf("history len = %d, want 2", len(hist))
	}
	if hist[0].Value != "v2" || hist[1].Value != "v1" {
		t.Errorf("history order wrong: [%q %q], want [v2 v1]", hist[0].Value, hist[1].Value)
	}
}

func TestSkillsProfile_ValueCap(t *testing.T) {
	sp := openTestSkillProfile(t)
	ctx := context.Background()

	// Over 64 KiB but under 256 KiB succeeds (skills raise the memory cap).
	big := strings.Repeat("x", 100*1024)
	if _, err := sp.putActiveSkill(ctx, "big", big, "main", "s", memory.TaintFalse, false, ""); err != nil {
		t.Errorf("100 KiB skill should succeed: %v", err)
	}

	// Over 256 KiB fails.
	huge := strings.Repeat("x", skillValueMaxBytes+1)
	if _, err := sp.putActiveSkill(ctx, "huge", huge, "main", "s", memory.TaintFalse, false, ""); err == nil {
		t.Error(">256 KiB skill should fail")
	}
}

func TestSkillsProfile_KeyValidation(t *testing.T) {
	sp := openTestSkillProfile(t)
	ctx := context.Background()

	for _, bad := range []string{"", "a/b", "a\\b"} {
		if _, err := sp.putActiveSkill(ctx, bad, "v", "main", "s", memory.TaintFalse, false, ""); err == nil {
			t.Errorf("key %q should be rejected", bad)
		}
	}
}

func TestSkillsProfile_ForgetTombstones(t *testing.T) {
	sp := openTestSkillProfile(t)
	ctx := context.Background()

	if _, err := sp.putActiveSkill(ctx, "gone", "v", "main", "s", memory.TaintFalse, false, ""); err != nil {
		t.Fatal(err)
	}
	if err := sp.forgetSkill(ctx, "gone"); err != nil {
		t.Fatalf("forget: %v", err)
	}

	// No longer active.
	if _, err := sp.getActiveSkill(ctx, "gone"); err != memory.ErrNotFound {
		t.Errorf("forgotten skill should be ErrNotFound, got %v", err)
	}

	// But history still shows the tombstone.
	hist, err := sp.historyForKey(ctx, "gone")
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 {
		t.Fatalf("history len = %d, want 1 (tombstone)", len(hist))
	}
	if hist[0].Status != memory.StatusSuperseded {
		t.Errorf("tombstone status = %q, want superseded", hist[0].Status)
	}
}

func TestSkillsProfile_ListAndSearch(t *testing.T) {
	sp := openTestSkillProfile(t)
	ctx := context.Background()

	if _, err := sp.putActiveSkill(ctx, "git-flow", "How we use git.", "main", "s", memory.TaintFalse, false, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := sp.putActiveSkill(ctx, "code-style", "Formatting rules.", "main", "s", memory.TaintFalse, false, ""); err != nil {
		t.Fatal(err)
	}

	list, err := sp.listActiveSkills(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Errorf("list len = %d, want 2", len(list))
	}

	hits, err := sp.searchSkills(ctx, "git")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Error("FTS search for 'git' returned no hits")
	}
}
