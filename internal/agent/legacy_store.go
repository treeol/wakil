package agent

import (
	"os"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/memory"
)

// LegacyWorkspacePath is the directory the pre-0.5.0 code keyed the shared
// store by: WAKIL_WORKSPACE_PATH when set, else the config's WorkspacePath()
// (host_workdir for docker and hisn, the workdir for direct). Every chat before
// 0.5.0 keyed its memory by this value, so all projects shared one store
// (0.5.0, D2). It is read, never written.
func LegacyWorkspacePath(cfg config.Config) string {
	if env := os.Getenv("WAKIL_WORKSPACE_PATH"); env != "" {
		return env
	}
	return cfg.WorkspacePath()
}

// LegacyMemoryDBPath is the shared pre-0.5.0 memory database path for cfg, or
// "" when no directory can be derived. It may not exist.
func LegacyMemoryDBPath(cfg config.Config) string {
	return MemoryDBPath(LegacyWorkspacePath(cfg))
}

// OpenLegacyMemory opens the legacy shared store read-only, or returns nil when
// it does not exist or cannot be read. It never creates or migrates the file.
// When the legacy path equals the project store (the same repository), there is
// nothing legacy to read and nil is returned.
func OpenLegacyMemory(cfg config.Config, projectDB string) *memory.Store {
	p := LegacyMemoryDBPath(cfg)
	if p == "" || p == projectDB {
		return nil
	}
	s, err := memory.OpenReadOnly(p, ProjectDir(cfg))
	if err != nil {
		return nil
	}
	return s
}
