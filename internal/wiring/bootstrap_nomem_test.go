package wiring

import (
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/tools"
)

// TestBuildApp_NoMemory_StripsMemoryStoreAndTools verifies that when
// BuildAppOpts.NoMemory is true, the App's MemoryStore is nil and no
// memory tool is offered in app.Tools. The memory tool names are derived
// from wtools.MemoryTools() (the single source) so the filter cannot
// drift if tools are renamed.
func TestBuildApp_NoMemory_StripsMemoryStoreAndTools(t *testing.T) {
	srv := sseServer(t, []string{contentChunk("done")})
	defer srv.Close()

	cfg := testManagerConfig(t, srv.URL)
	cfg.WorkDir = t.TempDir()
	cfg.HostWorkDir = cfg.WorkDir

	app, res, err := BuildApp(cfg, fakeExec{}, BuildAppOpts{
		IsHeadless: true,
		NoMemory:   true,
	})
	if err != nil {
		t.Fatalf("BuildApp: %v", err)
	}
	defer func() {
		app.StopAllBackgroundProcs()
		CloseResources(app, res)
	}()

	// MemoryStore must be nil (the store is not opened when NoMemory
	// is set; see bootstrap.go).
	if app.MemoryStore != nil {
		t.Errorf("MemoryStore = %v, want nil", app.MemoryStore)
	}

	// Build the set of memory tool names from the single source so
	// the check cannot drift if tools are renamed.
	memNames := make(map[string]bool)
	for _, tool := range tools.MemoryTools() {
		memNames[tool.Function.Name] = true
	}

	// No memory tool should be in app.Tools.
	for _, tool := range app.Tools {
		if memNames[tool.Function.Name] {
			t.Errorf("memory tool %q should not be in app.Tools when NoMemory is set", tool.Function.Name)
		}
	}
}

// TestBuildApp_NoMemory_LeavesNoMemoryModule verifies that with the
// context-modules switch on, --no-memory still removes every memory tool and
// leaves no memory module behind: no module owns a memory tool, and the
// module index names no memory tool (the registry is built before the
// bootstrap strip, so this is the assertion the strip cannot drift from).
func TestBuildApp_NoMemory_LeavesNoMemoryModule(t *testing.T) {
	srv := sseServer(t, []string{contentChunk("done")})
	defer srv.Close()

	cfg := testManagerConfig(t, srv.URL)
	cfg.WorkDir = t.TempDir()
	cfg.HostWorkDir = cfg.WorkDir
	cfg.ContextModules = "on"

	app, res, err := BuildApp(cfg, fakeExec{}, BuildAppOpts{
		IsHeadless: true,
		NoMemory:   true,
	})
	if err != nil {
		t.Fatalf("BuildApp: %v", err)
	}
	defer func() {
		app.StopAllBackgroundProcs()
		CloseResources(app, res)
	}()

	// The registry must exist (switch on) but own no memory tool.
	if app.Modules == nil {
		t.Fatal("Modules = nil with the switch on")
	}
	memNames := make(map[string]bool)
	for _, tool := range tools.MemoryTools() {
		memNames[tool.Function.Name] = true
	}
	for _, tool := range app.Tools {
		if memNames[tool.Function.Name] {
			t.Errorf("memory tool %q should not be in app.Tools when NoMemory is set (switch on)", tool.Function.Name)
		}
	}
	for _, modName := range app.Modules.ModuleNames() {
		if modName == "memory" {
			t.Error("a module named \"memory\" must not exist")
		}
		if owned := app.Modules.ToolModule("memory_get"); owned != "" {
			_ = owned
		}
	}
	if owned := app.Modules.ToolModule("memory_get"); owned != "" {
		t.Errorf("ToolModule(memory_get) = %q, want \"\" (memory tools are core, never module-owned)", owned)
	}
	idx := app.Modules.ModuleIndex()
	for name := range memNames {
		if strings.Contains(idx, name) {
			t.Errorf("module index must name no memory tool, but it names %q", name)
		}
	}
}

// TestBuildApp_WithMemory_OffersMemoryStoreAndTools verifies that when
// NoMemory is false (the default), the App's MemoryStore is non-nil and
// memory tools are offered. This is the control case for the NoMemory test.
func TestBuildApp_WithMemory_OffersMemoryStoreAndTools(t *testing.T) {
	srv := sseServer(t, []string{contentChunk("done")})
	defer srv.Close()

	cfg := testManagerConfig(t, srv.URL)
	cfg.WorkDir = t.TempDir()
	cfg.HostWorkDir = cfg.WorkDir

	app, res, err := BuildApp(cfg, fakeExec{}, BuildAppOpts{
		IsHeadless: true,
		NoMemory:   false,
	})
	if err != nil {
		t.Fatalf("BuildApp: %v", err)
	}
	defer func() {
		app.StopAllBackgroundProcs()
		CloseResources(app, res)
	}()

	// MemoryStore should be non-nil (opened by default).
	if app.MemoryStore == nil {
		t.Error("MemoryStore = nil, want non-nil when NoMemory is false")
	}

	// Memory tools should be present.
	memNames := make(map[string]bool)
	for _, tool := range tools.MemoryTools() {
		memNames[tool.Function.Name] = true
	}

	found := 0
	for _, tool := range app.Tools {
		if memNames[tool.Function.Name] {
			found++
		}
	}
	if found == 0 {
		t.Error("no memory tools in app.Tools, want at least one when NoMemory is false")
	}
}
