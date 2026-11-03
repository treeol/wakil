package exec

import (
	"errors"
	"fmt"
	"path/filepath"
)

// ProtectedSandboxFile is the per-project sandbox configuration, relative to
// the workspace root. It is the USER's decision (services, forwards, mounts,
// risk acknowledgement): the agent may read it but never change it. Every
// mutating Executor operation refuses it, so file tools and edit-tier
// subagents (which all go through the executor) are covered by one choke
// point. Shell commands are covered separately (hisn masks the file; see
// docs/sandbox-backends.md) and, above all, by the approval gate: a changed
// file does not take effect until the user approves it outside the agent.
const ProtectedSandboxFile = ".wakil/sandbox.json"

// ErrProtectedPath is returned for a write, delete or move that would touch a
// protected file (or move/delete the directory that holds it).
var ErrProtectedPath = errors.New("protected path")

func protectedErr(rel string) error {
	return fmt.Errorf("%w: %s is the user's sandbox configuration; the agent may not modify it (edit it yourself, then run `wakil sandbox trust`)", ErrProtectedPath, rel)
}

// protectedKind classifies path against root: 1 = the protected file, 2 = its
// directory (.wakil), 0 = neither. Both paths are cleaned; a relative path is
// taken relative to root.
func protectedKind(root, path string) int {
	if root == "" || path == "" {
		return 0
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	path = filepath.Clean(path)
	file := filepath.Join(filepath.Clean(root), filepath.FromSlash(ProtectedSandboxFile))
	switch path {
	case file:
		return 1
	case filepath.Dir(file):
		return 2
	}
	return 0
}

// IsProtectedPath reports whether path (absolute, or relative to root) is the
// protected sandbox file.
func IsProtectedPath(root, path string) bool { return protectedKind(root, path) == 1 }

// guardWrite refuses writing path. Deleting or moving the directory is checked
// by guardRemove.
func guardWrite(root, path string) error {
	if protectedKind(root, path) == 1 {
		return protectedErr(ProtectedSandboxFile)
	}
	return nil
}

// guardRemove refuses deleting or moving (either end of a move) the protected
// file or the directory that holds it: renaming `.wakil` away would let a
// fresh sandbox.json appear in its place.
func guardRemove(root string, paths ...string) error {
	for _, p := range paths {
		if protectedKind(root, p) != 0 {
			return protectedErr(ProtectedSandboxFile)
		}
	}
	return nil
}

// resolvedGuard additionally resolves symlinks on the host (direct mode,
// where the executor sees the real filesystem) before comparing, so a link to
// the file is refused too.
func resolvedGuard(root string, remove bool, paths ...string) error {
	rroot := root
	if r, err := filepath.EvalSymlinks(root); err == nil {
		rroot = r
	}
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, p)
		}
		for _, cand := range []string{p, resolveForGuard(p)} {
			for _, r := range []string{root, rroot} {
				k := protectedKind(r, cand)
				if k == 1 || (remove && k == 2) {
					return protectedErr(ProtectedSandboxFile)
				}
			}
		}
	}
	return nil
}

func resolveForGuard(p string) string {
	if r, err := resolveExistingAncestor(filepath.Clean(p)); err == nil {
		return r
	}
	return p
}
