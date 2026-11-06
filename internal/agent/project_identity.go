package agent

import (
	"os"

	"github.com/treeol/wakil/internal/config"
)

// ProjectDir is the directory a chat keys its per-project stores by, for every
// backend (0.5.0, D1): the launch directory, symlink-resolved. It is the same
// value `wakil plan gc` resolves from the same directory, so one repository
// has one store key. Neither WAKIL_WORKSPACE_PATH nor the config's host_workdir
// feeds it; host_workdir remains the docker bind mount only.
//
// The launch directory is the config's workdir: WorkDir in direct mode,
// HostWorkDir otherwise (the hisn and docker backends, which set HostWorkDir to
// the directory the user ran wakil in). An empty value falls back to the
// process working directory.
func ProjectDir(cfg config.Config) string {
	dir := cfg.HostWorkDir
	if cfg.ExecMode == "direct" {
		dir = cfg.WorkDir
	}
	if dir == "" {
		if wd, err := os.Getwd(); err == nil {
			dir = wd
		}
	}
	return canonicalWorkspace(dir)
}

// ChatProjectPath is kept for callers that name the chat's store directory. It
// is ProjectDir: store identity never depends on WAKIL_WORKSPACE_PATH.
func ChatProjectPath(cfg config.Config) string {
	return ProjectDir(cfg)
}
