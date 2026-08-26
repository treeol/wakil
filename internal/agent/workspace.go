package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
)

// canonicalWorkspace resolves ws to a stable, absolute, symlink-evaluated
// form. Falls back to Abs alone when EvalSymlinks fails (e.g. the directory
// doesn't exist yet, or no longer exists) so a missing/racy path never
// breaks resolution. Returns "" for an empty ws.
//
// This is the single source of truth for "what folder does this session/
// setting belong to" — repo-state (repostate.go) and session scoping
// (session.go) both resolve through this so they always agree.
func canonicalWorkspace(ws string) string {
	if ws == "" {
		return ""
	}
	resolved := ws
	if abs, err := filepath.Abs(ws); err == nil {
		resolved = abs
		if real, err := filepath.EvalSymlinks(abs); err == nil {
			resolved = real
		}
	}
	return resolved
}

// workspaceKey returns the SHA-256 hex digest of ws's canonical form, for use
// as a stable comparison key or filename. Returns "" for an empty ws.
//
// WAKIL_WORKSPACE_KEY override: when the env var is set to a 64-char hex
// string (a pre-computed SHA-256 key), it is returned directly without
// canonicalization or hashing. This is used by the Docker daemon path:
// the container cannot EvalSymlinks on the host path (it doesn't exist
// inside the container), so the host TUI computes the key and passes it
// via WAKIL_WORKSPACE_KEY. This guarantees host and container produce
// the same workspace identity regardless of symlinks.
func workspaceKey(ws string) string {
	if key := os.Getenv("WAKIL_WORKSPACE_KEY"); key != "" {
		return key
	}
	c := canonicalWorkspace(ws)
	if c == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(c))
	return hex.EncodeToString(sum[:])
}

// sameWorkspace reports whether a and b resolve to the same canonical
// workspace. Two empty strings are NOT considered the same workspace (both
// resolve to "", and empty means "no workspace known" — treating two
// unknowns as equal would silently match unrelated sessions with no
// recorded workspace).
func sameWorkspace(a, b string) bool {
	ka, kb := workspaceKey(a), workspaceKey(b)
	if ka == "" || kb == "" {
		return false
	}
	return ka == kb
}
