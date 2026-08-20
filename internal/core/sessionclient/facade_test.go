package sessionclient

import (
	"os/exec"
	"strings"
	"testing"
)

// TestPackageIsAgentFree is the structural guard for Gate #1's facade half: the
// sessionclient package must NOT import internal/agent, directly or
// transitively. It runs `go list -deps` and asserts no dependency path contains
// internal/agent. This catches both direct imports and transitive leaks through
// any package sessionclient pulls in.
func TestPackageIsAgentFree(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", ".")
	// Run from the sessionclient package directory so `go list .` resolves
	// to this package. go list uses the module root (found via go.mod
	// traversal) for the build, but the target is relative to Dir.
	cmd.Dir = "."
	out, err := cmd.Output()
	if err != nil {
		// Fallback: run from the module root with an explicit import path.
		out, err = exec.Command("go", "list", "-deps", "./internal/core/sessionclient/").Output()
		if err != nil {
			t.Fatalf("go list -deps failed: %v", err)
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.Contains(line, "internal/agent") {
			t.Errorf("sessionclient transitively imports %s — agent-free invariant violated", line)
		}
		if strings.Contains(line, "internal/wiring") {
			t.Errorf("sessionclient transitively imports %s — wiring-free invariant violated", line)
		}
	}
}

// TestContextLimitUsable verifies the neutral ContextLimit.Usable() mirrors
// agent.ContextLimit.Usable() — the TUI's ctx gauge and compaction thresholds
// depend on this.
func TestContextLimitUsable(t *testing.T) {
	tests := []struct {
		name string
		lim  ContextLimit
		want int
	}{
		{
			name: "usable_ctx authoritative",
			lim:  ContextLimit{NCtx: 196608, UsableCtx: 188416, ReasoningBudget: 4096, AnswerMargin: 4096},
			want: 188416,
		},
		{
			name: "fallback to nctx minus reservations",
			lim:  ContextLimit{NCtx: 196608, ReasoningBudget: 4096, AnswerMargin: 4096},
			want: 188416,
		},
		{
			name: "clamp to 1 when negative",
			lim:  ContextLimit{NCtx: 1000, ReasoningBudget: 4096, AnswerMargin: 4096},
			want: 1,
		},
		{
			name: "zero nctx clamps to 1",
			lim:  ContextLimit{},
			want: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.lim.Usable()
			if got != tc.want {
				t.Errorf("Usable() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestContextLimitFromBackend verifies the neutral ContextLimit.FromBackend()
// mirrors agent.ContextLimit.FromBackend().
func TestContextLimitFromBackend(t *testing.T) {
	if !(ContextLimit{Source: "backend"}.FromBackend()) {
		t.Error("FromBackend() = false for source=backend, want true")
	}
	if (ContextLimit{Source: "fallback"}.FromBackend()) {
		t.Error("FromBackend() = true for source=fallback, want false")
	}
}

// TestCommandResultValidate verifies that CommandResult.Validate rejects
// contradictory states (more than one of Quit/Submit/Rotate/SideQuestion).
func TestCommandResultValidate(t *testing.T) {
	// Valid: one action at a time.
	if err := (CommandResult{Handled: true, Quit: true}).Validate(); err != nil {
		t.Errorf("quit-only should validate: %v", err)
	}
	if err := (CommandResult{Handled: true, Submit: "text"}).Validate(); err != nil {
		t.Errorf("submit-only should validate: %v", err)
	}
	if err := (CommandResult{Handled: true, Rotate: &RotateRequest{Type: "new"}}).Validate(); err != nil {
		t.Errorf("rotate-only should validate: %v", err)
	}
	if err := (CommandResult{Handled: true, SideQuestion: "why?"}).Validate(); err != nil {
		t.Errorf("side-question-only should validate: %v", err)
	}
	// Valid: notice-only (no action).
	if err := (CommandResult{Handled: true, Notice: "hello"}).Validate(); err != nil {
		t.Errorf("notice-only should validate: %v", err)
	}

	// Invalid: two actions.
	if err := (CommandResult{Quit: true, Submit: "text"}).Validate(); err == nil {
		t.Error("quit+submit should be rejected")
	}
	if err := (CommandResult{Quit: true, Rotate: &RotateRequest{Type: "new"}}).Validate(); err == nil {
		t.Error("quit+rotate should be rejected")
	}
	if err := (CommandResult{Submit: "text", SideQuestion: "why?"}).Validate(); err == nil {
		t.Error("submit+sidequestion should be rejected")
	}

	// Invalid rotate type.
	if err := (CommandResult{Rotate: &RotateRequest{Type: "bogus"}}).Validate(); err == nil {
		t.Error("invalid rotate type should be rejected")
	}
}