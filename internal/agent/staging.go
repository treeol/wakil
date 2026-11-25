package agent

import (
	"fmt"
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
//
// Behavior: always returns filepath.Dir(repoStateDir()) — the parent of the
// repo-state directory. repoStateDir resolves to <data-root>/repo-state under
// the default/XDG layouts, so its parent is the data root. When
// WAKIL_REPO_STATE_DIR points at a custom path, its parent is used as the
// data root (callers placing global files under the data root should be aware
// the root then lives wherever the env var's parent is).
func stagingDataRoot() string {
	// repoStateDir already resolves the precedence; its parent is the
	// wakil data root.
	rsd := repoStateDir()
	if rsd == "" {
		return ""
	}
	return filepath.Dir(rsd)
}

// StoreKey is the 16-hex per-project key that names a project's store
// directories (memory, sessionhistory, sessionhost, hisn home). ws is
// canonicalised by workspaceKey, so a symlinked path yields the same key.
// Returns "" for an empty ws.
func StoreKey(ws string) string {
	return stagingKey(ws)
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

// SkillDBPath returns the host-side path to the global skills SQLite
// database. Unlike MemoryDBPath it is NOT workspace-keyed — skills are
// global reference docs shared across every session and every project.
// Path: <wakil-data-dir>/skills/skills.db
//
// Uses the same stagingDataRoot() precedence as memory (XDG_DATA_HOME /
// WAKIL_REPO_STATE_DIR / ~/.local/share default), NOT a hardcoded home path.
//
// WAKIL_SKILLS_DB, when set, replaces this entirely (bench data isolation);
// it must be an absolute, clean path with an existing parent directory
// that is not a symlink, and the file itself (if it exists) a regular
// file and not a symlink. An invalid value is an error — never a silent
// fallback to the real store. When unset, behaviour is byte-for-byte the
// default resolution. Returns "" with a nil error if the data directory
// cannot be determined. The directory is NOT created here — memory.Open
// creates it.
func SkillDBPath() (string, error) {
	if v := os.Getenv("WAKIL_SKILLS_DB"); v != "" {
		if err := validateSkillsDBOverride(v); err != nil {
			return "", err
		}
		return v, nil
	}
	dataDir := stagingDataRoot()
	if dataDir == "" {
		return "", nil
	}
	return filepath.Join(dataDir, "skills", "skills.db"), nil
}

// validateSkillsDBOverride checks the WAKIL_SKILLS_DB value: absolute,
// clean path, existing parent directory, and (if it exists) a regular file,
// not a symlink. Only the leaf and its immediate parent are checked —
// deeper ancestors (e.g. a symlinked /tmp) are not a reason to refuse.
// Every error names the variable so a mis-set value is diagnosable at
// startup instead of silently opening the real store.
func validateSkillsDBOverride(v string) error {
	if !filepath.IsAbs(v) {
		return fmt.Errorf("WAKIL_SKILLS_DB must be an absolute path: %q", v)
	}
	if filepath.Clean(v) != v {
		return fmt.Errorf("WAKIL_SKILLS_DB must be a clean path (no ./ or ../ elements): %q", v)
	}
	parent := filepath.Dir(v)
	pinfo, err := os.Stat(parent)
	if err != nil {
		return fmt.Errorf("WAKIL_SKILLS_DB parent directory does not exist: %q: %w", parent, err)
	}
	if !pinfo.IsDir() {
		return fmt.Errorf("WAKIL_SKILLS_DB parent is not a directory: %q", parent)
	}
	lpinfo, err := os.Lstat(parent)
	if err != nil {
		return fmt.Errorf("WAKIL_SKILLS_DB parent: %w", err)
	}
	if lpinfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("WAKIL_SKILLS_DB parent is a symlink: %q", parent)
	}
	linfo, err := os.Lstat(v)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("WAKIL_SKILLS_DB: %w", err)
	}
	if linfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("WAKIL_SKILLS_DB is a symlink: %q", v)
	}
	if !linfo.Mode().IsRegular() {
		return fmt.Errorf("WAKIL_SKILLS_DB is not a regular file: %q", v)
	}
	return nil
}

// SessionHistoryDBPath returns the host-side path to the session-history index
// SQLite database for the given workspace. Like MemoryDBPath it is
// workspace-keyed (same SHA-256-derived short key). Path:
// <wakil-data-dir>/sessionhistory/<short-key>/sessions.db
//
// Returns "" if the data directory cannot be determined or the workspace key
// is empty. The directory is NOT created here — sessionhistory.Open creates it.
func SessionHistoryDBPath(ws string) string {
	key := stagingKey(ws)
	if key == "" {
		return ""
	}
	dataDir := stagingDataRoot()
	if dataDir == "" {
		return ""
	}
	return filepath.Join(dataDir, "sessionhistory", key, "sessions.db")
}

// SessionHostDBPath returns the host-side path to the session-host event-log
// SQLite database for the given workspace. Like MemoryDBPath it is
// workspace-keyed (same SHA-256-derived short key). Path:
// <wakil-data-dir>/sessionhost/<short-key>/sessionhost.db
//
// This is the P1 SQLite-backed event store (D3). It replaces the P0
// in-memory MemLog for production use. Returns "" if the data directory cannot
// be determined or the workspace key is empty. The directory is NOT created
// here — sqlstore.NewSQLiteStore creates it.
func SessionHostDBPath(ws string) string {
	key := stagingKey(ws)
	if key == "" {
		return ""
	}
	dataDir := stagingDataRoot()
	if dataDir == "" {
		return ""
	}
	return filepath.Join(dataDir, "sessionhost", key, "sessionhost.db")
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
