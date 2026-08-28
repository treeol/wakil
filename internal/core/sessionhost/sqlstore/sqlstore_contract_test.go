package sqlstore

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionhost"
	"github.com/treeol/wakil/internal/core/sessionhost/storetest"
)

// TestSQLiteStoreContract runs the shared store contract suite against
// SQLiteStore, plus reopen/durability test.
func TestSQLiteStoreContract(t *testing.T) {
	storetest.RunContract(t, func(t *testing.T) sessionhost.Store {
		dir := t.TempDir()
		dbPath := filepath.Join(dir, "test.db")
		s, err := NewSQLiteStore(context.Background(), dbPath)
		if err != nil {
			t.Fatalf("NewSQLiteStore: %v", err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	})

	// Reopen durability: write, close, reopen, read.
	t.Run("ReopenDurability", func(t *testing.T) {
		dir := t.TempDir()
		dbPath := filepath.Join(dir, "test.db")
		ctx := context.Background()
		sid := event.SessionID("ses_reopen")
		tenant := event.TenantID("tnt_test")

		s1, err := NewSQLiteStore(ctx, dbPath)
		if err != nil {
			t.Fatalf("NewSQLiteStore 1: %v", err)
		}
		_, err = s1.Append(ctx, event.Event{
			TenantID: tenant, SessionID: sid, Ts: time.Now().UTC(),
			Kind:    event.KindSessionCreated,
			Payload: event.SessionCreated{WorkspaceID: "wsp_test", AgentName: "wakil", CreatedBy: "usr_test"},
		})
		if err != nil {
			t.Fatalf("Append: %v", err)
		}
		s1.Close()

		s2, err := NewSQLiteStore(ctx, dbPath)
		if err != nil {
			t.Fatalf("NewSQLiteStore 2: %v", err)
		}
		defer s2.Close()

		events, err := s2.Read(ctx, sid, 0, 0)
		if err != nil {
			t.Fatalf("Read after reopen: %v", err)
		}
		if len(events) != 1 {
			t.Fatalf("expected 1 event after reopen, got %d", len(events))
		}
	})
}
