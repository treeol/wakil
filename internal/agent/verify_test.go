package agent

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/exec"
	"github.com/treeol/wakil/internal/verify"
	"github.com/treeol/wakil/internal/workflow"
)

// TestResolveVerifyCommands_ConfigWins tests that explicit config commands
// take precedence over auto-detection.
func TestResolveVerifyCommands_ConfigWins(t *testing.T) {
	exec := newFakeExecutor()
	// Seed files that would trigger detection.
	exec.files["go.mod"] = "module test"
	exec.files["package.json"] = "{}"

	app := newTestApp("http://test", exec, nil)
	app.Cfg.Verify = []string{"make test", "make lint"}

	cmds := ResolveVerifyCommands(app)
	if len(cmds) != 2 {
		t.Fatalf("expected 2 config commands, got %d", len(cmds))
	}
	if cmds[0].Cmd != "make test" {
		t.Errorf("expected 'make test', got %s", cmds[0].Cmd)
	}
	if cmds[0].Source != "config" {
		t.Errorf("expected source 'config', got %s", cmds[0].Source)
	}
	if cmds[1].Cmd != "make lint" {
		t.Errorf("expected 'make lint', got %s", cmds[1].Cmd)
	}
}

// TestResolveVerifyCommands_AutoDetect tests that commands are detected from
// project manifests when config is empty.
func TestResolveVerifyCommands_AutoDetect(t *testing.T) {
	exec := newFakeExecutor()
	exec.files["go.mod"] = "module test"

	app := newTestApp("http://test", exec, nil)
	// Cfg.Verify is empty — detection should kick in.

	cmds := ResolveVerifyCommands(app)
	if len(cmds) != 2 {
		t.Fatalf("expected 2 detected commands for go.mod, got %d", len(cmds))
	}
	if cmds[0].Cmd != "go test ./..." {
		t.Errorf("expected 'go test ./...', got %s", cmds[0].Cmd)
	}
	if !strings.HasPrefix(cmds[0].Source, "detect:") {
		t.Errorf("expected source to start with 'detect:', got %s", cmds[0].Source)
	}
}

// TestResolveVerifyCommands_NoManifests tests that no commands are returned
// when no manifests are present.
func TestResolveVerifyCommands_NoManifests(t *testing.T) {
	exec := newFakeExecutor()
	exec.files["README.md"] = "readme"

	app := newTestApp("http://test", exec, nil)

	cmds := ResolveVerifyCommands(app)
	if len(cmds) != 0 {
		t.Fatalf("expected 0 commands for no manifests, got %d", len(cmds))
	}
}

// TestRunVerification_AllPass tests that passing commands produce a passing outcome.
func TestRunVerification_AllPass(t *testing.T) {
	exec := newFakeExecutor()
	app := newTestApp("http://test", exec, func(toolName, headline, detail string, readAction bool) bool {
		return true // auto-approve all
	})

	cmds := []verify.Command{
		{Cmd: "go test ./...", Source: "config"},
		{Cmd: "go vet ./...", Source: "config"},
	}

	outcome := RunVerification(context.Background(), app, cmds)
	if !outcome.Passed() {
		t.Error("expected outcome to pass")
	}
	if outcome.HasFailures() {
		t.Error("expected no failures")
	}
	// fakeExecutor.RunShell returns "ran: <cmd>" with nil error → pass.
	if len(exec.shellCalls) != 2 {
		t.Errorf("expected 2 shell calls, got %d", len(exec.shellCalls))
	}
}

// TestRunVerification_CommandFails tests that a failing command produces a
// failure outcome.
func TestRunVerification_CommandFails(t *testing.T) {
	exec := &erroringExecutor{base: newFakeExecutor()}

	app := newTestApp("http://test", exec, func(toolName, headline, detail string, readAction bool) bool {
		return true
	})

	cmds := []verify.Command{
		{Cmd: "go test ./...", Source: "config"},
	}

	outcome := RunVerification(context.Background(), app, cmds)
	if outcome.Passed() {
		t.Error("expected outcome to NOT pass (command failed)")
	}
	if !outcome.HasFailures() {
		t.Error("expected HasFailures")
	}
	if len(outcome.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(outcome.Results))
	}
	if outcome.Results[0].Status != verify.StatusFail {
		t.Errorf("expected status fail, got %s", outcome.Results[0].Status)
	}
}

// TestRunVerification_DeclinedByPolicy tests that a declined command produces
// a declined result, not a failure.
func TestRunVerification_DeclinedByPolicy(t *testing.T) {
	exec := newFakeExecutor()
	app := newTestApp("http://test", exec, func(toolName, headline, detail string, readAction bool) bool {
		return false // decline everything
	})

	cmds := []verify.Command{
		{Cmd: "go test ./...", Source: "config"},
	}

	outcome := RunVerification(context.Background(), app, cmds)
	if outcome.Passed() {
		t.Error("declined should not be Passed()")
	}
	if outcome.HasFailures() {
		t.Error("declined should not be HasFailures() — it's a consent issue")
	}
	if !outcome.AnyDeclined() {
		t.Error("expected AnyDeclined()=true")
	}
	if len(exec.shellCalls) != 0 {
		t.Errorf("expected 0 shell calls (declined before exec), got %d", len(exec.shellCalls))
	}
}

// TestRunVerification_NoExecutor tests that a missing executor produces an error result.
func TestRunVerification_NoExecutor(t *testing.T) {
	app := newTestApp("http://test", nil, func(toolName, headline, detail string, readAction bool) bool {
		return true
	})

	cmds := []verify.Command{
		{Cmd: "go test ./...", Source: "config"},
	}

	outcome := RunVerification(context.Background(), app, cmds)
	if outcome.Passed() {
		t.Error("expected outcome to NOT pass (no executor)")
	}
	if !outcome.HasFailures() {
		t.Error("expected HasFailures (error is a failure)")
	}
	if len(outcome.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(outcome.Results))
	}
	if outcome.Results[0].Status != verify.StatusError {
		t.Errorf("expected status error, got %s", outcome.Results[0].Status)
	}
}

// TestRunVerification_EmptyCommands tests that no commands produces a skipped outcome.
func TestRunVerification_EmptyCommands(t *testing.T) {
	exec := newFakeExecutor()
	app := newTestApp("http://test", exec, func(toolName, headline, detail string, readAction bool) bool {
		return true
	})

	outcome := RunVerification(context.Background(), app, nil)
	if !outcome.WasSkipped() {
		t.Error("expected WasSkipped()")
	}
	if outcome.Passed() {
		t.Error("empty outcome should not be Passed()")
	}
}

// TestRunVerification_StepLogAppended tests that verification results are
// appended to the step log when a workflow is active.
func TestRunVerification_StepLogAppended(t *testing.T) {
	exec := newFakeExecutor()
	exec.files[".wakil/plan.md"] = workflow.WFInitPlanContent("test task")

	app := newTestApp("http://test", exec, func(toolName, headline, detail string, readAction bool) bool {
		return true
	})
	app.Workflow = &workflow.WorkflowState{
		Task:     "test task",
		Phase:    workflow.WFImplement,
		PlanPath: ".wakil/plan.md",
	}

	cmds := []verify.Command{
		{Cmd: "go test ./...", Source: "config"},
	}

	RunVerification(context.Background(), app, cmds)

	// Read back the plan.md to check the step log was appended.
	content, err := exec.ReadFile(context.Background(), ".wakil/plan.md")
	if err != nil {
		t.Fatalf("could not read plan.md: %v", err)
	}
	if !strings.Contains(content, "## Step log") {
		t.Error("expected step log section in plan.md")
	}
	if !strings.Contains(content, "VERIFY") {
		t.Error("expected VERIFY entry in step log")
	}
}

// TestRunVerification_StopOnFirstDecline tests that verification stops after
// the first declined command — remaining commands should not run.
func TestRunVerification_StopOnFirstDecline(t *testing.T) {
	exec := newFakeExecutor()
	declineCount := 0
	app := newTestApp("http://test", exec, func(toolName, headline, detail string, readAction bool) bool {
		declineCount++
		if declineCount == 1 {
			return false // decline the first command
		}
		return true
	})

	cmds := []verify.Command{
		{Cmd: "go test ./...", Source: "config"},
		{Cmd: "go vet ./...", Source: "config"},
		{Cmd: "npm test", Source: "config"},
	}

	outcome := RunVerification(context.Background(), app, cmds)

	// Only 1 result — the second and third commands should not have run.
	if len(outcome.Results) != 1 {
		t.Fatalf("expected 1 result (stopped on decline), got %d", len(outcome.Results))
	}
	if outcome.Results[0].Status != verify.StatusDeclined {
		t.Errorf("expected status declined, got %s", outcome.Results[0].Status)
	}
	// No shell calls — the first command was declined before execution.
	if len(exec.shellCalls) != 0 {
		t.Errorf("expected 0 shell calls (declined before exec), got %d", len(exec.shellCalls))
	}
}

// erroringExecutor wraps fakeExecutor and makes RunShell return an error,
// simulating a non-zero exit code (test failure).
type erroringExecutor struct {
	base *fakeExecutor
}

func (e *erroringExecutor) RunShell(_ context.Context, c string) (string, error) {
	e.base.shellCalls = append(e.base.shellCalls, c)
	return "exit code 1", assertFail("exit status 1")
}

// All other methods delegate to the base fakeExecutor.
func (e *erroringExecutor) StatFile(ctx context.Context, p string) (int64, error) {
	return e.base.StatFile(ctx, p)
}
func (e *erroringExecutor) ReadFile(ctx context.Context, p string) (string, error) {
	return e.base.ReadFile(ctx, p)
}
func (e *erroringExecutor) ListDir(ctx context.Context, p string) (string, error) {
	return e.base.ListDir(ctx, p)
}
func (e *erroringExecutor) WriteFile(ctx context.Context, p, c string) (string, error) {
	return e.base.WriteFile(ctx, p, c)
}
func (e *erroringExecutor) Cwd() string           { return e.base.Cwd() }
func (e *erroringExecutor) WorkspaceRoot() string { return e.base.WorkspaceRoot() }
func (e *erroringExecutor) Describe() string      { return e.base.Describe() }
func (e *erroringExecutor) Close() error          { return e.base.Close() }
func (e *erroringExecutor) SandboxTools() string  { return e.base.SandboxTools() }
func (e *erroringExecutor) Generation() int       { return e.base.Generation() }
func (e *erroringExecutor) KVRSocketPath() string { return e.base.KVRSocketPath() }
func (e *erroringExecutor) KVRAvailable() bool    { return e.base.KVRAvailable() }
func (e *erroringExecutor) ContainerName() string { return e.base.ContainerName() }
func (e *erroringExecutor) ConfinePath(ctx context.Context, p string) (string, error) {
	return e.base.ConfinePath(ctx, p)
}
func (e *erroringExecutor) DeletePath(ctx context.Context, p string) error {
	return e.base.DeletePath(ctx, p)
}
func (e *erroringExecutor) MovePath(ctx context.Context, src, dst string) error {
	return e.base.MovePath(ctx, src, dst)
}
func (e *erroringExecutor) StartBackground(ctx context.Context, cmd, log string) (int, int, error) {
	return e.base.StartBackground(ctx, cmd, log)
}
func (e *erroringExecutor) KillPgid(ctx context.Context, pgid, sig int) error {
	return e.base.KillPgid(ctx, pgid, sig)
}
func (e *erroringExecutor) IsProcessAlive(ctx context.Context, pid int) bool {
	return e.base.IsProcessAlive(ctx, pid)
}
func (e *erroringExecutor) ReadFileTail(ctx context.Context, p string, m int64) (string, error) {
	return e.base.ReadFileTail(ctx, p, m)
}
func (e *erroringExecutor) StartInteractive(ctx context.Context, cmd string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, int, error) {
	return e.base.StartInteractive(ctx, cmd)
}
func (e *erroringExecutor) HostPathToURI(p string) (string, error) { return e.base.HostPathToURI(p) }
func (e *erroringExecutor) URIToHostPath(u string) (string, error) { return e.base.URIToHostPath(u) }

// assertFail returns an error (used in place of errors.New for the failing exec).
func assertFail(msg string) error {
	return &testError{msg: msg}
}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }

// Ensure the fake executors implement the interface.
var _ exec.Executor = (*fakeExecutor)(nil)
var _ exec.Executor = (*erroringExecutor)(nil)
