package agent

// T3 (0.4.4): read_file / read_file_full may read the sandbox's /tmp, read-only.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/exec"
)

func TestIsSandboxTmpPath(t *testing.T) {
	for p, want := range map[string]bool{
		"/tmp":               true,
		"/tmp/x":             true,
		"/tmp/a/../b":        true,
		"/tmp/../etc/passwd": false,
		"/tmpx/y":            false,
		"/tmp2":              false,
		"tmp/x":              false,
		"/etc/passwd":        false,
		"/tmp/../tmp/../etc": false,
		"/var/tmp/x":         false,
	} {
		if got := isSandboxTmpPath(p); got != want {
			t.Errorf("isSandboxTmpPath(%q) = %v, want %v", p, got, want)
		}
	}
}

// tmpReadApp builds an App over a real DirectExecutor whose workspace is NOT
// under /tmp, so the /tmp exception (not workspace membership) is what is tested.
func tmpReadApp(t *testing.T) (*App, string) {
	t.Helper()
	if st, err := os.Stat("/tmp"); err != nil || !st.IsDir() {
		t.Skip("no /tmp")
	}
	ws, err := os.MkdirTemp("/tmp", "t3-ws-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(ws) })
	// Workspace must be a different directory than the files under test, and
	// the files live in a sibling dir under /tmp (outside the workspace root).
	root := filepath.Join(ws, "project")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	scratch, err := os.MkdirTemp("/tmp", "t3-scratch-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(scratch) })
	exe, err := exec.NewDirectExecutor(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { exe.Close() })
	return &App{
		Exec:    sandboxedTmp{exe},
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
		Cfg:     config.DefaultConfig(),
	}, scratch
}

// sandboxedTmp makes a DirectExecutor report a sandbox-owned /tmp, standing in
// for the docker/backend executors in these tests.
type sandboxedTmp struct{ exec.Executor }

func (sandboxedTmp) SandboxTmpReadable() bool { return true }

// Under the direct executor /tmp is the HOST's and read_file is ungated: the
// exception must not apply.
func TestReadFile_TmpNotReadableUnderDirectExecutor(t *testing.T) {
	app, scratch := tmpReadApp(t)
	app.Exec = app.Exec.(sandboxedTmp).Executor // the plain DirectExecutor
	f := filepath.Join(scratch, "host.txt")
	if err := os.WriteFile(f, []byte("host-tmp"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := callTool(app, "read_file", fmt.Sprintf(`{"path":%q}`, f)); !strings.Contains(got, "outside workspace") {
		t.Errorf("direct mode must keep rejecting /tmp: %s", got)
	}
}

// Regression for the documented incident: read_file rejected /tmp/... as
// "outside workspace".
func TestReadFile_SandboxTmpReadable(t *testing.T) {
	app, scratch := tmpReadApp(t)
	f := filepath.Join(scratch, "note.txt")
	if err := os.WriteFile(f, []byte("tmp-content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"read_file", "read_file_full"} {
		got := callTool(app, tool, fmt.Sprintf(`{"path":%q}`, f))
		if !strings.Contains(got, "tmp-content") || strings.HasPrefix(got, "ERROR") {
			t.Errorf("%s of %s: %s", tool, f, got)
		}
	}
}

func TestReadFile_TmpExceptionIsReadOnlyAndFailClosed(t *testing.T) {
	app, scratch := tmpReadApp(t)
	secret := filepath.Join(scratch, "target.txt")
	if err := os.WriteFile(secret, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Symlink under /tmp pointing outside /tmp: rejected.
	link := filepath.Join(scratch, "link")
	if err := os.Symlink("/etc/hostname", link); err != nil {
		t.Fatal(err)
	}
	// Symlink under /tmp pointing at another /tmp file: allowed.
	okLink := filepath.Join(scratch, "oklink")
	if err := os.Symlink(secret, okLink); err != nil {
		t.Fatal(err)
	}

	if got := callTool(app, "read_file", fmt.Sprintf(`{"path":%q}`, link)); !strings.Contains(got, "outside workspace") {
		t.Errorf("symlink out of /tmp must be rejected: %s", got)
	}
	if got := callTool(app, "read_file", fmt.Sprintf(`{"path":%q}`, okLink)); strings.HasPrefix(got, "ERROR") {
		t.Errorf("symlink within /tmp must be readable: %s", got)
	}
	for _, p := range []string{"/tmp/../etc/hostname", "/tmpx/y", "/etc/hostname"} {
		if got := callTool(app, "read_file", fmt.Sprintf(`{"path":%q}`, p)); !strings.Contains(got, "outside workspace") {
			t.Errorf("read_file %s must be rejected as outside workspace: %s", p, got)
		}
	}
	// Writes keep the workspace confinement.
	w := filepath.Join(scratch, "new.txt")
	if got := callTool(app, "write_file", fmt.Sprintf(`{"path":%q,"content":"nope"}`, w)); !strings.Contains(got, "outside workspace") {
		t.Errorf("write_file into /tmp must stay rejected: %s", got)
	}
	if _, err := os.Stat(w); err == nil {
		t.Error("write_file created a file outside the workspace")
	}
}
