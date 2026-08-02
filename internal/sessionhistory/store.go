// Package sessionhistory implements a workspace-scoped, searchable index of
// prior session transcripts. It is a derived, disposable index: the source of
// truth is the on-disk session JSON files (written by internal/agent via
// WriteSession). The index exists solely to make old sessions full-text
// searchable from a new session — it is not the journal of record, and can be
// rebuilt from the session files at any time.
//
// Design notes:
//   - One row per session in `sessions` (metadata + optional per-session
//     summary), plus one row per indexed turn in `turns`, with an FTS5
//     external-content index over turn text.
//   - Only user turns and assistant TEXT content are indexed. Raw tool output
//     and assistant tool-call arguments are deliberately excluded (noisy, and a
//     primary secret-bearing surface). This is a security boundary, not a
//     performance choice.
//   - A session is replaced WHOLE-AND-ATOMICALLY on change. Turn ordinals are
//     unstable (compaction folds turns into a summary, hard-max drops turns,
//     /resume reopens and appends), so we never do incremental per-turn appends.
//   - Workspace scoping is fail-closed: an empty workspace returns no results
//     and ingests nothing. The caller must never rely on ListSessionsScoped,
//     which returns EVERYTHING for an empty workspace.
//   - The index is host-side and sandbox-excluded, mirroring the durable memory
//     store. Single-writer discipline via an app-level mutex + one SQLite
//     connection, same as memory.Store.
package sessionhistory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	_ "modernc.org/sqlite"
)

// Turn is one indexed turn within a session.
type Turn struct {
	Ordinal int    // 0-based ordinal of this user turn within the session
	Role    string // "user" or "assistant"
	Text    string // the text content (user text or assistant prose); never raw tool output
}

// IndexInput is the data the agent package hands to the store to index one
// session. It is built from a persisted Session by walking Conv.
type IndexInput struct {
	ChatID           string
	Workspace        string
	Created          time.Time
	Updated          time.Time
	Label            string
	Turns            []Turn
	Summary          string // generated or harvested per-session summary; may be empty
	SummaryGenerated bool   // true if Summary came from a model pass, false if harvested/absent
	Tainted          bool   // session-level taint (true if any source content is untrusted)
	SourceHash       string // content hash of the source transcript for change detection
	SizeBytes        int64  // size of the source file, for change detection
}

// Result is a single matched session from a search.
type Result struct {
	ChatID    string
	Workspace string
	Created   time.Time
	Updated   time.Time
	Label     string
	Summary   string
	Tainted   bool
	Turns     []Turn // matched turns (subset, capped), each a citation source
}

// IndexedMeta is reconciliation metadata for one indexed session.
type IndexedMeta struct {
	ChatID           string
	Updated          time.Time
	SourceHash       string
	SizeBytes        int64
	SummaryGenerated bool
}

// Store is the session-history index for one workspace.
type Store struct {
	db *sql.DB
	mu sync.Mutex // single-writer discipline (mirrors memory.Store)
}

const (
	maxQueryTokens  = 12  // cap on query tokens to bound cost
	resultLimit     = 8   // max sessions returned
	turnsPerSession = 4   // max turns shown per matched session
	searchCapRaw    = 100 // raw FTS hit cap before grouping in Go
)

// ErrNotFound is returned when no rows match.
var ErrNotFound = errors.New("sessionhistory: not found")

// ─── Open / Close ──────────────────────────────────────────────────────────

// Open opens (or creates) the session-history index at dbPath. The directory is
// created with 0o700 if it does not exist. Returns an error if the database
// cannot be opened or the schema cannot be created.
func Open(dbPath string) (*Store, error) {
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("sessionhistory: create db directory: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("sessionhistory: open db: %w", err)
	}
	// Single connection: serialize all reads/writes (WAL + one conn is the
	// memory store's proven low-volume pattern). Session-history queries are
	// on an explicit user command, not a hot path, so contention is a non-issue.
	db.SetMaxOpenConns(1)

	for _, pragma := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA busy_timeout = 5000",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("sessionhistory: set pragma %q: %w", pragma, err)
		}
	}

	if err := createSchema(db); err != nil {
		db.Close()
		return nil, err
	}

	return &Store{db: db}, nil
}

// Close closes the underlying database handle.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// ─── Schema ────────────────────────────────────────────────────────────────

var schemaStatements = []string{
	// One row per indexed session. `updated`, `source_hash`, `size_bytes` form
	// the manifest/watermark used by the backfill/reconciliation path to detect
	// unchanged sessions without re-parsing them.
	`CREATE TABLE IF NOT EXISTS sessions (
		chat_id TEXT PRIMARY KEY,
		workspace TEXT NOT NULL,
		created INTEGER NOT NULL,
		updated INTEGER NOT NULL,
		label TEXT NOT NULL DEFAULT '',
		summary TEXT NOT NULL DEFAULT '',
		summary_generated INTEGER NOT NULL DEFAULT 0,
		tainted INTEGER NOT NULL DEFAULT 1,
		source_hash TEXT NOT NULL DEFAULT '',
		size_bytes INTEGER NOT NULL DEFAULT 0
	)`,
	// One row per indexed turn. ordinal is the 0-based user-turn ordinal.
	`CREATE TABLE IF NOT EXISTS turns (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		chat_id TEXT NOT NULL,
		ordinal INTEGER NOT NULL,
		role TEXT NOT NULL,
		text TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_turns_chat ON turns(chat_id)`,
	`CREATE INDEX IF NOT EXISTS idx_sessions_workspace ON sessions(workspace)`,
	// FTS5 external-content index over turn text, kept in sync via triggers.
	`CREATE VIRTUAL TABLE IF NOT EXISTS turns_fts USING fts5(
		text, content='turns', content_rowid='id',
		tokenize='unicode61')`,
	`CREATE TRIGGER IF NOT EXISTS turns_fts_ai AFTER INSERT ON turns BEGIN
		INSERT INTO turns_fts(rowid, text) VALUES (new.id, new.text);
	END`,
	`CREATE TRIGGER IF NOT EXISTS turns_fts_ad AFTER DELETE ON turns BEGIN
		INSERT INTO turns_fts(turns_fts, rowid, text) VALUES('delete', old.id, old.text);
	END`,
	`CREATE TRIGGER IF NOT EXISTS turns_fts_au AFTER UPDATE ON turns BEGIN
		INSERT INTO turns_fts(turns_fts, rowid, text) VALUES('delete', old.id, old.text);
		INSERT INTO turns_fts(rowid, text) VALUES (new.id, new.text);
	END`,
	// FTS5 external-content index over session summaries, so recall can match
	// the distilled summary as well as raw turn text (the summary often carries
	// the recall signal for long sessions after compaction). Rowid = chat_id.
	`CREATE VIRTUAL TABLE IF NOT EXISTS sessions_fts USING fts5(
		summary, content='sessions', content_rowid='rowid',
		tokenize='unicode61')`,
	`CREATE TRIGGER IF NOT EXISTS sessions_fts_ai AFTER INSERT ON sessions BEGIN
		INSERT INTO sessions_fts(rowid, summary) VALUES (new.rowid, new.summary);
	END`,
	`CREATE TRIGGER IF NOT EXISTS sessions_fts_ad AFTER DELETE ON sessions BEGIN
		INSERT INTO sessions_fts(sessions_fts, rowid, summary) VALUES('delete', old.rowid, old.summary);
	END`,
	`CREATE TRIGGER IF NOT EXISTS sessions_fts_au AFTER UPDATE OF summary ON sessions BEGIN
		INSERT INTO sessions_fts(sessions_fts, rowid, summary) VALUES('delete', old.rowid, old.summary);
		INSERT INTO sessions_fts(rowid, summary) VALUES (new.rowid, new.summary);
	END`,
}

func createSchema(db *sql.DB) error {
	for _, stmt := range schemaStatements {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("sessionhistory: create schema: %w\nstatement: %s", err, stmt)
		}
	}
	return nil
}

// ─── Ingest ────────────────────────────────────────────────────────────────

// Index ingests (or replaces) one session WHOLE AND ATOMICALLY. If a session
// with the same chat_id is already indexed, all its turns are deleted and the
// session row is replaced. An empty workspace is rejected (fail-closed).
func (s *Store) Index(ctx context.Context, in IndexInput) error {
	if in.ChatID == "" {
		return fmt.Errorf("sessionhistory: chat_id is required")
	}
	if in.Workspace == "" {
		return fmt.Errorf("sessionhistory: refusing to index with empty workspace (fail-closed)")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sessionhistory: begin tx: %w", err)
	}
	defer tx.Rollback()

	// Whole-session atomic replace: delete old turns, replace the session row.
	if _, err := tx.ExecContext(ctx, "DELETE FROM turns WHERE chat_id = ?", in.ChatID); err != nil {
		return fmt.Errorf("sessionhistory: delete old turns: %w", err)
	}
	tainted := 0
	if in.Tainted {
		tainted = 1
	}
	gen := 0
	if in.SummaryGenerated {
		gen = 1
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO sessions (chat_id, workspace, created, updated, label, summary, summary_generated, tainted, source_hash, size_bytes)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(chat_id) DO UPDATE SET
			workspace=excluded.workspace, created=excluded.created, updated=excluded.updated,
			label=excluded.label, summary=excluded.summary, summary_generated=excluded.summary_generated,
			tainted=excluded.tainted, source_hash=excluded.source_hash, size_bytes=excluded.size_bytes`,
		in.ChatID, in.Workspace, in.Created.UnixMilli(), in.Updated.UnixMilli(), in.Label,
		in.Summary, gen, tainted, in.SourceHash, in.SizeBytes); err != nil {
		return fmt.Errorf("sessionhistory: upsert session: %w", err)
	}

	for _, t := range in.Turns {
		if strings.TrimSpace(t.Text) == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO turns (chat_id, ordinal, role, text) VALUES (?, ?, ?, ?)",
			in.ChatID, t.Ordinal, t.Role, t.Text); err != nil {
			return fmt.Errorf("sessionhistory: insert turn: %w", err)
		}
	}

	return tx.Commit()
}

// Delete removes a session and all its turns from the index atomically. Used
// when the source session file has been deleted.
func (s *Store) Delete(ctx context.Context, chatID string) error {
	if chatID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sessionhistory: begin delete tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, "DELETE FROM turns WHERE chat_id = ?", chatID); err != nil {
		return fmt.Errorf("sessionhistory: delete turns: %w", err)
	}
	res, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE chat_id = ?", chatID)
	if err != nil {
		return fmt.Errorf("sessionhistory: delete session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sessionhistory: commit delete: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetSummary returns the stored summary (and whether it was model-generated)
// for a session, or an empty string if the session is not indexed.
func (s *Store) GetSummary(ctx context.Context, chatID string) (summary string, generated bool, err error) {
	row := s.db.QueryRowContext(ctx,
		"SELECT summary, summary_generated FROM sessions WHERE chat_id = ?", chatID)
	var summaryText string
	var gen int
	if err := row.Scan(&summaryText, &gen); err != nil {
		if err == sql.ErrNoRows {
			return "", false, nil
		}
		return "", false, err
	}
	return summaryText, gen == 1, nil
}

// ListMeta returns reconciliation metadata for all indexed sessions in the
// workspace, so the backfill path can compute what changed without re-parsing
// every transcript. Empty workspace returns no rows (fail-closed).
func (s *Store) ListMeta(ctx context.Context, workspace string) ([]IndexedMeta, error) {
	if workspace == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT chat_id, updated, source_hash, size_bytes, summary_generated
		 FROM sessions WHERE workspace = ?`, workspace)
	if err != nil {
		return nil, fmt.Errorf("sessionhistory: list meta: %w", err)
	}
	defer rows.Close()

	var out []IndexedMeta
	for rows.Next() {
		var m IndexedMeta
		var upd int64
		var gen int
		if err := rows.Scan(&m.ChatID, &upd, &m.SourceHash, &m.SizeBytes, &gen); err != nil {
			return nil, err
		}
		m.Updated = time.UnixMilli(upd)
		m.SummaryGenerated = gen == 1
		out = append(out, m)
	}
	return out, rows.Err()
}

// ─── Search ────────────────────────────────────────────────────────────────

// Search returns up to limit sessions from the given workspace whose indexed
// turns match the query. Empty workspace returns no results (fail-closed).
// excludeChatID removes the current session from results. Results are grouped
// by session and ordered by FTS relevance (BM25) blended with recency.
func (s *Store) Search(ctx context.Context, query, workspace, excludeChatID string, limit int) ([]Result, error) {
	if workspace == "" {
		return nil, nil // fail-closed: empty workspace is never global
	}
	ftsQuery := sanitizeQuery(query)
	if ftsQuery == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = resultLimit
	}

	rows, err := s.db.QueryContext(ctx, `
		WITH matched AS (
			-- Matches from turn text (FTS over turns).
			SELECT t.chat_id AS chat_id, t.ordinal AS ordinal, t.role AS role, t.text AS text
			FROM turns_fts
			JOIN turns t ON t.id = turns_fts.rowid
			JOIN sessions sess ON sess.chat_id = t.chat_id
			WHERE turns_fts MATCH ? AND sess.workspace = ? AND t.chat_id != ?
			UNION ALL
			-- Matches from session summaries (FTS over sessions), as a synthetic turn.
			SELECT sess.chat_id, -1, 'summary', sess.summary
			FROM sessions_fts
			JOIN sessions sess ON sess.rowid = sessions_fts.rowid
			WHERE sessions_fts MATCH ? AND sess.workspace = ? AND sess.chat_id != ?
			  AND trim(COALESCE(sess.summary,'')) != ''
		)
		SELECT m.chat_id, m.ordinal, m.role, m.text,
		       sess.created, sess.updated, sess.label, sess.summary, sess.tainted
		FROM matched m
		JOIN sessions sess ON sess.chat_id = m.chat_id
		ORDER BY sess.updated DESC
		LIMIT ?`,
		ftsQuery, workspace, excludeChatID, ftsQuery, workspace, excludeChatID, searchCapRaw)
	if err != nil {
		return nil, fmt.Errorf("sessionhistory: search: %w", err)
	}
	defer rows.Close()

	type hit struct {
		created int64
		updated int64
		label   string
		summary string
		tainted int
		turn    Turn
	}
	bySession := make(map[string]*struct {
		meta  hit
		turns []Turn
	})
	var order []string

	for rows.Next() {
		var chatID string
		var h hit
		if err := rows.Scan(&chatID, &h.turn.Ordinal, &h.turn.Role, &h.turn.Text,
			&h.created, &h.updated, &h.label, &h.summary, &h.tainted); err != nil {
			return nil, err
		}
		sess, ok := bySession[chatID]
		if !ok {
			sess = &struct {
				meta  hit
				turns []Turn
			}{meta: h}
			bySession[chatID] = sess
			order = append(order, chatID)
		}
		if len(sess.turns) < turnsPerSession {
			sess.turns = append(sess.turns, h.turn)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]Result, 0, limit)
	for _, chatID := range order {
		if len(out) >= limit {
			break
		}
		sess := bySession[chatID]
		out = append(out, Result{
			ChatID:  chatID,
			Created: time.UnixMilli(sess.meta.created),
			Updated: time.UnixMilli(sess.meta.updated),
			Label:   sess.meta.label,
			Summary: sess.meta.summary,
			Tainted: sess.meta.tainted == 1,
			Turns:   sess.turns,
		})
	}
	return out, nil
}

// ─── Query sanitizer ───────────────────────────────────────────────────────

// sanitizeQuery builds an FTS5 MATCH query from free text while preserving
// Unicode (the shared agent sanitizer strips non-ASCII, which breaks CJK
// search; here we keep any letter/digit). Reserved FTS5 syntax is neutralized
// by quoting each token. Tokens are joined with OR for broad matching.
func sanitizeQuery(text string) string {
	var tokens []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() >= 2 { // keep tokens of len >= 2 (single chars are noise)
			tokens = append(tokens, strings.ReplaceAll(cur.String(), "\"", "\"\""))
		}
		cur.Reset()
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()

	if len(tokens) == 0 {
		return ""
	}
	if len(tokens) > maxQueryTokens {
		tokens = tokens[:maxQueryTokens]
	}
	quoted := make([]string, len(tokens))
	for i, t := range tokens {
		quoted[i] = "\"" + t + "\""
	}
	return strings.Join(quoted, " OR ")
}
