package exec

// L1 (0.4.4): helper commands whose output Wakil parses run under LC_ALL=C;
// the model's own commands keep the user's locale; "not found" is decided by
// exit status, not by English stderr text.

import (
	"context"
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDockerExecArgvPinsLocaleForHelpersOnly(t *testing.T) {
	helper := strings.Join(dockerExecArgv("c1", false, true, []string{"sh", "-c", "x"}), " ")
	if !strings.Contains(helper, "-e LC_ALL=C") || !strings.Contains(helper, "-e LANG=C") {
		t.Errorf("helper argv must pin the locale: %s", helper)
	}
	if !strings.HasPrefix(helper, "exec -e LC_ALL=C -e LANG=C c1 sh -c x") {
		t.Errorf("env flags must precede the container name: %s", helper)
	}
	user := strings.Join(dockerExecArgv("c1", false, false, []string{"sh", "-c", "x"}), " ")
	if strings.Contains(user, "LC_ALL") || strings.Contains(user, "LANG") {
		t.Errorf("user-command argv must not touch the locale: %s", user)
	}
	if got := strings.Join(dockerExecArgv("c1", true, true, []string{"a"}), " "); !strings.HasPrefix(got, "exec -i -e LC_ALL=C") {
		t.Errorf("interactive helper argv: %s", got)
	}
}

// Backend executor: the helper script runs with LC_ALL=C; RunShell (the user's
// command) sees the user's locale untouched.
func TestBackendExecutor_HelperLocalePinnedUserLocaleKept(t *testing.T) {
	t.Setenv("LC_ALL", "de_CH.UTF-8")
	t.Setenv("LANG", "de_CH.UTF-8")
	ex, _ := newTestBackendExec(t)
	ctx := context.Background()
	if err := ex.BindChat(ctx, "c"); err != nil {
		t.Fatal(err)
	}
	got, err := ex.sh(ctx, `printf %s "$LC_ALL/$LANG"`)
	if err != nil || got != "C/C" {
		t.Errorf("helper locale = %q, %v; want C/C", got, err)
	}
	got, err = ex.RunShell(ctx, `printf %s "$LC_ALL/$LANG"`)
	if err != nil || got != "de_CH.UTF-8/de_CH.UTF-8" {
		t.Errorf("user command locale = %q, %v; want the user's de_CH.UTF-8", got, err)
	}
}

// germanShims puts cat/head/stat shims first in PATH that print a German
// "not found" on a missing file regardless of the locale — the hardest case:
// stderr text can never match, only the exit status can classify it.
func germanShims(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	for _, tool := range []string{"cat", "head", "stat"} {
		real, err := osexec.LookPath(tool)
		if err != nil {
			t.Skipf("no %s", tool)
		}
		// No file operand (cat reading stdin) or an existing file: the real tool.
		script := "#!/bin/sh\nlast=\nfor a; do last=$a; done\n" +
			"if [ -z \"$last\" ] || [ -e \"$last\" ]; then exec " + real + " \"$@\"; fi\n" +
			"echo \"" + tool + ": '$last': Datei oder Verzeichnis nicht gefunden\" >&2\nexit 1\n"
		if err := os.WriteFile(filepath.Join(dir, tool), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// Regression for the documented failure: TestBackendExecutorLazyStartAndFileOps
// failed under a German locale because "not found" was matched in English.
func TestBackendExecutor_NotFoundUnderGermanStderr(t *testing.T) {
	germanShims(t)
	ex, _ := newTestBackendExec(t)
	ctx := context.Background()
	if err := ex.BindChat(ctx, "c"); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.WriteFile(ctx, "present.txt", "hi"); err != nil {
		t.Fatal(err)
	}
	if got, err := ex.ReadFile(ctx, "present.txt"); err != nil || got != "hi" {
		t.Fatalf("present file: %q, %v", got, err)
	}
	if _, err := ex.ReadFile(ctx, "missing"); !errors.Is(err, ErrFileNotFound) {
		t.Errorf("ReadFile(missing) = %v, want ErrFileNotFound", err)
	}
	if _, _, err := ex.ReadFileBounded(ctx, "missing", 10); !errors.Is(err, ErrFileNotFound) {
		t.Errorf("ReadFileBounded(missing) = %v, want ErrFileNotFound", err)
	}
	if _, err := ex.StatFile(ctx, "missing"); !errors.Is(err, ErrFileNotFound) {
		t.Errorf("StatFile(missing) = %v, want ErrFileNotFound", err)
	}
	abs := filepath.Join(ex.WorkspaceRoot(), "missing")
	if _, err := ex.FileMode(ctx, abs); !errors.Is(err, ErrFileNotFound) {
		t.Errorf("FileMode(missing) = %v, want ErrFileNotFound", err)
	}
	// Not "not found": a directory read must keep its own error, not be
	// misclassified by the status probe.
	if err := os.Mkdir(filepath.Join(ex.WorkspaceRoot(), "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.ReadFile(ctx, "adir"); errors.Is(err, ErrFileNotFound) {
		t.Errorf("ReadFile(dir) must not be ErrFileNotFound: %v", err)
	}
}
