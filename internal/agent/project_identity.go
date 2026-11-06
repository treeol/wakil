package agent

import (
	"os"

	"github.com/treeol/wakil/internal/config"
)

// ProjectDir is the project identity a chat keys every per-project store by
// (0.5.0, D1), for every backend, symlink-resolved, equal to ResolveProject(dir)
// in cmd/wakil (the `plan gc` key):
//
//   - an explicit project directory is the project: the --workdir flag, or a
//     WorkDir set on a Config that did not come from LoadConfig (programmatic
//     callers and test literals);
//   - otherwise the process working directory.
//
// A WorkDir that came from the config file, WAKIL_WORKDIR / ILM_WORKDIR, or
// a derived default never keys a store: it is not an explicit choice. Neither
// host_workdir nor WAKIL_WORKSPACE_PATH nor WAKIL_HOST_WORKDIR feeds it (the
// bind mount only). A stale environment value previously keyed rubin's store.
func ProjectDir(cfg config.Config) string {
	if cfg.WorkDirFlag || (cfg.WorkDir != "" && !cfg.WorkDirLoaded()) {
		return canonicalWorkspace(cfg.WorkDir)
	}
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return canonicalWorkspace(wd)
}

// ChatProjectPath is kept for callers that name the chat's store directory. It
// is ProjectDir: store identity never depends on WAKIL_WORKSPACE_PATH.
func ChatProjectPath(cfg config.Config) string {
	return ProjectDir(cfg)
}
