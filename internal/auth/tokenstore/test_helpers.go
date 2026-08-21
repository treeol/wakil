package tokenstore

import (
	"context"
	"database/sql"

	"github.com/treeol/wakil/internal/store/migrations"

	_ "modernc.org/sqlite"
)

// OpenTestDB creates a file-based SQLite DB at dbPath with all migrations
// applied. Used by tests that need a real DB-backed token store. The caller
// is responsible for closing the returned store's DB via Close().
func OpenTestDB(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA foreign_keys = ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, err
		}
	}
	if err := migrations.Apply(context.Background(), db); err != nil {
		db.Close()
		return nil, err
	}
	return New(db), nil
}

// Close closes the underlying DB handle.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}
