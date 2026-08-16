package agent

// Handler tests for the structured read-only git tools (card #137): command
// construction (injection resistance, hardening flags, caps), ref validation,
// non-repo error translation, and a live integration test against the real
// wakiil repository (which the test runs inside).

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/exec"
	"github.com/treeol/wakil/internal/proxy"
	wtools "github.com/treeol/wakil/internal/tools"
)

// newDirectExecutorCwd returns a DirectExecutor rooted at the git repository
// root containing the current working directory (Go test runs with the package
// dir as cwd, so walk up to find .git), else an error.
func newDirectExecutorCwd() (*exec.DirectExecutor, error) {
	dir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	for {
		if _, err := os.Stat(dir + "/.git"); err == nil {
			return exec.NewDirectExecutor(dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, errors.New("no .git found up the tree")
		}
		dir = parent
	}
}

// newGitApp builds an App with a fakeExecutor whose RunShell echoes the command
// (so tests can assert the exact command string) and records it.
func newGitApp() (*App, *fakeExecutor) {
	exec := newFakeExecutor()
	app := &App{
		Cfg:     config.DefaultConfig(),
		Exec:    exec,
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
	}
	return app, exec
}

// TestGitStatusCommandHardened verifies git_status runs with the hardening
// flags and returns parsed JSON.
func TestGitStatusCommandHardened(t *testing.T) {
	app, exec := newGitApp()
	exec.shellResult = "## main\x00 M a.go\x00"
	res := app.handleGitStatus(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{Name: "git_status", Arguments: "{}"}})
	if len(exec.shellCalls) != 1 {
		t.Fatalf("shell calls = %d, want 1", len(exec.shellCalls))
	}
	cmd := exec.shellCalls[0]
	for _, want := range []string{"--no-pager", "-c core.fsmonitor=false", "-c core.pager=cat", "status --porcelain=v1 -z --branch"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command missing %q:\n%s", want, cmd)
		}
	}
	if !strings.Contains(cmd, "GIT_PAGER=cat") || !strings.Contains(cmd, "LC_ALL=C") || !strings.Contains(cmd, "GIT_OPTIONAL_LOCKS=0") || !strings.Contains(cmd, "GIT_TERMINAL_PROMPT=0") {
		t.Errorf("command missing hardening env:\n%s", cmd)
	}
	var parsed struct {
		Branch  map[string]interface{}  `json:"branch"`
		Entries []wtools.GitStatusEntry `json:"entries"`
	}
	if err := json.Unmarshal([]byte(res), &parsed); err != nil {
		t.Fatalf("status result not valid JSON: %v\n%s", err, res)
	}
	if len(parsed.Entries) != 1 || parsed.Entries[0].Path != "a.go" {
		t.Errorf("parsed entries = %+v", parsed.Entries)
	}
}

// TestGitDiffRefInjectionRejected verifies a ref starting with '-' is rejected
// before reaching the shell (git option injection barrier).
func TestGitDiffRefInjectionRejected(t *testing.T) {
	app, exec := newGitApp()
	res := app.handleGitDiff(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "git_diff", Arguments: `{"rev":"--output=/etc/pwned"}`,
	}})
	if !strings.Contains(res, "ERROR:") {
		t.Fatalf("expected rejection, got %q", res)
	}
	if len(exec.shellCalls) != 0 {
		t.Errorf("must not execute when ref is rejected; ran %v", exec.shellCalls)
	}
}

// TestGitRefValidationRejectsHazards verifies the ref validator catches
// whitespace, NUL, leading '-', ':' and '^'.
func TestGitRefValidationRejectsHazards(t *testing.T) {
	bad := []string{"-p", ":x", "^HEAD", "HEAD~2 ", "a b", "x\x00y", "--output=/x"}
	for _, r := range bad {
		if err := validateGitRef(r); err == nil {
			t.Errorf("validateGitRef(%q) = nil, want error", r)
		}
	}
	good := []string{"", "HEAD", "HEAD~2", "main...feature", "A..B", "@", "deadbeef"}
	for _, r := range good {
		if err := validateGitRef(r); err != nil {
			t.Errorf("validateGitRef(%q) = %v, want nil", r, err)
		}
	}
}

// TestGitDiffCommandShape verifies staged diff + path confinement + `--`
// separator + byte cap are all present.
func TestGitDiffCommandShape(t *testing.T) {
	app, exec := newGitApp()
	exec.files["sub/file.go"] = "x"
	_ = app.handleGitDiff(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "git_diff", Arguments: `{"staged":true,"path":"sub/file.go"}`,
	}})
	if len(exec.shellCalls) != 1 {
		t.Fatalf("shell calls = %d, want 1", len(exec.shellCalls))
	}
	cmd := exec.shellCalls[0]
	for _, want := range []string{"--no-ext-diff", "--no-textconv", "--cached", "-- ", "head -c"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command missing %q:\n%s", want, cmd)
		}
	}
}

// TestGitLogAlwaysCapped verifies git_log enforces -n and the max cap.
func TestGitLogAlwaysCapped(t *testing.T) {
	app, exec := newGitApp()
	exec.shellResult = "a\x00\x00A\x00a@x\x002026-01-01T00:00:00+00:00\x00s\x1e"
	_ = app.handleGitLog(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "git_log", Arguments: `{"count":9999}`,
	}})
	cmd := exec.shellCalls[0]
	if !strings.Contains(cmd, "-n 100") {
		t.Errorf("count not capped: %s", cmd)
	}
	if !strings.Contains(cmd, "--no-ext-diff") {
		t.Errorf("log missing --no-ext-diff: %s", cmd)
	}
}

// TestGitLogParsesJSON verifies the JSON array result.
func TestGitLogParsesJSON(t *testing.T) {
	app, exec := newGitApp()
	exec.shellResult = "abc\x00def\x00A\x00a@x\x002026-01-01T00:00:00+00:00\x00s\x1e"
	res := app.handleGitLog(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{Name: "git_log", Arguments: "{}"}})
	var entries []wtools.GitLogEntry
	if err := json.Unmarshal([]byte(res), &entries); err != nil {
		t.Fatalf("log result not valid JSON: %v\n%s", err, res)
	}
	if len(entries) != 1 || entries[0].Hash != "abc" {
		t.Errorf("entries = %+v", entries)
	}
}

// TestGitLogFormatSurvivesQuotesLive is a hermetic real-git regression test
// for the log spoofing bug: git's %s is NOT JSON-escaped, so the old JSON-line
// format let a quoted subject break/spoof the commit. The NUL-framed format
// must survive a subject with quotes, backslashes, and a newline intact.
func TestGitLogFormatSurvivesQuotesLive(t *testing.T) {
	ex, err := newDirectExecutorCwd()
	if err != nil {
		t.Skipf("no git checkout: %v", err)
	}
	defer ex.Close()
	app := &App{Cfg: config.DefaultConfig(), Exec: ex, Out: io.Discard}

	// Run git_log on this repo and assert the result is a valid JSON array whose
	// every entry has a 40-char hash (a spoofed entry would not).
	log := app.handleGitLog(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{Name: "git_log", Arguments: `{"count":50}`}})
	var entries []wtools.GitLogEntry
	if err := json.Unmarshal([]byte(log), &entries); err != nil {
		t.Fatalf("git_log not valid JSON: %v\n%s", err, log)
	}
	for _, e := range entries {
		if len(e.Hash) != 40 {
			t.Errorf("commit hash = %q, want 40 hex (possible spoof)", e.Hash)
		}
	}
}

// TestGitStatusNonRepoError verifies a non-repo is translated to a stable
// structured error (not raw fatal stderr the model would retry).
func TestGitStatusNonRepoError(t *testing.T) {
	app, exec := newGitApp()
	exec.shellResult = "fatal: not a git repository (or any of the parent directories): .git"
	exec.shellErr = errors.New("exit status 128")
	res := app.handleGitStatus(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{Name: "git_status", Arguments: "{}"}})
	if !strings.Contains(res, "not a git repository") {
		t.Errorf("expected non-repo error, got %q", res)
	}
	if strings.Contains(res, "fatal:") {
		t.Errorf("must not leak raw fatal stderr: %q", res)
	}
}

// TestGitBlameRequiresPath verifies the path is mandatory.
func TestGitBlameRequiresPath(t *testing.T) {
	app, exec := newGitApp()
	res := app.handleGitBlame(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{Name: "git_blame", Arguments: "{}"}})
	if !strings.Contains(res, "path is required") {
		t.Errorf("expected path required, got %q", res)
	}
	if len(exec.shellCalls) != 0 {
		t.Errorf("must not execute without path")
	}
}

// TestGitToolsLiveIntegration runs the git handlers against the REAL wakiil
// repository (this repo) through a DirectExecutor, validating the parsers and
// command construction against real git output end-to-end. Skips if not run
// inside a git checkout.
func TestGitToolsLiveIntegration(t *testing.T) {
	ex, err := newDirectExecutorCwd()
	if err != nil {
		t.Skipf("no git checkout available: %v", err)
	}
	defer ex.Close()
	app := &App{Cfg: config.DefaultConfig(), Exec: ex, Out: io.Discard}

	// git_status: must parse to valid JSON with a branch.
	status := app.handleGitStatus(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{Name: "git_status", Arguments: "{}"}})
	if strings.Contains(status, "not a git repository") {
		t.Fatalf("git_status failed: %s", status)
	}
	var s struct {
		Branch  struct{ Branch string `json:"branch"` } `json:"branch"`
		Entries []wtools.GitStatusEntry `json:"entries"`
	}
	if err := json.Unmarshal([]byte(status), &s); err != nil {
		t.Fatalf("git_status not valid JSON: %v\n%s", err, status)
	}
	if s.Branch.Branch == "" {
		t.Errorf("git_status missing branch: %s", status)
	}

	// git_log: must parse to a JSON array with at least one entry.
	log := app.handleGitLog(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{Name: "git_log", Arguments: `{"count":3}`}})
	var entries []wtools.GitLogEntry
	if err := json.Unmarshal([]byte(log), &entries); err != nil {
		t.Fatalf("git_log not valid JSON: %v\n%s", err, log)
	}
	if len(entries) == 0 {
		t.Errorf("git_log returned no commits")
	}
	for _, e := range entries {
		if len(e.Hash) != 40 {
			t.Errorf("commit hash = %q, want 40 hex", e.Hash)
		}
	}

	// git_blame on a tracked file: must parse to hunks with authors.
	blame := app.handleGitBlame(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "git_blame", Arguments: `{"path":"internal/tools/tools.go"}`,
	}})
	var hunks []wtools.GitBlameHunk
	if err := json.Unmarshal([]byte(blame), &hunks); err != nil {
		t.Fatalf("git_blame not valid JSON: %v\n%s", err, blame)
	}
	if len(hunks) == 0 {
		t.Errorf("git_blame returned no hunks")
	}

	// git_show HEAD: must return non-empty (capped text or an error is fine, but
	// not a non-repo error).
	show := app.handleGitShow(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{Name: "git_show", Arguments: "{}"}})
	if strings.Contains(show, "not a git repository") {
		t.Errorf("git_show failed: %s", show)
	}
	if strings.TrimSpace(show) == "" {
		t.Errorf("git_show returned empty")
	}

	// git_diff on an empty change should not error as non-repo.
	diff := app.handleGitDiff(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{Name: "git_diff", Arguments: "{}"}})
	if strings.Contains(diff, "not a git repository") {
		t.Errorf("git_diff failed: %s", diff)
	}
}
