package wiring

import (
	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
)

// WorkspaceKeysReport renders the --print-workspace-key output for cfg. It is a
// thin seam: cmd/wakil must not import internal/agent (TestHeadlessNoAgentImport),
// so the report is reached through wiring.
func WorkspaceKeysReport(cfg config.Config) string {
	return agent.ResolveWorkspaceKeys(cfg).Format()
}

// WorkspaceEnvOverrideLine is the one startup line for WAKIL_WORKSPACE_PATH
// ("" when the variable is not set).
func WorkspaceEnvOverrideLine(cfg config.Config) string {
	return agent.ResolveWorkspaceKeys(cfg).EnvOverrideLine()
}
