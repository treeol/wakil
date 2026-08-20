// Package sqlstore implements sessionhost.Store backed by SQLite (card #148 P1).
//
// It replaces the P0 in-memory MemLog for production use. The Store interface
// (EventAppender + EventLog) is unchanged; the host injects SQLiteStore via
// WithStore. SessionCreated appends atomically create the sessions row + first
// event in one transaction; all other appends require an existing session row
// (enforced by the composite FK and a tenant-qualified UPDATE).
//
// The store uses modernc.org/sqlite (pure Go, no CGO) with WAL journaling and
// a single connection (SetMaxOpenConns(1)). PRAGMAs (foreign_keys, busy_timeout,
// journal_mode) are applied on every Open. The encoding column on events is
// 'json-v1' (PayloadEncoding); the reader dispatches on this value.
package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/store/migrations"

	_ "modernc.org/sqlite"
)

// SQLiteStore implements sessionhost.Store backed by SQLite.
type SQLiteStore struct {
	db *sql.DB
	mu sync.Mutex // serializes appends (defense-in-depth; SetMaxOpenConns(1) also serializes)
}

// NewSQLiteStore opens (or creates) a SQLite database at dbPath, applies
// pending migrations, and returns a ready-to-use store. The caller owns the
// store and must call Close when done.
func NewSQLiteStore(ctx context.Context, dbPath string) (*SQLiteStore, error) {
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("sqlstore: create db directory: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("sqlstore: open db: %w", err)
	}
	// Single connection: serializes all access and avoids pool-related
	// "database is locked" errors. Volume is negligible for P1's few-session
	// embedded mode.
	db.SetMaxOpenConns(1)

	// Pragmas must be set per-connection before migrations. PRAGMA journal_mode
	// is persistent (stored in the DB file); foreign_keys and busy_timeout are
	// per-connection and must be re-applied on every Open.
	for _, pragma := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA foreign_keys = ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("sqlstore: set pragma %q: %w", pragma, err)
		}
	}

	if err := migrations.Apply(ctx, db); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlstore: apply migrations: %w", err)
	}

	return &SQLiteStore{db: db}, nil
}

// Close releases the database handle.
func (s *SQLiteStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Append implements core.EventAppender. It validates the draft, rejects
// ephemeral drafts, and atomically assigns the next sequence and persists the
// event. For KindSessionCreated, it also inserts the sessions row in the same
// transaction. For all other kinds, it requires an existing session row
// (enforced by a tenant-qualified UPDATE + the composite FK).
func (s *SQLiteStore) Append(ctx context.Context, draft event.Event) (event.Event, error) {
	if err := ctx.Err(); err != nil {
		return event.Event{}, err
	}
	if err := draft.ValidateDraft(); err != nil {
		return event.Event{}, err
	}
	if draft.Kind.Class() == event.ClassEphemeral {
		return event.Event{}, fmt.Errorf("sqlstore: append rejected ephemeral kind %q (durable log only)", draft.Kind)
	}

	// Marshal the payload before the transaction (validation is cheap and
	// avoids a partial tx on encoding failure).
	payloadBytes, err := event.MarshalPayload(draft.Kind, draft.Payload)
	if err != nil {
		return event.Event{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if draft.Kind == event.KindSessionCreated {
		return s.appendSessionCreated(ctx, draft, payloadBytes)
	}
	return s.appendEvent(ctx, draft, payloadBytes)
}

// appendSessionCreated atomically inserts the sessions row and the first event
// (seq=1) in one transaction.
func (s *SQLiteStore) appendSessionCreated(ctx context.Context, draft event.Event, payloadBytes []byte) (event.Event, error) {
	sc, ok := draft.Payload.(event.SessionCreated)
	if !ok {
		// The payload should already be validated by ValidateDraft, but a
		// pointer form would reach here. Normalize via type switch.
		if ptr, ok2 := draft.Payload.(*event.SessionCreated); ok2 && ptr != nil {
			sc = *ptr
		} else {
			return event.Event{}, fmt.Errorf("sqlstore: SessionCreated payload type %T is not event.SessionCreated", draft.Payload)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return event.Event{}, fmt.Errorf("sqlstore: begin tx: %w", err)
	}
	defer tx.Rollback()

	// Insert the sessions row with last_seq=1.
	_, err = tx.ExecContext(ctx, `INSERT INTO sessions (id, tenant_id, workspace, created_by, created_at, last_seq)
		VALUES (?, ?, ?, ?, ?, 1)`,
		string(draft.SessionID),
		string(draft.TenantID),
		string(sc.WorkspaceID),
		string(sc.CreatedBy),
		draft.Ts.UnixNano(),
	)
	if err != nil {
		// PK violation = duplicate session creation.
		return event.Event{}, fmt.Errorf("sqlstore: insert session: %w", err)
	}

	committed := draft
	committed.Seq = 1

	// Insert the event row with seq=1.
	_, err = tx.ExecContext(ctx, `INSERT INTO events (tenant_id, session_id, seq, ts, kind, payload, encoding)
		VALUES (?, ?, 1, ?, ?, ?, ?)`,
		string(draft.TenantID),
		string(draft.SessionID),
		draft.Ts.UnixNano(),
		string(draft.Kind),
		payloadBytes,
		event.PayloadEncoding,
	)
	if err != nil {
		return event.Event{}, fmt.Errorf("sqlstore: insert session_created event: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return event.Event{}, fmt.Errorf("sqlstore: commit session_created: %w", err)
	}
	return committed, nil
}

// appendEvent increments last_seq and inserts the event in one transaction.
// The session must already exist and the tenant must match.
func (s *SQLiteStore) appendEvent(ctx context.Context, draft event.Event, payloadBytes []byte) (event.Event, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return event.Event{}, fmt.Errorf("sqlstore: begin tx: %w", err)
	}
	defer tx.Rollback()

	// Tenant-qualified UPDATE: fails (RowsAffected=0) if the session doesn't
	// exist or the tenant doesn't match.
	result, err := tx.ExecContext(ctx,
		"UPDATE sessions SET last_seq = last_seq + 1 WHERE id = ? AND tenant_id = ?",
		string(draft.SessionID), string(draft.TenantID))
	if err != nil {
		return event.Event{}, fmt.Errorf("sqlstore: update last_seq: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return event.Event{}, fmt.Errorf("sqlstore: rows affected: %w", err)
	}
	if affected != 1 {
		return event.Event{}, fmt.Errorf("%w: session %s not found for tenant %s",
			core.ErrSessionNotFound, draft.SessionID, draft.TenantID)
	}

	// Read the assigned seq.
	var seq uint64
	if err := tx.QueryRowContext(ctx,
		"SELECT last_seq FROM sessions WHERE id = ? AND tenant_id = ?",
		string(draft.SessionID), string(draft.TenantID)).Scan(&seq); err != nil {
		return event.Event{}, fmt.Errorf("sqlstore: read last_seq: %w", err)
	}

	committed := draft
	committed.Seq = event.Seq(seq)

	// Insert the event row.
	_, err = tx.ExecContext(ctx, `INSERT INTO events (tenant_id, session_id, seq, ts, kind, payload, encoding)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		string(draft.TenantID),
		string(draft.SessionID),
		seq,
		draft.Ts.UnixNano(),
		string(draft.Kind),
		payloadBytes,
		event.PayloadEncoding,
	)
	if err != nil {
		return event.Event{}, fmt.Errorf("sqlstore: insert event: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return event.Event{}, fmt.Errorf("sqlstore: commit: %w", err)
	}
	return committed, nil
}

// Read implements core.EventLog. It returns durable events with seq > after,
// ascending by seq, up to limit entries. limit <= 0 means no limit.
func (s *SQLiteStore) Read(ctx context.Context, sessionID event.SessionID, after event.Seq, limit int) ([]event.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Build the query with optional LIMIT. When limit <= 0, omit the clause.
	// (SQLite LIMIT 0 returns zero rows; LIMIT -1 is invalid. Omitting the
	// clause returns all matching rows.)
	q := `SELECT tenant_id, session_id, seq, ts, kind, payload, encoding
		FROM events
		WHERE session_id = ? AND seq > ?
		ORDER BY seq ASC`
	var args []any
	args = append(args, string(sessionID), uint64(after))
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlstore: read: %w", err)
	}
	defer rows.Close()

	var out []event.Event
	for rows.Next() {
		var tenantID, sessID, kind, encoding string
		var seq, ts int64
		var payloadBytes []byte
		if err := rows.Scan(&tenantID, &sessID, &seq, &ts, &kind, &payloadBytes, &encoding); err != nil {
			return nil, fmt.Errorf("sqlstore: scan event: %w", err)
		}

		// Dispatch on encoding.
		if encoding != event.PayloadEncoding {
			return nil, fmt.Errorf("sqlstore: unknown encoding %q for event %s/%d", encoding, sessID, seq)
		}

		// Unmarshal the payload.
		payload, err := event.UnmarshalPayload(event.Kind(kind), payloadBytes)
		if err != nil {
			return nil, fmt.Errorf("sqlstore: decode payload for %s/%d: %w", sessID, seq, err)
		}

		ev := event.Event{
			TenantID:  event.TenantID(tenantID),
			SessionID: event.SessionID(sessID),
			Seq:       event.Seq(seq),
			Ts:        time.Unix(0, ts),
			Kind:      event.Kind(kind),
			Payload:   payload,
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// LastSeq implements core.EventLog. It returns the highest committed durable
// seq for the session, or 0 if none (or the session does not exist).
func (s *SQLiteStore) LastSeq(ctx context.Context, sessionID event.SessionID) (event.Seq, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	var seq int64
	err := s.db.QueryRowContext(ctx,
		"SELECT last_seq FROM sessions WHERE id = ?",
		string(sessionID)).Scan(&seq)
	if err == sql.ErrNoRows {
		return 0, nil // session doesn't exist → 0 (matches MemLog contract)
	}
	if err != nil {
		return 0, fmt.Errorf("sqlstore: last_seq: %w", err)
	}
	return event.Seq(seq), nil
}

// Compile-time proof that SQLiteStore implements sessionhost.Store.
var _ interface {
	core.EventAppender
	core.EventLog
} = (*SQLiteStore)(nil)

// ErrStoreClosed is returned by operations on a closed store. (Not currently
// reachable — Close just closes the *sql.DB — but defined for future use.)
var ErrStoreClosed = errors.New("sqlstore: store closed")
