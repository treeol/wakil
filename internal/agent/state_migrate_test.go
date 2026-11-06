package agent

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"

	"github.com/treeol/wakil/internal/memory"
)

func fileSHA(t *testing.T, p string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

// C3: attribution. One project by anchors; none or outside stays legacy;
// anchors under two projects are ambiguous.
func TestAttributeEntry(t *testing.T) {
	a, b := "/work/alpha", "/work/beta"
	projects := []string{a, b}
	mk := func(paths ...string) *memory.Entry {
		e := &memory.Entry{}
		for _, p := range paths {
			e.Anchors = append(e.Anchors, memory.Anchor{Path: p})
		}
		return e
	}
	cases := []struct {
		name      string
		e         *memory.Entry
		want      string
		ambiguous bool
	}{
		{"no anchors", mk(), "", false},
		{"relative anchor is unattributable", mk("src/x.go"), "", false},
		{"absolute under beta", mk("/work/beta/y.go"), b, false},
		{"outside every project", mk("/elsewhere/z.go"), "", false},
		{"two absolute projects", mk("/work/alpha/x.go", "/work/beta/y.go"), "", true},
		{"one absolute, one relative", mk("/work/alpha/x.go", "src/y.go"), "", false},
		{"dotdot escape", mk("/work/alpha/../beta/q.go"), b, false},
	}
	for _, c := range cases {
		got, amb := attributeEntry(c.e, projects)
		if got != c.want || amb != c.ambiguous {
			t.Errorf("%s: got (%q,%v) want (%q,%v)", c.name, got, amb, c.want, c.ambiguous)
		}
	}
}

// C3: dry-run writes nothing; apply copies attributable entries, records their
// legacy IDs, leaves the legacy file byte-identical, and a re-run copies nothing.
func TestLegacyMigrationDryRunAndApply(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("WAKIL_WORKSPACE_KEY", "")
	ctx := context.Background()

	projA := t.TempDir()
	projB := t.TempDir()
	legacyDB := filepath.Join(t.TempDir(), "legacy.db")
	lw, err := memory.Open(legacyDB, "")
	if err != nil {
		t.Fatal(err)
	}
	// alpha: anchored under projA; beta: anchored under projB; orphan: no
	// anchors; ambiguous: anchors under both.
	if err := os.WriteFile(filepath.Join(projA, "a.go"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := lw.PutActive(ctx, "alpha/note", "A", "note", "durable", "t", "s", 0, nil, []string{filepath.Join(projA, "a.go")}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := lw.PutActive(ctx, "beta/note", "B", "note", "durable", "t", "s", 0, nil, []string{filepath.Join(projB, "b.go")}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := lw.PutActive(ctx, "orphan/note", "O", "note", "durable", "t", "s", 0, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := lw.PutActive(ctx, "both/note", "X", "note", "durable", "t", "s", 0, nil, []string{filepath.Join(projA, "a.go"), filepath.Join(projB, "b.go")}, ""); err != nil {
		t.Fatal(err)
	}
	lw.Close()
	before := fileSHA(t, legacyDB)
	projects := []string{projA, projB}

	// Dry-run: counts, nothing written to any project store.
	plan, err := PlanLegacyMigration(ctx, legacyDB, "", projects, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Total != 4 || plan.Unattributed != 2 || plan.Ambiguous != 1 {
		t.Fatalf("counts total=%d unattributed=%d ambiguous=%d", plan.Total, plan.Unattributed, plan.Ambiguous)
	}
	if plan.PerProject[projA] != 1 || plan.PerProject[projB] != 1 {
		t.Fatalf("per project %v", plan.PerProject)
	}
	if _, err := os.Stat(MemoryDBPath(projA)); !os.IsNotExist(err) {
		t.Fatal("dry-run created a project store")
	}

	// Apply: copies the two attributable entries.
	copied, _, err := ApplyLegacyMigration(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	if copied != 2 {
		t.Fatalf("copied %d, want 2", copied)
	}
	ps, err := memory.Open(MemoryDBPath(projA), projA)
	if err != nil {
		t.Fatal(err)
	}
	if e, err := ps.Get(ctx, "alpha/note"); err != nil || e.Value != "A" {
		t.Fatalf("alpha not copied: %v", err)
	}
	ps.Close()

	// Legacy file untouched; sidecars record the copied IDs.
	if fileSHA(t, legacyDB) != before {
		t.Fatal("apply modified the legacy store")
	}
	marked, err := LoadMigrationMarkers(projA)
	if err != nil || len(marked) != 1 {
		t.Fatalf("markers A: %v %v", marked, err)
	}

	// Re-run: everything already copied, nothing new.
	already := map[string]map[int64]bool{}
	for _, p := range projects {
		m, _ := LoadMigrationMarkers(p)
		already[p] = m
	}
	again, err := PlanLegacyMigration(ctx, legacyDB, "", projects, already)
	if err != nil {
		t.Fatal(err)
	}
	if again.PerProject[projA]+again.PerProject[projB] != 0 {
		t.Fatalf("re-run would copy again: %v", again.PerProject)
	}
}

// C3: a missing legacy store is an error and creates nothing.
func TestLegacyMigrationMissingStore(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "none.db")
	if _, err := PlanLegacyMigration(context.Background(), missing, "", []string{"/x"}, nil); err == nil {
		t.Fatal("missing legacy store must be an error")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("planning created the legacy file")
	}
}

// C3: a relative-anchor entry is attributed through its writer session's
// recorded workspace; a writer in an unknown workspace stays legacy.
func TestLegacyMigrationAttributesByWriterSession(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("WAKIL_WORKSPACE_KEY", "")
	t.Setenv("WAKIL_SESSIONS_DIR", t.TempDir())
	ctx := context.Background()

	projA := t.TempDir()
	projOther := t.TempDir() // not a known project
	writeSess := func(chat, ws string) {
		b := []byte(`{"chat_id":"` + chat + `","model":"m","workspace":"` + ws + `","conv":[]}`)
		if err := os.WriteFile(filepath.Join(os.Getenv("WAKIL_SESSIONS_DIR"), chat+".json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeSess("chat-a", projA)
	writeSess("chat-x", projOther)

	legacyDB := filepath.Join(t.TempDir(), "legacy.db")
	lw, err := memory.Open(legacyDB, "")
	if err != nil {
		t.Fatal(err)
	}
	// Relative anchor, writer chat-a: attributable by session.
	if _, err := lw.PutActive(ctx, "by/session", "S", "note", "durable", "t", "chat-a", 0, nil, []string{"src/x.go"}, ""); err != nil {
		t.Fatal(err)
	}
	// Relative anchor, writer chat-x (unknown workspace): stays legacy.
	if _, err := lw.PutActive(ctx, "unknown/writer", "U", "note", "durable", "t", "chat-x", 0, nil, []string{"src/y.go"}, ""); err != nil {
		t.Fatal(err)
	}
	lw.Close()

	plan, err := PlanLegacyMigration(ctx, legacyDB, "", []string{projA}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.PerProject[projA] != 1 || plan.Unattributed != 1 {
		t.Fatalf("writer attribution: per=%v unattributed=%d", plan.PerProject, plan.Unattributed)
	}
	if len(plan.Moves) != 1 || plan.Moves[0].Entry.Key != "by/session" {
		t.Fatalf("wrong entry moved: %+v", plan.Moves)
	}
}

// Host finding (0.5.0 review): every 0.4.x writer recorded the shared store's
// own path as its workspace, so that recorded workspace is not evidence. The
// writer rule must skip it; only absolute anchors under a project attribute.
func TestLegacyMigrationIgnoresSharedStoreWriterWorkspace(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("WAKIL_WORKSPACE_KEY", "")
	t.Setenv("WAKIL_SESSIONS_DIR", t.TempDir())
	ctx := context.Background()

	projA := t.TempDir()
	legacyWS := t.TempDir() // the old shared host_workdir
	writeSess := func(chat, ws string) {
		b := []byte(`{"chat_id":"` + chat + `","model":"m","workspace":"` + ws + `","conv":[]}`)
		if err := os.WriteFile(filepath.Join(os.Getenv("WAKIL_SESSIONS_DIR"), chat+".json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeSess("chat-shared", legacyWS) // writer recorded the shared path

	legacyDB := filepath.Join(t.TempDir(), "legacy.db")
	lw, err := memory.Open(legacyDB, "")
	if err != nil {
		t.Fatal(err)
	}
	// Writer recorded the shared path, but an absolute anchor names projA:
	// attributed by anchors.
	if _, err := lw.PutActive(ctx, "anchored", "A", "note", "durable", "t", "chat-shared", 0, nil,
		[]string{filepath.Join(projA, "a.go")}, ""); err != nil {
		t.Fatal(err)
	}
	// Writer recorded the shared path and no anchor: stays legacy.
	if _, err := lw.PutActive(ctx, "bare", "B", "note", "durable", "t", "chat-shared", 0, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	lw.Close()

	plan, err := PlanLegacyMigration(ctx, legacyDB, legacyWS, []string{projA}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Only the anchored entry attributes (by its absolute anchor); the bare
	// entry written from the shared path stays legacy.
	if plan.PerProject[projA] != 1 || len(plan.Moves) != 1 || plan.Moves[0].Entry.Key != "anchored" {
		t.Fatalf("expected only 'anchored' to attribute: per=%v moves=%d", plan.PerProject, len(plan.Moves))
	}
	if plan.Unattributed != 1 {
		t.Fatalf("the bare entry must stay legacy, unattributed=%d", plan.Unattributed)
	}
}
