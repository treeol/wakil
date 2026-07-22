package agent

import (
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/workflow"
)

// consent_extra_test.go — tests for the security-boundary functions that were
// at 0% or low coverage: buildPolicyInput, ShellCmdFromDetail, and the
// SuspendAuto carve-outs beyond external_backend.

// ── ShellCmdFromDetail ─────────────────────────────────────────────────────

func TestShellCmdFromDetail_BasicCommand(t *testing.T) {
	detail := "$ git status\n  (docker exec, cwd=/work)"
	cmd := ShellCmdFromDetail(detail)
	if cmd != "git status" {
		t.Errorf("ShellCmdFromDetail = %q, want %q", cmd, "git status")
	}
}

func TestShellCmdFromDetail_WithPhaseWarning(t *testing.T) {
	detail := "⚠ workflow phase: GATHER — no writes\n$ ls -la\n  (docker exec, cwd=/work)"
	cmd := ShellCmdFromDetail(detail)
	if cmd != "ls -la" {
		t.Errorf("ShellCmdFromDetail = %q, want %q (should skip phase warning line)", cmd, "ls -la")
	}
}

func TestShellCmdFromDetail_FallbackToFirstLine(t *testing.T) {
	detail := "some non-standard format without $ prefix"
	cmd := ShellCmdFromDetail(detail)
	if cmd != "some non-standard format without $ prefix" {
		t.Errorf("ShellCmdFromDetail fallback = %q, want first line", cmd)
	}
}

func TestShellCmdFromDetail_Empty(t *testing.T) {
	cmd := ShellCmdFromDetail("")
	if cmd != "" {
		t.Errorf("ShellCmdFromDetail('') = %q, want empty", cmd)
	}
}

func TestShellCmdFromDetail_MultilineCommand(t *testing.T) {
	detail := "$ echo hello\n$ echo world\n  (docker exec)"
	cmd := ShellCmdFromDetail(detail)
	if cmd != "echo hello" {
		t.Errorf("ShellCmdFromDetail = %q, want %q (should match first $ line)", cmd, "echo hello")
	}
}

// ── buildPolicyInput ───────────────────────────────────────────────────────

func TestBuildPolicyInput_ShellCommand(t *testing.T) {
	input := buildPolicyInput("run_shell", "$ rm -rf /tmp\n  (docker exec)", false)
	if input.ToolName != "run_shell" {
		t.Errorf("ToolName = %q, want run_shell", input.ToolName)
	}
	if input.Command != "rm -rf /tmp" {
		t.Errorf("Command = %q, want 'rm -rf /tmp'", input.Command)
	}
	if !input.Destructive {
		t.Error("Destructive should be true for rm command")
	}
	if input.ExternalBackend {
		t.Error("ExternalBackend should be false for run_shell")
	}
}

func TestBuildPolicyInput_ReadOnlyShell(t *testing.T) {
	input := buildPolicyInput("run_shell", "$ ls -la\n  (docker exec)", false)
	if input.Destructive {
		t.Error("Destructive should be false for ls command")
	}
	if input.Command != "ls -la" {
		t.Errorf("Command = %q, want 'ls -la'", input.Command)
	}
}

func TestBuildPolicyInput_ExternalBackend(t *testing.T) {
	input := buildPolicyInput("external_backend", "switching to openrouter", false)
	if !input.ExternalBackend {
		t.Error("ExternalBackend should be true for external_backend tool")
	}
}

func TestBuildPolicyInput_FileMutation(t *testing.T) {
	for _, tool := range []string{"delete_file", "move_file"} {
		input := buildPolicyInput(tool, "some detail", false)
		if !input.Destructive {
			t.Errorf("Destructive should be true for %s", tool)
		}
	}
}

func TestBuildPolicyInput_NonDestructiveFileOp(t *testing.T) {
	input := buildPolicyInput("write_file", "writing config.json", false)
	if input.Destructive {
		t.Error("Destructive should be false for write_file (not in destructive list)")
	}
	if input.Command != "" {
		t.Errorf("Command should be empty for non-shell tool, got %q", input.Command)
	}
}

func TestBuildPolicyInput_RunBackground(t *testing.T) {
	input := buildPolicyInput("run_background", "$ npm run dev (background)", false)
	if input.Command != "npm run dev (background)" {
		t.Errorf("Command = %q, want 'npm run dev (background)'", input.Command)
	}
	// npm is not in destructiveCmds, so not destructive
	if input.Destructive {
		t.Error("Destructive should be false for npm command")
	}
}

func TestBuildPolicyInput_ReadAction(t *testing.T) {
	input := buildPolicyInput("run_shell", "$ cat file.txt\n  (docker exec)", true)
	if !input.ReadAction {
		t.Error("ReadAction should be true")
	}
}

// ── SuspendAuto carve-outs ────────────────────────────────────────────────

func TestSuspendAuto_DestructiveShellAllowedWithGrant(t *testing.T) {
	app := &App{}
	app.SetConsent(ConsentSnapshot{AutoApprove: true, AllowDestructive: true, AllowReads: false})
	// rm is destructive, but AllowDestructive is true → should NOT be suspended
	reason := SuspendAuto("run_shell", app, "$ rm -rf /tmp/test\n  (docker exec)")
	if reason != "" {
		t.Errorf("SuspendAuto should return empty when AllowDestructive is true; got %q", reason)
	}
}

func TestSuspendAuto_DestructiveShellNotApproved(t *testing.T) {
	app := &App{}
	app.SetConsent(ConsentSnapshot{AutoApprove: true, AllowDestructive: false, AllowReads: false})
	reason := SuspendAuto("run_shell", app, "$ rm -rf /tmp/test\n  (docker exec)")
	if reason == "" {
		t.Error("SuspendAuto should return non-empty for destructive command without AllowDestructive")
	}
	if !strings.Contains(strings.ToLower(reason), "destructive") {
		t.Errorf("reason should mention 'destructive'; got %q", reason)
	}
}

func TestSuspendAuto_ReadOnlyShellInAutoMode(t *testing.T) {
	app := &App{}
	app.SetConsent(ConsentSnapshot{AutoApprove: true, AllowDestructive: false, AllowReads: false})
	// ls is read-only, not in pre-implementation phase → should NOT be suspended
	reason := SuspendAuto("run_shell", app, "$ ls -la\n  (docker exec)")
	if reason != "" {
		t.Errorf("SuspendAuto should return empty for read-only command in auto mode; got %q", reason)
	}
}

func TestSuspendAuto_NonReadOnlyShellInPreImplementPhase(t *testing.T) {
	app := &App{}
	app.SetConsent(ConsentSnapshot{AutoApprove: true, AllowDestructive: false, AllowReads: false})
	app.Workflow = &workflow.WorkflowState{
		Phase: workflow.WFGather,
	}
	// npm is not read-only and not destructive, but in GATHER phase → should be suspended
	reason := SuspendAuto("run_shell", app, "$ npm install\n  (docker exec)")
	if reason == "" {
		t.Error("SuspendAuto should return non-empty for non-read-only command in pre-implementation phase")
	}
	if !strings.Contains(strings.ToLower(reason), "pre-implementation") {
		t.Errorf("reason should mention 'pre-implementation'; got %q", reason)
	}
}

func TestSuspendAuto_ReadOnlyShellInPreImplementPhase(t *testing.T) {
	app := &App{}
	app.SetConsent(ConsentSnapshot{AutoApprove: true, AllowDestructive: false, AllowReads: false})
	app.Workflow = &workflow.WorkflowState{
		Phase: workflow.WFGather,
	}
	// ls is read-only → should NOT be suspended even in pre-implementation phase
	reason := SuspendAuto("run_shell", app, "$ ls -la\n  (docker exec)")
	if reason != "" {
		t.Errorf("SuspendAuto should return empty for read-only command in pre-implementation phase; got %q", reason)
	}
}

func TestSuspendAuto_RunBackgroundDestructive(t *testing.T) {
	app := &App{}
	app.SetConsent(ConsentSnapshot{AutoApprove: true, AllowDestructive: false, AllowReads: false})
	// run_background with a destructive command → should be suspended
	reason := SuspendAuto("run_background", app, "$ rm -rf /tmp (background)")
	if reason == "" {
		t.Error("SuspendAuto should return non-empty for destructive run_background command")
	}
}

func TestSuspendAuto_NonShellTool(t *testing.T) {
	app := &App{}
	app.SetConsent(ConsentSnapshot{AutoApprove: true, AllowDestructive: false, AllowReads: false})
	// write_file is not in the switch → should return "" (auto-approve proceeds)
	reason := SuspendAuto("write_file", app, "writing config.json")
	if reason != "" {
		t.Errorf("SuspendAuto should return empty for write_file; got %q", reason)
	}
}

func TestSuspendAuto_EmptyDetail(t *testing.T) {
	app := &App{}
	app.SetConsent(ConsentSnapshot{AutoApprove: true, AllowDestructive: false, AllowReads: false})
	reason := SuspendAuto("run_shell", app, "")
	if reason != "" {
		t.Errorf("SuspendAuto should return empty for empty detail; got %q", reason)
	}
}

// TestSuspendAuto_ExternalBackendWithDestructiveGrant verifies the egress gate
// is NEVER bypassed by AllowDestructive — the hardest pair-invariant in the
// consent system. /auto destructive grants auto-approval for destructive shell
// commands, but external backend egress always requires explicit approval.
func TestSuspendAuto_ExternalBackendWithDestructiveGrant(t *testing.T) {
	app := &App{}
	app.SetConsent(ConsentSnapshot{AutoApprove: true, AllowDestructive: true, AllowReads: true})
	reason := SuspendAuto("external_backend", app, "switching to openrouter")
	if reason == "" {
		t.Error("SuspendAuto should return non-empty for external_backend even with AllowDestructive=true")
	}
	if !strings.Contains(strings.ToLower(reason), "external") {
		t.Errorf("reason should mention 'external'; got %q", reason)
	}
}
