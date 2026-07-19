package memory

// store_skills.go holds the Store methods added for the skills capability.
// They live in the memory package (same Store engine, same SQLite schema,
// same FTS5 index and supersedes-history machinery) but are separated into
// this file to make clear that they serve a different product surface
// (global user-authored reference docs) than workspace-scoped memory.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// skillStoreValueMaxBytes is the per-skill content cap enforced at the STORE
// boundary by PutSkillActive. It matches the agent-side skillsProfile cap
// (256 KiB) so the ceiling holds even if a caller bypasses the wrapper. The
// memory store's own 64 KiB maxValueLen invariant is untouched — this is a
// skill-specific ceiling for the global reference-doc store.
const skillStoreValueMaxBytes = 256 * 1024

// ErrSkillExists is returned by PutSkillActive with expectExists=false when an
// active skill already exists for the key (create-only violated).
var ErrSkillExists = errors.New("memory: skill already exists")

// ErrSkillNotFound is returned by PutSkillActive with expectExists=true when no
// active skill exists for the key (update-requires-existing violated), and by
// ForgetSkill when the expected active row is gone.
var ErrSkillNotFound = errors.New("memory: skill not found")

// PutSkillActive writes an immediately-active DURABLE entry, enforcing the
// create/update invariant ATOMICALLY inside the transaction.
//
// expectExists selects the mode:
//   - false (create): the transaction FAILS with ErrSkillExists if an active
//     row already exists for the key. This makes save_skill's create-only
//     guarantee race-safe — a concurrent save in another process cannot turn
//     this write into a silent supersede, because the existence check happens
//     in the same transaction as the insert, not in a handler pre-check.
//   - true (update): the transaction FAILS with ErrSkillNotFound if no active
//     row exists. This makes update_skill's requires-existing guarantee
//     race-safe — a concurrent forget cannot turn this write into a fresh
//     create with a broken supersedes chain.
//
// It differs from PutActive in these ways, all deliberate:
//
//  1. No 64 KiB value cap, but a 256 KiB skill cap enforced HERE at the store
//     boundary (skillValueMaxBytes below) so the ceiling holds even if a future
//     caller bypasses the agent-side skillsProfile wrapper. The memory store's
//     own 64 KiB validateValue invariant is untouched.
//  2. No anchors, no expiry. Global skills have no stable workspace root to
//     anchor against, and they never expire (durable, no TTL).
//
// The supersede ordering discipline matches PutActive: supersede the old
// active entry FIRST, then INSERT, then set superseded_by — the partial unique
// index (idx_one_active_per_key) aborts the transaction if two active rows for
// the same key ever coexist.
func (s *Store) PutSkillActive(ctx context.Context, key, value, kind, writer, sessionID string, tainted int, expectExists bool, note string) (*Entry, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	// Enforce the skill value cap at the store boundary (see doc comment).
	if value == "" {
		return nil, fmt.Errorf("memory: skill value is required")
	}
	if len(value) > skillStoreValueMaxBytes {
		return nil, fmt.Errorf("memory: skill value exceeds %d bytes (got %d)", skillStoreValueMaxBytes, len(value))
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

	// Enforce the create/update invariant INSIDE the transaction — this is the
	// compare-and-swap that makes the handler's pre-check race-safe.
	if !expectExists && oldID.Valid {
		return nil, ErrSkillExists
	}
	if expectExists && !oldID.Valid {
		return nil, ErrSkillNotFound
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
