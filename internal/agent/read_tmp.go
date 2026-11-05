package agent

import (
	"context"
	"path"
	"strings"

	"github.com/treeol/wakil/internal/exec"
)

// confineReadPath is ConfinePath for the READ tools (read_file, read_file_full)
// with one extension: absolute paths under the sandbox's own /tmp are readable,
// so the model can open the logs and scratch files its own shell commands
// wrote there (e.g. a background job's /tmp/wakil-bg-*.log).
//
// Scope: only executors whose /tmp is the sandbox's own (exec.SandboxTmpReader:
// docker, backend). The sandbox is the boundary there, and run_shell can already
// `cat` these files — but run_shell is gated and read_file is not, so the
// exception is limited to the sandbox's private /tmp and never opens the
// host's (DirectExecutor). It is read-only: every write, edit, move, delete,
// list_dir and search_files call still goes through Exec.ConfinePath, which is
// unchanged. Fail closed: if ConfinePath rejects and the path is not literally
// /tmp or under /tmp/ (after Clean, so no ".."), the original error is returned;
// a failed, empty or ambiguous canonicalisation, or a canonical path that left
// /tmp (a symlink to elsewhere), is also the original error.
//
// The canonical path is resolved inside the sandbox with one `readlink -f`
// call that also resolves /tmp itself, so a /tmp that is a symlink
// (macOS: /private/tmp) is compared against its own canonical form.
func (a *App) confineReadPath(ctx context.Context, p string) (string, error) {
	canonical, err := a.Exec.ConfinePath(ctx, p)
	if err == nil {
		return canonical, nil
	}
	if !isSandboxTmpPath(p) {
		return "", err
	}
	// Only where /tmp belongs to the sandbox. Under the direct executor /tmp is
	// the host's and read_file is ungated, so the exception does not apply.
	if r, ok := a.Exec.(exec.SandboxTmpReader); !ok || !r.SandboxTmpReadable() {
		return "", err
	}
	out, rerr := a.Exec.RunShell(ctx, "readlink -f -- "+shellQuote(path.Clean(p))+" && readlink -f -- /tmp")
	if rerr != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || lines[0] == "" || lines[1] == "" {
		return "", err
	}
	target, tmpRoot := lines[0], lines[1]
	if !path.IsAbs(target) || !path.IsAbs(tmpRoot) {
		return "", err
	}
	if target != tmpRoot && !strings.HasPrefix(target, strings.TrimSuffix(tmpRoot, "/")+"/") {
		return "", err
	}
	return target, nil
}

// isSandboxTmpPath reports whether p is an absolute path that is /tmp or lies
// under /tmp/ once cleaned (component boundary: /tmpx is not under /tmp).
func isSandboxTmpPath(p string) bool {
	if !strings.HasPrefix(p, "/") {
		return false
	}
	c := path.Clean(p)
	return c == "/tmp" || strings.HasPrefix(c, "/tmp/")
}
