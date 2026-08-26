package wiring

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/exec"
)

// managerPrincipal is the embedded principal the manager defaults to.
func managerPrincipal() core.Principal {
	return core.Principal{
		TenantID: event.EmbeddedTenantID,
		UserID:   event.EmbeddedUserID,
		Role:     core.RoleOwner,
	}
}

// testManagerConfig builds a direct-mode config pointing at a fake SSE server.
func testManagerConfig(t *testing.T, url string) config.Config {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.ExecMode = "direct"
	cfg.WorkDir = t.TempDir()
	cfg.HostWorkDir = cfg.WorkDir
	if cfg.Endpoints == nil {
		cfg.Endpoints = map[string]config.EndpointConfig{}
	}
	ep := cfg.Endpoints["default"]
	ep.BaseURL = url
	ep.Model = "ilm"
	ep.Kind = "openai"
	cfg.Endpoints["default"] = ep
	cfg.EndpointName = "default"
	cfg.Endpoint = ep
	return cfg
}

// TestManagerEnablesAsyncApproval verifies the m4b B1 fix: conversations built
// by the ConversationManager park approvals in awaiting_approval instead of
// using the sync inline resolver (which, with no resolver configured,
// declines everything — an interactive TUI could never approve a tool).
//
// The turn runs a write_file tool call (a gated tool) through a fake SSE
// backend; the ApprovalRequested event must arrive while the session is
// parked, and RespondToApproval(allow_once) must unblock the turn to
// completion.
func TestManagerEnablesAsyncApproval(t *testing.T) {
	// Tool call → confirm gate; after resolution the second backend call
	// returns final text so the turn terminates.
	srv := sseServer(t,
		toolCallFrames("call_1", "write_file", `{"path":"a.txt","content":"x"}`),
		[]string{contentChunk("done after approval")})
	defer srv.Close()

	var exe exec.Executor = fakeExec{}
	mgr, err := NewConversationManager(testManagerConfig(t, srv.URL), exe, managerPrincipal())
	if err != nil {
		t.Fatalf("NewConversationManager: %v", err)
	}
	ctx := context.Background()
	f, err := mgr.NewConversation(ctx, managerPrincipal(), nil)
	if err != nil {
		t.Fatalf("NewConversation: %v", err)
	}
	defer func() { _ = mgr.Close(f) }()
	wf := f.(*wiringFacade)

	// Subscribe live-only (at head, as BootstrapTUI does). Delivery is via
	// ListEvents polling instead of a blocking Next — simpler and immune to
	// ordering races with the parked turn.
	if _, err := f.SubmitInput(ctx, managerPrincipal(), core.SubmitInputRequest{
		SessionID: wf.sessionID,
		Text:      "write the file",
	}); err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}

	// Wait for the parked approval: the ApprovalRequested event carries the
	// ApprovalID we answer with.
	var approvalID event.ApprovalID
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		events, err := f.ListEvents(ctx, managerPrincipal(), wf.sessionID, 0, 0)
		if err != nil {
			t.Fatalf("ListEvents: %v", err)
		}
		for _, ev := range events {
			if ev.Kind == event.KindApprovalRequested {
				approvalID = ev.Payload.(event.ApprovalRequested).ApprovalID
			}
		}
		if approvalID != "" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if approvalID == "" {
		t.Fatal("no ApprovalRequested event arrived — async approval not enabled (B1 regression)")
	}

	// The session must be parked in awaiting_approval (the whole point of
	// WithAsyncApproval: the turn goroutine blocks, the TUI answers).
	g, err := wf.host.GetSession(ctx, managerPrincipal(), wf.sessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if g.State != core.SessionAwaitingApproval {
		t.Fatalf("session state = %v, want SessionAwaitingApproval", g.State)
	}

	// Answer it: allow_once. This must unblock the turn goroutine.
	if err := f.RespondToApproval(ctx, managerPrincipal(), core.ApprovalDecision{
		SessionID:  wf.sessionID,
		ApprovalID: approvalID,
		Outcome:    core.ApprovalAllowOnce,
	}); err != nil {
		t.Fatalf("RespondToApproval: %v", err)
	}

	// Wait for the turn to complete; assert the approval resolved approved
	// and the turn completed (not cancelled/stream_error).
	var (
		mu            sync.Mutex
		resolvedState string
		turnOutcome   string
		idle          bool
	)
	for time.Now().Before(deadline) && !idle {
		events, err := f.ListEvents(ctx, managerPrincipal(), wf.sessionID, 0, 0)
		if err != nil {
			t.Fatalf("ListEvents: %v", err)
		}
		mu.Lock()
		for _, ev := range events {
			switch ev.Kind {
			case event.KindApprovalResolved:
				resolvedState = ev.Payload.(event.ApprovalResolved).Outcome
			case event.KindTurnCompleted:
				turnOutcome = ev.Payload.(event.TurnCompleted).Outcome
			}
		}
		mu.Unlock()
		if turnOutcome != "" {
			if g, err := wf.host.GetSession(ctx, managerPrincipal(), wf.sessionID); err == nil {
				idle = g.State == core.SessionIdle
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if turnOutcome == "" {
		t.Fatal("turn never completed after approval")
	}
	if turnOutcome != "complete" {
		t.Fatalf("turn outcome = %q, want complete (the tool ran)", turnOutcome)
	}
	if resolvedState != "approved" {
		t.Fatalf("ApprovalResolved outcome = %q, want approved", resolvedState)
	}
}

// TestManagerRotationFinalizesOldConversation verifies the m4b B4 fix:
// NewConversation with a current facade keeps the OLD facade's App untouched
// (the old /new path cleared Conv and rotated ChatID inside
// HandleTUICommand) while building a genuinely fresh conversation.
func TestManagerRotationFinalizesOldConversation(t *testing.T) {
	srv := sseServer(t, []string{contentChunk("ok")})
	defer srv.Close()

	var exe exec.Executor = fakeExec{}
	mgr, err := NewConversationManager(testManagerConfig(t, srv.URL), exe, managerPrincipal())
	if err != nil {
		t.Fatalf("NewConversationManager: %v", err)
	}
	ctx := context.Background()
	f1, err := mgr.NewConversation(ctx, managerPrincipal(), nil)
	if err != nil {
		t.Fatalf("first NewConversation: %v", err)
	}
	wf1 := f1.(*wiringFacade)
	oldChat := wf1.app.Client.ChatID
	oldSessionID := wf1.sessionID

	// Rotate: pass the old facade as current.
	f2, err := mgr.NewConversation(ctx, managerPrincipal(), f1)
	if err != nil {
		t.Fatalf("rotated NewConversation: %v", err)
	}
	defer func() { _ = mgr.Close(f2) }()

	wf2 := f2.(*wiringFacade)
	if wf2.app.Client.ChatID == oldChat {
		t.Error("new conversation reuses the old ChatID — rotation built no fresh App")
	}
	if wf2.sessionID == oldSessionID {
		t.Error("new conversation reuses the old session ID")
	}
	// The OLD facade's App must be untouched by rotation.
	if wf1.app.Client.ChatID != oldChat {
		t.Error("rotation mutated the old App's ChatID")
	}
}
