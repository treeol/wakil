package wiring

import (
	"context"
	"testing"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionhost"
	"github.com/treeol/wakil/internal/proxy"
)

// testFacade builds a minimal wiringFacade backed by a real sessionhost.Host
// and a minimal *agent.App, for testing thin-passthrough facade methods.
// It uses the same pattern as facade_test.go's EventPump tests.
func testFacade(t *testing.T) (*wiringFacade, *sessionhost.Host, func()) {
	t.Helper()

	turn := func(ctx context.Context, in sessionhost.TurnInput) (string, error) {
		return "test", nil
	}
	host := sessionhost.New(turn)

	principal := core.Principal{
		TenantID: event.EmbeddedTenantID,
		UserID:   event.EmbeddedUserID,
		Role:     core.RoleOwner,
	}

	app := &agent.App{
		Cfg:    config.DefaultConfig(),
		Client: &proxy.Client{ChatID: "test-chat"},
	}

	// Create a HostTurnHandle for the app (needed by newWiringFacade).
	appOwnersMu.Lock()
	appOwners = map[*agent.App]*hostTurn{}
	appOwnersMu.Unlock()
	handle, err := NewHostTurnHandle(app)
	if err != nil {
		host.Close(context.Background())
		t.Fatalf("NewHostTurnHandle: %v", err)
	}

	f := newWiringFacade(app, handle, host, nil, principal)
	cleanup := func() {
		handle.Release()
		host.Close(context.Background())
	}
	return f, host, cleanup
}

func TestFacade_CreateSession(t *testing.T) {
	f, host, cleanup := testFacade(t)
	defer cleanup()

	principal := core.Principal{
		TenantID: event.EmbeddedTenantID,
		UserID:   event.EmbeddedUserID,
		Role:     core.RoleOwner,
	}
	ws, _ := event.NewWorkspaceID("wsp_test")
	sess, err := f.CreateSession(context.Background(), principal, core.CreateSessionRequest{Workspace: ws})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if sess.ID == "" {
		t.Error("expected non-empty session ID")
	}
	// Cleanup the session.
	host.CloseSession(context.Background(), principal, sess.ID)
}

func TestFacade_Interrupt(t *testing.T) {
	f, _, cleanup := testFacade(t)
	defer cleanup()

	principal := core.Principal{
		TenantID: event.EmbeddedTenantID,
		UserID:   event.EmbeddedUserID,
		Role:     core.RoleOwner,
	}
	ws, _ := event.NewWorkspaceID("wsp_test")
	sess, _ := f.CreateSession(context.Background(), principal, core.CreateSessionRequest{Workspace: ws})

	// Interrupt on a session with no active turn returns an error
	// (invalid state transition) — this is the expected behavior.
	err := f.Interrupt(context.Background(), principal, sess.ID)
	if err == nil {
		t.Error("Interrupt on idle session should return error (no active turn)")
	}
}

func TestFacade_CloseSession(t *testing.T) {
	f, _, cleanup := testFacade(t)
	defer cleanup()

	principal := core.Principal{
		TenantID: event.EmbeddedTenantID,
		UserID:   event.EmbeddedUserID,
		Role:     core.RoleOwner,
	}
	ws, _ := event.NewWorkspaceID("wsp_test")
	sess, _ := f.CreateSession(context.Background(), principal, core.CreateSessionRequest{Workspace: ws})

	if err := f.CloseSession(context.Background(), principal, sess.ID); err != nil {
		t.Errorf("CloseSession: %v", err)
	}
}

func TestFacade_Consent(t *testing.T) {
	f, _, cleanup := testFacade(t)
	defer cleanup()

	c := f.Consent()
	// Default config: all consent fields should be false.
	if c.AutoApprove {
		t.Error("AutoApprove should be false by default")
	}
	if c.AllowDestructive {
		t.Error("AllowDestructive should be false by default")
	}
}

func TestFacade_CompletionSource(t *testing.T) {
	f, _, cleanup := testFacade(t)
	defer cleanup()

	cs := f.CompletionSource()
	if cs == nil {
		t.Fatal("CompletionSource returned nil")
	}
}

func TestFacade_SetAllowDestructive(t *testing.T) {
	f, _, cleanup := testFacade(t)
	defer cleanup()

	f.SetAllowDestructive(true)
	if !f.Consent().AllowDestructive {
		t.Error("AllowDestructive should be true after SetAllowDestructive(true)")
	}
	f.SetAllowDestructive(false)
	if f.Consent().AllowDestructive {
		t.Error("AllowDestructive should be false after SetAllowDestructive(false)")
	}
}

func TestFacade_RevokeAuto(t *testing.T) {
	f, _, cleanup := testFacade(t)
	defer cleanup()

	f.SetAutoApprove(true)
	if !f.Consent().AutoApprove {
		t.Error("AutoApprove should be true after SetAutoApprove(true)")
	}
	f.RevokeAuto()
	if f.Consent().AutoApprove {
		t.Error("AutoApprove should be false after RevokeAuto")
	}
}

func TestFacade_AppendSystemMessage(t *testing.T) {
	f, _, cleanup := testFacade(t)
	defer cleanup()

	content := "test message"
	f.AppendSystemMessage(proxy.Message{Role: "system", Content: &content})
	// Verify the message was appended to the app's conversation.
	if len(f.app.Conv) == 0 {
		t.Error("expected system message to be appended to Conv")
	}
}

func TestFacade_ConsumeStartupNote(t *testing.T) {
	f, _, cleanup := testFacade(t)
	defer cleanup()

	f.app.StartupNote = "hello world"
	note := f.ConsumeStartupNote()
	if note != "hello world" {
		t.Errorf("ConsumeStartupNote = %q, want %q", note, "hello world")
	}
	// Second call should return empty (consumed).
	note2 := f.ConsumeStartupNote()
	if note2 != "" {
		t.Errorf("ConsumeStartupNote after consume = %q, want empty", note2)
	}
}

func TestFacade_SetInfoPanelOpen(t *testing.T) {
	f, _, cleanup := testFacade(t)
	defer cleanup()

	f.SetInfoPanelOpen(true)
	if !f.app.InfoPanelOpen {
		t.Error("InfoPanelOpen should be true after SetInfoPanelOpen(true)")
	}
	f.SetInfoPanelOpen(false)
	if f.app.InfoPanelOpen {
		t.Error("InfoPanelOpen should be false after SetInfoPanelOpen(false)")
	}
}

func TestFacade_ClearPendingImages(t *testing.T) {
	f, _, cleanup := testFacade(t)
	defer cleanup()

	f.AddPendingImage(proxy.ImagePart{Path: "test.png"})
	f.ClearPendingImages()
	// No direct accessor; just verify no panic.
}

func TestFacade_ReplacePendingImages(t *testing.T) {
	f, _, cleanup := testFacade(t)
	defer cleanup()

	f.ReplacePendingImages([]proxy.ImagePart{{Path: "a.png"}, {Path: "b.png"}})
	// No direct accessor; just verify no panic.
}
