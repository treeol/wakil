package agent

import (
	"os"
	"path/filepath"
)

// stagingKeyShortLen is the number of hex chars from the workspace SHA-256
// used as the staging directory name. The full 64-char hash makes the UDS
// socket path exceed the 108-byte sun_path limit on Linux
// (~/.local/share/wakil/staging/<64 hex>/kvr.sock = 111+ bytes). 16 hex chars
// (64 bits) is more than sufficient for collision resistance in a per-user
// directory.
const stagingKeyShortLen = 16

// stagingKey returns the first stagingKeyShortLen hex chars of the workspace
// SHA-256, for use as the staging directory name. This is derived from the
// same workspaceKey used by repo-state/session scoping, so the staging dir
// always matches the repo identity — just truncated for path length.
func stagingKey(ws string) string {
	key := workspaceKey(ws)
	if len(key) > stagingKeyShortLen {
		return key[:stagingKeyShortLen]
	}
	return key
}

// StagingPath returns the host-side staging directory path for a given
// workspace. It reuses the same workspaceKey (SHA-256 of the canonical
// workspace path) used by repo-state and session scoping, truncated to
// 16 hex chars to keep the UDS socket path under the 108-byte sun_path limit.
//
// Path: <wakil-data-dir>/staging/<short-key>/
// where <wakil-data-dir> is the parent of repoStateDir()
// (i.e. ~/.local/share/wakil/ or $WAKIL_REPO_STATE_DIR/.. or
// $XDG_DATA_HOME/wakil/).
//
// Returns "" if the data directory cannot be determined or the workspace
// key is empty. The directory is NOT created here — the caller (docker
// executor setup) creates it with the right permissions.
func StagingPath(ws string) string {
	key := stagingKey(ws)
	if key == "" {
		return ""
	}
	dataDir := stagingDataRoot()
	if dataDir == "" {
		return ""
	}
	return filepath.Join(dataDir, "staging", key)
}

// stagingDataRoot returns the wakil data directory that is the parent of
// repoStateDir(): $WAKIL_REPO_STATE_DIR/.. , $XDG_DATA_HOME/wakil/ , or
// ~/.local/share/wakil/.
func stagingDataRoot() string {
	// repoStateDir already resolves the precedence; its parent is the
	// wakil data root.
	rsd := repoStateDir()
	if rsd == "" {
		return ""
	}
	// repoStateDir is .../wakil/repo-state; parent is .../wakil.
	// But if WAKIL_REPO_STATE_DIR points somewhere unusual, use its
	// parent only when it ends in /repo-state; otherwise use the dir
	// itself as the data root.
	parent := filepath.Dir(rsd)
	if filepath.Base(rsd) == "repo-state" && filepath.Base(parent) == "wakil" {
		return parent
	}
	// Custom WAKIL_REPO_STATE_DIR: use its parent as the data root.
	return parent
}

// StagingSocketPath returns the host-side path to the kvr UDS socket for
// the given workspace's staging directory.
func StagingSocketPath(ws string) string {
	sp := StagingPath(ws)
	if sp == "" {
		return ""
	}
	return filepath.Join(sp, "kvr.sock")
}

// MemoryDBPath returns the host-side path to the durable memory SQLite
// database for the given workspace. Reuses the same workspace-key derivation
// as StagingPath (16 hex chars of the SHA-256 of the canonical workspace path).
// Path: <wakil-data-dir>/memory/<short-key>/memory.db
//
// Returns "" if the data directory cannot be determined or the workspace
// key is empty. The directory is NOT created here — memory.Open creates it.
func MemoryDBPath(ws string) string {
	key := stagingKey(ws)
	if key == "" {
		return ""
	}
	dataDir := stagingDataRoot()
	if dataDir == "" {
		return ""
	}
	return filepath.Join(dataDir, "memory", key, "memory.db")
}

// EnsureStagingDir creates the staging directory for ws with 0o700
// permissions, owned by the current user. Returns the path. If the
// directory already exists with looser permissions, they are tightened
// to 0o700.
func EnsureStagingDir(ws string) (string, error) {
	sp := StagingPath(ws)
	if sp == "" {
		return "", os.ErrNotExist
	}
	if err := os.MkdirAll(sp, 0o700); err != nil {
		return "", err
	}
	// Enforce 0o700 even if the dir already existed with looser perms.
	// MkdirAll does not tighten existing dirs.
	if err := os.Chmod(sp, 0o700); err != nil {
		return "", err
	}
	return sp, nil
}
