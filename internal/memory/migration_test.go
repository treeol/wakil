package memory

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// TestMigrateSchemaFromV0 creates a database with an old schema (missing
// the columns added in later versions), then verifies that Open migrates it
// and all queries (Get, Search, List) work without "no such column" errors.
//
// This reproduces the bug reported in the Trello card: a DB created with an
// older schema lacks columns like session_id, supersedes, promoted_by, etc.
// createSchema's CREATE TABLE IF NOT EXISTS is a no-op on the existing table,
// so the new columns are never added — and every SELECT that references them
// fails with "SQL logic error: no such column".
func TestMigrateSchemaFromV0(t *testing.T) {
	dir := t.TempDir()
	wsRoot := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(wsRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "memory", "test.db")

	// Create the parent directory — memory.Open does this, but we're
	// creating the v0 DB directly with database/sql before Open runs.
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}

	// Create the v0 schema — the original entries table before any of the
	// provenance/supersede columns were added. This is the shape that breaks
	// modern queries.
	v0Schema := []string{
		`CREATE TABLE entries (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			key TEXT NOT NULL,
			value TEXT NOT NULL,
			kind TEXT NOT NULL,
			tier TEXT NOT NULL,
			status TEXT NOT NULL,
			writer TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			expires_at INTEGER
		)`,
		`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT)`,
	}
	v0DB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open v0 db: %v", err)
	}
	for _, stmt := range v0Schema {
		if _, err := v0DB.Exec(stmt); err != nil {
			v0DB.Close()
			t.Fatalf("v0 schema exec %q: %v", stmt, err)
		}
	}
	// Insert a row with the old schema shape (no new columns).
	if _, err := v0DB.Exec(
		`INSERT INTO entries (key, value, kind, tier, status, writer, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"arch/test", "test value", "note", TierDurable, StatusActive, "main", 1000,
	); err != nil {
		v0DB.Close()
		t.Fatalf("v0 insert: %v", err)
	}
	if err := v0DB.Close(); err != nil {
		t.Fatalf("close v0 db: %v", err)
	}

	// Now open with the current code — this should trigger migrateSchema.
	s, err := Open(dbPath, wsRoot)
	if err != nil {
		t.Fatalf("Open should migrate v0 schema: %v", err)
	}
	defer s.Close()
	s.nowFunc = func() int64 { return 2000 }

	c := context.Background()

	// Get should work — previously failed with "no such column: session_id".
	got, err := s.Get(c, "arch/test")
	if err != nil {
		t.Fatalf("Get after migration failed: %v", err)
	}
	if got.Value != "test value" {
		t.Fatalf("expected 'test value', got %q", got.Value)
	}

	// Search should work — previously failed with "no such column" in the
	// outer SELECT over entryColumns.
	results, err := s.Search(c, "test", "", false)
	if err != nil {
		t.Fatalf("Search after migration failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 search result, got %d", len(results))
	}

	// List should work.
	entries, err := s.List(c, "", "", "")
	if err != nil {
		t.Fatalf("List after migration failed: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 list entry, got %d", len(entries))
	}

	// Stats should work.
	stats, err := s.Stats(c, 10)
	if err != nil {
		t.Fatalf("Stats after migration failed: %v", err)
	}
	if stats.ActiveDurable != 1 {
		t.Fatalf("expected 1 active durable, got %d", stats.ActiveDurable)
	}

	// Verify the migrated columns have the right defaults for the old row.
	got2, _ := s.GetByID(c, got.ID)
	if got2.SessionID != "" {
		t.Fatalf("migrated session_id should default to empty string, got %q", got2.SessionID)
	}
	if got2.Tainted != TaintUnknown {
		t.Fatalf("migrated tainted should default to TaintUnknown (2), got %d", got2.Tainted)
	}
	if got2.PromotedBy != "" {
		t.Fatalf("migrated promoted_by should default to empty string, got %q", got2.PromotedBy)
	}
}

// TestMigrateSchemaIdempotent verifies that opening a current-schema DB
// (all columns present) does not error and does not re-run migrations.
func TestMigrateSchemaIdempotent(t *testing.T) {
	dir := t.TempDir()
	wsRoot := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(wsRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "memory", "test.db")

	// First open creates the full current schema.
	s1, err := Open(dbPath, wsRoot)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	s1.Close()

	// Second open should be a no-op migration — all columns already present.
	s2, err := Open(dbPath, wsRoot)
	if err != nil {
		t.Fatalf("second Open (idempotent): %v", err)
	}
	defer s2.Close()

	// Verify the store still works.
	c := context.Background()
	_, err = s2.PutActive(c, "test/idempotent", "value", "note", TierMid, "main", "s1", TaintUnknown, nil, nil, "")
	if err != nil {
		t.Fatalf("PutActive after idempotent migration: %v", err)
	}
}

// TestMigrateFTSRebuildOnStaleIndex reproduces the partial-migration hole
// flagged by review: a DB that has all columns present (no ALTER needed)
// but whose FTS index is empty/stale. This happens when createSchema creates
// the FTS virtual table on a non-empty entries table (triggers only fire on
// future writes) or after a previous migration crashed after adding columns
// but before rebuilding the index.
//
// The fix uses a persistent meta flag (fts_rebuilt) that is only set after
// the rebuild succeeds. If the flag is missing, the next Open retries.
func TestMigrateFTSRebuildOnStaleIndex(t *testing.T) {
	dir := t.TempDir()
	wsRoot := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(wsRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "memory", "test.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}

	// Open once to create the full schema, insert a row, then close.
	s1, err := Open(dbPath, wsRoot)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	c := context.Background()
	_, err = s1.PutActive(c, "test/stale-fts", "findable content", "note", TierMid, "main", "s1", TaintUnknown, nil, nil, "")
	if err != nil {
		s1.Close()
		t.Fatalf("PutActive: %v", err)
	}
	s1.Close()

	// Simulate a stale FTS index: clear the fts_rebuilt flag and delete all
	// FTS rows. On next Open, migration sees the flag missing, rebuilds.
	staleDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open stale db: %v", err)
	}
	// Clear the flag so the migration thinks it needs to rebuild.
	if _, err := staleDB.Exec("DELETE FROM meta WHERE key = 'fts_rebuilt'"); err != nil {
		staleDB.Close()
		t.Fatalf("clear fts_rebuilt flag: %v", err)
	}
	// Delete all FTS rows using the FTS5 delete-all command for external
	// content tables.
	if _, err := staleDB.Exec("INSERT INTO entries_fts(entries_fts, rowid, key, value) SELECT 'delete', id, key, value FROM entries"); err != nil {
		staleDB.Close()
		t.Fatalf("delete FTS rows: %v", err)
	}
	staleDB.Close()

	// Re-open — migration should see fts_rebuilt flag missing, entryCount > 0,
	// and rebuild the index.
	s2, err := Open(dbPath, wsRoot)
	if err != nil {
		t.Fatalf("second Open (stale FTS): %v", err)
	}
	defer s2.Close()

	// Search should now find the row.
	results, err := s2.Search(c, "findable", "", false)
	if err != nil {
		t.Fatalf("Search after FTS rebuild: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 search result after FTS rebuild, got %d", len(results))
	}
	if results[0].Key != "test/stale-fts" {
		t.Fatalf("expected key test/stale-fts, got %s", results[0].Key)
	}
}
