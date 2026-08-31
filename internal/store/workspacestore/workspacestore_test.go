package workspacestore

import (
	"context"
	"database/sql"
	"errors"
	"testing"

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

const (
	testTenant = "tnt_local"
	testWSID   = "wsp_test1"
)

func TestCreateAndGet(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()

	if err := s.Create(ctx, CreateParams{
		ID:        testWSID,
		TenantID:  testTenant,
		Name:      "my-project",
		HostPath:  "/home/user/project",
		VCSRemote: "https://github.com/user/project",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	row, err := s.Get(ctx, testWSID, testTenant)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.ID != testWSID {
		t.Errorf("ID = %q, want %q", row.ID, testWSID)
	}
	if row.Name != "my-project" {
		t.Errorf("Name = %q, want %q", row.Name, "my-project")
	}
	if row.HostPath != "/home/user/project" {
		t.Errorf("HostPath = %q, want %q", row.HostPath, "/home/user/project")
	}
	if row.VCSRemote != "https://github.com/user/project" {
		t.Errorf("VCSRemote = %q, want %q", row.VCSRemote, "https://github.com/user/project")
	}
	if row.CreatedAt == "" {
		t.Error("CreatedAt is empty, want RFC 3339 timestamp")
	}
}

func TestGetNotFound(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()

	_, err := s.Get(ctx, "wsp_nonexistent", testTenant)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Get nonexistent: err = %v, want sql.ErrNoRows wrapped", err)
	}
}

func TestGetWrongTenant(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()

	if err := s.Create(ctx, CreateParams{
		ID:       testWSID,
		TenantID: testTenant,
		Name:     "my-project",
		HostPath: "/home/user/project",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err := s.Get(ctx, testWSID, "tnt_other")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Get wrong tenant: err = %v, want sql.ErrNoRows wrapped", err)
	}
}

func TestList(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()

	for _, w := range []struct {
		id   string
		name string
	}{
		{"wsp_a", "alpha"},
		{"wsp_b", "beta"},
	} {
		if err := s.Create(ctx, CreateParams{
			ID:       w.id,
			TenantID: testTenant,
			Name:     w.name,
			HostPath: "/home/user/" + w.name,
		}); err != nil {
			t.Fatalf("Create %s: %v", w.id, err)
		}
	}

	rows, err := s.List(ctx, testTenant)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("List returned %d rows, want 2", len(rows))
	}
	got := make(map[string]bool)
	for _, r := range rows {
		got[r.ID] = true
	}
	if !got["wsp_a"] || !got["wsp_b"] {
		t.Errorf("List: missing workspace(s), got IDs = %v", got)
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
		t.Fatalf("List empty: got %d rows, want nil/empty", len(rows))
	}
}

func TestDelete(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()

	if err := s.Create(ctx, CreateParams{
		ID:       testWSID,
		TenantID: testTenant,
		Name:     "my-project",
		HostPath: "/home/user/project",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := s.Delete(ctx, testWSID, testTenant); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err := s.Get(ctx, testWSID, testTenant)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Get after delete: err = %v, want sql.ErrNoRows wrapped", err)
	}
}

func TestDeleteNotFound(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()

	err := s.Delete(ctx, "wsp_nonexistent", testTenant)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Delete nonexistent: err = %v, want sql.ErrNoRows", err)
	}
}

func TestDeleteWrongTenant(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()

	if err := s.Create(ctx, CreateParams{
		ID:       testWSID,
		TenantID: testTenant,
		Name:     "my-project",
		HostPath: "/home/user/project",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	err := s.Delete(ctx, testWSID, "tnt_other")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Delete wrong tenant: err = %v, want sql.ErrNoRows", err)
	}

	// Should still exist under correct tenant.
	if _, err := s.Get(ctx, testWSID, testTenant); err != nil {
		t.Errorf("Get after wrong-tenant delete: %v", err)
	}
}

func TestCreateDuplicate(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()

	p := CreateParams{ID: testWSID, TenantID: testTenant, Name: "my-project", HostPath: "/p"}
	if err := s.Create(ctx, p); err != nil {
		t.Fatalf("Create first: %v", err)
	}
	if err := s.Create(ctx, p); err == nil {
		t.Error("Create duplicate: expected error, got nil")
	}
}
