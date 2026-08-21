// facade.go: the remote Facade implementation (card #148 P2e).
//
// RemoteFacade implements sessionclient.Facade by calling Connect RPCs on the
// daemon. It is the remote counterpart of wiring.wiringFacade — every call
// travels over the Unix socket instead of an in-process method call.
//
// The TUI holds one RemoteFacade per conversation. Rotation (/new, /resume)
// swaps the whole facade. The RemoteConversationManager (manager.go) creates
// and closes facades.
//
// Key differences from wiringFacade:
//   - No *agent.App: the agent loop lives in the daemon. The facade cannot
//     read App fields directly. Instead, it builds ClientSnapshot from the
//     session snapshot + live event stream.
//   - SessionService calls go through Connect RPCs (Session.CreateSession,
//     SubmitInput, etc.) instead of direct host method calls.
//   - EventReader calls go through Connect RPCs (Event.StreamEvents,
//     ListEvents, GetSessionSnapshot).
//   - TUI-specific surfaces (Snapshot, Consent, Info, etc.) are limited to
//     what the daemon exposes. The daemon does NOT expose the full agent.App
//     state over RPC — it exposes the session state and event stream. The
//     remote facade projects the conversation from events rather than reading
//     App.Conv directly. Consent is not available remotely (the daemon owns
//     it); the remote facade returns zero-consent and the TUI's consent
//     toggle is a no-op (or a future "remote consent" RPC).
//   - Slash-command dispatch is limited: commands that mutate App state
//     (/backend, /model, /auto) cannot run remotely without daemon-side
//     support. DispatchCommand returns Handled=false for these, and the TUI
//     treats them as regular input (submitted as a turn). This is the same
//     behavior as the embedded path when a command is not recognized.

package remote

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	v1alpha1 "github.com/treeol/wakil/api/gen/wakil/v1alpha1"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionclient"
	"github.com/treeol/wakil/internal/protoconv"
	"github.com/treeol/wakil/internal/proxy"
)

// Compile-time proof that RemoteFacade satisfies the Facade interface.
var _ sessionclient.Facade = (*RemoteFacade)(nil)

// RemoteFacade implements sessionclient.Facade by calling Connect RPCs on the
// daemon. It is created by RemoteConversationManager.
type RemoteFacade struct {
	mu        sync.Mutex
	clients   *Clients
	principal core.Principal

	// sessionID is set once after CreateSession.
	sessionID event.SessionID

	// workspace is the workspace ID used in CreateSession.
	workspace event.WorkspaceID

	// pump is the remote event pump driving the StreamEvents stream.
	pump *RemoteEventPump

	// conv is the projected conversation (user + assistant messages).
	// Built from the session snapshot and updated from live events.
	conv []proxy.Message

	// title is the session title (from CreateSession or the first user message).
	title string

	// snapshotVersion increments on every facade-mediated mutation.
	snapshotVersion uint64

	// closed is true after Close; subsequent calls return ErrSessionClosed.
	closed bool
}

// newRemoteFacade creates a facade backed by the daemon clients. The caller
// (RemoteConversationManager) creates the session and calls setSession before
// returning the facade to the TUI.
func newRemoteFacade(clients *Clients, principal core.Principal, workspace event.WorkspaceID) *RemoteFacade {
	return &RemoteFacade{
		clients:   clients,
		principal: principal,
		workspace: workspace,
	}
}

// setSession records the session ID after CreateSession/ResumeConversation.
func (f *RemoteFacade) setSession(sid event.SessionID) {
	f.mu.Lock()
	f.sessionID = sid
	f.mu.Unlock()
}

// ---- SessionService delegation (via Connect RPCs) ----

func (f *RemoteFacade) CreateSession(ctx context.Context, principal core.Principal, req core.CreateSessionRequest) (core.Session, error) {
	resp, err := f.clients.Session.CreateSession(ctx, connect.NewRequest(&v1alpha1.CreateSessionRequest{
		Workspace: string(req.Workspace),
		Title:     req.Title,
	}))
	if err != nil {
		return core.Session{}, fmt.Errorf("remote: CreateSession: %w", err)
	}
	s := protoconv.SessionFromProto(resp.Msg)
	return core.Session{
		ID:        event.SessionID(s.ID),
		TenantID:  event.TenantID(s.TenantID),
		Workspace: event.WorkspaceID(s.Workspace),
		State:     core.SessionState(s.State),
		LastSeq:   event.Seq(s.LastSeq),
		CreatedBy: event.UserID(s.CreatedBy),
		Title:     s.Title,
		CreatedAt: s.CreatedAt,
	}, nil
}

func (f *RemoteFacade) SubmitInput(ctx context.Context, principal core.Principal, req core.SubmitInputRequest) (core.TurnAck, error) {
	resp, err := f.clients.Session.SubmitInput(ctx, connect.NewRequest(&v1alpha1.SubmitInputRequest{
		SessionId:  string(req.SessionID),
		Text:       req.Text,
		ReadAction: req.ReadAction,
		RequestId:  req.RequestID,
	}))
	if err != nil {
		return core.TurnAck{}, fmt.Errorf("remote: SubmitInput: %w", err)
	}
	return core.TurnAck{
		SessionID: event.SessionID(resp.Msg.SessionId),
		TurnID:    event.TurnID(resp.Msg.TurnId),
	}, nil
}

func (f *RemoteFacade) RespondToApproval(ctx context.Context, principal core.Principal, d core.ApprovalDecision) error {
	var outcome v1alpha1.ApprovalOutcome
	switch d.Outcome {
	case core.ApprovalDeny:
		outcome = v1alpha1.ApprovalOutcome_APPROVAL_OUTCOME_DENY
	case core.ApprovalAllowOnce:
		outcome = v1alpha1.ApprovalOutcome_APPROVAL_OUTCOME_ALLOW_ONCE
	case core.ApprovalAllowReadsOnce:
		outcome = v1alpha1.ApprovalOutcome_APPROVAL_OUTCOME_ALLOW_READS_ONCE
	default:
		return fmt.Errorf("remote: invalid approval outcome %q", d.Outcome)
	}
	_, err := f.clients.Session.RespondToApproval(ctx, connect.NewRequest(&v1alpha1.RespondToApprovalRequest{
		SessionId:  string(d.SessionID),
		ApprovalId: string(d.ApprovalID),
		Outcome:    outcome,
		Reason:     d.Reason,
	}))
	if err != nil {
		return fmt.Errorf("remote: RespondToApproval: %w", err)
	}
	return nil
}

func (f *RemoteFacade) Interrupt(ctx context.Context, principal core.Principal, sessionID event.SessionID) error {
	_, err := f.clients.Session.Interrupt(ctx, connect.NewRequest(&v1alpha1.InterruptRequest{
		SessionId: string(sessionID),
	}))
	if err != nil {
		return fmt.Errorf("remote: Interrupt: %w", err)
	}
	return nil
}

func (f *RemoteFacade) CloseSession(ctx context.Context, principal core.Principal, sessionID event.SessionID) error {
	_, err := f.clients.Session.CloseSession(ctx, connect.NewRequest(&v1alpha1.CloseSessionRequest{
		SessionId: string(sessionID),
	}))
	if err != nil {
		return fmt.Errorf("remote: CloseSession: %w", err)
	}
	return nil
}

// ---- EventReader delegation (via Connect RPCs) ----

// Subscribe starts the remote event pump. The pump consumes the
// StreamEvents server-stream and delivers events to the callback. The
// subscription begins at the session's current durable head (not seq 0):
// the TUI hydrates its view from the facade snapshot, and replaying the
// durable history would re-open dead confirm gates.
func (f *RemoteFacade) Subscribe(ctx context.Context, principal core.Principal, sessionID event.SessionID, after event.Seq, deliver func(event.Event)) (core.EventSubscription, error) {
	f.mu.Lock()
	closed := f.closed
	f.mu.Unlock()
	if closed {
		return nil, fmt.Errorf("facade closed")
	}

	// Get the session snapshot to determine the current head.
	head := after
	snap, err := f.SessionSnapshot(ctx, principal, sessionID)
	if err == nil && snap.LastSeq > head {
		head = snap.LastSeq
	}

	pump := NewRemoteEventPump(f.clients, sessionID, head, deliver)
	f.mu.Lock()
	f.pump = pump
	f.mu.Unlock()

	// The remote subscription is a no-op handle — the pump itself is the
	// subscription. Close on facade Close.
	return &remoteSubscription{}, nil
}

// remoteSubscription is a no-op EventSubscription — the remote pump manages
// its own lifecycle via context cancellation. It exists only to satisfy the
// core.EventSubscription interface returned by Subscribe.
type remoteSubscription struct{}

func (s *remoteSubscription) Next(ctx context.Context) (event.Event, error) {
	<-ctx.Done()
	return event.Event{}, ctx.Err()
}
func (s *remoteSubscription) Close() error { return nil }

func (f *RemoteFacade) StartEventPump(ctx context.Context) {
	f.mu.Lock()
	pump := f.pump
	f.mu.Unlock()
	if pump != nil {
		go pump.Run(ctx)
	}
}

func (f *RemoteFacade) ListEvents(ctx context.Context, principal core.Principal, sessionID event.SessionID, after event.Seq, limit int) ([]event.Event, error) {
	resp, err := f.clients.Event.ListEvents(ctx, connect.NewRequest(&v1alpha1.ListEventsRequest{
		SessionId: string(sessionID),
		AfterSeq:  uint64(after),
		Limit:     int32(limit),
	}))
	if err != nil {
		return nil, fmt.Errorf("remote: ListEvents: %w", err)
	}
	events := make([]event.Event, 0, len(resp.Msg.Events))
	for _, pb := range resp.Msg.Events {
		ev, err := protoconv.EventFromProto(pb)
		if err != nil {
			continue // skip unknown kinds
		}
		events = append(events, ev)
	}
	return events, nil
}

func (f *RemoteFacade) SessionSnapshot(ctx context.Context, principal core.Principal, sessionID event.SessionID) (core.SessionSnapshot, error) {
	resp, err := f.clients.Event.GetSessionSnapshot(ctx, connect.NewRequest(&v1alpha1.GetSessionSnapshotRequest{
		SessionId: string(sessionID),
	}))
	if err != nil {
		return core.SessionSnapshot{}, fmt.Errorf("remote: GetSessionSnapshot: %w", err)
	}
	s := protoconv.SessionFromProto(resp.Msg.Session)
	events := make([]event.Event, 0, len(resp.Msg.Events))
	for _, pb := range resp.Msg.Events {
		ev, err := protoconv.EventFromProto(pb)
		if err != nil {
			continue // skip unknown kinds
		}
		events = append(events, ev)
	}
	return core.SessionSnapshot{
		Session: core.Session{
			ID:        event.SessionID(s.ID),
			TenantID:  event.TenantID(s.TenantID),
			Workspace: event.WorkspaceID(s.Workspace),
			State:     core.SessionState(s.State),
			LastSeq:   event.Seq(s.LastSeq),
			CreatedBy: event.UserID(s.CreatedBy),
			Title:     s.Title,
			CreatedAt: s.CreatedAt,
		},
		Events:  events,
		LastSeq: event.Seq(resp.Msg.LastSeq),
	}, nil
}

// ---- TUI-specific surfaces ----

// Snapshot returns the client-visible projection of the session state. The
// remote facade builds this from the event stream rather than reading App
// fields directly. Fields the daemon does not expose (ModelList, BackendList,
// Tools, etc.) are zero-valued — the TUI renders what it can and the rest
// stays empty. This is the P2e limitation; a future "session info" RPC could
// populate these.
func (f *RemoteFacade) Snapshot() sessionclient.ClientSnapshot {
	f.mu.Lock()
	version := f.snapshotVersion
	sid := f.sessionID
	title := f.title
	conv := f.conv
	f.mu.Unlock()

	return sessionclient.ClientSnapshot{
		SessionID: sid,
		Title:     title,
		Conv:      append([]proxy.Message(nil), conv...),
		Version:   version,
	}
}

// Consent returns the zero consent state. The daemon owns consent; the remote
// facade cannot read it. The TUI's consent toggles are no-ops in daemon mode
// (or a future "remote consent" RPC could expose them).
func (f *RemoteFacade) Consent() sessionclient.Consent {
	return sessionclient.Consent{}
}

func (f *RemoteFacade) CompletionSource() sessionclient.CompletionSource {
	return &remoteCompletionSource{}
}

// Info returns the limited info the remote facade has. Most fields are empty
// because they require agent.App access.
func (f *RemoteFacade) Info() sessionclient.InfoSnapshot {
	return sessionclient.InfoSnapshot{}
}

// ---- Client-initiated mutations ----
// These are no-ops or errors in remote mode — the daemon owns the agent
// state. The TUI's consent/model/backend toggles cannot mutate remote state
// without daemon-side RPCs (future work).

func (f *RemoteFacade) bumpVersion() {
	f.mu.Lock()
	f.snapshotVersion++
	f.mu.Unlock()
}

func (f *RemoteFacade) SetAutoApprove(v bool)                          { f.bumpVersion() }
func (f *RemoteFacade) SetAllowDestructive(v bool)                     { f.bumpVersion() }
func (f *RemoteFacade) RevokeAuto()                                    { f.bumpVersion() }
func (f *RemoteFacade) SetWorkflow(wf *sessionclient.WorkflowSnapshot) { f.bumpVersion() }
func (f *RemoteFacade) AppendSystemMessage(m proxy.Message) {
	f.mu.Lock()
	f.conv = append(f.conv, m)
	f.mu.Unlock()
	f.bumpVersion()
}
func (f *RemoteFacade) SaveSession()                                               {}
func (f *RemoteFacade) ConsumeStartupNote() string                                 { return "" }
func (f *RemoteFacade) SaveRepoState(mutate func(*sessionclient.RepoStateMutator)) {}
func (f *RemoteFacade) SetInfoPanelOpen(open bool)                                 { f.bumpVersion() }
func (f *RemoteFacade) SetCtxLimit(lim sessionclient.ContextLimit)                 { f.bumpVersion() }
func (f *RemoteFacade) SetModelList(models []string)                               { f.bumpVersion() }
func (f *RemoteFacade) SetTools(tools []proxy.Tool)                                { f.bumpVersion() }
func (f *RemoteFacade) ReplacePendingImages(imgs []proxy.ImagePart)                { f.bumpVersion() }
func (f *RemoteFacade) AddPendingImage(img proxy.ImagePart)                        { f.bumpVersion() }
func (f *RemoteFacade) ClearPendingImages()                                        { f.bumpVersion() }

// ---- Side questions ----
// Side questions require daemon-side support. In P2e they are not available
// remotely; the methods return no-ops.

func (f *RemoteFacade) StartSideQuestion(ctx context.Context, question string) (sessionclient.OpID, context.CancelFunc) {
	return "", func() {}
}
func (f *RemoteFacade) CancelSideQuestion(opID sessionclient.OpID) {}

// ---- Session listing ----

func (f *RemoteFacade) ListSessions(scope sessionclient.SessionScope) ([]sessionclient.SessionSummary, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	resp, err := f.clients.Session.ListSessions(ctx, connect.NewRequest(&v1alpha1.ListSessionsRequest{}))
	if err != nil {
		return nil, 0, fmt.Errorf("remote: ListSessions: %w", err)
	}
	out := make([]sessionclient.SessionSummary, 0, len(resp.Msg.Sessions))
	for _, s := range resp.Msg.Sessions {
		out = append(out, sessionclient.SessionSummary{
			ChatID:    s.Id,
			Model:     "",
			Label:     s.Title,
			Workspace: s.Workspace,
		})
	}
	return out, 0, nil
}

func (f *RemoteFacade) LoadSession(idOrPrefix string) (*sessionclient.SessionSummary, error) {
	// The daemon's ListSessions doesn't support prefix search. We list all
	// and filter client-side — acceptable for small session counts.
	sessions, _, err := f.ListSessions(sessionclient.SessionScope{All: true})
	if err != nil {
		return nil, err
	}
	for _, s := range sessions {
		if strings.HasPrefix(s.ChatID, idOrPrefix) {
			return &s, nil
		}
	}
	return nil, fmt.Errorf("remote: session %q not found", idOrPrefix)
}

// ---- Slash-command dispatch ----
// In remote mode, slash commands that mutate App state are not supported.
// DispatchCommand classifies a few client-side commands (quit, new, resume,
// handoff) and returns the rest as "not handled" so the TUI submits them as
// regular input (the daemon's agent will process them as turns).
func (f *RemoteFacade) DispatchCommand(line string) sessionclient.CommandResult {
	if fields := strings.Fields(line); len(fields) > 0 {
		switch fields[0] {
		case "/quit", "/exit", "/q":
			return sessionclient.CommandResult{Handled: true, Quit: true}
		case "/new", "/reset":
			return sessionclient.CommandResult{
				Handled:  true,
				Rotate:   &sessionclient.RotateRequest{Type: "new"},
				Rotating: true,
			}
		case "/resume":
			if len(fields) == 1 || (len(fields) == 2 && fields[1] == "all") {
				return sessionclient.CommandResult{Handled: true, ResumePicker: true}
			}
			return sessionclient.CommandResult{
				Handled: true,
				Rotate: &sessionclient.RotateRequest{
					Type: "resume",
				},
				Rotating: true,
			}
		}
	}
	return sessionclient.CommandResult{}
}

// ---- Lifecycle ----

func (f *RemoteFacade) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	pump := f.pump
	f.pump = nil
	sessionID := f.sessionID
	principal := f.principal
	f.mu.Unlock()

	// Stop the event pump.
	if pump != nil {
		pump.Stop()
		select {
		case <-pump.Done():
		case <-time.After(5 * time.Second):
		}
	}

	// Close the session on the daemon (best-effort; bounded).
	if sessionID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = f.CloseSession(ctx, principal, sessionID)
		cancel()
	}
	return nil
}

// ---- CompletionSource ----

type remoteCompletionSource struct{}

func (c *remoteCompletionSource) Models() []string                  { return nil }
func (c *remoteCompletionSource) Backends() []sessionclient.Backend { return nil }
func (c *remoteCompletionSource) Sessions() []sessionclient.SessionSummary {
	return nil
}

// SetTitle sets the session title (used by the manager after CreateSession).
func (f *RemoteFacade) SetTitle(title string) {
	f.mu.Lock()
	f.title = title
	f.mu.Unlock()
}

// AppendConv appends a message to the projected conversation. Used by the
// event pump's deliver callback to update the facade's conversation view.
func (f *RemoteFacade) AppendConv(m proxy.Message) {
	f.mu.Lock()
	f.conv = append(f.conv, m)
	f.mu.Unlock()
	f.bumpVersion()
}

// SetConv replaces the projected conversation (used after snapshot hydration).
func (f *RemoteFacade) SetConv(conv []proxy.Message) {
	f.mu.Lock()
	f.conv = append([]proxy.Message(nil), conv...)
	f.mu.Unlock()
	f.bumpVersion()
}

// OutputMode returns the config's output mode (the remote facade reads it
// from the config the TUI was launched with, not from the daemon).
func (f *RemoteFacade) OutputMode(cfg config.Config) config.OutputMode {
	return cfg.OutputMode
}
