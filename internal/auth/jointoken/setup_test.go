package jointoken

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/treeol/wakil/internal/auth/tokenstore"
	"github.com/treeol/wakil/internal/store/migrations"

	_ "modernc.org/sqlite"
)

// setupTestStore creates a file-based SQLite DB with all migrations applied
// and returns a tokenstore + cleanup. WAL mode requires a file (not :memory:).
func setupTestStore(t *testing.T) (*tokenstore.Store, func()) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA foreign_keys = ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			t.Fatalf("pragma %q: %v", pragma, err)
		}
	}
	if err := migrations.Apply(context.Background(), db); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	store := tokenstore.New(db)
	return store, func() { db.Close() }
}
