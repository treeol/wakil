package agent

// L2 (0.4.4): apply_skill_template decides "target does not exist" without
// matching English stderr. Root cause of the documented failure: under a German
// locale the shell reports "Datei oder Verzeichnis nicht gefunden", which an
// English text match never recognised, so applying into an empty directory
// failed instead of creating the files.

import (
	"context"
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/exec"
	"github.com/treeol/wakil/internal/sandbox"
	"github.com/treeol/wakil/internal/sandbox/fake"
)

// germanShimsAgent puts cat/head/stat shims first in PATH that always report a
// missing file in German, whatever the locale.
func germanShimsAgent(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	for _, tool := range []string{"cat", "head", "stat"} {
		real, err := osexec.LookPath(tool)
		if err != nil {
			t.Skipf("no %s", tool)
		}
		script := "#!/bin/sh\nlast=\nfor a; do last=$a; done\n" +
			"if [ -z \"$last\" ] || [ -e \"$last\" ]; then exec " + real + " \"$@\"; fi\n" +
			"echo \"" + tool + ": '$last': Datei oder Verzeichnis nicht gefunden\" >&2\nexit 1\n"
		if err := os.WriteFile(filepath.Join(dir, tool), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// realShellRun runs each exec's argv with the real host tools, standing in for
// a sandbox whose userland speaks German.
func realShellRun(spec sandbox.ExecSpec) int {
	cmd := osexec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Stdin = spec.Stdin
	cmd.Stdout = spec.Stdout
	cmd.Stderr = spec.Stdout
	if err := cmd.Run(); err != nil {
		var ee *osexec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		return 127
	}
	return 0
}

func TestApplySkillTemplate_EmptyDirUnderGermanLocale(t *testing.T) {
	t.Setenv("LANG", "de_CH.UTF-8")
	t.Setenv("LC_ALL", "")
	germanShimsAgent(t)

	app := skillTestApp(t, false, nil)
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	be := fake.New(ws, ws)
	be.Run = realShellRun
	ex := exec.NewBackendExecutor(be, exec.BackendOptions{Label: "fake"})
	t.Cleanup(func() { _ = ex.Close() })
	if err := ex.BindChat(context.Background(), "chat-l2"); err != nil {
		t.Fatal(err)
	}
	app.Exec = ex
	confirms := 0
	app.Confirm = func(_, _, _ string, _ bool) bool { confirms++; return true }

	// Capture a template, then apply it into a directory that does not exist.
	res := captureDeploy(t, app, ws)
	if !strings.Contains(res, "saved template skill: swarm") {
		t.Fatalf("capture: %s", res)
	}
	confirms = 0 // capture prompts too; count the apply alone
	res = app.handleApplySkillTemplate(context.Background(), skillTC("apply_skill_template",
		`{"key":"swarm","params":{"STACK_NAME":"blog"},"target_dir":"out"}`))
	if !strings.Contains(res, "applied template skill swarm") {
		t.Fatalf("apply into an empty dir under LANG=de_CH.UTF-8 failed:\n%s", res)
	}
	want := strings.ReplaceAll(deployBody, "shop-prod", "blog")
	if got := readWS(t, ws, "out/deploy.sh"); got != want {
		t.Errorf("deploy.sh = %q, want %q", got, want)
	}
	if confirms != 1 {
		t.Errorf("confirms = %d, want 1", confirms)
	}
	// A second apply sees the files as existing (status, not text) and is a no-op.
	res = app.handleApplySkillTemplate(context.Background(), skillTC("apply_skill_template",
		`{"key":"swarm","params":{"STACK_NAME":"blog"},"target_dir":"out"}`))
	if !strings.HasPrefix(res, "no-op") {
		t.Errorf("re-apply: %s", res)
	}
}
