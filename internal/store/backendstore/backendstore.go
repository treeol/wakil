// Package backendstore provides DB-backed CRUD for the backends table (P4g).
//
// API keys are envelope-encrypted before storage; the plaintext key is never
// persisted. All queries are tenant-scoped: every method requires a tenantID
// and every SQL statement includes a tenant_id predicate.
package backendstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/treeol/wakil/internal/crypto"
)

// Store wraps a *sql.DB for backend credential queries.
type Store struct {
	db *sql.DB
}

// New creates a backend store backed by db.
func New(db *sql.DB) *Store {
	return &Store{db: db}
}

// BackendRow is the metadata view of a backend. No encrypted material or
// plaintext keys — only display fields.
type BackendRow struct {
	ID             string
	TenantID       string
	Label          string
	BackendType    string
	BaseURL        string
	APIKeyLastFour string
	CreatedAt      string // RFC 3339
	UpdatedAt      string // RFC 3339
}

// EncryptedKey holds the envelope-encrypted API key material as stored in DB.
type EncryptedKey struct {
	Cipher    []byte
	DEK       []byte
	DataNonce []byte
	DEKNonce  []byte
	KeyID     string
}

// CreateParams holds the parameters for creating a new backend.
type CreateParams struct {
	ID           string
	TenantID     string
	Label        string
	BackendType  string
	BaseURL      string
	EncryptedKey EncryptedKey
	LastFour     string
}

// Create inserts a new backend with an envelope-encrypted API key.
func (s *Store) Create(ctx context.Context, p CreateParams) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `INSERT INTO backends
		(id, tenant_id, label, backend_type, base_url,
		 api_key_cipher, api_key_dek, api_key_data_nonce, api_key_dek_nonce, api_key_key_id,
		 api_key_last_four, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.TenantID, p.Label, p.BackendType, p.BaseURL,
		p.EncryptedKey.Cipher, p.EncryptedKey.DEK, p.EncryptedKey.DataNonce, p.EncryptedKey.DEKNonce, p.EncryptedKey.KeyID,
		p.LastFour, now, now)
	if err != nil {
		return fmt.Errorf("backendstore: create: %w", err)
	}
	return nil
}

// Get retrieves a single backend by ID, scoped to tenantID. Returns
// sql.ErrNoRows if not found or if the backend belongs to a different tenant.
func (s *Store) Get(ctx context.Context, id, tenantID string) (BackendRow, EncryptedKey, error) {
	var row BackendRow
	var ek EncryptedKey
	err := s.db.QueryRowContext(ctx, `SELECT
		id, tenant_id, label, backend_type, base_url,
		api_key_cipher, api_key_dek, api_key_data_nonce, api_key_dek_nonce, api_key_key_id,
		api_key_last_four, created_at, updated_at
		FROM backends WHERE id = ? AND tenant_id = ?`,
		id, tenantID).Scan(
		&row.ID, &row.TenantID, &row.Label, &row.BackendType, &row.BaseURL,
		&ek.Cipher, &ek.DEK, &ek.DataNonce, &ek.DEKNonce, &ek.KeyID,
		&row.APIKeyLastFour, &row.CreatedAt, &row.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return BackendRow{}, EncryptedKey{}, fmt.Errorf("backendstore: not found: %w", err)
		}
		return BackendRow{}, EncryptedKey{}, fmt.Errorf("backendstore: get: %w", err)
	}
	return row, ek, nil
}

// List returns all backends for a tenant. No encrypted material is returned.
func (s *Store) List(ctx context.Context, tenantID string) ([]BackendRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT
		id, tenant_id, label, backend_type, base_url,
		api_key_last_four, created_at, updated_at
		FROM backends WHERE tenant_id = ? ORDER BY created_at DESC`,
		tenantID)
	if err != nil {
		return nil, fmt.Errorf("backendstore: list: %w", err)
	}
	defer rows.Close()

	var out []BackendRow
	for rows.Next() {
		var row BackendRow
		if err := rows.Scan(&row.ID, &row.TenantID, &row.Label, &row.BackendType, &row.BaseURL,
			&row.APIKeyLastFour, &row.CreatedAt, &row.UpdatedAt); err != nil {
			return nil, fmt.Errorf("backendstore: scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// UpdateParams holds optional update fields. Empty strings mean "unchanged".
// If EncryptedKey is non-nil, the API key is re-encrypted.
type UpdateParams struct {
	ID           string
	TenantID     string
	Label        string        // empty = unchanged
	BaseURL      string        // empty = unchanged
	EncryptedKey *EncryptedKey // nil = unchanged
	LastFour     string        // only set when EncryptedKey is non-nil
}

// Update modifies a backend. Only non-empty fields are updated.
func (s *Store) Update(ctx context.Context, p UpdateParams) error {
	now := time.Now().UTC().Format(time.RFC3339)

	if p.EncryptedKey != nil {
		_, err := s.db.ExecContext(ctx, `UPDATE backends SET
			label = ?, base_url = ?,
			api_key_cipher = ?, api_key_dek = ?, api_key_data_nonce = ?, api_key_dek_nonce = ?, api_key_key_id = ?,
			api_key_last_four = ?, updated_at = ?
			WHERE id = ? AND tenant_id = ?`,
			p.Label, p.BaseURL,
			p.EncryptedKey.Cipher, p.EncryptedKey.DEK, p.EncryptedKey.DataNonce, p.EncryptedKey.DEKNonce, p.EncryptedKey.KeyID,
			p.LastFour, now, p.ID, p.TenantID)
		if err != nil {
			return fmt.Errorf("backendstore: update with key: %w", err)
		}
		return nil
	}

	// Update without key change. Use COALESCE to preserve existing values
	// for empty fields.
	_, err := s.db.ExecContext(ctx, `UPDATE backends SET
		label = CASE WHEN ? = '' THEN label ELSE ? END,
		base_url = CASE WHEN ? = '' THEN base_url ELSE ? END,
		updated_at = ?
		WHERE id = ? AND tenant_id = ?`,
		p.Label, p.Label, p.BaseURL, p.BaseURL, now, p.ID, p.TenantID)
	if err != nil {
		return fmt.Errorf("backendstore: update: %w", err)
	}
	return nil
}

// Delete removes a backend, scoped to tenantID.
func (s *Store) Delete(ctx context.Context, id, tenantID string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM backends WHERE id = ? AND tenant_id = ?", id, tenantID)
	if err != nil {
		return fmt.Errorf("backendstore: delete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("backendstore: rows affected: %w", err)
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// DecryptAPIKey decrypts the API key from an EncryptedKey using the master key.
// The AAD is bound to the backend's tenant_id and ID to prevent cross-row
// ciphertext swapping.
func DecryptAPIKey(mk *crypto.MasterKey, ek EncryptedKey, tenantID, backendID string) ([]byte, error) {
	env := &crypto.Envelope{
		KeyID:         ek.KeyID,
		DEKCiphertext: ek.DEK,
		Ciphertext:    ek.Cipher,
		DataNonce:     ek.DataNonce,
		DEKNonce:      ek.DEKNonce,
	}
	return mk.Decrypt(env, crypto.AAD{
		Purpose:  "backend.api_key",
		TenantID: tenantID,
		RowID:    backendID,
	})
}

// EncryptAPIKey encrypts a plaintext API key using the master key, returning
// the EncryptedKey for storage. The AAD is bound to tenant_id and backend_id.
func EncryptAPIKey(mk *crypto.MasterKey, plaintext []byte, tenantID, backendID string) (EncryptedKey, error) {
	env, err := mk.Encrypt(plaintext, crypto.AAD{
		Purpose:  "backend.api_key",
		TenantID: tenantID,
		RowID:    backendID,
	})
	if err != nil {
		return EncryptedKey{}, err
	}
	return EncryptedKey{
		Cipher:    env.Ciphertext,
		DEK:       env.DEKCiphertext,
		DataNonce: env.DataNonce,
		DEKNonce:  env.DEKNonce,
		KeyID:     env.KeyID,
	}, nil
}

// LastFour returns the last 4 characters of a key, or an empty string if
// the key is too short.
func LastFour(key string) string {
	if len(key) <= 4 {
		return ""
	}
	return key[len(key)-4:]
}
