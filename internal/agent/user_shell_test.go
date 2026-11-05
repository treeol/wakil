package agent

// T4 (0.4.4): run_shell / run_background run the model's command under bash
// when the sandbox has bash, sh otherwise; the harness still reports the real
// exit code for bash-only constructs.

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	wexec "github.com/treeol/wakil/internal/exec"
	"github.com/treeol/wakil/internal/proxy"
)

func needBash(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
}

func TestBashCommandQuoting(t *testing.T) {
	needBash(t)
	for _, c := range []string{
		`echo 'single' "double" $HOME`,
		"echo it's",
		"echo a\\",
		"cat <<'EOF'\nheredoc $x\nEOF",
		`printf '%s\n' a b`,
	} {
		want, _ := exec.Command("bash", "-c", c).CombinedOutput()
		got, _ := exec.Command("sh", "-c", wexec.BashCommand(c)).CombinedOutput()
		if string(got) != string(want) {
			t.Errorf("BashCommand(%q): got %q want %q", c, got, want)
		}
	}
}

// The spec's example: PIPESTATUS is bash-only. Under sh it is empty; under
// bash it prints the status of the first pipeline element. The command's own
// exit status (last element of the pipeline, then echo) stays separate.
func TestRunShell_BashConstructsWork(t *testing.T) {
	needBash(t)
	app := newRetentionApp(t)
	got := callTool(app, "run_shell", `{"command":"false | true; echo ${PIPESTATUS[0]}"}`)
	if !strings.Contains(got, "1") || strings.HasPrefix(got, "ERROR") {
		t.Errorf("PIPESTATUS[0] should print 1 under bash, got %q", got)
	}
	got = callTool(app, "run_shell", `{"command":"[[ -d . ]] && arr=(a b c) && echo ${#arr[@]}"}`)
	if strings.TrimSpace(got) != "3" {
		t.Errorf("[[ ]] / arrays: got %q", got)
	}
	// A failing command still reports its real exit status.
	got = callTool(app, "run_shell", `{"command":"[[ 1 -eq 2 ]]"}`)
	if !strings.Contains(got, "exit status 1") {
		t.Errorf("failing [[ ]] should report exit status 1: %q", got)
	}
}

// Background path: the marker records the real code of bash constructs.
func TestRunBackground_BashExitCodeThroughMarker(t *testing.T) {
	needBash(t)
	app := newRetentionApp(t)
	id := startBgAndWait(t, app, "false | true; (exit ${PIPESTATUS[0]}) ; arr=(x y); [[ ${#arr[@]} -eq 2 ]] && exit 5")
	got := callTool(app, "read_process_log", fmt.Sprintf(`{"id":%q}`, id))
	if !strings.Contains(got, "exited (code 5)") {
		t.Errorf("want code 5 via marker, got: %s", got)
	}
}

// With bash present a syntax error is reported by bash (exit 2) and the marker
// is written — not "code unknown" as with a bare sh parse failure.
func TestRunBackground_BashSyntaxErrorIsCode2(t *testing.T) {
	needBash(t)
	app := newRetentionApp(t)
	id := startBgAndWait(t, app, "if then")
	got := callTool(app, "read_process_log", fmt.Sprintf(`{"id":%q}`, id))
	if !strings.Contains(got, "exited (code 2)") {
		t.Errorf("want code 2 for a bash syntax error, got: %s", got)
	}
}

// No bash: the command reaches RunShell unchanged (sh) — fake executor.
type shExec struct {
	*fakeExecutor
	shell string
}

func (s shExec) UserShell(context.Context) string { return s.shell }
func (s shExec) PeekUserShell() string            { return s.shell }

func TestUserCommand_FallbackAndWrap(t *testing.T) {
	ctx := context.Background()
	if got := wexec.UserCommand(ctx, shExec{newFakeExecutor(), "sh"}, "echo hi"); got != "echo hi" {
		t.Errorf("sh: command must pass unchanged, got %q", got)
	}
	if got := wexec.UserCommand(ctx, shExec{newFakeExecutor(), "bash"}, "echo hi"); got != "bash -c 'echo hi'" {
		t.Errorf("bash: got %q", got)
	}
	if got := wexec.UserCommand(ctx, newFakeExecutor(), "echo hi"); got != "echo hi" {
		t.Errorf("executor without a prober must pass unchanged, got %q", got)
	}
}

func TestToolDescriptionNamesShell(t *testing.T) {
	tools := []proxy.Tool{
		{Type: "function", Function: proxy.ToolFunction{Name: "run_shell", Description: "Run it."}},
		{Type: "function", Function: proxy.ToolFunction{Name: "read_file", Description: "Read."}},
	}
	app := &App{Exec: shExec{newFakeExecutor(), "bash"}}
	got := app.toolsWithShellNote(tools)
	if !strings.Contains(got[0].Function.Description, "under bash") {
		t.Errorf("run_shell description: %q", got[0].Function.Description)
	}
	if got[1].Function.Description != "Read." {
		t.Errorf("other tools must be untouched: %q", got[1].Function.Description)
	}
	if tools[0].Function.Description != "Run it." {
		t.Error("a.Tools must not be mutated")
	}
	// Idempotent, and no note before the probe has run.
	again := app.toolsWithShellNote(got)
	if strings.Count(again[0].Function.Description, "under bash") != 1 {
		t.Errorf("note duplicated: %q", again[0].Function.Description)
	}
	app.Exec = shExec{newFakeExecutor(), ""}
	if app.toolsWithShellNote(tools)[0].Function.Description != "Run it." {
		t.Error("before the probe the description must stay generic")
	}
}
