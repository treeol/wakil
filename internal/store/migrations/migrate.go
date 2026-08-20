// Package migrations implements numbered, forward-only SQL migrations for the
// wakild SQLite store (card #148 P1).
//
// Migrations are embedded SQL files applied at store open. Each migration runs
// in its own transaction; its version is recorded in schema_migrations only
// after the SQL succeeds. The runner creates schema_migrations if it does not
// exist. Concurrent Apply calls from the same process are serialized by
// SetMaxOpenConns(1) on the *sql.DB; cross-process serialization relies on
// SQLite's write lock.
package migrations

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed *.sql
var migrationFS embed.FS

// migration represents one numbered SQL file.
type migration struct {
	version int
	sql     string
}

// Apply applies all pending migrations to db. It is idempotent: if all
// migrations are already applied, it is a no-op. It creates schema_migrations
// if it does not exist. Each migration runs in its own transaction; the version
// is recorded only after the SQL succeeds.
func Apply(ctx context.Context, db *sql.DB) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}

	// Bootstrap: create schema_migrations if it doesn't exist.
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("migrations: create schema_migrations: %w", err)
	}

	// Read applied versions.
	applied := make(map[int]bool)
	rows, err := db.QueryContext(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return fmt.Errorf("migrations: read applied: %w", err)
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return fmt.Errorf("migrations: scan version: %w", err)
		}
		applied[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("migrations: rows err: %w", err)
	}

	// Apply pending migrations in order.
	for _, m := range migrations {
		if applied[m.version] {
			continue
		}
		if err := applyOne(ctx, db, m); err != nil {
			return fmt.Errorf("migrations: apply %d: %w", m.version, err)
		}
	}
	return nil
}

// applyOne runs one migration in its own transaction and records the version.
func applyOne(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("exec migration: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		"INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)",
		m.version, nowNanos()); err != nil {
		return fmt.Errorf("record version: %w", err)
	}

	return tx.Commit()
}

// loadMigrations reads embedded SQL files, sorted by version number.
func loadMigrations() ([]migration, error) {
	entries, err := migrationFS.ReadDir(".")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	var out []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".sql")
		// Parse version from filename: "001_init" → 1.
		parts := strings.SplitN(name, "_", 2)
		if len(parts) < 1 {
			return nil, fmt.Errorf("unexpected migration filename %q", e.Name())
		}
		v, err := strconv.Atoi(parts[0])
		if err != nil {
			return nil, fmt.Errorf("parse migration version from %q: %w", e.Name(), err)
		}
		data, err := migrationFS.ReadFile(e.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", e.Name(), err)
		}
		out = append(out, migration{version: v, sql: string(data)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// nowNanos returns the current time in Unix nanoseconds.
func nowNanos() int64 {
	return time.Now().UnixNano()
}
