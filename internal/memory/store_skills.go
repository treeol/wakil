package memory

// store_skills.go holds the Store methods added for the skills capability.
// They live in the memory package (same Store engine, same SQLite schema,
// same FTS5 index and supersedes-history machinery) but are separated into
// this file to make clear that they serve a different product surface
// (global user-authored reference docs) than workspace-scoped memory.

import (
	"context"
	"database/sql"
	"fmt"
)

// PutSkillActive writes an immediately-active DURABLE entry, superseding any
// existing active entry for the same key. It differs from PutActive in two
// ways, both deliberate:
//
//  1. No 64 KiB value cap. Skills are reference docs (templates, runbooks,
//     specs) — larger than remembered facts. The 256 KiB skill cap is
//     enforced by the caller (skillsProfile), so this method intentionally
//     skips validateValue. The store's own validateValue / maxValueLen
//     invariant for memory entries is untouched.
//  2. No anchors, no expiry. Global skills have no stable workspace root to
//     anchor against, and they never expire (durable, no TTL).
//
// The kind is set by the caller (e.g. "skill"). The supersede ordering
// discipline matches PutActive: supersede the old active entry FIRST, then
// INSERT, then set superseded_by — the partial unique index
// (idx_one_active_per_key) aborts the transaction if two active rows for the
// same key ever coexist.
func (s *Store) PutSkillActive(ctx context.Context, key, value, kind, writer, sessionID string, tainted int, note string) (*Entry, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	// NOTE: validateValue (64 KiB cap) deliberately NOT called — see doc comment.
	// Non-emptiness IS enforced here at the store boundary so the invariant
	// holds even if a future caller bypasses the skillsProfile wrapper. The
	// 256 KiB ceiling stays wrapper-level (store_skills policy), but an empty
	// skill is meaningless in all cases.
	if value == "" {
		return nil, fmt.Errorf("memory: skill value is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("memory: skill put: begin tx: %w", err)
	}
	defer tx.Rollback()

	// Find existing active entry for this key.
	var oldID sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		"SELECT id FROM entries WHERE key = ? AND status = 'active'", key).Scan(&oldID); err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("memory: skill put: find active: %w", err)
	}

	// Supersede the old active entry first (partial unique index safety).
	if oldID.Valid {
		if _, err := tx.ExecContext(ctx,
			"UPDATE entries SET status = 'superseded' WHERE id = ?",
			oldID.Int64); err != nil {
			return nil, fmt.Errorf("memory: skill put: supersede old: %w", err)
		}
	}

	// Insert the new active entry. anchors is always empty, expires_at NULL.
	var supersedes sql.NullInt64
	if oldID.Valid {
		supersedes = oldID
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO entries (key, value, kind, tier, status, writer, session_id, tainted, created_at, expires_at, anchors, note, supersedes)
		VALUES (?, ?, ?, 'durable', 'active', ?, ?, ?, ?, NULL, '', ?, ?)`,
		key, value, kind, writer, sessionID, tainted, now, note, supersedes)
	if err != nil {
		return nil, fmt.Errorf("memory: skill put: insert: %w", err)
	}
	newID, _ := result.LastInsertId()

	// Set superseded_by on the old entry now that we have the new ID.
	if oldID.Valid {
		if _, err := tx.ExecContext(ctx,
			"UPDATE entries SET superseded_by = ? WHERE id = ?",
			newID, oldID.Int64); err != nil {
			return nil, fmt.Errorf("memory: skill put: set superseded_by: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("memory: skill put: commit: %w", err)
	}

	return &Entry{
		ID:        newID,
		Key:       key,
		Value:     value,
		Kind:      kind,
		Tier:      TierDurable,
		Status:    StatusActive,
		Writer:    writer,
		SessionID: sessionID,
		Tainted:   tainted,
		CreatedAt: now,
		Note:      note,
		Supersedes: func() *int64 {
			if oldID.Valid {
				return &oldID.Int64
			}
			return nil
		}(),
	}, nil
}

// HistoryForKey returns the full version chain for key — every entry with
// that key regardless of status — ordered newest first. This includes the
// active version, all superseded versions, tombstoned (forgotten) versions,
// and any proposed/rejected rows. Used by skill_history to render the
// complete provenance trail.
//
// Unlike Get (which returns only the active entry) or renderSupersedesHistory
// (which follows a single supersedes hop), this is a first-class history
// query: one SQL statement, no chain-walking recursion, no dangling-reference
// handling needed because we select by key rather than following IDs.
func (s *Store) HistoryForKey(ctx context.Context, key string) ([]*Entry, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx,
		selectEntrySQL+` WHERE key = ? ORDER BY created_at DESC, id DESC`, key)
	if err != nil {
		return nil, fmt.Errorf("memory: skill history: %w", err)
	}
	defer rows.Close()

	var entries []*Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}
