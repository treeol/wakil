// Package agentstore provides DB-backed CRUD for the agents and
// agent_revisions tables (P5c).
//
// All queries are tenant-scoped: every method requires a tenantID and every
// SQL statement includes a tenant_id predicate (design §6.3).
package agentstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// Store wraps a *sql.DB for agent queries.
type Store struct {
	db *sql.DB
}

// New creates an agent store backed by db.
func New(db *sql.DB) *Store {
	return &Store{db: db}
}

// AgentRow is the metadata view of an agent.
type AgentRow struct {
	ID        string
	TenantID  string
	Name      string
	HeadRevID string
	CreatedAt string // RFC 3339
}

// AgentRevisionRow is the metadata view of an agent revision.
type AgentRevisionRow struct {
	ID        string
	TenantID  string
	AgentID   string
	RevNumber int32
	Spec      string
	SpecHash  string
	CreatedBy string
	CreatedAt string // RFC 3339
}

// CreateParams holds the parameters for creating a new agent.
type CreateParams struct {
	ID       string
	TenantID string
	Name     string
}

// Create inserts a new agent with no revisions.
func (s *Store) Create(ctx context.Context, p CreateParams) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `INSERT INTO agents
		(id, tenant_id, name, head_rev_id, created_at)
		VALUES (?, ?, ?, '', ?)`,
		p.ID, p.TenantID, p.Name, now)
	if err != nil {
		return fmt.Errorf("agentstore: create: %w", err)
	}
	return nil
}

// Get retrieves a single agent by ID, scoped to tenantID.
func (s *Store) Get(ctx context.Context, id, tenantID string) (AgentRow, error) {
	var row AgentRow
	err := s.db.QueryRowContext(ctx, `SELECT
		id, tenant_id, name, head_rev_id, created_at
		FROM agents WHERE id = ? AND tenant_id = ?`,
		id, tenantID).Scan(
		&row.ID, &row.TenantID, &row.Name, &row.HeadRevID, &row.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AgentRow{}, fmt.Errorf("agentstore: not found: %w", err)
		}
		return AgentRow{}, fmt.Errorf("agentstore: get: %w", err)
	}
	return row, nil
}

// List returns all agents for a tenant.
func (s *Store) List(ctx context.Context, tenantID string) ([]AgentRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT
		id, tenant_id, name, head_rev_id, created_at
		FROM agents WHERE tenant_id = ? ORDER BY created_at DESC`,
		tenantID)
	if err != nil {
		return nil, fmt.Errorf("agentstore: list: %w", err)
	}
	defer rows.Close()

	var out []AgentRow
	for rows.Next() {
		var row AgentRow
		if err := rows.Scan(&row.ID, &row.TenantID, &row.Name, &row.HeadRevID, &row.CreatedAt); err != nil {
			return nil, fmt.Errorf("agentstore: scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// Delete removes an agent and its revisions (cascaded by FK), scoped to tenantID.
func (s *Store) Delete(ctx context.Context, id, tenantID string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM agents WHERE id = ? AND tenant_id = ?", id, tenantID)
	if err != nil {
		return fmt.Errorf("agentstore: delete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("agentstore: rows affected: %w", err)
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// CreateRevisionParams holds the parameters for creating a new agent revision.
type CreateRevisionParams struct {
	ID        string
	TenantID  string
	AgentID   string
	Spec      string // JSON
	CreatedBy string
}

// CreateRevision inserts a new immutable agent revision, assigns the next
// rev_number, computes the spec hash, and updates the agent's head_rev_id.
func (s *Store) CreateRevision(ctx context.Context, p CreateRevisionParams) (AgentRevisionRow, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	specHash := hashSpec(p.Spec)

	// Get the next rev_number in a transaction.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AgentRevisionRow{}, fmt.Errorf("agentstore: begin tx: %w", err)
	}
	defer tx.Rollback()

	// Verify the agent exists and belongs to this tenant.
	var agentExists bool
	err = tx.QueryRowContext(ctx,
		"SELECT 1 FROM agents WHERE id = ? AND tenant_id = ?",
		p.AgentID, p.TenantID).Scan(&agentExists)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AgentRevisionRow{}, fmt.Errorf("agentstore: agent not found: %w", err)
		}
		return AgentRevisionRow{}, fmt.Errorf("agentstore: check agent: %w", err)
	}

	// Get the next rev_number.
	var nextRev int32
	err = tx.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(rev_number), 0) + 1 FROM agent_revisions WHERE agent_id = ?",
		p.AgentID).Scan(&nextRev)
	if err != nil {
		return AgentRevisionRow{}, fmt.Errorf("agentstore: next rev: %w", err)
	}

	// Insert the revision.
	_, err = tx.ExecContext(ctx, `INSERT INTO agent_revisions
		(id, tenant_id, agent_id, rev_number, spec, spec_hash, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.TenantID, p.AgentID, nextRev, p.Spec, specHash, p.CreatedBy, now)
	if err != nil {
		return AgentRevisionRow{}, fmt.Errorf("agentstore: insert revision: %w", err)
	}

	// Update the agent's head_rev_id.
	_, err = tx.ExecContext(ctx,
		"UPDATE agents SET head_rev_id = ? WHERE id = ? AND tenant_id = ?",
		p.ID, p.AgentID, p.TenantID)
	if err != nil {
		return AgentRevisionRow{}, fmt.Errorf("agentstore: update head_rev: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return AgentRevisionRow{}, fmt.Errorf("agentstore: commit: %w", err)
	}

	return AgentRevisionRow{
		ID:        p.ID,
		TenantID:  p.TenantID,
		AgentID:   p.AgentID,
		RevNumber: nextRev,
		Spec:      p.Spec,
		SpecHash:  specHash,
		CreatedBy: p.CreatedBy,
		CreatedAt: now,
	}, nil
}

// ListRevisions returns all revisions for an agent, scoped to tenantID.
func (s *Store) ListRevisions(ctx context.Context, agentID, tenantID string) ([]AgentRevisionRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT
		id, tenant_id, agent_id, rev_number, spec, spec_hash, created_by, created_at
		FROM agent_revisions WHERE agent_id = ? AND tenant_id = ?
		ORDER BY rev_number DESC`,
		agentID, tenantID)
	if err != nil {
		return nil, fmt.Errorf("agentstore: list revisions: %w", err)
	}
	defer rows.Close()

	var out []AgentRevisionRow
	for rows.Next() {
		var row AgentRevisionRow
		if err := rows.Scan(&row.ID, &row.TenantID, &row.AgentID, &row.RevNumber,
			&row.Spec, &row.SpecHash, &row.CreatedBy, &row.CreatedAt); err != nil {
			return nil, fmt.Errorf("agentstore: scan revision: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// hashSpec computes the SHA-256 hash of the spec JSON.
func hashSpec(spec string) string {
	h := sha256.Sum256([]byte(spec))
	return hex.EncodeToString(h[:])
}
