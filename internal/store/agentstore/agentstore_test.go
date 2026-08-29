package agentstore

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
	testAgent  = "agt_test1"
)

func TestCreateAndGet(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()

	if err := s.Create(ctx, CreateParams{
		ID:       testAgent,
		TenantID: testTenant,
		Name:     "my-agent",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	row, err := s.Get(ctx, testAgent, testTenant)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.ID != testAgent {
		t.Errorf("ID = %q, want %q", row.ID, testAgent)
	}
	if row.TenantID != testTenant {
		t.Errorf("TenantID = %q, want %q", row.TenantID, testTenant)
	}
	if row.Name != "my-agent" {
		t.Errorf("Name = %q, want %q", row.Name, "my-agent")
	}
	if row.HeadRevID != "" {
		t.Errorf("HeadRevID = %q, want empty", row.HeadRevID)
	}
	if row.CreatedAt == "" {
		t.Error("CreatedAt is empty, want RFC 3339 timestamp")
	}
}

func TestGetNotFound(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()

	_, err := s.Get(ctx, "agt_nonexistent", testTenant)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Get nonexistent: err = %v, want sql.ErrNoRows wrapped", err)
	}
}

func TestGetWrongTenant(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()

	if err := s.Create(ctx, CreateParams{
		ID:       testAgent,
		TenantID: testTenant,
		Name:     "my-agent",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A different tenant should not see the agent — the tenant_id predicate
	// means the row is not found, returning sql.ErrNoRows.
	_, err := s.Get(ctx, testAgent, "tnt_other")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Get wrong tenant: err = %v, want sql.ErrNoRows wrapped", err)
	}
}

func TestList(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()

	agents := []struct {
		id   string
		name string
	}{
		{"agt_a", "alpha"},
		{"agt_b", "beta"},
		{"agt_c", "gamma"},
	}
	for _, a := range agents {
		if err := s.Create(ctx, CreateParams{
			ID:       a.id,
			TenantID: testTenant,
			Name:     a.name,
		}); err != nil {
			t.Fatalf("Create %s: %v", a.id, err)
		}
	}

	rows, err := s.List(ctx, testTenant)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("List returned %d rows, want 3", len(rows))
	}

	// Verify all agents are present (order is by created_at DESC, but
	// timestamps may collide — verify by ID set).
	got := make(map[string]bool)
	for _, r := range rows {
		got[r.ID] = true
	}
	for _, a := range agents {
		if !got[a.id] {
			t.Errorf("List: agent %s not found in results", a.id)
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
		t.Fatalf("List empty: got %d rows, want nil/empty", len(rows))
	}
}

func TestDelete(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()

	if err := s.Create(ctx, CreateParams{
		ID:       testAgent,
		TenantID: testTenant,
		Name:     "my-agent",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := s.Delete(ctx, testAgent, testTenant); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Verify gone.
	_, err := s.Get(ctx, testAgent, testTenant)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Get after delete: err = %v, want sql.ErrNoRows wrapped", err)
	}
}

func TestDeleteNotFound(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()

	err := s.Delete(ctx, "agt_nonexistent", testTenant)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Delete nonexistent: err = %v, want sql.ErrNoRows", err)
	}
}

func TestDeleteWrongTenant(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()

	if err := s.Create(ctx, CreateParams{
		ID:       testAgent,
		TenantID: testTenant,
		Name:     "my-agent",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Deleting with wrong tenant should not find the row.
	err := s.Delete(ctx, testAgent, "tnt_other")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Delete wrong tenant: err = %v, want sql.ErrNoRows", err)
	}

	// The agent should still exist under the correct tenant.
	if _, err := s.Get(ctx, testAgent, testTenant); err != nil {
		t.Errorf("Get after wrong-tenant delete: %v", err)
	}
}

func TestCreateDuplicate(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()

	p := CreateParams{ID: testAgent, TenantID: testTenant, Name: "my-agent"}
	if err := s.Create(ctx, p); err != nil {
		t.Fatalf("Create first: %v", err)
	}
	err := s.Create(ctx, p)
	if err == nil {
		t.Error("Create duplicate: expected error, got nil")
	}
}

func TestCreateRevision(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()

	if err := s.Create(ctx, CreateParams{
		ID:       testAgent,
		TenantID: testTenant,
		Name:     "my-agent",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	rev1, err := s.CreateRevision(ctx, CreateRevisionParams{
		ID:        "rev_1",
		TenantID:  testTenant,
		AgentID:   testAgent,
		Spec:      `{"model":"gpt-4"}`,
		CreatedBy: "usr_local",
	})
	if err != nil {
		t.Fatalf("CreateRevision 1: %v", err)
	}
	if rev1.RevNumber != 1 {
		t.Errorf("RevNumber = %d, want 1", rev1.RevNumber)
	}
	if rev1.SpecHash == "" {
		t.Error("SpecHash is empty, want SHA-256 hex")
	}

	// Verify head_rev_id was updated.
	agent, err := s.Get(ctx, testAgent, testTenant)
	if err != nil {
		t.Fatalf("Get after revision: %v", err)
	}
	if agent.HeadRevID != "rev_1" {
		t.Errorf("HeadRevID = %q, want %q", agent.HeadRevID, "rev_1")
	}

	// Second revision should increment rev_number.
	rev2, err := s.CreateRevision(ctx, CreateRevisionParams{
		ID:        "rev_2",
		TenantID:  testTenant,
		AgentID:   testAgent,
		Spec:      `{"model":"claude"}`,
		CreatedBy: "usr_local",
	})
	if err != nil {
		t.Fatalf("CreateRevision 2: %v", err)
	}
	if rev2.RevNumber != 2 {
		t.Errorf("RevNumber = %d, want 2", rev2.RevNumber)
	}
}

func TestCreateRevisionAgentNotFound(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()

	_, err := s.CreateRevision(ctx, CreateRevisionParams{
		ID:        "rev_1",
		TenantID:  testTenant,
		AgentID:   "agt_nonexistent",
		Spec:      `{}`,
		CreatedBy: "usr_local",
	})
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("CreateRevision for nonexistent agent: err = %v, want sql.ErrNoRows wrapped", err)
	}
}

func TestListRevisions(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()

	if err := s.Create(ctx, CreateParams{
		ID:       testAgent,
		TenantID: testTenant,
		Name:     "my-agent",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	for i, spec := range []string{`{"v":1}`, `{"v":2}`, `{"v":3}`} {
		revID := "rev_" + string(rune('A'+i))
		if _, err := s.CreateRevision(ctx, CreateRevisionParams{
			ID:        revID,
			TenantID:  testTenant,
			AgentID:   testAgent,
			Spec:      spec,
			CreatedBy: "usr_local",
		}); err != nil {
			t.Fatalf("CreateRevision %s: %v", revID, err)
		}
	}

	revs, err := s.ListRevisions(ctx, testAgent, testTenant)
	if err != nil {
		t.Fatalf("ListRevisions: %v", err)
	}
	if len(revs) != 3 {
		t.Fatalf("ListRevisions returned %d, want 3", len(revs))
	}

	// Should be ordered by rev_number DESC.
	if revs[0].RevNumber != 3 || revs[2].RevNumber != 1 {
		t.Errorf("ListRevisions order: rev_numbers = %d %d %d, want 3 2 1",
			revs[0].RevNumber, revs[1].RevNumber, revs[2].RevNumber)
	}
}

func TestListRevisionsEmpty(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()

	revs, err := s.ListRevisions(ctx, testAgent, testTenant)
	if err != nil {
		t.Fatalf("ListRevisions empty: %v", err)
	}
	if len(revs) != 0 {
		t.Fatalf("ListRevisions empty: got %d, want nil/empty", len(revs))
	}
}

func TestDeleteCascadesRevisions(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()

	if err := s.Create(ctx, CreateParams{
		ID:       testAgent,
		TenantID: testTenant,
		Name:     "my-agent",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.CreateRevision(ctx, CreateRevisionParams{
		ID:        "rev_1",
		TenantID:  testTenant,
		AgentID:   testAgent,
		Spec:      `{}`,
		CreatedBy: "usr_local",
	}); err != nil {
		t.Fatalf("CreateRevision: %v", err)
	}

	if err := s.Delete(ctx, testAgent, testTenant); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Revisions should be cascaded by FK ON DELETE CASCADE.
	revs, err := s.ListRevisions(ctx, testAgent, testTenant)
	if err != nil {
		t.Fatalf("ListRevisions after delete: %v", err)
	}
	if len(revs) != 0 {
		t.Errorf("ListRevisions after delete: got %d rows, want 0 (cascade)", len(revs))
	}
}
