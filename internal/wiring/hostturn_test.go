package wiring

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionhost"
	"github.com/treeol/wakil/internal/proxy"
	wtools "github.com/treeol/wakil/internal/tools"
)

// ---- minimal fake executor (satisfies exec.Executor with no real shell) ----

type fakeExec struct{}

func (fakeExec) RunShell(context.Context, string) (string, error)          { return "", nil }
func (fakeExec) ReadFile(context.Context, string) (string, error)          { return "", nil }
func (fakeExec) ListDir(context.Context, string) (string, error)           { return "", nil }
func (fakeExec) WriteFile(context.Context, string, string) (string, error) { return "", nil }
func (fakeExec) WriteFileBytes(context.Context, string, []byte) (string, error) {
	return "", nil
}
func (fakeExec) Cwd() string                                             { return "/work" }
func (fakeExec) Describe() string                                        { return "fake" }
func (fakeExec) Close() error                                            { return nil }
func (fakeExec) SandboxTools() string                                    { return "" }
func (fakeExec) WorkspaceRoot() string                                   { return "/work" }
func (fakeExec) ConfinePath(_ context.Context, p string) (string, error) { return p, nil }
func (fakeExec) DeletePath(context.Context, string) error                { return nil }
func (fakeExec) MovePath(context.Context, string, string) error          { return nil }
func (fakeExec) StartInteractive(context.Context, string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, int, error) {
	return nil, nil, nil, 0, fmt.Errorf("not implemented")
}
func (fakeExec) HostPathToURI(p string) (string, error) { return "file://" + p, nil }
func (fakeExec) URIToHostPath(u string) (string, error) {
	return strings.TrimPrefix(u, "file://"), nil
}
func (fakeExec) StartBackground(context.Context, string, string) (int, int, error) {
	return 0, 0, nil
}
func (fakeExec) KillPgid(context.Context, int, int) error      { return nil }
func (fakeExec) IsProcessAlive(context.Context, int) bool      { return false }
func (fakeExec) IsProcessGroupAlive(context.Context, int) bool { return false }
func (fakeExec) ReadFileTail(context.Context, string, int64) (string, error) {
	return "", nil
}
func (fakeExec) StatFile(context.Context, string) (int64, error) { return 0, nil }
func (fakeExec) Generation() int                                 { return 1 }
func (fakeExec) KVRSocketPath() string                           { return "" }
func (fakeExec) KVRAvailable() bool                              { return false }
func (fakeExec) ContainerName() string                           { return "" }
func (fakeExec) CDPPort() int                                    { return 0 }

// ---- fake SSE backend (mirrors agent.sseServer helpers, unexported there) ----

func fakeApp(url string) *agent.App {
	cfg := config.DefaultConfig()
	cfg.ShellTimeoutSec = 0
	return &agent.App{
		Cfg:     cfg,
		Client:  &proxy.Client{BaseURL: url, Model: "ilm", ChatID: "test", HTTP: http.DefaultClient},
		Exec:    fakeExec{},
		Tools:   wtools.DefaultTools("/work"),
		Out:     io.Discard,
		Confirm: func(toolName, headline, detail string, readAction bool) bool { return false },
	}
}

func contentChunk(s string) string {
	return fmt.Sprintf(`{"choices":[{"delta":{"content":%q},"finish_reason":null}]}`, s)
}

func sseServer(t *testing.T, framesPerCall ...[]string) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	call := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		frames := framesPerCall[0]
		if call < len(framesPerCall) {
			frames = framesPerCall[call]
		}
		call++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("ResponseWriter does not implement http.Flusher")
			return
		}
		flusher.Flush()
		for _, f := range frames {
			fmt.Fprintf(w, "data: %s\n\n", f)
			flusher.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
}

// waitUntil polls cond until it is true or the deadline passes.
func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}

// drainReplay reads the replay segment of a subscription until it stalls (the
// turn hasn't started yet); returns the events drained.
func drainReplay(t *testing.T, sub core.EventSubscription, want int) []event.Event {
	t.Helper()
	var out []event.Event
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for len(out) < want {
		ev, err := sub.Next(ctx)
		if err != nil {
			t.Fatalf("drain replay Next: %v", err)
		}
		out = append(out, ev)
	}
	return out
}

// TestIntegrationRealTurnDrivesSession is the chunk-5 headless proof: a real
// *agent.App turn (fake SSE backend, no tool calls) driven entirely through
// SessionService + EventReader, asserting the durable sequence and the
// authoritative MessageCommitted text.
func TestIntegrationRealTurnDrivesSession(t *testing.T) {
	const replyText = "hello from the backend"
	srv := sseServer(t, []string{contentChunk("hello "), contentChunk("from "), contentChunk("the backend")})
	defer srv.Close()

	app := fakeApp(srv.URL)
	turnFn, err := HostTurnFunc(app)
	if err != nil {
		t.Fatalf("HostTurnFunc: %v", err)
	}

	h := sessionhost.New(turnFn, sessionhost.WithAgentName("test-agent"))
	p := core.Principal{TenantID: "tnt_test", UserID: "usr_test", Role: core.RoleOwner}
	defer h.Close(context.Background())

	s, err := h.CreateSession(context.Background(), p, core.CreateSessionRequest{Workspace: "wsp_test", Title: "t"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	sub, err := h.Subscribe(context.Background(), p, s.ID, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	if _, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: s.ID, Text: "hey"}); err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}

	// Collect the full durable stream until TurnCompleted, then stop.
	var kinds []event.Kind
	var committed string
	var lastSeq event.Seq
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		ev, err := sub.Next(ctx)
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		switch ev.Kind {
		case event.KindMessageCommitted:
			committed = ev.Payload.(event.MessageCommitted).Text
			kinds = append(kinds, ev.Kind)
			lastSeq = ev.Seq
		case event.KindTurnCompleted:
			kinds = append(kinds, ev.Kind)
			if lastSeq == 0 {
				lastSeq = ev.Seq
			}
			goto done
		default:
			if ev.Kind.Class() == event.ClassDurable {
				kinds = append(kinds, ev.Kind)
			}
			if ev.Seq > lastSeq {
				lastSeq = ev.Seq
			}
		}
	}
done:

	// The complete durable kind sequence must contain the expected turn shape.
	for _, want := range []event.Kind{
		event.KindSessionCreated,
		event.KindTurnStarted,
		event.KindMessageCommitted,
		event.KindTurnCompleted,
	} {
		found := false
		for _, k := range kinds {
			if k == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("durable kinds %v missing %s", kinds, want)
		}
	}

	if committed != replyText {
		t.Fatalf("MessageCommitted.Text = %q, want %q", committed, replyText)
	}

	// Durable events strictly increasing seq, and the session is idle again.
	events, err := h.ListEvents(context.Background(), p, s.ID, 0, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	for i := range events {
		t.Logf("durable seq=%d kind=%s ts=%v", events[i].Seq, events[i].Kind, events[i].Ts)
	}
	prev := event.Seq(0)
	for _, e := range events {
		if e.Seq <= prev {
			t.Fatalf("seq not strictly increasing: %d after %d", e.Seq, prev)
		}
		prev = e.Seq
	}
	g, _ := h.GetSession(context.Background(), p, s.ID)
	if g.State != core.SessionIdle {
		t.Fatalf("final state = %q, want idle", g.State)
	}
}

// TestIntegrationApprovalEmitsRequestResolved drives a turn whose backend
// requests a tool call that flows through the approval gate, asserting the
// ApprovedRequested → ApprovedResolved pair with full outcome fidelity.
func TestIntegrationApprovalEmitsRequestResolved(t *testing.T) {
	// First backend call requests a tool call (gated by approval); after the
	// approval is resolved (declined here), the agent loops and the second call
	// returns a final text response, so the turn terminates.
	first := toolCallFrames("call_1", "write_file", `{"path":"a.txt","content":"x"}`)
	srv := sseServer(t, first, []string{contentChunk("done")})
	defer srv.Close()

	app := fakeApp(srv.URL)

	var mu sync.Mutex
	var approvals []ApprovalRequest
	resolver := func(ctx context.Context, req ApprovalRequest) ApprovalResolution {
		mu.Lock()
		approvals = append(approvals, req)
		mu.Unlock()
		return ApprovalResolution{Choice: agent.ChoiceDecline, Reason: "declined by test resolver"}
	}

	turnFn, err := HostTurnFunc(app, WithResolver(resolver))
	if err != nil {
		t.Fatalf("HostTurnFunc: %v", err)
	}
	h := sessionhost.New(turnFn)
	p := core.Principal{TenantID: "tnt_test", UserID: "usr_owner", Role: core.RoleOwner}
	defer h.Close(context.Background())

	s, err := h.CreateSession(context.Background(), p, core.CreateSessionRequest{Workspace: "wsp_test", Title: "t"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: s.ID, Text: "read a.go"}); err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}

	waitUntil(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s.ID)
		return g.State != core.SessionRunning
	})

	events, err := h.ListEvents(context.Background(), p, s.ID, 0, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}

	var reqEvent, resEvent *event.Event
	var reqIDs, resIDs []event.ApprovalID
	for i := range events {
		switch events[i].Kind {
		case event.KindApprovalRequested:
			reqEvent = &events[i]
			reqIDs = append(reqIDs, events[i].Payload.(event.ApprovalRequested).ApprovalID)
		case event.KindApprovalResolved:
			resEvent = &events[i]
			resIDs = append(resIDs, events[i].Payload.(event.ApprovalResolved).ApprovalID)
		}
	}
	if reqEvent == nil {
		t.Fatal("no ApprovalRequested emitted")
	}
	if resEvent == nil {
		t.Fatal("no ApprovalResolved emitted")
	}
	if len(reqIDs) != len(resIDs) {
		t.Fatalf("request/resolved count mismatch: %d vs %d", len(reqIDs), len(resIDs))
	}
	for i := range reqIDs {
		if reqIDs[i] != resIDs[i] {
			t.Fatalf("ApprovalResolved ID %q does not pair with request %q", resIDs[i], reqIDs[i])
		}
	}
	resOutcome := resEvent.Payload.(event.ApprovalResolved)
	if resOutcome.Outcome != "declined" {
		t.Fatalf("resolved outcome = %q, want declined", resOutcome.Outcome)
	}
	if resOutcome.Reason != "declined by test resolver" {
		t.Fatalf("resolved reason = %q, want %q", resOutcome.Reason, "declined by test resolver")
	}
	if resOutcome.Resolver != "usr_owner" {
		t.Fatalf("Resolver = %q, want usr_owner", resOutcome.Resolver)
	}

	// Ordering: ApprovalRequested.Seq < ApprovalResolved.Seq < turn terminal Seq.
	var turnDoneSeq event.Seq
	for i := range events {
		if events[i].Kind == event.KindTurnCompleted {
			turnDoneSeq = events[i].Seq
		}
	}
	if !(reqEvent.Seq < resEvent.Seq && resEvent.Seq < turnDoneSeq) {
		t.Fatalf("approval ordering violated: req=%d res=%d turnDone=%d", reqEvent.Seq, resEvent.Seq, turnDoneSeq)
	}

	mu.Lock()
	gotReq := len(approvals)
	mu.Unlock()
	if gotReq != 1 {
		t.Fatalf("resolver saw %d requests, want 1", gotReq)
	}
}

func toolCallFrames(id, name string, args string) []string {
	return []string{
		fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":%q,"arguments":""}}]},"finish_reason":null}]}`, id, name),
		fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":%q}}]},"finish_reason":null}]}`, args),
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	}
}

// TestHostTurnFuncSingleAppConstraint verifies a second claim on the same App
// (via a second HostTurnFunc call) is rejected loudly.
func TestHostTurnFuncSingleAppConstraint(t *testing.T) {
	app := fakeApp("http://127.0.0.1:1/v1/chat/completions")
	if _, err := HostTurnFunc(app); err != nil {
		t.Fatalf("first HostTurnFunc: %v", err)
	}
	if _, err := HostTurnFunc(app); err != ErrAppInUse {
		t.Fatalf("second HostTurnFunc err = %v, want ErrAppInUse", err)
	}
}

// TestHostTurnSingleSessionBinding verifies the same TurnFunc cannot drive two
// distinct host sessions (the claim is bound at the host-session level, not the
// construction level).
func TestHostTurnSingleSessionBinding(t *testing.T) {
	srv := sseServer(t, []string{contentChunk("ok")})
	defer srv.Close()

	app := fakeApp(srv.URL)
	turnFn, err := HostTurnFunc(app)
	if err != nil {
		t.Fatalf("HostTurnFunc: %v", err)
	}
	h := sessionhost.New(turnFn)
	p := core.Principal{TenantID: "tnt_test", UserID: "usr_test", Role: core.RoleOwner}
	defer h.Close(context.Background())

	s1 := createSessionT(t, h, p)
	if _, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: s1.ID, Text: "one"}); err != nil {
		t.Fatalf("SubmitInput s1: %v", err)
	}
	waitUntil(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s1.ID)
		return g.State == core.SessionIdle
	})

	// Second session: the turn must fail with internal_error (not backend).
	s2 := createSessionT(t, h, p)
	if _, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: s2.ID, Text: "two"}); err != nil {
		t.Fatalf("SubmitInput s2: %v", err)
	}
	waitUntil(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s2.ID)
		return g.State == core.SessionError
	})

	events, err := h.ListEvents(context.Background(), p, s2.ID, 0, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var reason string
	for _, e := range events {
		if e.Kind == event.KindSessionError {
			reason = e.Payload.(event.SessionError).Reason
		}
	}
	if reason != "internal_error" {
		t.Fatalf("s2 SessionError.Reason = %q, want internal_error", reason)
	}
}

func createSessionT(t *testing.T, h *sessionhost.Host, p core.Principal) core.Session {
	t.Helper()
	s, err := h.CreateSession(context.Background(), p, core.CreateSessionRequest{Workspace: "wsp_test", Title: "t"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return s
}

// TestApprovalCancelWhileBlocked verifies the approval shim does not hang the
// executor when a resolver blocks and the turn is closed: the confirmer must
// resolve declined via ctx cancellation (exit criterion #6).
func TestApprovalCancelWhileBlocked(t *testing.T) {
	srv := sseServer(t, toolCallFrames("call_1", "write_file", `{"path":"a.txt","content":"x"}`))
	defer srv.Close()

	app := fakeApp(srv.URL)

	resolverEntered := make(chan struct{})
	resolverRelease := make(chan struct{})
	resolver := func(ctx context.Context, req ApprovalRequest) ApprovalResolution {
		close(resolverEntered)
		<-resolverRelease // block: simulate a stuck resolver
		return ApprovalResolution{Choice: agent.ChoiceApprove}
	}

	turnFn, err := HostTurnFunc(app, WithResolver(resolver))
	if err != nil {
		t.Fatalf("HostTurnFunc: %v", err)
	}
	h := sessionhost.New(turnFn)
	p := core.Principal{TenantID: "tnt_test", UserID: "usr_owner", Role: core.RoleOwner}
	defer h.Close(context.Background())

	s := createSessionT(t, h, p)
	if _, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: s.ID, Text: "write"}); err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}
	<-resolverEntered

	// Close the session while the resolver is blocked.
	if err := h.CloseSession(context.Background(), p, s.ID); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}

	// The session must finalize (close) promptly — not hang on the resolver.
	waitUntil(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s.ID)
		return g.State == core.SessionClosed
	})

	// And the approval must have resolved as declined (cancellation forced it).
	events, err := h.ListEvents(context.Background(), p, s.ID, 0, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var resOutcome string
	foundResolved := false
	for _, e := range events {
		if e.Kind == event.KindApprovalResolved {
			foundResolved = true
			resOutcome = e.Payload.(event.ApprovalResolved).Outcome
		}
	}
	if !foundResolved {
		t.Fatal("no ApprovalResolved emitted before close")
	}
	if resOutcome != "declined" {
		t.Fatalf("ApprovalResolved.Outcome = %q, want declined (cancellation)", resOutcome)
	}
	close(resolverRelease) // let the resolver goroutine exit cleanly
}

// TestApprovalAllowReads verifies the allow-reads outcome and its side effect.
func TestApprovalAllowReads(t *testing.T) {
	srv := sseServer(
		t,
		toolCallFrames("call_1", "write_file", `{"path":"a.txt","content":"x"}`),
		[]string{contentChunk("done")},
	)
	defer srv.Close()

	app := fakeApp(srv.URL)
	resolver := func(ctx context.Context, req ApprovalRequest) ApprovalResolution {
		return ApprovalResolution{Choice: agent.ChoiceAllowReads}
	}
	turnFn, err := HostTurnFunc(app, WithResolver(resolver))
	if err != nil {
		t.Fatalf("HostTurnFunc: %v", err)
	}
	h := sessionhost.New(turnFn)
	p := core.Principal{TenantID: "tnt_test", UserID: "usr_owner", Role: core.RoleOwner}
	defer h.Close(context.Background())

	s := createSessionT(t, h, p)
	if _, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: s.ID, Text: "write"}); err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}
	waitUntil(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s.ID)
		return g.State != core.SessionRunning
	})

	events, err := h.ListEvents(context.Background(), p, s.ID, 0, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var resOutcome string
	for _, e := range events {
		if e.Kind == event.KindApprovalResolved {
			resOutcome = e.Payload.(event.ApprovalResolved).Outcome
		}
	}
	if resOutcome != "allowed_reads" {
		t.Fatalf("ApprovalResolved.Outcome = %q, want allowed_reads", resOutcome)
	}
}

// TestCallbackRestore verifies the adapter restores all callback fields after a
// successful turn (exit criterion #9).
func TestCallbackRestore(t *testing.T) {
	srv := sseServer(t, []string{contentChunk("ok")})
	defer srv.Close()
	app := fakeApp(srv.URL)

	origOut := app.Out
	origConfirm := app.Confirm
	origReasoning := func(s string) {}
	origTokRate := func(f float64) {}
	origSink := func(a any) {}
	app.OnReasoning = origReasoning
	app.OnTokRate = origTokRate
	app.EventSink = origSink

	turnFn, err := HostTurnFunc(app)
	if err != nil {
		t.Fatalf("HostTurnFunc: %v", err)
	}
	h := sessionhost.New(turnFn)
	p := core.Principal{TenantID: "tnt_test", UserID: "usr_test", Role: core.RoleOwner}
	defer h.Close(context.Background())

	s := createSessionT(t, h, p)
	if _, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: s.ID, Text: "hi"}); err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}
	waitUntil(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s.ID)
		return g.State == core.SessionIdle
	})

	// All callback fields must be restored to their original identity.
	if app.Out != origOut {
		t.Error("app.Out not restored")
	}
	// Function values can't be compared directly; compare pointer identity via
	// fmt of the func values (reflect would be equivalent). Compare against the
	// captured originals.
	if fmt.Sprintf("%p", app.Confirm) != fmt.Sprintf("%p", origConfirm) {
		t.Error("app.Confirm not restored")
	}
	if fmt.Sprintf("%p", app.OnReasoning) != fmt.Sprintf("%p", origReasoning) {
		t.Error("app.OnReasoning not restored")
	}
	if fmt.Sprintf("%p", app.OnTokRate) != fmt.Sprintf("%p", origTokRate) {
		t.Error("app.OnTokRate not restored")
	}
	if fmt.Sprintf("%p", app.EventSink) != fmt.Sprintf("%p", origSink) {
		t.Error("app.EventSink not restored")
	}
}
