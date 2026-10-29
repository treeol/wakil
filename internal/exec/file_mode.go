package exec

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
)

// FileModer is an optional Executor capability: read and set permission bits
// of a workspace file. Implemented by DirectExecutor and DockerExecutor (and
// therefore by the docker worktree executor, which embeds *DockerExecutor and
// works on absolute, pre-confined paths). Callers type-assert and degrade
// gracefully when the executor does not implement it (test fakes).
//
// All methods require ABSOLUTE paths: the Docker implementations run without
// a cd into the workspace, so a relative path would resolve against the
// container's working directory (and the worktree executor's root differs
// from the parent's anyway).
//
// FileMode follows symlinks (callers pass ConfinePath'd canonical paths) and
// returns the permission bits plus a type indicator: 0 for a regular file,
// fs.ModeDir for a directory, fs.ModeIrregular for anything else (FIFO,
// device, socket). Missing files yield an ErrFileNotFound-wrapped error.
// Chmod sets only the permission bits (0o777).
//
// EntryExists reports whether a directory entry exists at path WITHOUT
// following a final symlink (a dangling symlink exists). Any probe failure
// other than "no such entry" is an error, never "absent".
type FileModer interface {
	FileMode(ctx context.Context, path string) (fs.FileMode, error)
	Chmod(ctx context.Context, path string, mode fs.FileMode) error
	EntryExists(ctx context.Context, path string) (bool, error)
}

// EntryExists reports whether path has a directory entry (lstat semantics).
func (e *DirectExecutor) EntryExists(_ context.Context, path string) (bool, error) {
	_, err := os.Lstat(e.resolve(path))
	switch {
	case err == nil:
		return true, nil
	case os.IsNotExist(err):
		return false, nil
	default:
		return false, err
	}
}

// EntryExists reports whether path has a directory entry inside the
// container. `[ -e ] || [ -L ]` exits 1 only when neither holds; any other
// exit status (docker failure, shell error) is an error.
func (d *DockerExecutor) EntryExists(ctx context.Context, path string) (bool, error) {
	out, err := d.execCtx(ctx, false, "sh", "-c", `[ -e "$1" ] || [ -L "$1" ]`, "sh", path)
	if err == nil {
		return true, nil
	}
	var ee interface{ ExitCode() int }
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("probe %s: %s: %w", path, strings.TrimSpace(out), err)
}

// FileMode returns the permission bits and type indicator of path.
func (e *DirectExecutor) FileMode(_ context.Context, path string) (fs.FileMode, error) {
	info, err := os.Stat(e.resolve(path))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, fmt.Errorf("%w: %s", ErrFileNotFound, path)
		}
		return 0, err
	}
	return normalizeMode(info.Mode()), nil
}

// Chmod sets the permission bits of path.
func (e *DirectExecutor) Chmod(_ context.Context, path string, mode fs.FileMode) error {
	return os.Chmod(e.resolve(path), mode.Perm())
}

// FileMode returns the permission bits and type indicator of path inside the
// container. %f is the raw st_mode in hex (type + permission bits).
func (d *DockerExecutor) FileMode(ctx context.Context, path string) (fs.FileMode, error) {
	out, err := d.execCtx(ctx, false, "stat", "-L", "-c", "%f", "--", path)
	if err != nil {
		msg := strings.TrimSpace(out)
		if isShellNotFound(msg) {
			return 0, fmt.Errorf("%w: %s", ErrFileNotFound, path)
		}
		return 0, fmt.Errorf("stat %s: %s", path, msg)
	}
	return parseRawMode(strings.TrimSpace(out))
}

// Chmod sets the permission bits of path inside the container. The mode and
// path are passed as argv (no shell), so no quoting is involved.
func (d *DockerExecutor) Chmod(ctx context.Context, path string, mode fs.FileMode) error {
	out, err := d.execCtx(ctx, false, "chmod", fmt.Sprintf("%04o", uint32(mode.Perm())), "--", path)
	if err != nil {
		return fmt.Errorf("chmod %s: %s", path, strings.TrimSpace(out))
	}
	return nil
}

// normalizeMode maps an fs.FileMode onto the FileModer contract.
func normalizeMode(m fs.FileMode) fs.FileMode {
	switch {
	case m.IsRegular():
		return m.Perm()
	case m.IsDir():
		return fs.ModeDir | m.Perm()
	default:
		return fs.ModeIrregular | m.Perm()
	}
}

// parseRawMode parses stat's %f output (hex st_mode, e.g. "81ed").
func parseRawMode(s string) (fs.FileMode, error) {
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return 0, fmt.Errorf("unexpected stat mode output %q", s)
	}
	perm := fs.FileMode(v & 0o777)
	switch v & 0o170000 {
	case 0o100000:
		return perm, nil
	case 0o040000:
		return fs.ModeDir | perm, nil
	default:
		return fs.ModeIrregular | perm, nil
	}
}
