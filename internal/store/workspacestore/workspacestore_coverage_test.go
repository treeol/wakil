package workspacestore

import (
	"context"
	"testing"
)

// ─── Error branch tests (closed-DB) ────────────────────────────────────────
//
// These tests exercise the non-ErrNoRows error returns in Create, Get, List,
// and Delete by closing the underlying *sql.DB before calling the method.
// A closed DB produces a "database is closed" error that is NOT sql.ErrNoRows,
// so it hits the generic error branch (not the not-found branch).

func TestGetDBError(t *testing.T) {
	s := openTestDB(t)
	s.db.Close()

	_, err := s.Get(context.Background(), "wsp_x", testTenant)
	if err == nil {
		t.Fatal("Get with closed DB: expected error, got nil")
	}
}

func TestListDBError(t *testing.T) {
	s := openTestDB(t)
	s.db.Close()

	_, err := s.List(context.Background(), testTenant)
	if err == nil {
		t.Fatal("List with closed DB: expected error, got nil")
	}
}

func TestDeleteDBError(t *testing.T) {
	s := openTestDB(t)
	s.db.Close()

	err := s.Delete(context.Background(), "wsp_x", testTenant)
	if err == nil {
		t.Fatal("Delete with closed DB: expected error, got nil")
	}
}

func TestCreateDBError(t *testing.T) {
	s := openTestDB(t)
	// First create works (DB open).
	if err := s.Create(context.Background(), CreateParams{
		ID: testWSID, TenantID: testTenant, Name: "x", HostPath: "/p",
	}); err != nil {
		t.Fatalf("Create (open DB): %v", err)
	}
	// Close DB, then Create should fail with a generic error.
	s.db.Close()
	err := s.Create(context.Background(), CreateParams{
		ID: "wsp_new", TenantID: testTenant, Name: "y", HostPath: "/q",
	})
	if err == nil {
		t.Fatal("Create with closed DB: expected error, got nil")
	}
}
