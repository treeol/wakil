package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/memory"
	"github.com/treeol/wakil/internal/proxy"
)

// B2: plan gc must find the rows the session's memory tools wrote. The rows
// are created through the real handlers in a tainted session (not hand-built),
// the store is closed, and PlanGCStore — the function the CLI uses — reopens
// it by workspace. Before the fix the CLI keyed the store by the container
// path and reported zeros.
func TestPlanGCFindsQuarantinedRowsWrittenByHandlers(t *testing.T) {
	dir := t.TempDir()
	ws := filepath.Join(dir, "Coding", "wakil")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAKIL_REPO_STATE_DIR", filepath.Join(dir, "state"))

	storePath := MemoryDBPath(ws)
	if storePath == "" {
		t.Fatal("MemoryDBPath empty")
	}
	store, err := memory.Open(storePath, ws)
	if err != nil {
		t.Fatal(err)
	}
	app := &App{MemoryStore: store, AgentPrefix: "main"}
	app.addExternalGrounding(proxy.GroundingEntry{Type: "web", Label: "https://example.com"})

	res := app.ExecuteToolCall(memCtx(t), memToolCall("memory_put",
		`{"key":"plan/current","value":"plan from tainted session","kind":"note","ttl_seconds":3600}`))
	if strings.Contains(res.text, "ERROR") {
		t.Fatalf("memory_put failed: %s", res.text)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	gcStore, opened, err := PlanGCStore(ws, "")
	if err != nil {
		t.Fatalf("PlanGCStore: %v", err)
	}
	defer gcStore.Close()
	if opened != storePath {
		t.Errorf("opened %q, want %q", opened, storePath)
	}
	entries, err := gcStore.List(memCtx(t), "plan/", "", memory.StatusProposed)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if strings.HasPrefix(e.Key, "plan/") {
			found = true
		}
	}
	if !found {
		t.Fatalf("plan gc store has no plan/ rows; the handler wrote %q", res.text)
	}
}

// A missing store is an error, never zeros, and no file is created.
func TestPlanGCStoreMissingIsErrorAndCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	ws := filepath.Join(dir, "nowhere")
	t.Setenv("WAKIL_REPO_STATE_DIR", filepath.Join(dir, "state"))

	store, path, err := PlanGCStore(ws, "")
	if err == nil {
		store.Close()
		t.Fatal("expected an error for a missing store")
	}
	if !strings.Contains(err.Error(), "no memory store") {
		t.Errorf("error should say no store, got: %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("missing store was created at %s", path)
	}
}
