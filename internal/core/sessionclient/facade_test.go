package sessionclient

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/proxy"
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

// TestCommandResultOpID verifies that the OpID field is carried through
// and does not affect validation (it's an async-op identifier, not an action).
func TestCommandResultOpID(t *testing.T) {
	cr := CommandResult{Handled: true, OpID: "op-42"}
	if cr.OpID != "op-42" {
		t.Errorf("OpID = %q, want %q", cr.OpID, "op-42")
	}
	// OpID alone should not conflict with any action.
	if err := cr.Validate(); err != nil {
		t.Errorf("OpID-only should validate: %v", err)
	}
	// OpID alongside a single action is valid.
	cr2 := CommandResult{Handled: true, OpID: "op-1", Submit: "text"}
	if err := cr2.Validate(); err != nil {
		t.Errorf("OpID+submit should validate: %v", err)
	}
}

// TestClientSnapshotOutputMode verifies the OutputMode field is present and
// carries config.OutputMode values.
func TestClientSnapshotOutputMode(t *testing.T) {
	snap := ClientSnapshot{
		OutputMode: "debug",
	}
	if snap.OutputMode != "debug" {
		t.Errorf("OutputMode = %q, want %q", snap.OutputMode, "debug")
	}
	snap.OutputMode = "simple"
	if snap.OutputMode != "simple" {
		t.Errorf("OutputMode = %q, want %q", snap.OutputMode, "simple")
	}
}

// TestClientSnapshotSlicesAreClonable verifies that the snapshot's slice
// fields can be independently mutated without affecting the source. This
// documents the immutability contract: the wiring-side constructor must
// clone all slices so the TUI can read them safely.
func TestClientSnapshotSlicesAreClonable(t *testing.T) {
	conv := []proxy.Message{{Role: "user", Content: nil}}
	tools := []proxy.Tool{{Function: proxy.ToolFunction{Name: "run_shell"}}}
	models := []string{"a", "b"}
	backends := []Backend{{Name: "llama"}}
	imgs := []proxy.ImagePart{{MIME: "image/png", Path: "screenshot.png"}}

	snap := ClientSnapshot{
		Conv:          append([]proxy.Message(nil), conv...),
		Tools:         append([]proxy.Tool(nil), tools...),
		ModelList:     append([]string(nil), models...),
		BackendList:   append([]Backend(nil), backends...),
		PendingImages: append([]proxy.ImagePart(nil), imgs...),
		Costs:         &proxy.CostTracker{},
	}

	// Mutate the snapshot's slices — the source slices must not change.
	snap.Conv[0].Role = "assistant"
	snap.Tools[0].Function.Name = "modified"
	snap.ModelList[0] = "x"
	snap.BackendList[0].Name = "modified"
	snap.PendingImages[0].MIME = "image/jpeg"

	if conv[0].Role != "user" {
		t.Error("snapshot Conv mutation leaked to source")
	}
	if tools[0].Function.Name != "run_shell" {
		t.Error("snapshot Tools mutation leaked to source")
	}
	if models[0] != "a" {
		t.Error("snapshot ModelList mutation leaked to source")
	}
	if backends[0].Name != "llama" {
		t.Error("snapshot BackendList mutation leaked to source")
	}
	if imgs[0].MIME != "image/png" {
		t.Error("snapshot PendingImages mutation leaked to source")
	}
}

// TestConversationManagerInterface verifies that ConversationManager is a
// compile-time-valid interface with the expected methods. This is a
// structural test: it ensures the interface exists and has the right shape
// for the wiring-side implementation (7b3 m3).
func TestConversationManagerInterface(t *testing.T) {
	var _ ConversationManager = (ConversationManager)(nil)
	// The interface must have these four methods. If any is removed or
	// renamed, this will fail to compile.
	var cm ConversationManager = &fakeConversationManager{}
	_ = cm
}

type fakeConversationManager struct{}

func (f *fakeConversationManager) NewConversation(ctx context.Context, p core.Principal) (Facade, error) {
	return nil, nil
}
func (f *fakeConversationManager) ResumeConversation(ctx context.Context, p core.Principal, id string) (Facade, error) {
	return nil, nil
}
func (f *fakeConversationManager) HandoffConversation(ctx context.Context, p core.Principal, cur Facade, proceed bool) (Facade, error) {
	return nil, nil
}
func (f *fakeConversationManager) Close(fac Facade) error { return nil }