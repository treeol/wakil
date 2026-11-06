package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/treeol/wakil/internal/memory"
)

// Part C safety: migration never overwrites a newer project value, keeps
// anchor hashes and provenance, refuses a legacy destination, and a rerun is
// idempotent.
func TestMigrationSafety(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("WAKIL_WORKSPACE_KEY", "")
	ctx := context.Background()

	proj := t.TempDir()
	anchored := filepath.Join(proj, "a.go")
	if err := os.WriteFile(anchored, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	legacyDB := filepath.Join(t.TempDir(), "legacy.db")
	// The legacy store is opened with the project as its root, so the anchor
	// is hashed when written; the copy must keep that hash.
	lw, err := memory.Open(legacyDB, proj)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lw.PutActive(ctx, "k/anchored", "legacy-value", "note", "durable", "writer-x", "sess-1", 1, nil, []string{anchored}, "n"); err != nil {
		t.Fatal(err)
	}
	lw.Close()

	plan, err := PlanLegacyMigration(ctx, legacyDB, "", []string{proj}, nil)
	if err != nil {
		t.Fatal(err)
	}
	copied, newer, err := ApplyLegacyMigration(ctx, plan)
	if err != nil || copied != 1 || newer != 0 {
		t.Fatalf("first apply copied=%d newer=%d err=%v", copied, newer, err)
	}

	// Provenance and anchor hashes survive the copy.
	ps, err := memory.Open(MemoryDBPath(proj), proj)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ps.Get(ctx, "k/anchored")
	if err != nil || got == nil {
		t.Fatalf("copied entry missing: %v", err)
	}
	if got.Writer != "writer-x" || got.SessionID != "sess-1" || got.Tainted != 1 || got.Note != "n" || got.Tier != "durable" {
		t.Errorf("provenance lost: %+v", got)
	}
	// The copy must carry the legacy row's stored anchor hash verbatim (not
	// re-hash from disk). Read the legacy row to compare.
	lr, err := memory.OpenReadOnly(legacyDB, "")
	if err != nil {
		t.Fatal(err)
	}
	src, err := lr.Get(ctx, "k/anchored")
	lr.Close()
	if err != nil || src == nil {
		t.Fatalf("legacy row: %v", err)
	}
	if len(got.Anchors) != len(src.Anchors) || got.Anchors[0] != src.Anchors[0] {
		t.Errorf("anchors not preserved: copy %+v, legacy %+v", got.Anchors, src.Anchors)
	}

	// The project now holds a newer value: a rerun against an unmarked legacy
	// entry must not overwrite it.
	if _, err := ps.PutActive(ctx, "k/anchored", "project-newer", "note", "durable", "proj", "s2", 0, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	ps.Close()
	if err := os.Remove(filepath.Join(filepath.Dir(MemoryDBPath(proj)), migrationMarkerFile)); err != nil {
		t.Fatal(err)
	}
	plan2, err := PlanLegacyMigration(ctx, legacyDB, "", []string{proj}, nil)
	if err != nil {
		t.Fatal(err)
	}
	copied, newer, err = ApplyLegacyMigration(ctx, plan2)
	if err != nil || copied != 0 || newer != 1 {
		t.Fatalf("rerun must skip the newer project value: copied=%d newer=%d err=%v", copied, newer, err)
	}
	ps, err = memory.Open(MemoryDBPath(proj), proj)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := ps.Get(ctx, "k/anchored"); v == nil || v.Value != "project-newer" {
		t.Errorf("newer project value was overwritten: %+v", v)
	}
	ps.Close()

	// Identity guard: a plan whose destination is the legacy file is refused
	// before any writable open.
	bad := MigrationPlan{LegacyDB: legacyDB, Moves: []MigrationMove{{Project: legacyDB, Entry: &memory.Entry{Key: "x", Value: "y"}}}}
	if _, _, err := ApplyLegacyMigration(ctx, bad); err == nil {
		t.Error("migrating into the legacy store must be refused")
	}
}
