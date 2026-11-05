package agent

import "github.com/treeol/wakil/internal/config"

// ChatProjectPath is the directory a chat keys its per-project stores by: the
// same value SessionWorkspace returns. It honours WAKIL_WORKSPACE_PATH, which
// plan gc (cmd/wakil ResolveProject) does not. The two sources agree when that
// variable is unset and the chat runs in the same directory.
func ChatProjectPath(cfg config.Config) string {
	return cfg.WorkspacePath()
}
