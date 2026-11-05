package agent

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/treeol/wakil/internal/memory"
)

// PlanGCStore resolves and opens the memory store that `wakil plan gc`
// inspects. It is the same store the session's memory tools use for ws (the
// host workspace path): MemoryDBPath(ws) keys it through workspaceKey.
//
// The store must already exist. memory.Open creates a missing DB, so a wrong
// path would otherwise report zero entries with no error — the 0.4.2 failure
// mode. storePath, when set, is used instead of the workspace-derived path.
// The returned path is the one opened, so the caller can print it.
func PlanGCStore(ws, storePath string) (*memory.Store, string, error) {
	path := storePath
	if path == "" {
		path = MemoryDBPath(ws)
	}
	if path == "" {
		return nil, "", fmt.Errorf("plan gc: cannot determine memory store path")
	}
	store, err := memory.OpenExisting(path, ws)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, path, fmt.Errorf("plan gc: no memory store for workspace %q at %s; nothing to do", ws, path)
		}
		return nil, path, fmt.Errorf("plan gc: memory open error: %w", err)
	}
	return store, path, nil
}
