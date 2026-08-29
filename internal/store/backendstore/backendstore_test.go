package backendstore

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/treeol/wakil/internal/crypto"
	"github.com/treeol/wakil/internal/store/migrations"

	_ "modernc.org/sqlite"
)

// openTestDB creates a fresh SQLite DB at a temp path with all migrations applied.
func openTestDB(t *testing.T) *Store {
	t.Helper()
	dbPath := t.TempDir() + "/test.db"
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		db.Close()
		t.Fatalf("pragma: %v", err)
	}
	if err := migrations.Apply(context.Background(), db); err != nil {
		db.Close()
		t.Fatalf("apply migrations: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return New(db)
}

// testMasterKey returns a deterministic MasterKey for testing.
func testMasterKey(t *testing.T) *crypto.MasterKey {
	t.Helper()
	key := make([]byte, crypto.KeySize)
	for i := range key {
		key[i] = byte(i + 1)
	}
	mk, err := crypto.NewMasterKey("mk_test", key)
	if err != nil {
		t.Fatalf("NewMasterKey: %v", err)
	}
	return mk
}

const (
	testTenant      = "tnt_local"
	testBackend     = "be_test1"
	apiKeyPlaintext = "sk-0123456789abcdef"
)

func TestCreateAndGet(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()
	mk := testMasterKey(t)

	ek, err := EncryptAPIKey(mk, []byte(apiKeyPlaintext), testTenant, testBackend)
	if err != nil {
		t.Fatalf("EncryptAPIKey: %v", err)
	}

	if err := s.Create(ctx, CreateParams{
		ID:           testBackend,
		TenantID:     testTenant,
		Label:        "my-backend",
		BackendType:  "openai",
		BaseURL:      "https://api.openai.com",
		EncryptedKey: ek,
		LastFour:     LastFour(apiKeyPlaintext),
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	row, gotEK, err := s.Get(ctx, testBackend, testTenant)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.ID != testBackend {
		t.Errorf("ID = %q, want %q", row.ID, testBackend)
	}
	if row.Label != "my-backend" {
		t.Errorf("Label = %q, want %q", row.Label, "my-backend")
	}
	if row.BackendType != "openai" {
		t.Errorf("BackendType = %q, want %q", row.BackendType, "openai")
	}
	if row.APIKeyLastFour != "cdef" {
		t.Errorf("APIKeyLastFour = %q, want %q", row.APIKeyLastFour, "cdef")
	}

	// Verify the encrypted key round-trips.
	plaintext, err := DecryptAPIKey(mk, gotEK, testTenant, testBackend)
	if err != nil {
		t.Fatalf("DecryptAPIKey: %v", err)
	}
	if string(plaintext) != apiKeyPlaintext {
		t.Errorf("Decrypted key = %q, want %q", string(plaintext), apiKeyPlaintext)
	}
}

func TestGetNotFound(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()

	_, _, err := s.Get(ctx, "be_nonexistent", testTenant)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Get nonexistent: err = %v, want sql.ErrNoRows wrapped", err)
	}
}

func TestGetWrongTenant(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()
	mk := testMasterKey(t)

	ek, err := EncryptAPIKey(mk, []byte(apiKeyPlaintext), testTenant, testBackend)
	if err != nil {
		t.Fatalf("EncryptAPIKey: %v", err)
	}

	if err := s.Create(ctx, CreateParams{
		ID:           testBackend,
		TenantID:     testTenant,
		Label:        "my-backend",
		BackendType:  "openai",
		BaseURL:      "",
		EncryptedKey: ek,
		LastFour:     "cdef",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, _, err = s.Get(ctx, testBackend, "tnt_other")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Get wrong tenant: err = %v, want sql.ErrNoRows wrapped", err)
	}
}

func TestList(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()
	mk := testMasterKey(t)

	for _, b := range []struct {
		id, label string
	}{
		{"be_a", "alpha"},
		{"be_b", "beta"},
	} {
		ek, err := EncryptAPIKey(mk, []byte(apiKeyPlaintext), testTenant, b.id)
		if err != nil {
			t.Fatalf("EncryptAPIKey %s: %v", b.id, err)
		}
		if err := s.Create(ctx, CreateParams{
			ID:           b.id,
			TenantID:     testTenant,
			Label:        b.label,
			BackendType:  "openai",
			EncryptedKey: ek,
			LastFour:     "cdef",
		}); err != nil {
			t.Fatalf("Create %s: %v", b.id, err)
		}
	}

	rows, err := s.List(ctx, testTenant)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("List returned %d, want 2", len(rows))
	}
	// List does NOT return encrypted material — verify by checking that
	// the returned rows have display fields only.
	for _, r := range rows {
		if r.APIKeyLastFour != "cdef" {
			t.Errorf("APIKeyLastFour = %q, want %q", r.APIKeyLastFour, "cdef")
		}
	}
}

func TestListEmpty(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()

	rows, err := s.List(ctx, testTenant)
	if err != nil {
		t.Fatalf("List empty: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("List empty: got %d, want nil/empty", len(rows))
	}
}

func TestUpdateLabel(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()
	mk := testMasterKey(t)

	ek, err := EncryptAPIKey(mk, []byte(apiKeyPlaintext), testTenant, testBackend)
	if err != nil {
		t.Fatalf("EncryptAPIKey: %v", err)
	}

	if err := s.Create(ctx, CreateParams{
		ID:           testBackend,
		TenantID:     testTenant,
		Label:        "old-label",
		BackendType:  "openai",
		EncryptedKey: ek,
		LastFour:     "cdef",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := s.Update(ctx, UpdateParams{
		ID:       testBackend,
		TenantID: testTenant,
		Label:    "new-label",
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	row, _, err := s.Get(ctx, testBackend, testTenant)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if row.Label != "new-label" {
		t.Errorf("Label = %q, want %q", row.Label, "new-label")
	}
}

func TestUpdateWithNewKey(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()
	mk := testMasterKey(t)

	ek, _ := EncryptAPIKey(mk, []byte(apiKeyPlaintext), testTenant, testBackend)

	if err := s.Create(ctx, CreateParams{
		ID:           testBackend,
		TenantID:     testTenant,
		Label:        "my-backend",
		BackendType:  "openai",
		EncryptedKey: ek,
		LastFour:     "cdef",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Rotate the key.
	newKey := "sk-newkey1234567890"
	newEK, err := EncryptAPIKey(mk, []byte(newKey), testTenant, testBackend)
	if err != nil {
		t.Fatalf("EncryptAPIKey new: %v", err)
	}

	if err := s.Update(ctx, UpdateParams{
		ID:           testBackend,
		TenantID:     testTenant,
		Label:        "my-backend",
		EncryptedKey: &newEK,
		LastFour:     LastFour(newKey),
	}); err != nil {
		t.Fatalf("Update with key: %v", err)
	}

	row, gotEK, err := s.Get(ctx, testBackend, testTenant)
	if err != nil {
		t.Fatalf("Get after key rotation: %v", err)
	}
	if row.APIKeyLastFour != LastFour(newKey) {
		t.Errorf("APIKeyLastFour = %q, want %q", row.APIKeyLastFour, LastFour(newKey))
	}

	// Verify the new key decrypts correctly.
	plaintext, err := DecryptAPIKey(mk, gotEK, testTenant, testBackend)
	if err != nil {
		t.Fatalf("DecryptAPIKey after rotation: %v", err)
	}
	if string(plaintext) != newKey {
		t.Errorf("Decrypted key = %q, want %q", string(plaintext), newKey)
	}
}

func TestDelete(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()
	mk := testMasterKey(t)

	ek, _ := EncryptAPIKey(mk, []byte(apiKeyPlaintext), testTenant, testBackend)

	if err := s.Create(ctx, CreateParams{
		ID:           testBackend,
		TenantID:     testTenant,
		Label:        "my-backend",
		BackendType:  "openai",
		EncryptedKey: ek,
		LastFour:     "cdef",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := s.Delete(ctx, testBackend, testTenant); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, _, err := s.Get(ctx, testBackend, testTenant)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Get after delete: err = %v, want sql.ErrNoRows wrapped", err)
	}
}

func TestDeleteNotFound(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()

	err := s.Delete(ctx, "be_nonexistent", testTenant)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Delete nonexistent: err = %v, want sql.ErrNoRows", err)
	}
}

func TestDeleteWrongTenant(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()
	mk := testMasterKey(t)

	ek, _ := EncryptAPIKey(mk, []byte(apiKeyPlaintext), testTenant, testBackend)

	if err := s.Create(ctx, CreateParams{
		ID:           testBackend,
		TenantID:     testTenant,
		Label:        "my-backend",
		BackendType:  "openai",
		EncryptedKey: ek,
		LastFour:     "cdef",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	err := s.Delete(ctx, testBackend, "tnt_other")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Delete wrong tenant: err = %v, want sql.ErrNoRows", err)
	}

	// Should still exist under correct tenant.
	if _, _, err := s.Get(ctx, testBackend, testTenant); err != nil {
		t.Errorf("Get after wrong-tenant delete: %v", err)
	}
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	mk := testMasterKey(t)
	plaintext := []byte("sk-test-key-round-trip")

	ek, err := EncryptAPIKey(mk, plaintext, testTenant, testBackend)
	if err != nil {
		t.Fatalf("EncryptAPIKey: %v", err)
	}

	decrypted, err := DecryptAPIKey(mk, ek, testTenant, testBackend)
	if err != nil {
		t.Fatalf("DecryptAPIKey: %v", err)
	}
	if string(decrypted) != string(plaintext) {
		t.Errorf("Round-trip: got %q, want %q", string(decrypted), string(plaintext))
	}
}

func TestDecryptWrongTenant(t *testing.T) {
	mk := testMasterKey(t)
	plaintext := []byte("sk-test-key")

	ek, err := EncryptAPIKey(mk, plaintext, testTenant, testBackend)
	if err != nil {
		t.Fatalf("EncryptAPIKey: %v", err)
	}

	// Decrypting with the wrong tenant should fail (AAD mismatch).
	_, err = DecryptAPIKey(mk, ek, "tnt_other", testBackend)
	if err == nil {
		t.Error("DecryptAPIKey wrong tenant: expected error, got nil")
	}
}

func TestLastFour(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"sk-abcdefghijkl", "ijkl"}, // last 4 of 16 chars
		{"sk-abcd", "abcd"},         // 7 chars → last 4
		{"ab", ""},                  // len <= 4 → empty
		{"abcd", ""},                // len <= 4 → empty
		{"abcde", "bcde"},           // 5 chars → last 4
		{"", ""},
	}
	for _, tc := range tests {
		got := LastFour(tc.input)
		if got != tc.want {
			t.Errorf("LastFour(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
