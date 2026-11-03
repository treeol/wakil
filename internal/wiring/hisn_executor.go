package wiring

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/exec"
	"github.com/treeol/wakil/internal/sandbox/hisn"
)

// newHisnExecutor builds the executor for sandbox.backend "hisn". Every
// failure is returned: there is no fallback to docker and never an unsandboxed
// executor. The session itself starts lazily, once a chat is bound.
func newHisnExecutor(cfg config.Config) (exec.Executor, error) {
	// Defence in depth for hand-built configs (LoadConfig already resolves
	// the backend and forces browser_enabled off under hisn).
	if cfg.ExecMode != "docker" {
		return nil, fmt.Errorf("sandbox backend hisn cannot be combined with exec mode %q", cfg.ExecMode)
	}
	if cfg.BrowserEnabled {
		return nil, fmt.Errorf("browser_enabled is not supported with sandbox backend hisn")
	}
	h := cfg.Sandbox.Hisn
	state := hisnStateDir()
	if state == "" {
		return nil, fmt.Errorf("sandbox backend hisn: cannot determine the Wakil state directory")
	}
	home := h.HomeRoot
	if home == "" {
		key := agent.WorkspaceKey(cfg.WorkspacePath())
		if key == "" {
			return nil, fmt.Errorf("sandbox backend hisn: cannot derive a workspace key for home_root")
		}
		home = filepath.Join(state, "hisn-home", key)
	}
	surface, _, err := loadProjectSurface(cfg)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	hisn.SetDiagnostics(os.Stderr)
	b, err := hisn.Open(ctx, hisn.Options{
		Binary:      h.Binary,
		StateDir:    filepath.Join(state, "hisn"),
		HomeRoot:    home,
		HostCwd:     cfg.HostWorkDir,
		GrantableRW: h.GrantableRW,
		EnvPass:     h.EnvPass,
		TmpSizeMB:   h.TmpSizeMB,
		ShmSizeMB:   h.ShmSizeMB,
		ExecViaCLI:  h.ExecViaCLI,
		Surface:     surface,
	})
	if err != nil {
		return nil, err
	}
	return exec.NewBackendExecutor(b, exec.BackendOptions{
		Label:       "hisn",
		GrantableRW: h.GrantableRW,
	}), nil
}

// hisnStateDir is Wakil's per-user state root (same resolution as the other
// durable stores: $XDG_DATA_HOME/wakil, else ~/.local/share/wakil).
func hisnStateDir() string {
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "wakil")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "wakil")
}
