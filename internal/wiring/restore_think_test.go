package wiring

// M2 (0.4.4): --think must apply on /new AND /resume even when the workspace
// has no saved repo-state. cm.restoreRepoState returned early on "no state",
// so a fresh workspace ignored --think on both paths' manager restore.

import (
	"context"
	"testing"

	"github.com/treeol/wakil/internal/agent"
)

func thinkCfg(t *testing.T, url, effort string) (*conversationManager, func()) {
	t.Helper()
	cfg := testManagerConfig(t, url)
	cfg.ThinkExplicit = true
	cfg.ThinkEffort = effort
	m, err := NewConversationManager(cfg, fakeExec{}, managerPrincipal())
	if err != nil {
		t.Fatalf("NewConversationManager: %v", err)
	}
	return m.(*conversationManager), func() {}
}

func TestRestoreRepoState_AppliesThinkWithoutSavedState(t *testing.T) {
	srv := sseServer(t, []string{contentChunk("x")})
	defer srv.Close()
	cm, _ := thinkCfg(t, srv.URL, "high")
	ctx := context.Background()

	// Fresh workspace: nothing saved for it.
	for _, resume := range []bool{false, true} {
		f, err := cm.newConversation(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = cm.Close(f) }()
		if st, _ := agent.RestoreRepoStateRead(f.app); st != nil {
			t.Fatalf("test premise: workspace must have no saved repo-state, got %+v", st)
		}
		note := cm.restoreRepoState(ctx, f.app, resume)
		if f.app.ReasoningEffort != "high" {
			t.Errorf("resume=%v: ReasoningEffort = %q, want high (--think on a workspace without saved state)", resume, f.app.ReasoningEffort)
		}
		if note == "" {
			t.Errorf("resume=%v: expected a note naming the applied --think setting", resume)
		}
	}
}

// With saved state the flag still wins over the saved /thinking value on both paths.
func TestRestoreRepoState_ThinkFlagBeatsSavedStateOnResumeToo(t *testing.T) {
	srv := sseServer(t, []string{contentChunk("x")})
	defer srv.Close()
	cm, _ := thinkCfg(t, srv.URL, "low")
	ctx := context.Background()
	for _, resume := range []bool{false, true} {
		f, err := cm.newConversation(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = cm.Close(f) }()
		f.app.SaveRepoState(func(st *agent.RepoState) { st.ReasoningEffort = "max" })
		if st, _ := agent.RestoreRepoStateRead(f.app); st == nil || st.ReasoningEffort != "max" {
			t.Fatalf("test premise: repo-state with /thinking=max must be saved, got %+v", st)
		}
		cm.restoreRepoState(ctx, f.app, resume)
		if f.app.ReasoningEffort != "low" {
			t.Errorf("resume=%v: ReasoningEffort = %q, want low (the flag beats saved /thinking)", resume, f.app.ReasoningEffort)
		}
	}
}

// No --think: nothing changes on a fresh workspace.
func TestRestoreRepoState_NoThinkFlagNoChange(t *testing.T) {
	srv := sseServer(t, []string{contentChunk("x")})
	defer srv.Close()
	m, err := NewConversationManager(testManagerConfig(t, srv.URL), fakeExec{}, managerPrincipal())
	if err != nil {
		t.Fatal(err)
	}
	cm := m.(*conversationManager)
	f, err := cm.newConversation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cm.Close(f) }()
	if note := cm.restoreRepoState(context.Background(), f.app, true); note != "" || f.app.ReasoningEffort != "" {
		t.Errorf("no --think and no state must change nothing, got note %q effort %q", note, f.app.ReasoningEffort)
	}
}
