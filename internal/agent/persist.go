package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// resolveDataDir resolves a wakil data subdirectory from environment variables
// or the XDG data home. It checks, in order:
//  1. envKey (e.g. WAKIL_SESSIONS_DIR, WAKIL_REPO_STATE_DIR) — used as-is.
//  2. $XDG_DATA_HOME/wakil/subdir
//  3. ~/.local/share/wakil/subdir
//
// Returns "" if none can be resolved (os.UserHomeDir failed). subdir is the
// final path component (e.g. "sessions", "repo-state").
func resolveDataDir(envKey, subdir string) string {
	if x := os.Getenv(envKey); x != "" {
		return x
	}
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "wakil", subdir)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "wakil", subdir)
}

// atomicWriteJSON marshals v as indented JSON and writes it to path using a
// crash-durable atomic replace: the data is written to a temp file in the same
// directory (so rename is an atomic move on the same filesystem), fsync'd to
// stable storage, then renamed over the target path. The parent directory is
// best-effort synced so the rename itself is durable against a power loss
// immediately after the function returns.
//
// Guarantees:
//   - The target file is never partially written — a crash mid-write leaves
//     the old file intact (or absent on first write) and the temp file orphaned.
//   - After the function returns successfully, the data is fsync'd to stable
//     storage and the rename is recorded in the parent directory (best-effort
//     dir sync; dir sync may silently fail on filesystems that don't support
//     fsync on directories, which is acceptable — the file content is durable
//     even if the rename's visibility to a post-crash readdir is not).
//
// The temp file is created via os.CreateTemp (random name) in the same
// directory as the target, so two concurrent writers to the same path don't
// race on a fixed temp filename. The write itself is still last-writer-wins —
// callers needing read-modify-write atomicity must serialize externally.
// On any error the temp file is removed before returning.
//
// The file is created with 0644 permissions (matching the previous
// os.WriteFile(tmp, b, 0o644) behavior — os.CreateTemp defaults to 0600,
// so an explicit Chmod is applied before the fsync).
// The parent directory must already exist (call os.MkdirAll before calling
// this).
func atomicWriteJSON(path string, v interface{}) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	// os.CreateTemp creates files with mode 0600; explicitly set 0644 to
	// match the previous os.WriteFile(tmp, b, 0o644) behavior so the
	// migration doesn't silently tighten permissions on existing files.
	if err := tmp.Chmod(0o644); err != nil {
		cleanup()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	// Write and fsync the data to stable storage before renaming.
	if _, err := tmp.Write(b); err != nil {
		cleanup()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close temp file: %w", err)
	}
	// Rename the temp file over the target. On POSIX this is an atomic
	// operation: the target is either the old file or the new file, never
	// a mix.
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename temp file: %w", err)
	}
	// Best-effort: fsync the parent directory so the rename is durable.
	// On filesystems or platforms that don't support directory fsync, this
	// silently fails — the file content is already durable, only the
	// rename's visibility to a post-crash readdir is at risk.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}
