// Package memory implements the durable, trusted, host-side memory store (Tier 2).
//
// The store lives on the HOST at <wakil-data>/memory/<workspace-key>/memory.db,
// is never mounted into the sandbox, and is only writable through Wakil
// host-process code paths. It uses SQLite via modernc.org/sqlite (pure Go,
// no cgo) in WAL mode with a single-writer discipline: an app-level mutex
// serializes all writes within one Store instance, and SetMaxOpenConns(1)
// ensures a single SQLite connection. The partial unique index
// (idx_one_active_per_key) provides the database-level invariant; the mutex
// prevents constraint violations from concurrent writers.
//
// Two lifetime tiers:
//   - mid: TTL 1h–7d, direct active writes, auto-expires.
//   - durable: no TTL, writes land as PROPOSED; promotion to ACTIVE requires
//     the main agent via memory_promote, or the user.
//
// One ACTIVE entry per key is enforced by a partial unique index. Supersede
// history is kept (old entries → status=superseded), never overwritten.
// Hard-deletion only happens for expired entries past a 30-day auditability
// window, during the session-start sweep.
//
// AUTOINCREMENT is used for the primary key (not plain INTEGER PRIMARY KEY)
// because the 30-day hard-delete means rowids would get reused under plain
// INTEGER PRIMARY KEY, and reused rowids would silently corrupt supersedes
// chains pointing at dead entries. AUTOINCREMENT guarantees monotonic IDs
// that are never reused, so a dangling supersedes reference always points
// at a genuinely deleted entry, never at a recycled unrelated row.
package memory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// ─── Constants ─────────────────────────────────────────────────────────────

const (
	TierMid     = "mid"
	TierDurable = "durable"
)

const (
	StatusActive     = "active"
	StatusProposed   = "proposed"
	StatusRejected   = "rejected"
	StatusSuperseded = "superseded"
	StatusExpired    = "expired"
)

// Tainted tri-state: 0=false, 1=true, 2=unknown.
const (
	TaintFalse   = 0
	TaintTrue    = 1
	TaintUnknown = 2
)

const (
	searchCap       = 20
	listCap         = 200
	auditWindowDays = 30
	maxKeyLen       = 256
	maxValueLen     = 64 * 1024
)

// Sentinel errors.
var (
	ErrNotFound    = errors.New("memory: not found")
	ErrNotProposed = errors.New("memory: entry is not proposed")
	ErrEmptyKey    = errors.New("memory: key is required")
	ErrKeyTooLong  = errors.New("memory: key exceeds 256 bytes")
	ErrValueTooBig = errors.New("memory: value exceeds 64 KiB")
)

// ─── Types ─────────────────────────────────────────────────────────────────

// Anchor is a file-anchor recorded at write time: the workspace-relative path
// and the SHA-256 hash of the file content at that moment. At read time, the
// current hash is recomputed; a mismatch or missing file marks the entry STALE.
type Anchor struct {
	Path string `json:"path"`
	Hash string `json:"hash"`
}

// Entry is a single memory store entry.
type Entry struct {
	ID           int64
	Key          string
	Value        string
	Kind         string
	Tier         string
	Status       string
	Writer       string
	SessionID    string
	Tainted      int
	CreatedAt    int64
	ExpiresAt    *int64
	Anchors      []Anchor
	Note         string
	Supersedes   *int64
	SupersededBy *int64
	PromotedBy   string

	// StaleAnchors and TotalAnchors are computed at read time.
	StaleAnchors int
	TotalAnchors int
}

// Stats is the compact digest used for session-start context injection.
type Stats struct {
	ActiveDurable   int
	ActiveMid       int
	PendingProposed int
	RecentKeys      []string // top-N durable keys by recency
}

// Store is the durable host-side memory store.
type Store struct {
	db            *sql.DB
	mu            sync.Mutex // single-writer discipline
	workspaceRoot string
	nowFunc       func() int64 // epoch ms; defaults to time.Now().UnixMilli()
}

// ─── Open / Close ──────────────────────────────────────────────────────────

// Open opens (or creates) the memory store at dbPath. The directory is created
// with 0o700 if it does not exist. workspaceRoot is used for anchor staleness
// checks at read time. Returns an error if the database cannot be opened or
// the schema cannot be created.
func Open(dbPath, workspaceRoot string) (*Store, error) {
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("memory: create db directory: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("memory: open db: %w", err)
	}
	// Single connection: SQLite WAL allows concurrent readers, but
	// modernc.org/sqlite's connection pool can produce "database is locked"
	// under contention. One connection serializes everything safely.
	// Volume is negligible — this is not a hot path.
	db.SetMaxOpenConns(1)

	for _, pragma := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA busy_timeout = 5000",
		// Foreign keys OFF (default): the 30-day hard-delete intentionally
		// leaves dangling supersedes references. The render path handles
		// them gracefully ("history unavailable").
		"PRAGMA foreign_keys = OFF",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("memory: set pragma %q: %w", pragma, err)
		}
	}

	if err := createSchema(db); err != nil {
		db.Close()
		return nil, err
	}

	return &Store{
		db:            db,
		workspaceRoot: workspaceRoot,
		nowFunc:       func() int64 { return time.Now().UnixMilli() },
	}, nil
}

// Close closes the underlying database handle.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// now returns the current time in epoch milliseconds.
func (s *Store) now() int64 {
	if s.nowFunc != nil {
		return s.nowFunc()
	}
	return time.Now().UnixMilli()
}

// ─── Schema ────────────────────────────────────────────────────────────────

var schemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS entries (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		key TEXT NOT NULL,
		value TEXT NOT NULL,
		kind TEXT NOT NULL,
		tier TEXT NOT NULL CHECK(tier IN ('mid','durable')),
		status TEXT NOT NULL CHECK(status IN ('active','proposed','rejected','superseded','expired')),
		writer TEXT NOT NULL,
		session_id TEXT NOT NULL,
		tainted INTEGER NOT NULL DEFAULT 2,
		created_at INTEGER NOT NULL,
		expires_at INTEGER,
		anchors TEXT,
		note TEXT,
		supersedes INTEGER REFERENCES entries(id),
		superseded_by INTEGER REFERENCES entries(id),
		promoted_by TEXT
	)`,
	// Partial unique index: exactly one ACTIVE entry per key. Allows unlimited
	// PROPOSED, SUPERSEDED, REJECTED, and EXPIRED entries for the same key.
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_one_active_per_key ON entries(key) WHERE status = 'active'`,
	`CREATE INDEX IF NOT EXISTS idx_tier_status ON entries(tier, status)`,
	`CREATE INDEX IF NOT EXISTS idx_expires_at ON entries(expires_at)`,
	`CREATE INDEX IF NOT EXISTS idx_key ON entries(key)`,
	// FTS5 virtual table over (key, value) for memory_search, kept in sync
	// via triggers. External content table — FTS5 reads from entries directly.
	`CREATE VIRTUAL TABLE IF NOT EXISTS entries_fts USING fts5(key, value, content='entries', content_rowid='id')`,
	`CREATE TRIGGER IF NOT EXISTS entries_fts_ai AFTER INSERT ON entries BEGIN
		INSERT INTO entries_fts(rowid, key, value) VALUES (new.id, new.key, new.value);
	END`,
	`CREATE TRIGGER IF NOT EXISTS entries_fts_ad AFTER DELETE ON entries BEGIN
		INSERT INTO entries_fts(entries_fts, rowid, key, value) VALUES('delete', old.id, old.key, old.value);
	END`,
	`CREATE TRIGGER IF NOT EXISTS entries_fts_au AFTER UPDATE ON entries BEGIN
		INSERT INTO entries_fts(entries_fts, rowid, key, value) VALUES('delete', old.id, old.key, old.value);
		INSERT INTO entries_fts(rowid, key, value) VALUES (new.id, new.key, new.value);
	END`,
	`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT)`,
	`INSERT OR IGNORE INTO meta (key, value) VALUES ('schema_version', '1')`,
}

func createSchema(db *sql.DB) error {
	for _, stmt := range schemaStatements {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("memory: create schema: %w\nstatement: %s", err, stmt)
		}
	}
	return nil
}

// ─── Validation ────────────────────────────────────────────────────────────

func validateKey(key string) error {
	if key == "" {
		return ErrEmptyKey
	}
	if len(key) > maxKeyLen {
		return ErrKeyTooLong
	}
	return nil
}

func validateValue(value string) error {
	if len(value) > maxValueLen {
		return ErrValueTooBig
	}
	return nil
}

// ─── Anchor hashing ────────────────────────────────────────────────────────

// computeAnchorHashes computes SHA-256 hashes for the given workspace-relative
// file paths at write time. Missing files are recorded with an empty hash
// (which will be stale at read time). Paths that escape the workspace root
// via ".." traversal are treated as missing (empty hash) — the store never
// reads files outside the workspace.
func computeAnchorHashes(workspaceRoot string, paths []string) []Anchor {
	if len(paths) == 0 {
		return nil
	}
	anchors := make([]Anchor, 0, len(paths))
	for _, p := range paths {
		fullPath := filepath.Join(workspaceRoot, p)
		// Prevent path traversal: the resolved path must stay within
		// the workspace root. This is defense-in-depth — the tool layer
		// should also validate, but the store never trusts paths.
		if !isWithinWorkspace(workspaceRoot, fullPath) {
			anchors = append(anchors, Anchor{Path: p, Hash: ""})
			continue
		}
		data, err := os.ReadFile(fullPath)
		if err != nil {
			anchors = append(anchors, Anchor{Path: p, Hash: ""})
			continue
		}
		sum := sha256.Sum256(data)
		anchors = append(anchors, Anchor{Path: p, Hash: hex.EncodeToString(sum[:])})
	}
	return anchors
}

// isWithinWorkspace reports whether target is within root after symlink
// evaluation. Falls back to filepath.Rel comparison if EvalSymlinks fails
// (e.g. file does not exist yet).
func isWithinWorkspace(root, target string) bool {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return false
	}
	// Try symlink resolution for a stronger check.
	if realRoot, err := filepath.EvalSymlinks(absRoot); err == nil {
		absRoot = realRoot
	}
	if realTarget, err := filepath.EvalSymlinks(absTarget); err == nil {
		absTarget = realTarget
	}
	rel, err := filepath.Rel(absRoot, absTarget)
	if err != nil {
		return false
	}
	// If the relative path starts with "..", it's outside the root.
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// checkAnchorStaleness recomputes hashes for the stored anchors and returns
// the count of stale anchors (mismatched hash or missing file) and the total.
func checkAnchorStaleness(workspaceRoot string, anchors []Anchor) (stale, total int) {
	total = len(anchors)
	for _, a := range anchors {
		fullPath := filepath.Join(workspaceRoot, a.Path)
		if !isWithinWorkspace(workspaceRoot, fullPath) {
			stale++
			continue
		}
		data, err := os.ReadFile(fullPath)
		if err != nil {
			stale++
			continue
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != a.Hash {
			stale++
		}
	}
	return stale, total
}

// anchorsToJSON serializes anchors to a JSON string. Returns "" for nil/empty.
func anchorsToJSON(anchors []Anchor) string {
	if len(anchors) == 0 {
		return ""
	}
	b, err := json.Marshal(anchors)
	if err != nil {
		return ""
	}
	return string(b)
}

// parseAnchors deserializes anchors from a JSON string. Returns nil for empty.
func parseAnchors(s string) []Anchor {
	if s == "" {
		return nil
	}
	var anchors []Anchor
	if err := json.Unmarshal([]byte(s), &anchors); err != nil {
		return nil
	}
	return anchors
}

// ─── Write operations (mutex-protected) ────────────────────────────────────

// PutActive writes an active entry, superseding any existing active entry for
// the same key. Used for mid-tier (TTL) writes and promoted durable entries.
// anchorPaths are workspace-relative file paths whose hashes are computed at
// write time. The tier should be TierMid or TierDurable.
func (s *Store) PutActive(ctx context.Context, key, value, kind, tier, writer, sessionID string, tainted int, expiresAt *int64, anchorPaths []string, note string) (*Entry, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	if err := validateValue(value); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	anchors := computeAnchorHashes(s.workspaceRoot, anchorPaths)
	anchorsJSON := anchorsToJSON(anchors)
	now := s.now()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("memory: begin tx: %w", err)
	}
	defer tx.Rollback()

	// Find existing active entry for this key.
	var oldID sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		"SELECT id FROM entries WHERE key = ? AND status = 'active'", key).Scan(&oldID); err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("memory: find active: %w", err)
	}
	// The partial unique index (idx_one_active_per_key) prevents two active
	// entries for the same key — if we INSERT before superseding, the index
	// violation aborts the transaction. superseded_by is set after the INSERT
	// (we need the new ID). The old entry is briefly superseded with a NULL
	// superseded_by, then updated — this is safe within the transaction.
	if oldID.Valid {
		if _, err := tx.ExecContext(ctx,
			"UPDATE entries SET status = 'superseded' WHERE id = ?",
			oldID.Int64); err != nil {
			return nil, fmt.Errorf("memory: supersede old: %w", err)
		}
	}

	// Insert the new active entry.
	var supersedes sql.NullInt64
	if oldID.Valid {
		supersedes = oldID
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO entries (key, value, kind, tier, status, writer, session_id, tainted, created_at, expires_at, anchors, note, supersedes)
		VALUES (?, ?, ?, ?, 'active', ?, ?, ?, ?, ?, ?, ?, ?)`,
		key, value, kind, tier, writer, sessionID, tainted, now, expiresAt, anchorsJSON, note, supersedes)
	if err != nil {
		return nil, fmt.Errorf("memory: insert active: %w", err)
	}
	newID, _ := result.LastInsertId()

	// Set superseded_by on the old entry now that we have the new ID.
	if oldID.Valid {
		if _, err := tx.ExecContext(ctx,
			"UPDATE entries SET superseded_by = ? WHERE id = ?",
			newID, oldID.Int64); err != nil {
			return nil, fmt.Errorf("memory: set superseded_by: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("memory: commit: %w", err)
	}

	return &Entry{
		ID:        newID,
		Key:       key,
		Value:     value,
		Kind:      kind,
		Tier:      tier,
		Status:    StatusActive,
		Writer:    writer,
		SessionID: sessionID,
		Tainted:   tainted,
		CreatedAt: now,
		ExpiresAt: expiresAt,
		Anchors:   anchors,
		Note:      note,
		Supersedes: func() *int64 {
			if oldID.Valid {
				return &oldID.Int64
			}
			return nil
		}(),
	}, nil
}

// PutProposed writes a proposed entry. Multiple proposed entries per key are
// allowed. Used for durable-tier writes (no TTL).
func (s *Store) PutProposed(ctx context.Context, key, value, kind, writer, sessionID string, tainted int, anchorPaths []string, note string) (*Entry, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	if err := validateValue(value); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	anchors := computeAnchorHashes(s.workspaceRoot, anchorPaths)
	anchorsJSON := anchorsToJSON(anchors)
	now := s.now()

	result, err := s.db.ExecContext(ctx, `
		INSERT INTO entries (key, value, kind, tier, status, writer, session_id, tainted, created_at, expires_at, anchors, note)
		VALUES (?, ?, ?, 'durable', 'proposed', ?, ?, ?, ?, NULL, ?, ?)`,
		key, value, kind, writer, sessionID, tainted, now, anchorsJSON, note)
	if err != nil {
		return nil, fmt.Errorf("memory: insert proposed: %w", err)
	}
	id, _ := result.LastInsertId()

	return &Entry{
		ID:        id,
		Key:       key,
		Value:     value,
		Kind:      kind,
		Tier:      TierDurable,
		Status:    StatusProposed,
		Writer:    writer,
		SessionID: sessionID,
		Tainted:   tainted,
		CreatedAt: now,
		Anchors:   anchors,
		Note:      note,
	}, nil
}

// Promote promotes a proposed entry to active. If editedValue is non-nil,
// a new active entry is written with the edited value; the proposed entry
// is superseded and its original value preserved. If editedValue is nil,
// the proposed entry is promoted in place. Any existing active entry for
// the same key is superseded. promotedBy records who performed the promotion.
func (s *Store) Promote(ctx context.Context, id int64, editedValue *string, promotedBy string) (*Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("memory: begin tx: %w", err)
	}
	defer tx.Rollback()

	// Load the proposed entry.
	e, err := scanEntryByIDTx(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if e.Status != StatusProposed {
		return nil, fmt.Errorf("%w (status=%s)", ErrNotProposed, e.Status)
	}

	now := s.now()

	// Find existing active entry for this key.
	var oldID sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		"SELECT id FROM entries WHERE key = ? AND status = 'active'", e.Key).Scan(&oldID); err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("memory: find active: %w", err)
	}

	if editedValue != nil {
		if err := validateValue(*editedValue); err != nil {
			return nil, err
		}
		// Supersede the old active entry FIRST, before inserting the new one.
		// Same ordering discipline as PutActive: the partial unique index
		// (idx_one_active_per_key) prevents two active entries for the same
		// key — if we INSERT before superseding, the index violation aborts
		// the transaction.
		if oldID.Valid {
			if _, err := tx.ExecContext(ctx,
				"UPDATE entries SET status = 'superseded' WHERE id = ?",
				oldID.Int64); err != nil {
				return nil, fmt.Errorf("memory: supersede old active: %w", err)
			}
		}
		// Write a new active entry with the edited value.
		// supersedes points to the proposed entry (the direct cause of promotion).
		result, err := tx.ExecContext(ctx, `
			INSERT INTO entries (key, value, kind, tier, status, writer, session_id, tainted, created_at, expires_at, anchors, note, supersedes, promoted_by)
			VALUES (?, ?, ?, 'durable', 'active', ?, ?, ?, ?, NULL, ?, ?, ?, ?)`,
			e.Key, *editedValue, e.Kind, e.Writer, e.SessionID, e.Tainted, now,
			anchorsToJSON(e.Anchors), e.Note, sql.NullInt64{Int64: id, Valid: true}, promotedBy)
		if err != nil {
			return nil, fmt.Errorf("memory: insert promoted: %w", err)
		}
		newID, _ := result.LastInsertId()

		// Set superseded_by on the old active entry now that we have the new ID.
		if oldID.Valid {
			if _, err := tx.ExecContext(ctx,
				"UPDATE entries SET superseded_by = ? WHERE id = ?",
				newID, oldID.Int64); err != nil {
				return nil, fmt.Errorf("memory: set superseded_by on old active: %w", err)
			}
		}
		// Supersede the proposed entry.
		if _, err := tx.ExecContext(ctx,
			"UPDATE entries SET status = 'superseded', superseded_by = ? WHERE id = ?",
			newID, id); err != nil {
			return nil, fmt.Errorf("memory: supersede proposed: %w", err)
		}

		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("memory: commit: %w", err)
		}

		return &Entry{
			ID:        newID,
			Key:       e.Key,
			Value:     *editedValue,
			Kind:      e.Kind,
			Tier:      TierDurable,
			Status:    StatusActive,
			Writer:    e.Writer,
			SessionID: e.SessionID,
			Tainted:   e.Tainted,
			CreatedAt: now,
			Anchors:    e.Anchors,
			Note:       e.Note,
			Supersedes: &id,
			PromotedBy: promotedBy,
		}, nil
	}

	// Promote in place: supersede old active FIRST (to satisfy the partial
	// unique index), then flip the proposed entry to active.
	if oldID.Valid {
		if _, err := tx.ExecContext(ctx,
			"UPDATE entries SET status = 'superseded', superseded_by = ? WHERE id = ?",
			id, oldID.Int64); err != nil {
			return nil, fmt.Errorf("memory: supersede old active: %w", err)
		}
	}
	var supersedesVal sql.NullInt64
	if oldID.Valid {
		supersedesVal = oldID
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE entries SET status = 'active', promoted_by = ?, supersedes = ? WHERE id = ?",
		promotedBy, supersedesVal, id); err != nil {
		return nil, fmt.Errorf("memory: promote in place: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("memory: commit: %w", err)
	}

	e.Status = StatusActive
	e.PromotedBy = promotedBy
	if oldID.Valid {
		e.Supersedes = &oldID.Int64
	}
	return e, nil
}

// Reject rejects a proposed entry. reason is stored in the note column.
func (s *Store) Reject(ctx context.Context, id int64, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var status string
	err := s.db.QueryRowContext(ctx, "SELECT status FROM entries WHERE id = ?", id).Scan(&status)
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if status != StatusProposed {
		return fmt.Errorf("%w (status=%s)", ErrNotProposed, status)
	}

	note := reason
	if note == "" {
		note = "rejected"
	}
	_, err = s.db.ExecContext(ctx,
		"UPDATE entries SET status = 'rejected', note = ? WHERE id = ?", note, id)
	return err
}

// Forget supersedes the active entry for key with a tombstone note.
// Nothing is ever hard-deleted by agents.
func (s *Store) Forget(ctx context.Context, key string) error {
	if err := validateKey(key); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var id int64
	err := s.db.QueryRowContext(ctx,
		"SELECT id FROM entries WHERE key = ? AND status = 'active'", key).Scan(&id)
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return err
	}

	tombstone := fmt.Sprintf("forgotten: %s at %s", key, time.UnixMilli(s.now()).UTC().Format(time.RFC3339))
	_, err = s.db.ExecContext(ctx,
		"UPDATE entries SET status = 'superseded', note = ? WHERE id = ?", tombstone, id)
	return err
}

// ─── Read operations ───────────────────────────────────────────────────────

// Get returns the active entry for key, with anchor staleness checked.
// Expired entries (where expires_at < now) are treated as not found.
// Stale entries (anchors mismatched) are still returned, flagged — never
// silently dropped.
func (s *Store) Get(ctx context.Context, key string) (*Entry, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}

	now := s.now()
	row := s.db.QueryRowContext(ctx, selectEntrySQL+`
		WHERE key = ? AND status = 'active' AND (expires_at IS NULL OR expires_at >= ?)`, key, now)

	e, err := scanEntry(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	s.checkStaleness(e)
	return e, nil
}

// GetByID returns the entry with the given ID, regardless of status.
// Used by the tool layer to render proposed entries for promotion review
// and to follow supersedes chains. Returns ErrNotFound for dangling references
// (entries hard-deleted past the 30-day auditability window).
func (s *Store) GetByID(ctx context.Context, id int64) (*Entry, error) {
	row := s.db.QueryRowContext(ctx, selectEntrySQL+` WHERE id = ?`, id)
	e, err := scanEntry(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	s.checkStaleness(e)
	return e, nil
}

// Search searches the FTS5 index for the query. Returns up to searchCap
// entries, ordered by created_at DESC. By default, only active entries are
// returned; includeProposed adds proposed entries. Expired entries are always
// filtered. Stale entries are returned with the flag — never silently dropped.
func (s *Store) Search(ctx context.Context, query, tier string, includeProposed bool) ([]*Entry, error) {
	if query == "" {
		return nil, nil
	}

	now := s.now()
	statuses := []string{StatusActive}
	if includeProposed {
		statuses = append(statuses, StatusProposed)
	}

	placeholders := make([]string, len(statuses))
	args := []interface{}{query}
	for i, st := range statuses {
		placeholders[i] = "?"
		args = append(args, st)
	}
	args = append(args, now)

	q := fmt.Sprintf(`
		SELECT %s FROM entries
		WHERE id IN (SELECT rowid FROM entries_fts WHERE entries_fts MATCH ?)
		AND status IN (%s)
		AND (expires_at IS NULL OR expires_at >= ?)`,
		entryColumns, joinStrings(placeholders, ","))

	if tier != "" {
		q += " AND tier = ?"
		args = append(args, tier)
	}
	q += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, searchCap)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("memory: search: %w", err)
	}
	defer rows.Close()

	var entries []*Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		s.checkStaleness(e)
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// List lists entries by prefix, tier, and status. Returns up to listCap
// entries with full data (including Value).
func (s *Store) List(ctx context.Context, prefix, tier, status string) ([]*Entry, error) {
	if status == "" {
		status = StatusActive
	}

	now := s.now()
	args := []interface{}{}
	q := fmt.Sprintf(`SELECT %s FROM entries WHERE 1=1`, entryColumns)

	if prefix != "" {
		q += " AND key LIKE ? || '%'"
		args = append(args, prefix)
	}
	if tier != "" {
		q += " AND tier = ?"
		args = append(args, tier)
	}
	q += " AND status = ?"
	args = append(args, status)

	// Filter expired entries from active results.
	if status == StatusActive {
		q += " AND (expires_at IS NULL OR expires_at >= ?)"
		args = append(args, now)
	}

	q += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, listCap)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("memory: list: %w", err)
	}
	defer rows.Close()

	var entries []*Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		s.checkStaleness(e)
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// Stats returns a compact digest for session-start context injection.
func (s *Store) Stats(ctx context.Context, recentN int) (*Stats, error) {
	now := s.now()
	stats := &Stats{}

	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entries WHERE tier = 'durable' AND status = 'active' AND (expires_at IS NULL OR expires_at >= ?)`,
		now).Scan(&stats.ActiveDurable); err != nil {
		return nil, err
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entries WHERE tier = 'mid' AND status = 'active' AND (expires_at IS NULL OR expires_at >= ?)`,
		now).Scan(&stats.ActiveMid); err != nil {
		return nil, err
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entries WHERE status = 'proposed'`).Scan(&stats.PendingProposed); err != nil {
		return nil, err
	}

	if recentN > 0 {
		rows, err := s.db.QueryContext(ctx,
			`SELECT key FROM entries WHERE tier = 'durable' AND status = 'active' AND (expires_at IS NULL OR expires_at >= ?) GROUP BY key ORDER BY MAX(created_at) DESC LIMIT ?`,
			now, recentN)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				return nil, err
			}
			stats.RecentKeys = append(stats.RecentKeys, key)
		}
	}

	return stats, nil
}

// ─── Sweep ─────────────────────────────────────────────────────────────────

// Sweep marks expired entries and hard-deletes entries past the 30-day
// auditability window. Should be called at session start.
func (s *Store) Sweep(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	auditCutoff := now - int64(auditWindowDays)*24*60*60*1000

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("memory: sweep begin: %w", err)
	}
	defer tx.Rollback()

	// Mark expired: active entries past their TTL.
	if _, err := tx.ExecContext(ctx,
		`UPDATE entries SET status = 'expired' WHERE status = 'active' AND expires_at IS NOT NULL AND expires_at < ?`,
		now); err != nil {
		return fmt.Errorf("memory: sweep expire: %w", err)
	}

	// Hard-delete expired entries past the 30-day auditability window.
	// This intentionally leaves dangling supersedes references — the render
	// path handles them gracefully ("history unavailable").
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM entries WHERE status = 'expired' AND expires_at < ?`,
		auditCutoff); err != nil {
		return fmt.Errorf("memory: sweep hard-delete: %w", err)
	}

	return tx.Commit()
}

// ─── Scanning helpers ──────────────────────────────────────────────────────

const entryColumns = `id, key, value, kind, tier, status, writer, session_id, tainted, created_at, expires_at, anchors, note, supersedes, superseded_by, promoted_by`

var selectEntrySQL = `SELECT ` + entryColumns + ` FROM entries`

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...interface{}) error
}

func scanEntry(row scanner) (*Entry, error) {
	var (
		e            Entry
		expiresAt    sql.NullInt64
		anchorsStr   sql.NullString
		note         sql.NullString
		supersedes   sql.NullInt64
		supersededBy sql.NullInt64
		promotedBy   sql.NullString
	)
	err := row.Scan(
		&e.ID, &e.Key, &e.Value, &e.Kind, &e.Tier, &e.Status,
		&e.Writer, &e.SessionID, &e.Tainted, &e.CreatedAt,
		&expiresAt, &anchorsStr, &note, &supersedes, &supersededBy, &promotedBy,
	)
	if err != nil {
		return nil, err
	}
	if expiresAt.Valid {
		e.ExpiresAt = &expiresAt.Int64
	}
	e.Anchors = parseAnchors(anchorsStr.String)
	e.Note = note.String
	if supersedes.Valid {
		e.Supersedes = &supersedes.Int64
	}
	if supersededBy.Valid {
		e.SupersededBy = &supersededBy.Int64
	}
	e.PromotedBy = promotedBy.String
	return &e, nil
}

// scanEntryByIDTx loads an entry by ID within a transaction.
func scanEntryByIDTx(ctx context.Context, tx *sql.Tx, id int64) (*Entry, error) {
	row := tx.QueryRowContext(ctx, selectEntrySQL+` WHERE id = ?`, id)
	e, err := scanEntry(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return e, err
}

// checkStaleness populates StaleAnchors and TotalAnchors on the entry.
func (s *Store) checkStaleness(e *Entry) {
	if len(e.Anchors) == 0 {
		return
	}
	e.StaleAnchors, e.TotalAnchors = checkAnchorStaleness(s.workspaceRoot, e.Anchors)
}

// joinStrings joins strings with a separator.
func joinStrings(ss []string, sep string) string {
	if len(ss) == 0 {
		return ""
	}
	result := ss[0]
	for i := 1; i < len(ss); i++ {
		result += sep + ss[i]
	}
	return result
}
