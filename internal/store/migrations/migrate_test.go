package migrations

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// newTestDB opens a fresh SQLite database in a temp dir and returns the *sql.DB
// and its path. The caller must Close the DB and clean the dir.
func newTestDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	// Set pragmas — PRAGMA foreign_keys is OFF by default and must be enabled
	// per-connection for FK enforcement.
	for _, pragma := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA foreign_keys = ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			t.Fatalf("set pragma %q: %v", pragma, err)
		}
	}
	return db, dbPath
}

// TestApplyFreshDB verifies that Apply creates all expected tables on a fresh DB.
func TestApplyFreshDB(t *testing.T) {
	db, _ := newTestDB(t)
	defer db.Close()

	if err := Apply(context.Background(), db); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Verify tables exist.
	for _, table := range []string{"schema_migrations", "sessions", "events"} {
		var name string
		err := db.QueryRow(
			"SELECT name FROM sqlite_master WHERE type='table' AND name=?", table,
		).Scan(&name)
		if err == sql.ErrNoRows {
			t.Fatalf("table %s not created", table)
		}
		if err != nil {
			t.Fatalf("query %s: %v", table, err)
		}
	}

	// Verify schema_migrations has version 1.
	var version int
	if err := db.QueryRow("SELECT version FROM schema_migrations").Scan(&version); err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	if version != 1 {
		t.Fatalf("expected version 1, got %d", version)
	}
}

// TestApplyIdempotent verifies that applying twice is a no-op.
func TestApplyIdempotent(t *testing.T) {
	db, _ := newTestDB(t)
	defer db.Close()

	if err := Apply(context.Background(), db); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if err := Apply(context.Background(), db); err != nil {
		t.Fatalf("second Apply: %v", err)
	}

	// Should still have exactly one migration recorded.
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 migration, got %d", count)
	}
}

// TestApplyConstraints verifies that CHECK constraints are enforced.
func TestApplyConstraints(t *testing.T) {
	db, _ := newTestDB(t)
	defer db.Close()

	if err := Apply(context.Background(), db); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Insert a valid session row for FK target.
	_, err := db.Exec(`INSERT INTO sessions (id, tenant_id, workspace, created_by, created_at, last_seq)
		VALUES ('ses_test1', 'tnt_test', 'wsp_test', 'usr_test', 1000, 1)`)
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}

	// CHECK (seq > 0) — inserting seq=0 should fail.
	_, err = db.Exec(`INSERT INTO events (tenant_id, session_id, seq, ts, kind, payload, encoding)
		VALUES ('tnt_test', 'ses_test1', 0, 1000, 'test', '{}', 'json-v1')`)
	if err == nil {
		t.Fatal("insert with seq=0 should fail CHECK constraint")
	}

	// CHECK (encoding IN ('json-v1')) — inserting with bogus encoding should fail.
	_, err = db.Exec(`INSERT INTO events (tenant_id, session_id, seq, ts, kind, payload, encoding)
		VALUES ('tnt_test', 'ses_test1', 1, 1000, 'test', '{}', 'xml-v1')`)
	if err == nil {
		t.Fatal("insert with encoding='xml-v1' should fail CHECK constraint")
	}

	// FK — inserting an event with a mismatched tenant_id should fail.
	_, err = db.Exec(`INSERT INTO events (tenant_id, session_id, seq, ts, kind, payload, encoding)
		VALUES ('tnt_other', 'ses_test1', 1, 1000, 'test', '{}', 'json-v1')`)
	if err == nil {
		t.Fatal("insert with mismatched tenant_id should fail FK constraint")
	}
}

// TestApplyReopen verifies that a DB created by Apply can be reopened and
// Apply is idempotent across the reopen.
func TestApplyReopen(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	// First open: apply migrations.
	db1, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db1: %v", err)
	}
	db1.SetMaxOpenConns(1)
	if err := Apply(context.Background(), db1); err != nil {
		t.Fatalf("Apply on db1: %v", err)
	}
	// Insert a session so we can verify it persists.
	_, err = db1.Exec(`INSERT INTO sessions (id, tenant_id, workspace, created_by, created_at, last_seq)
		VALUES ('ses_persist', 'tnt_test', 'wsp_test', 'usr_test', 1000, 1)`)
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}
	if err := db1.Close(); err != nil {
		t.Fatalf("close db1: %v", err)
	}

	// Reopen: Apply should be a no-op; data should persist.
	db2, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db2: %v", err)
	}
	db2.SetMaxOpenConns(1)
	defer db2.Close()
	if err := Apply(context.Background(), db2); err != nil {
		t.Fatalf("Apply on db2: %v", err)
	}
	// Verify the session row persisted.
	var id string
	err = db2.QueryRow("SELECT id FROM sessions WHERE id = 'ses_persist'").Scan(&id)
	if err != nil {
		t.Fatalf("persisted session not found after reopen: %v", err)
	}
	if id != "ses_persist" {
		t.Fatalf("expected ses_persist, got %s", id)
	}
}

// TestLoadMigrations verifies that embedded migrations are loaded and sorted.
func TestLoadMigrations(t *testing.T) {
	ms, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(ms) == 0 {
		t.Fatal("no migrations loaded")
	}
	// Verify sorted by version.
	for i := 1; i < len(ms); i++ {
		if ms[i].version <= ms[i-1].version {
			t.Fatalf("migrations not sorted: %d after %d", ms[i].version, ms[i-1].version)
		}
	}
	// Verify version 1 exists.
	if ms[0].version != 1 {
		t.Fatalf("expected first migration version 1, got %d", ms[0].version)
	}
}

// TestApplyFileMissing verifies that a missing db file is created by sql.Open.
func TestApplyFileMissing(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "nonexistent", "test.db")

	// The directory doesn't exist — sql.Open succeeds but the first Exec fails.
	// Create the dir first.
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	if err := Apply(context.Background(), db); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}
