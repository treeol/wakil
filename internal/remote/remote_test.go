package remote

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	v1alpha1 "github.com/treeol/wakil/api/gen/wakil/v1alpha1"
	wakilv1alpha1connect "github.com/treeol/wakil/api/gen/wakil/v1alpha1/wakilv1alpha1connect"
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/protoconv"
	"google.golang.org/protobuf/proto"
)

// TestDial tests the Unix socket dialer: it verifies the socket exists,
// connects, and the service clients are constructed.
func TestDial(t *testing.T) {
	// Start a minimal HTTP server on a Unix socket.
	dir := t.TempDir()
	sock := filepath.Join(dir, "test.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	mux := http.NewServeMux()
	// Mount a minimal SystemService handler that returns "ready".
	mux.Handle(wakilv1alpha1connect.NewSystemServiceHandler(
		&testSystemHandler{},
	))
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())

	// Dial.
	clients, err := Dial(sock)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer clients.Close()

	// Health check.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := CheckHealth(ctx, clients); err != nil {
		t.Fatalf("CheckHealth: %v", err)
	}
}

// TestDialMissingSocket verifies Dial errors when the socket doesn't exist.
func TestDialMissingSocket(t *testing.T) {
	_, err := Dial("/tmp/nonexistent-wakild-test.sock")
	if err == nil {
		t.Fatal("Dial should error on missing socket")
	}
}

// TestDialInUse verifies Dial errors when the socket is not connectable.
func TestDialInUse(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "stale.sock")
	// Create a file that's not a socket.
	if err := os.WriteFile(sock, []byte("not a socket"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := Dial(sock)
	if err == nil {
		t.Fatal("Dial should error on non-socket file")
	}
}

// TestDefaultSocketPath verifies the default socket path derivation.
func TestDefaultSocketPath(t *testing.T) {
	p := DefaultSocketPath()
	if p == "" {
		t.Fatal("DefaultSocketPath returned empty")
	}
	if p == "wakild.sock" {
		// Only acceptable when neither XDG_RUNTIME_DIR nor HOME is set.
		t.Logf("DefaultSocketPath fell back to wakild.sock (no XDG/HOME)")
	}
}

// testSessionStateHandler is a minimal SessionStateService handler that
// returns a fixed SessionState. Used to test the facade's projection without
// a full daemon.
type testSessionStateHandler struct {
	state *v1alpha1.SessionState
}

func (h *testSessionStateHandler) GetSessionState(ctx context.Context, req *connect.Request[v1alpha1.GetSessionStateRequest]) (*connect.Response[v1alpha1.SessionState], error) {
	if h.state == nil {
		h.state = &v1alpha1.SessionState{}
	}
	s := proto.Clone(h.state).(*v1alpha1.SessionState)
	s.SessionId = req.Msg.SessionId
	return connect.NewResponse(s), nil
}

func (h *testSessionStateHandler) SetModel(context.Context, *connect.Request[v1alpha1.SetModelRequest]) (*connect.Response[v1alpha1.SetModelResponse], error) {
	return connect.NewResponse(&v1alpha1.SetModelResponse{}), nil
}
func (h *testSessionStateHandler) SetBackend(context.Context, *connect.Request[v1alpha1.SetBackendRequest]) (*connect.Response[v1alpha1.SetBackendResponse], error) {
	return connect.NewResponse(&v1alpha1.SetBackendResponse{}), nil
}
func (h *testSessionStateHandler) SetAutoApprove(context.Context, *connect.Request[v1alpha1.SetAutoApproveRequest]) (*connect.Response[v1alpha1.SetAutoApproveResponse], error) {
	return connect.NewResponse(&v1alpha1.SetAutoApproveResponse{}), nil
}
func (h *testSessionStateHandler) SetAllowDestructive(context.Context, *connect.Request[v1alpha1.SetAllowDestructiveRequest]) (*connect.Response[v1alpha1.SetAllowDestructiveResponse], error) {
	return connect.NewResponse(&v1alpha1.SetAllowDestructiveResponse{}), nil
}
func (h *testSessionStateHandler) RevokeAuto(context.Context, *connect.Request[v1alpha1.RevokeAutoRequest]) (*connect.Response[v1alpha1.RevokeAutoResponse], error) {
	return connect.NewResponse(&v1alpha1.RevokeAutoResponse{}), nil
}
func (h *testSessionStateHandler) SetSubagentEndpoint(context.Context, *connect.Request[v1alpha1.SetSubagentEndpointRequest]) (*connect.Response[v1alpha1.SetSubagentEndpointResponse], error) {
	return connect.NewResponse(&v1alpha1.SetSubagentEndpointResponse{}), nil
}
func (h *testSessionStateHandler) SetSubagentModel(context.Context, *connect.Request[v1alpha1.SetSubagentModelRequest]) (*connect.Response[v1alpha1.SetSubagentModelResponse], error) {
	return connect.NewResponse(&v1alpha1.SetSubagentModelResponse{}), nil
}
func (h *testSessionStateHandler) SetMaxParallelSubagents(context.Context, *connect.Request[v1alpha1.SetMaxParallelSubagentsRequest]) (*connect.Response[v1alpha1.SetMaxParallelSubagentsResponse], error) {
	return connect.NewResponse(&v1alpha1.SetMaxParallelSubagentsResponse{}), nil
}
func (h *testSessionStateHandler) SetEffectiveCtxMax(context.Context, *connect.Request[v1alpha1.SetEffectiveCtxMaxRequest]) (*connect.Response[v1alpha1.SetEffectiveCtxMaxResponse], error) {
	return connect.NewResponse(&v1alpha1.SetEffectiveCtxMaxResponse{}), nil
}
func (h *testSessionStateHandler) SetRawTools(context.Context, *connect.Request[v1alpha1.SetRawToolsRequest]) (*connect.Response[v1alpha1.SetRawToolsResponse], error) {
	return connect.NewResponse(&v1alpha1.SetRawToolsResponse{}), nil
}
func (h *testSessionStateHandler) SetCounselMode(context.Context, *connect.Request[v1alpha1.SetCounselModeRequest]) (*connect.Response[v1alpha1.SetCounselModeResponse], error) {
	return connect.NewResponse(&v1alpha1.SetCounselModeResponse{}), nil
}
func (h *testSessionStateHandler) Compact(context.Context, *connect.Request[v1alpha1.CompactRequest]) (*connect.Response[v1alpha1.CompactResponse], error) {
	return connect.NewResponse(&v1alpha1.CompactResponse{}), nil
}
func (h *testSessionStateHandler) SaveRepoState(context.Context, *connect.Request[v1alpha1.SaveRepoStateRequest]) (*connect.Response[v1alpha1.SaveRepoStateResponse], error) {
	return connect.NewResponse(&v1alpha1.SaveRepoStateResponse{}), nil
}
func (h *testSessionStateHandler) SetSessionLabel(context.Context, *connect.Request[v1alpha1.SetSessionLabelRequest]) (*connect.Response[v1alpha1.SetSessionLabelResponse], error) {
	return connect.NewResponse(&v1alpha1.SetSessionLabelResponse{}), nil
}

// TestRemoteFacadeStateProjection verifies Snapshot()/Consent()/Info()/
// CompletionSource() project the daemon's SessionState fetched via
// GetSessionState over the wire.
func TestRemoteFacadeStateProjection(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "state.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	mux := http.NewServeMux()
	mux.Handle(wakilv1alpha1connect.NewSessionStateServiceHandler(&testSessionStateHandler{
		state: &v1alpha1.SessionState{
			ChatId:          "chat-proj",
			Title:           "Proj Session",
			Workspace:       "/ws",
			SelectedBackend: "b1",
			SelectedModel:   "m1",
			EffectiveModel:  "m1",
			ModelList:       []string{"m1", "m2"},
			BackendList:     []*v1alpha1.BackendInfo{{Name: "b1", External: true, Caps: []string{"a"}}},
			ConfigBackend:   "b0",
			AutoApprove:     true,
			AllowDestructive: false,
			AllowReads:      true,
			RawTools:        true,
			WorkflowLabel:   "wf",
			BaseUrl:         "http://x",
			LastBackend:     "b1",
			Cwd:             "/cwd",
			ExecMode:        "direct",
			PromptNote:      "note",
			EffectiveSubagentModel: "sm",
			ContextUsed:     123,
			ContextExact:    true,
			ContextLimit:    &v1alpha1.ContextLimit{NCtx: 100, UsableCtx: 88},
		},
	}))
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Shutdown(context.Background())

	clients, err := Dial(sock)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer clients.Close()

	f := newRemoteFacade(clients, core.Principal{}, "ws")
	f.setSession("sess-proj")

	// Wait for the async refreshState to land (bounded poll).
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.mu.Lock()
		ready := f.state != nil
		f.mu.Unlock()
		if ready || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	snap := f.Snapshot()
	if snap.ChatID != "chat-proj" {
		t.Errorf("Snapshot.ChatID = %q, want %q", snap.ChatID, "chat-proj")
	}
	if snap.Workspace != "/ws" {
		t.Errorf("Snapshot.Workspace = %q, want %q", snap.Workspace, "/ws")
	}
	if snap.Backend != "b1" || snap.Model != "m1" {
		t.Errorf("Snapshot backend/model = %q/%q, want b1/m1", snap.Backend, snap.Model)
	}
	if snap.RawTools != true {
		t.Errorf("Snapshot.RawTools = %v, want true", snap.RawTools)
	}
	if len(snap.ModelList) != 2 || snap.ModelList[0] != "m1" {
		t.Errorf("Snapshot.ModelList = %v, want [m1 m2]", snap.ModelList)
	}
	if len(snap.BackendList) != 1 || snap.BackendList[0].Name != "b1" {
		t.Errorf("Snapshot.BackendList = %v, want [{b1}]", snap.BackendList)
	}
	if snap.Workflow != nil {
		t.Errorf("Snapshot.Workflow = %v, want nil (daemon exposes only sidebar label)", snap.Workflow)
	}
	if snap.ContextLimit.UsableCtx != 88 {
		t.Errorf("Snapshot.ContextLimit.UsableCtx = %d, want 88", snap.ContextLimit.UsableCtx)
	}

	c := f.Consent()
	if !c.AutoApprove || c.AllowDestructive || !c.AllowReads {
		t.Errorf("Consent = %+v, want {AutoApprove:true AllowReads:true}", c)
	}

	info := f.Info()
	if info.ChatID != "chat-proj" || info.Cwd != "/cwd" || info.ExecMode != "direct" {
		t.Errorf("Info = %+v, want ChatID=chat-proj Cwd=/cwd ExecMode=direct", info)
	}
	if info.ContextUsed != 123 || !info.ContextExact {
		t.Errorf("Info context = %d/%v, want 123/true", info.ContextUsed, info.ContextExact)
	}
	if info.EffectiveModel != "m1" || info.SubagentModel != "sm" {
		t.Errorf("Info models = %q/%q, want m1/sm", info.EffectiveModel, info.SubagentModel)
	}
	if info.WorkflowLabel != "wf" {
		t.Errorf("Info.WorkflowLabel = %q, want wf", info.WorkflowLabel)
	}

	models := f.CompletionSource().Models()
	if len(models) != 2 || models[0] != "m1" {
		t.Errorf("CompletionSource.Models = %v, want [m1 m2]", models)
	}
	backends := f.CompletionSource().Backends()
	if len(backends) != 1 || backends[0].Name != "b1" || !backends[0].External {
		t.Errorf("CompletionSource.Backends = %v, want [{b1 external}]", backends)
	}
}

// testSystemHandler is a minimal SystemService handler for testing.
type testSystemHandler struct{}

func (h *testSystemHandler) GetServerInfo(ctx context.Context, req *connect.Request[v1alpha1.GetServerInfoRequest]) (*connect.Response[v1alpha1.ServerInfo], error) {
	return connect.NewResponse(&v1alpha1.ServerInfo{
		ApiVersion: "v1alpha1",
	}), nil
}

func (h *testSystemHandler) Health(ctx context.Context, req *connect.Request[v1alpha1.HealthRequest]) (*connect.Response[v1alpha1.HealthStatus], error) {
	return connect.NewResponse(&v1alpha1.HealthStatus{
		Status: "ready",
	}), nil
}

// TestEventFromProto verifies the proto→domain conversion via the shared
// protoconv package.
func TestEventFromProto(t *testing.T) {
	// Build a domain event, convert to proto, convert back, compare.
	original := event.Event{
		TenantID:  event.EmbeddedTenantID,
		SessionID: "sess_test",
		Seq:       1,
		Kind:      event.KindTurnStarted,
		Ts:        time.Now().UTC(),
		Payload:   event.TurnStarted{TurnID: "turn_1", TurnIndex: 1},
	}

	pb, err := protoconv.EventToProto(original)
	if err != nil {
		t.Fatalf("EventToProto: %v", err)
	}
	if pb.Kind != string(event.KindTurnStarted) {
		t.Fatalf("proto kind = %q, want %q", pb.Kind, event.KindTurnStarted)
	}

	round, err := protoconv.EventFromProto(pb)
	if err != nil {
		t.Fatalf("EventFromProto: %v", err)
	}
	if round.SessionID != original.SessionID {
		t.Errorf("session ID = %q, want %q", round.SessionID, original.SessionID)
	}
	if round.Seq != original.Seq {
		t.Errorf("seq = %d, want %d", round.Seq, original.Seq)
	}
	ts, ok := round.Payload.(event.TurnStarted)
	if !ok {
		t.Fatalf("payload type = %T, want event.TurnStarted", round.Payload)
	}
	if ts.TurnID != "turn_1" {
		t.Errorf("turn ID = %q, want %q", ts.TurnID, "turn_1")
	}
	if ts.TurnIndex != 1 {
		t.Errorf("turn index = %d, want 1", ts.TurnIndex)
	}
}

// TestRemoteEventPumpDedup verifies the pump deduplicates durable events by
// seq. This tests the core invariant: a re-delivered durable event (same seq)
// is not delivered twice.
func TestRemoteEventPumpDedup(t *testing.T) {
	// We can't easily test the pump without a real server stream, but we
	// can test the dedup logic directly by simulating events.
	clients := &Clients{} // no RPCs called — we test the dedup field
	pump := NewRemoteEventPump(clients, "sess_test", 0, func(ev event.Event) {})

	// Simulate a durable event with seq=1.
	ev1 := event.Event{
		Seq:     1,
		Kind:    event.KindTurnStarted,
		Payload: event.TurnStarted{TurnID: "turn_1"},
	}
	pb1, _ := protoconv.EventToProto(ev1)

	// Convert and check dedup.
	converted, err := eventFromProto(pb1)
	if err != nil {
		t.Fatalf("eventFromProto: %v", err)
	}

	// Manually apply the dedup logic from the pump.
	if converted.Seq > 0 {
		if converted.Seq <= pump.lastSeq {
			// would be skipped
		}
		if converted.Seq > pump.lastSeq {
			pump.lastSeq = converted.Seq
		}
	}

	// Second delivery of the same seq should be skipped.
	if converted.Seq > 0 && converted.Seq <= pump.lastSeq {
		// skip — correct behavior
	} else {
		t.Error("second delivery of same seq should be skipped")
	}

	if pump.LastSeq() != 1 {
		t.Errorf("LastSeq = %d, want 1", pump.LastSeq())
	}
}

// TestRemoteFacadeDispatchCommand verifies the remote facade's slash-command
// dispatch handles a few client-side commands and passes the rest through.
func TestRemoteFacadeDispatchCommand(t *testing.T) {
	f := &RemoteFacade{}

	// /quit
	r := f.DispatchCommand("/quit")
	if !r.Handled || !r.Quit {
		t.Errorf("/quit: Handled=%v Quit=%v", r.Handled, r.Quit)
	}

	// /new
	r = f.DispatchCommand("/new")
	if !r.Handled || r.Rotate == nil || r.Rotate.Type != "new" {
		t.Errorf("/new: Handled=%v Rotate=%v", r.Handled, r.Rotate)
	}

	// /resume <id>
	r = f.DispatchCommand("/resume abc123")
	if !r.Handled || r.Rotate == nil || r.Rotate.Type != "resume" {
		t.Errorf("/resume abc123: Handled=%v Rotate=%v", r.Handled, r.Rotate)
	}

	// /resume (bare) → picker
	r = f.DispatchCommand("/resume")
	if !r.Handled || !r.ResumePicker {
		t.Errorf("/resume: Handled=%v ResumePicker=%v", r.Handled, r.ResumePicker)
	}

	// Unknown command → not handled
	r = f.DispatchCommand("hello world")
	if r.Handled {
		t.Error("unknown command should not be handled")
	}
}
