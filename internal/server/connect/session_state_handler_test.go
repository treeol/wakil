package connect

import (
	"context"
	"io"
	"testing"

	"connectrpc.com/connect"
	v1alpha1 "github.com/treeol/wakil/api/gen/wakil/v1alpha1"
	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/proxy"
)

// testSessionStateHandler creates a SessionStateHandler backed by a minimal
// test App for handler-level testing. The App is bare (no real client/exec)
// but has consent initialized and Cfg set to defaults.
func testSessionStateHandler(t *testing.T) *SessionStateHandler {
	t.Helper()
	app := &agent.App{
		Cfg: config.DefaultConfig(),
		Out: io.Discard,
	}
	app.SetConsent(agent.ConsentSnapshot{})
	app.Session = &agent.Session{
		ChatID: "chat-test",
		Label:  "Test Session",
	}
	app.Client = &proxy.Client{
		ChatID:  "chat-test",
		BaseURL: "http://localhost:8080",
		Model:   "test-model",
	}
	resolver := &fakeResolver{principal: core.Principal{
		TenantID: "tnt_test",
		UserID:   "usr_test",
		Role:     core.RoleAdmin,
	}}
	return NewSessionStateHandler(app, resolver)
}

func TestGetSessionState(t *testing.T) {
	h := testSessionStateHandler(t)

	resp, err := h.GetSessionState(context.Background(), connect.NewRequest(&v1alpha1.GetSessionStateRequest{
		SessionId: "sess-123",
	}))
	if err != nil {
		t.Fatalf("GetSessionState: %v", err)
	}

	state := resp.Msg
	if state.SessionId != "sess-123" {
		t.Errorf("SessionId = %q, want %q", state.SessionId, "sess-123")
	}
	if state.ChatId != "chat-test" {
		t.Errorf("ChatId = %q, want %q", state.ChatId, "chat-test")
	}
	if state.Title != "Test Session" {
		t.Errorf("Title = %q, want %q", state.Title, "Test Session")
	}
	if state.EffectiveModel != "test-model" {
		t.Errorf("EffectiveModel = %q, want %q", state.EffectiveModel, "test-model")
	}
	if state.BaseUrl != "http://localhost:8080" {
		t.Errorf("BaseUrl = %q, want %q", state.BaseUrl, "http://localhost:8080")
	}
	if state.ContextLimit == nil {
		t.Fatal("ContextLimit should not be nil")
	}
}

func TestGetSessionStateUnauthenticated(t *testing.T) {
	app := &agent.App{Cfg: config.DefaultConfig(), Out: io.Discard}
	app.SetConsent(agent.ConsentSnapshot{})
	// Resolver that returns ErrUnauthenticated.
	badResolver := &fakeErrResolver{err: errUnauthenticated}
	h := NewSessionStateHandler(app, badResolver)

	_, err := h.GetSessionState(context.Background(), connect.NewRequest(&v1alpha1.GetSessionStateRequest{}))
	if err == nil {
		t.Fatal("expected error for unauthenticated request")
	}
	// Should be mapped to CodeUnauthenticated.
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("expected CodeUnauthenticated, got %v", connect.CodeOf(err))
	}
}

func TestSetModel(t *testing.T) {
	h := testSessionStateHandler(t)

	resp, err := h.SetModel(context.Background(), connect.NewRequest(&v1alpha1.SetModelRequest{
		SessionId: "sess-123",
		Model:     "gpt-5-turbo",
	}))
	if err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	if resp.Msg.Notice == "" {
		t.Error("Notice should not be empty")
	}
	if h.app.SelectedModel != "gpt-5-turbo" {
		t.Errorf("app.SelectedModel = %q, want %q", h.app.SelectedModel, "gpt-5-turbo")
	}
}

func TestSetModelEmpty(t *testing.T) {
	h := testSessionStateHandler(t)

	_, err := h.SetModel(context.Background(), connect.NewRequest(&v1alpha1.SetModelRequest{
		Model: "",
	}))
	if err == nil {
		t.Fatal("expected error for empty model")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("expected CodeInvalidArgument, got %v", connect.CodeOf(err))
	}
}

func TestSetBackend(t *testing.T) {
	h := testSessionStateHandler(t)

	resp, err := h.SetBackend(context.Background(), connect.NewRequest(&v1alpha1.SetBackendRequest{
		SessionId: "sess-123",
		Backend:   "openrouter/anthropic/claude-opus-4",
	}))
	if err != nil {
		t.Fatalf("SetBackend: %v", err)
	}
	if resp.Msg.Notice == "" {
		t.Error("Notice should not be empty")
	}
	if h.app.SelectedBackend != "openrouter" {
		t.Errorf("app.SelectedBackend = %q, want %q", h.app.SelectedBackend, "openrouter")
	}
	if h.app.SelectedModel != "openrouter/anthropic/claude-opus-4" {
		t.Errorf("app.SelectedModel = %q, want %q", h.app.SelectedModel, "openrouter/anthropic/claude-opus-4")
	}
}

func TestSetAutoApprove(t *testing.T) {
	h := testSessionStateHandler(t)

	resp, err := h.SetAutoApprove(context.Background(), connect.NewRequest(&v1alpha1.SetAutoApproveRequest{
		SessionId: "sess-123",
		Value:     true,
	}))
	if err != nil {
		t.Fatalf("SetAutoApprove: %v", err)
	}
	if resp.Msg.Notice == "" {
		t.Error("Notice should not be empty")
	}
	if !h.app.Consent().AutoApprove {
		t.Error("AutoApprove should be true")
	}

	// Toggle off.
	_, err = h.SetAutoApprove(context.Background(), connect.NewRequest(&v1alpha1.SetAutoApproveRequest{
		Value: false,
	}))
	if err != nil {
		t.Fatalf("SetAutoApprove(false): %v", err)
	}
	if h.app.Consent().AutoApprove {
		t.Error("AutoApprove should be false after revoke")
	}
}

func TestSetAllowDestructive(t *testing.T) {
	h := testSessionStateHandler(t)

	// Enable auto first (required before destructive).
	_, _ = h.SetAutoApprove(context.Background(), connect.NewRequest(&v1alpha1.SetAutoApproveRequest{
		Value: true,
	}))

	resp, err := h.SetAllowDestructive(context.Background(), connect.NewRequest(&v1alpha1.SetAllowDestructiveRequest{
		Value: true,
	}))
	if err != nil {
		t.Fatalf("SetAllowDestructive: %v", err)
	}
	if !h.app.Consent().AllowDestructive {
		t.Error("AllowDestructive should be true")
	}
	if resp.Msg.Notice == "" {
		t.Error("Notice should not be empty")
	}
}

func TestSetAllowDestructiveRequiresAuto(t *testing.T) {
	h := testSessionStateHandler(t)

	resp, err := h.SetAllowDestructive(context.Background(), connect.NewRequest(&v1alpha1.SetAllowDestructiveRequest{
		Value: true,
	}))
	if err != nil {
		t.Fatalf("SetAllowDestructive: %v", err)
	}
	// Should not enable — auto is OFF.
	if h.app.Consent().AllowDestructive {
		t.Error("AllowDestructive should not be enabled without auto")
	}
	if resp.Msg.Notice == "" {
		t.Error("Notice should explain auto is OFF")
	}
}

func TestRevokeAuto(t *testing.T) {
	h := testSessionStateHandler(t)

	// Set up auto + destructive.
	h.app.SetAutoApprove(true)
	h.app.SetAllowDestructive(true)

	_, err := h.RevokeAuto(context.Background(), connect.NewRequest(&v1alpha1.RevokeAutoRequest{
		SessionId: "sess-123",
	}))
	if err != nil {
		t.Fatalf("RevokeAuto: %v", err)
	}
	cs := h.app.Consent()
	if cs.AutoApprove {
		t.Error("AutoApprove should be false after revoke")
	}
	if cs.AllowDestructive {
		t.Error("AllowDestructive should be false after revoke")
	}
}

func TestSetSubagentEndpoint(t *testing.T) {
	h := testSessionStateHandler(t)

	// "inherit" clears.
	_, err := h.SetSubagentEndpoint(context.Background(), connect.NewRequest(&v1alpha1.SetSubagentEndpointRequest{
		Endpoint: "inherit",
	}))
	if err != nil {
		t.Fatalf("SetSubagentEndpoint(inherit): %v", err)
	}
	if h.app.SubagentEndpointOverride != "" {
		t.Errorf("SubagentEndpointOverride = %q, want empty", h.app.SubagentEndpointOverride)
	}
}

func TestSetSubagentModel(t *testing.T) {
	h := testSessionStateHandler(t)

	resp, err := h.SetSubagentModel(context.Background(), connect.NewRequest(&v1alpha1.SetSubagentModelRequest{
		Model: "claude-sonnet-4",
	}))
	if err != nil {
		t.Fatalf("SetSubagentModel: %v", err)
	}
	if resp.Msg.Notice == "" {
		t.Error("Notice should not be empty")
	}
	if h.app.SubagentModelOverride != "claude-sonnet-4" {
		t.Errorf("SubagentModelOverride = %q, want %q", h.app.SubagentModelOverride, "claude-sonnet-4")
	}

	// Inherit clears.
	_, err = h.SetSubagentModel(context.Background(), connect.NewRequest(&v1alpha1.SetSubagentModelRequest{
		Model: "inherit",
	}))
	if err != nil {
		t.Fatalf("SetSubagentModel(inherit): %v", err)
	}
	if h.app.SubagentModelOverride != "" {
		t.Errorf("SubagentModelOverride = %q, want empty", h.app.SubagentModelOverride)
	}
}

func TestSetMaxParallelSubagents(t *testing.T) {
	h := testSessionStateHandler(t)

	resp, err := h.SetMaxParallelSubagents(context.Background(), connect.NewRequest(&v1alpha1.SetMaxParallelSubagentsRequest{
		Value: 8,
	}))
	if err != nil {
		t.Fatalf("SetMaxParallelSubagents: %v", err)
	}
	if resp.Msg.Notice == "" {
		t.Error("Notice should not be empty")
	}
	if h.app.Cfg.MaxParallelSubagents != 8 {
		t.Errorf("MaxParallelSubagents = %d, want 8", h.app.Cfg.MaxParallelSubagents)
	}
}

func TestSetMaxParallelSubagentsCap(t *testing.T) {
	h := testSessionStateHandler(t)

	_, err := h.SetMaxParallelSubagents(context.Background(), connect.NewRequest(&v1alpha1.SetMaxParallelSubagentsRequest{
		Value: 100,
	}))
	if err != nil {
		t.Fatalf("SetMaxParallelSubagents: %v", err)
	}
	if h.app.Cfg.MaxParallelSubagents != 64 {
		t.Errorf("MaxParallelSubagents = %d, want 64 (capped)", h.app.Cfg.MaxParallelSubagents)
	}
}

func TestSetMaxParallelSubagentsInvalid(t *testing.T) {
	h := testSessionStateHandler(t)

	_, err := h.SetMaxParallelSubagents(context.Background(), connect.NewRequest(&v1alpha1.SetMaxParallelSubagentsRequest{
		Value: 0,
	}))
	if err == nil {
		t.Fatal("expected error for value 0")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("expected CodeInvalidArgument, got %v", connect.CodeOf(err))
	}
}

func TestSetEffectiveCtxMax(t *testing.T) {
	h := testSessionStateHandler(t)

	resp, err := h.SetEffectiveCtxMax(context.Background(), connect.NewRequest(&v1alpha1.SetEffectiveCtxMaxRequest{
		Value: 50000,
	}))
	if err != nil {
		t.Fatalf("SetEffectiveCtxMax: %v", err)
	}
	if resp.Msg.Notice == "" {
		t.Error("Notice should not be empty")
	}
	if h.app.EffectiveCtxMaxCharsOverride != 50000 {
		t.Errorf("EffectiveCtxMaxCharsOverride = %d, want 50000", h.app.EffectiveCtxMaxCharsOverride)
	}

	// 0 = disabled.
	_, err = h.SetEffectiveCtxMax(context.Background(), connect.NewRequest(&v1alpha1.SetEffectiveCtxMaxRequest{
		Value: 0,
	}))
	if err != nil {
		t.Fatalf("SetEffectiveCtxMax(0): %v", err)
	}
	if h.app.EffectiveCtxMaxCharsOverride != 0 {
		t.Errorf("EffectiveCtxMaxCharsOverride = %d, want 0", h.app.EffectiveCtxMaxCharsOverride)
	}
}

func TestSetEffectiveCtxMaxNegative(t *testing.T) {
	h := testSessionStateHandler(t)

	_, err := h.SetEffectiveCtxMax(context.Background(), connect.NewRequest(&v1alpha1.SetEffectiveCtxMaxRequest{
		Value: -1,
	}))
	if err == nil {
		t.Fatal("expected error for negative value")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("expected CodeInvalidArgument, got %v", connect.CodeOf(err))
	}
}

func TestSetRawTools(t *testing.T) {
	h := testSessionStateHandler(t)

	resp, err := h.SetRawTools(context.Background(), connect.NewRequest(&v1alpha1.SetRawToolsRequest{
		Value: true,
	}))
	if err != nil {
		t.Fatalf("SetRawTools: %v", err)
	}
	if resp.Msg.Notice == "" {
		t.Error("Notice should not be empty")
	}
	if !h.app.RawTools {
		t.Error("RawTools should be true")
	}

	// Toggle off.
	_, err = h.SetRawTools(context.Background(), connect.NewRequest(&v1alpha1.SetRawToolsRequest{
		Value: false,
	}))
	if err != nil {
		t.Fatalf("SetRawTools(false): %v", err)
	}
	if h.app.RawTools {
		t.Error("RawTools should be false")
	}
}

func TestSetCounselMode(t *testing.T) {
	h := testSessionStateHandler(t)

	for _, mode := range []string{"auto", "suggest", "off"} {
		resp, err := h.SetCounselMode(context.Background(), connect.NewRequest(&v1alpha1.SetCounselModeRequest{
			Mode: mode,
		}))
		if err != nil {
			t.Fatalf("SetCounselMode(%s): %v", mode, err)
		}
		if resp.Msg.Notice == "" {
			t.Errorf("Notice should not be empty for mode %s", mode)
		}
		if h.app.CounselMode != mode {
			t.Errorf("CounselMode = %q, want %q", h.app.CounselMode, mode)
		}
	}
}

func TestSetCounselModeInvalid(t *testing.T) {
	h := testSessionStateHandler(t)

	_, err := h.SetCounselMode(context.Background(), connect.NewRequest(&v1alpha1.SetCounselModeRequest{
		Mode: "bogus",
	}))
	if err == nil {
		t.Fatal("expected error for invalid mode")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("expected CodeInvalidArgument, got %v", connect.CodeOf(err))
	}
}

func TestCompact(t *testing.T) {
	h := testSessionStateHandler(t)

	// Empty Conv: nothing to compact.
	resp, err := h.Compact(context.Background(), connect.NewRequest(&v1alpha1.CompactRequest{
		SessionId: "sess-123",
	}))
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if resp.Msg.Compacted {
		t.Error("Compacted should be false for empty conv")
	}
	if resp.Msg.Notice == "" {
		t.Error("Notice should not be empty")
	}
}

func TestSetSessionLabel(t *testing.T) {
	h := testSessionStateHandler(t)

	resp, err := h.SetSessionLabel(context.Background(), connect.NewRequest(&v1alpha1.SetSessionLabelRequest{
		SessionId: "sess-123",
		Label:     "My New Label",
	}))
	if err != nil {
		t.Fatalf("SetSessionLabel: %v", err)
	}
	if resp.Msg.Notice == "" {
		t.Error("Notice should not be empty")
	}
	if h.app.Session.Label != "My New Label" {
		t.Errorf("Session.Label = %q, want %q", h.app.Session.Label, "My New Label")
	}
}

func TestSetSessionLabelNoSession(t *testing.T) {
	h := testSessionStateHandler(t)
	h.app.Session = nil

	resp, err := h.SetSessionLabel(context.Background(), connect.NewRequest(&v1alpha1.SetSessionLabelRequest{
		Label: "test",
	}))
	if err != nil {
		t.Fatalf("SetSessionLabel: %v", err)
	}
	if resp.Msg.Notice == "" {
		t.Error("Notice should explain no active session")
	}
}

func TestSaveRepoStateDescribe(t *testing.T) {
	h := testSessionStateHandler(t)

	resp, err := h.SaveRepoState(context.Background(), connect.NewRequest(&v1alpha1.SaveRepoStateRequest{
		SessionId: "sess-123",
		Clear:     false,
	}))
	if err != nil {
		t.Fatalf("SaveRepoState: %v", err)
	}
	if resp.Msg.Notice == "" {
		t.Error("Notice should not be empty (describe output)")
	}
}

func TestServerWithSessionState(t *testing.T) {
	// Verify the Server can mount the SessionStateService handler.
	app := &agent.App{Cfg: config.DefaultConfig(), Out: io.Discard}
	app.SetConsent(agent.ConsentSnapshot{})
	resolver := NewEmbeddedResolver()
	h := NewSessionStateHandler(app, resolver)

	srv := &Server{
		session:      NewSessionHandler(nil, nil, resolver),
		system:       NewSystemHandler(false),
		resolver:     resolver,
		sessionState: h,
	}

	handler := srv.Handler()
	if handler == nil {
		t.Fatal("Handler() should not return nil")
	}

	// The handler should serve the SessionStateService path.
	// (A full HTTP test would require a running server; here we just verify
	// the Server struct composes without panicking.)
}
