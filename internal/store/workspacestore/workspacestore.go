// Package workspacestore provides DB-backed CRUD for the workspaces table (P5b).
//
// All queries are tenant-scoped: every method requires a tenantID and every
// SQL statement includes a tenant_id predicate (design §6.3).
package workspacestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Store wraps a *sql.DB for workspace queries.
type Store struct {
	db *sql.DB
}

// New creates a workspace store backed by db.
func New(db *sql.DB) *Store {
	return &Store{db: db}
}

// WorkspaceRow is the metadata view of a workspace.
type WorkspaceRow struct {
	ID        string
	TenantID  string
	Name      string
	HostPath  string
	VCSRemote string
	CreatedAt string // RFC 3339
}

// CreateParams holds the parameters for creating a new workspace.
type CreateParams struct {
	ID        string
	TenantID  string
	Name      string
	HostPath  string
	VCSRemote string
}

// Create inserts a new workspace.
func (s *Store) Create(ctx context.Context, p CreateParams) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `INSERT INTO workspaces
		(id, tenant_id, name, host_path, vcs_remote, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		p.ID, p.TenantID, p.Name, p.HostPath, p.VCSRemote, now)
	if err != nil {
		return fmt.Errorf("workspacestore: create: %w", err)
	}
	return nil
}

// Get retrieves a single workspace by ID, scoped to tenantID. Returns
// sql.ErrNoRows if not found or if the workspace belongs to a different tenant.
func (s *Store) Get(ctx context.Context, id, tenantID string) (WorkspaceRow, error) {
	var row WorkspaceRow
	err := s.db.QueryRowContext(ctx, `SELECT
		id, tenant_id, name, host_path, vcs_remote, created_at
		FROM workspaces WHERE id = ? AND tenant_id = ?`,
		id, tenantID).Scan(
		&row.ID, &row.TenantID, &row.Name, &row.HostPath, &row.VCSRemote, &row.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WorkspaceRow{}, fmt.Errorf("workspacestore: not found: %w", err)
		}
		return WorkspaceRow{}, fmt.Errorf("workspacestore: get: %w", err)
	}
	return row, nil
}

// List returns all workspaces for a tenant.
func (s *Store) List(ctx context.Context, tenantID string) ([]WorkspaceRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT
		id, tenant_id, name, host_path, vcs_remote, created_at
		FROM workspaces WHERE tenant_id = ? ORDER BY created_at DESC`,
		tenantID)
	if err != nil {
		return nil, fmt.Errorf("workspacestore: list: %w", err)
	}
	defer rows.Close()

	var out []WorkspaceRow
	for rows.Next() {
		var row WorkspaceRow
		if err := rows.Scan(&row.ID, &row.TenantID, &row.Name, &row.HostPath, &row.VCSRemote, &row.CreatedAt); err != nil {
			return nil, fmt.Errorf("workspacestore: scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// Delete removes a workspace, scoped to tenantID.
func (s *Store) Delete(ctx context.Context, id, tenantID string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM workspaces WHERE id = ? AND tenant_id = ?", id, tenantID)
	if err != nil {
		return fmt.Errorf("workspacestore: delete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("workspacestore: rows affected: %w", err)
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
