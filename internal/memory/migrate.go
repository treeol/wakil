package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Migration outcomes for PutMigrated.
const (
	MigratedCopied = "copied" // inserted as the active entry
	MigratedNewer  = "newer"  // the project already has an active entry at least as new: left alone
)

// PutMigrated copies one legacy entry into this store. It is the only copy
// path for the 0.5.0 legacy migration, and differs from PutActive in three
// ways that keep the legacy record intact:
//
//   - anchors are copied with their stored hashes (not re-hashed from disk),
//     so staleness still reflects the state at the time they were written;
//   - writer, session, taint, kind, tier, note and created_at are preserved;
//   - an existing active entry for the key is replaced only when it is older
//     than the legacy entry. A newer project value is never overwritten; the
//     result is MigratedNewer.
//
// The legacy entry is inserted as active and the previous active entry (if
// any) is superseded, in one transaction.
func (s *Store) PutMigrated(ctx context.Context, e *Entry) (string, error) {
	if err := validateKey(e.Key); err != nil {
		return "", err
	}
	if err := validateValue(e.Value); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("memory: begin tx: %w", err)
	}
	defer tx.Rollback()

	var oldID sql.NullInt64
	var oldCreated sql.NullInt64
	err = tx.QueryRowContext(ctx,
		"SELECT id, created_at FROM entries WHERE key = ? AND status = 'active'", e.Key).Scan(&oldID, &oldCreated)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("memory: find active: %w", err)
	}
	if oldID.Valid && oldCreated.Valid && oldCreated.Int64 >= e.CreatedAt {
		return MigratedNewer, nil
	}
	if oldID.Valid {
		if _, err := tx.ExecContext(ctx, "UPDATE entries SET status = 'superseded' WHERE id = ?", oldID.Int64); err != nil {
			return "", fmt.Errorf("memory: supersede old: %w", err)
		}
	}
	var supersedes sql.NullInt64
	if oldID.Valid {
		supersedes = oldID
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO entries (key, value, kind, tier, status, writer, session_id, tainted, created_at, expires_at, anchors, note, supersedes)
		VALUES (?, ?, ?, ?, 'active', ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Key, e.Value, e.Kind, e.Tier, e.Writer, e.SessionID, e.Tainted, e.CreatedAt, e.ExpiresAt,
		anchorsToJSON(e.Anchors), e.Note, supersedes)
	if err != nil {
		return "", fmt.Errorf("memory: insert migrated: %w", err)
	}
	newID, _ := res.LastInsertId()
	if oldID.Valid {
		if _, err := tx.ExecContext(ctx, "UPDATE entries SET superseded_by = ? WHERE id = ?", newID, oldID.Int64); err != nil {
			return "", fmt.Errorf("memory: set superseded_by: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("memory: commit: %w", err)
	}
	return MigratedCopied, nil
}
