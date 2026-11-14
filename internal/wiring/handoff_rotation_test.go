package wiring

import (
	"context"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionclient"
	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/workflow"
)

// newTestManager builds a ConversationManager over a fake config for
// handoff/rotation tests. The built Apps point at an unreachable backend —
// handoff's summarizer falls back to the capped-render path (no summarizer
// configured via fakeApp), so no network round trip happens.
func newTestManager(t *testing.T) *conversationManager {
	t.Helper()
	mgr, err := NewConversationManager(
		config.DefaultConfig(),
		fakeExec{},
		core.Principal{TenantID: event.EmbeddedTenantID, UserID: event.EmbeddedUserID, Role: core.RoleOwner},
	)
	if err != nil {
		t.Fatalf("NewConversationManager: %v", err)
	}
	return mgr.(*conversationManager)
}

// TestHandoffKeepsTaskTextVerbatim pins Group E rule h: after a handoff
// through the real HandoffConversation path, the new session's first
// request carries the original task text unchanged, every later user
// message unchanged, and the progress/plan file path when one exists.
func TestHandoffKeepsTaskTextVerbatim(t *testing.T) {
	mgr := newTestManager(t)

	const taskText = "Finish the fix round on branch work/0.8.3, then fix mid-turn collapse and handoff."
	const laterText = "And add the CHANGELOG entries for group E."
	old := newTestFacade(t)
	old.app.Conv = []proxy.Message{
		{Role: "system", Content: agent.StrPtr("preamble")},
		{Role: "user", Content: agent.StrPtr(taskText)},
		{Role: "assistant", Content: agent.StrPtr("done. next: the fix round")},
		{Role: "user", Content: agent.StrPtr(laterText)},
		{Role: "assistant", Content: agent.StrPtr("agreed")},
	}
	old.app.Session = &agent.Session{ChatID: old.app.Client.ChatID}
	old.app.SetWorkflow(&workflow.WorkflowState{
		Task:      taskText,
		PlanPath:  "/work/.wakil/plan.md",
		Phase:     workflow.WFImplement,
		StepIdx:   2,
		StepCount: 5,
	})
	t.Setenv("WAKIL_SESSIONS_DIR", t.TempDir())
	// The summarizer has no network here: generateHandoffPayload falls
	// back to the capped render, so the task texts carried verbatim are
	// the only thing being verified.
	newF, err := mgr.HandoffConversation(context.Background(), core.Principal{
		TenantID: event.EmbeddedTenantID, UserID: event.EmbeddedUserID, Role: core.RoleOwner,
	}, old, true)
	if err != nil {
		t.Fatalf("HandoffConversation: %v", err)
	}
	defer newF.Close()
	nf := newF.(*wiringFacade)

	// Stop-mode seed (the pinned system message) and proceed seed (the
	// pending continuation prompt) both carry the verbatim texts.
	seed := ""
	for _, m := range nf.app.Conv {
		if m.Content != nil {
			seed += *m.Content + "\n"
		}
	}
	if pending := nf.ConsumePendingContinuation(); pending != "" {
		seed += pending + "\n"
	}
	if !strings.Contains(seed, taskText) {
		t.Fatalf("original task text not present in the new session's seed:\n%s", seed)
	}
	if !strings.Contains(seed, laterText) {
		t.Fatalf("later user message not present in the new session's seed:\n%s", seed)
	}
	if !strings.Contains(seed, "/work/.wakil/plan.md") {
		t.Fatalf("plan/progress file path not present in the new session's seed:\n%s", seed)
	}
}

// TestHandoffConversationSeedsContext verifies the real HandoffConversation
// (7b3 m4): the old session is saved, the new conversation carries the handoff
// context as a pinned system message, and pending images do not leak across.
func TestHandoffConversationSeedsContext(t *testing.T) {
	mgr := newTestManager(t)

	// Build the "old" facade with a handoff-able conversation.
	old := newTestFacade(t)
	old.app.Conv = []proxy.Message{
		{Role: "system", Content: agent.StrPtr("preamble")},
		{Role: "user", Content: agent.StrPtr("fix the login bug")},
		{Role: "assistant", Content: agent.StrPtr("fixed it")},
	}
	old.app.Session = &agent.Session{ChatID: old.app.Client.ChatID}
	old.app.PendingImages = []proxy.ImagePart{{Path: "leak.png"}}

	// Point session writes at a temp dir so the test never touches real data.
	t.Setenv("WAKIL_SESSIONS_DIR", t.TempDir())

	newF, err := mgr.HandoffConversation(context.Background(), core.Principal{
		TenantID: event.EmbeddedTenantID, UserID: event.EmbeddedUserID, Role: core.RoleOwner,
	}, old, false)
	if err != nil {
		t.Fatalf("HandoffConversation: %v", err)
	}
	defer newF.Close()

	// The new facade is a wiringFacade with a non-empty conversation: the
	// pinned handoff context must be present.
	nf := newF.(*wiringFacade)
	found := false
	for _, m := range nf.app.Conv {
		if m.Role == "system" && m.Pinned {
			found = true
		}
	}
	if !found {
		t.Error("new conversation carries no pinned handoff system message — seeding missing")
	}
	if len(nf.app.PendingImages) != 0 {
		t.Errorf("pending images leaked across handoff: %d", len(nf.app.PendingImages))
	}
	// The new session's ChatID differs from the old one.
	if nf.app.Client.ChatID == old.app.Client.ChatID {
		t.Error("new conversation reuses the old ChatID — rotation did not happen")
	}
}

// TestHandoffConversationRejectsEmpty verifies the guards: an empty
// conversation fails the handoff and leaves rotation untouched.
func TestHandoffConversationRejectsEmpty(t *testing.T) {
	mgr := newTestManager(t)
	old := newTestFacade(t)
	old.app.Session = &agent.Session{ChatID: old.app.Client.ChatID}
	t.Setenv("WAKIL_SESSIONS_DIR", t.TempDir())

	_, err := mgr.HandoffConversation(context.Background(), core.Principal{
		TenantID: event.EmbeddedTenantID, UserID: event.EmbeddedUserID, Role: core.RoleOwner,
	}, old, false)
	if err == nil {
		t.Fatal("handoff of empty conversation should fail")
	}
}

// TestFacadeInfoSnapshot verifies the Info() surface carries the deep-state
// fields the info panel and status line render (context gauge, workflow label,
// endpoint, model selection) — the reads the old TUI made on *agent.App.
func TestFacadeInfoSnapshot(t *testing.T) {
	f := newTestFacade(t)

	f.SetWorkflow(&sessionclient.WorkflowSnapshot{
		Task:  "info test",
		Phase: "gather",
	})

	info := f.Info()
	if info.WorkflowLabel == "" {
		t.Error("Info().WorkflowLabel empty with an active workflow")
	}
	if info.EffectiveModel == "" {
		t.Error("Info().EffectiveModel empty")
	}
	if info.ChatID != f.app.Client.ChatID {
		t.Errorf("Info().ChatID = %q, want %q", info.ChatID, f.app.Client.ChatID)
	}
	// MentionBase and endpoints are present (completion source parity).
	if len(info.Endpoints) == 0 || info.Endpoints[0] != "inherit" {
		t.Errorf("Info().Endpoints = %v, want inherit-first", info.Endpoints)
	}
	// InfoSnapshot is a plain copy: mutating it must not affect the facade.
	info.WorkflowLabel = ""
	if f.Info().WorkflowLabel == "" {
		t.Error("Info() result aliases live state — expected a copy")
	}
}

// Compile-time interface check (mirrors facade_test's, scoped to this file).
var _ sessionclient.ConversationManager = (*conversationManager)(nil)
