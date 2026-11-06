package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/memory"
	"github.com/treeol/wakil/internal/proxy"
)

func legacyMemCall(t *testing.T, args map[string]any) proxy.ToolCall {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return proxy.ToolCall{Function: proxy.FunctionCall{Name: "memory", Arguments: string(b)}}
}

// C2: project store first, legacy on miss; legacy results are marked and the
// legacy file is never written.
func TestMemoryReadThroughOrderAndMarker(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("WAKIL_WORKSPACE_KEY", "")
	t.Setenv("WAKIL_WORKSPACE_PATH", "")
	ctx := context.Background()

	proj := t.TempDir()
	legacyDir := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.ExecMode = "direct"
	cfg.WorkDir = proj

	// Legacy shared store at the old key derivation (legacy workspace = proj's
	// WorkDir here, so use a distinct legacy path to make the stores differ).
	legacyWS := legacyDir
	t.Setenv("WAKIL_WORKSPACE_PATH", legacyWS)
	legacyDB := LegacyMemoryDBPath(cfg)
	if legacyDB == "" {
		t.Fatal("no legacy db path")
	}
	lw, err := memory.Open(legacyDB, legacyWS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lw.PutActive(ctx, "shared/only-legacy", "from the old store", "note", "durable", "t", "s", 0, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := lw.PutActive(ctx, "shared/both", "legacy copy", "note", "durable", "t", "s", 0, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	lw.Close()
	// WAKIL_WORKSPACE_PATH stays set: it is what derived the legacy key, and the
	// read-through must use the same derivation the legacy store was written under.

	projDB := MemoryDBPath(ProjectDir(cfg))
	ps, err := memory.Open(projDB, proj)
	if err != nil {
		t.Fatal(err)
	}
	defer ps.Close()
	if _, err := ps.PutActive(ctx, "shared/both", "project copy", "note", "durable", "t", "s", 0, nil, nil, ""); err != nil {
		t.Fatal(err)
	}

	app := &App{Cfg: cfg, MemoryStore: ps, LegacyMemory: OpenLegacyMemory(cfg, projDB)}
	if app.LegacyMemory == nil {
		t.Fatal("legacy store must open read-only")
	}

	// Project hit: no legacy marker.
	got := app.handleMemoryGet(ctx, legacyMemCall(t, map[string]any{"key": "shared/both"}))
	if !strings.Contains(got, "project copy") || strings.Contains(got, "[legacy") {
		t.Fatalf("project store must answer first, unmarked:\n%s", got)
	}
	// Legacy-only key: answered from legacy, marked.
	got = app.handleMemoryGet(ctx, legacyMemCall(t, map[string]any{"key": "shared/only-legacy"}))
	if !strings.Contains(got, "from the old store") || !strings.Contains(got, "[legacy") {
		t.Fatalf("legacy fallthrough must answer and mark:\n%s", got)
	}
	// Missing everywhere: not found, no legacy marker.
	got = app.handleMemoryGet(ctx, legacyMemCall(t, map[string]any{"key": "nope/none"}))
	if !strings.HasPrefix(got, "not found") {
		t.Fatalf("missing key: %s", got)
	}

	// Search: project hit first, legacy-only hit marked after it.
	got = app.handleMemorySearch(ctx, legacyMemCall(t, map[string]any{"query": "shared"}))
	pi := strings.Index(got, "project copy")
	li := strings.Index(got, "from the old store")
	if pi < 0 || li < 0 || pi > li {
		t.Fatalf("search order: project must precede legacy:\n%s", got)
	}
	if !strings.Contains(got[strings.LastIndex(got[:li], "\n---\n"):li], "[legacy") {
		t.Fatalf("legacy search hit must be marked:\n%s", got)
	}
}

// C2 invariant: the legacy file is byte-identical after read-through.
func TestLegacyReadThroughDoesNotWrite(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("WAKIL_WORKSPACE_KEY", "")
	ctx := context.Background()
	proj := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.ExecMode = "direct"
	cfg.WorkDir = proj
	legacyWS := t.TempDir()
	t.Setenv("WAKIL_WORKSPACE_PATH", legacyWS)
	db := LegacyMemoryDBPath(cfg)
	lw, err := memory.Open(db, legacyWS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lw.PutActive(ctx, "k/x", "v", "note", "durable", "t", "s", 0, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	lw.Close()
	before, _ := os.ReadFile(db)

	projDB := MemoryDBPath(ProjectDir(cfg))
	ps, err := memory.Open(projDB, proj)
	if err != nil {
		t.Fatal(err)
	}
	defer ps.Close()
	app := &App{Cfg: cfg, MemoryStore: ps, LegacyMemory: OpenLegacyMemory(cfg, projDB)}
	app.handleMemoryGet(ctx, legacyMemCall(t, map[string]any{"key": "k/x"}))
	app.handleMemorySearch(ctx, legacyMemCall(t, map[string]any{"query": "v"}))
	app.LegacyMemory.Close()

	after, _ := os.ReadFile(db)
	if string(before) != string(after) {
		t.Fatal("legacy store was modified by read-through")
	}
}

// C2: with no legacy store file, nothing is created and the legacy handle is nil.
func TestOpenLegacyMemoryAbsentIsNil(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("WAKIL_WORKSPACE_KEY", "")
	cfg := config.DefaultConfig()
	cfg.ExecMode = "direct"
	cfg.WorkDir = t.TempDir()
	t.Setenv("WAKIL_WORKSPACE_PATH", t.TempDir())
	if s := OpenLegacyMemory(cfg, ""); s != nil {
		t.Fatal("absent legacy store must be nil")
	}
	if _, err := os.Stat(LegacyMemoryDBPath(cfg)); !os.IsNotExist(err) {
		t.Fatal("legacy file must not be created")
	}
	_ = filepath.Join
}

// C2: sessions recorded under the legacy path are listed as legacy, not hidden.
func TestListLegacySessionsSplit(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("WAKIL_WORKSPACE_KEY", "")
	proj := t.TempDir()
	legacy := t.TempDir()
	scope := SessionScope{Workspace: proj, LegacyWorkspace: legacy, IncludeLegacy: true}
	if got, err := ListLegacySessions(scope); err != nil || len(got) != 0 {
		t.Fatalf("no sessions yet: %v %v", got, err)
	}
	// Same path for project and legacy: no legacy listing at all.
	same := SessionScope{Workspace: proj, LegacyWorkspace: proj}
	if got, _ := ListLegacySessions(same); got != nil {
		t.Fatal("legacy listing must be empty when the paths coincide")
	}
}
