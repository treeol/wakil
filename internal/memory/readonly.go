package memory

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
)

// OpenReadOnly opens an existing store read-only (0.5.0, C2). It is the only
// way the legacy shared store is opened: the file is opened with mode=ro, so
// SQLite rejects every write and creates nothing. No directory is created, no
// pragma is set (journal_mode=WAL would write), no schema is created and no
// migration runs. A missing file is an error.
//
// Only the read methods (Get, GetByID, List, Search, SearchRaw, Stats) are
// meaningful on the returned store; write methods fail at the database.
// Sweep must not be called on it.
func OpenReadOnly(dbPath, workspaceRoot string) (*Store, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("memory: no store at %s: %w", dbPath, err)
	}
	dsn := "file:" + (&url.URL{Path: dbPath}).EscapedPath() + "?mode=ro"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("memory: open read-only: %w", err)
	}
	db.SetMaxOpenConns(1)
	// Probe: a read must succeed. A corrupt or foreign file fails here.
	var n int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master").Scan(&n); err != nil {
		db.Close()
		return nil, fmt.Errorf("memory: read-only probe %s: %w", dbPath, err)
	}
	return &Store{
		db:            db,
		workspaceRoot: workspaceRoot,
		readOnly:      true,
		nowFunc:       func() int64 { return timeNowMilli() },
	}, nil
}

// ReadOnly reports whether the store was opened with OpenReadOnly.
func (s *Store) ReadOnly() bool { return s != nil && s.readOnly }
