package wiring

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/sandbox/hisn"
	"github.com/treeol/wakil/internal/sandbox/project"
)

// loadProjectSurface reads <workspace>/.wakil/sandbox.json and returns the
// surface to start hisn with. A file that asks for anything is honoured ONLY
// when the user approved exactly this content (see package project): an agent
// that wrote it cannot approve it. The file itself is masked inside the sandbox
// when it exists (defence in depth; the executor refuses writes to it too).
func loadProjectSurface(cfg config.Config) (hisn.Surface, project.Loaded, error) {
	ws := cfg.HostWorkDir
	l, err := project.Load(ws)
	if err != nil {
		return hisn.Surface{}, l, err
	}
	if l.Needs() {
		key := agent.WorkspaceKey(cfg.WorkspacePath())
		if !project.Approved(filepath.Join(hisnStateDir(), "hisn"), key, l.Hash) {
			return hisn.Surface{}, l, &project.ErrNotApproved{Path: l.Path}
		}
	}
	s := l.Surface
	if l.Present && s.Profile == "" {
		s.Mask = []string{l.Path}
	}
	return s, l, nil
}

// SandboxTrust shows the project's sandbox file and, after an interactive
// "yes" read from the controlling terminal, records its approval. There is
// deliberately no flag that skips the question and no non-terminal path: the
// approval is the user's decision, and a process an agent can drive must not
// be able to make it.
func SandboxTrust(out io.Writer, dir string, openTTY func() (io.ReadCloser, error)) error {
	if dir == "" {
		dir = "."
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	ws, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return fmt.Errorf("workspace %q: %w", dir, err)
	}
	l, err := project.Load(ws)
	if err != nil {
		return err
	}
	if !l.Needs() {
		fmt.Fprintf(out, "%s needs no approval (absent, empty or asks for nothing).\n", project.RelPath)
		return nil
	}
	key := agent.WorkspaceKey(ws)
	if key == "" {
		return fmt.Errorf("cannot derive a workspace key for %s", ws)
	}
	stateDir := filepath.Join(hisnStateDir(), "hisn")
	if project.Approved(stateDir, key, l.Hash) {
		fmt.Fprintf(out, "%s is already approved (sha256 %s).\n", project.RelPath, l.Hash[:16])
		return nil
	}
	fmt.Fprintf(out, "%s in %s\n\n%s\n\nIt asks for:\n", project.RelPath, ws, strings.TrimSpace(string(l.Raw)))
	for _, line := range project.Summary(l) {
		fmt.Fprintf(out, "  - %s\n", line)
	}
	fmt.Fprintf(out, "\nsha256 %s\nApprove this exact content for this workspace? Type 'yes' to approve: ", l.Hash)
	tty, err := openTTY()
	if err != nil {
		fmt.Fprintln(out)
		return fmt.Errorf("approval needs an interactive terminal (/dev/tty): %w", err)
	}
	defer tty.Close()
	line, _ := bufio.NewReader(tty).ReadString('\n')
	if strings.TrimSpace(line) != "yes" {
		fmt.Fprintln(out, "not approved.")
		return fmt.Errorf("not approved")
	}
	if err := project.Approve(stateDir, key, l.Hash); err != nil {
		return err
	}
	fmt.Fprintln(out, "approved.")
	return nil
}

// OpenControllingTTY opens /dev/tty for the approval answer.
func OpenControllingTTY() (io.ReadCloser, error) { return os.Open("/dev/tty") }
