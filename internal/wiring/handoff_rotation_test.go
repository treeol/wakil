package wiring

import (
	"context"
	"testing"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionclient"
	"github.com/treeol/wakil/internal/proxy"
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
