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
//     read App fields directly. Instead, it projects ClientSnapshot from the
//     cached SessionState (GetSessionState RPC) plus the live event stream.
//   - SessionService calls go through Connect RPCs (Session.CreateSession,
//     SubmitInput, etc.) instead of direct host method calls.
//   - EventReader calls go through Connect RPCs (Event.StreamEvents,
//     ListEvents, GetSessionSnapshot).
//   - TUI-specific surfaces (Snapshot, Consent, Info, CompletionSource) are
//     projected from a SessionState snapshot fetched over the SessionStateService
//     GetSessionState RPC (P6b). The daemon exposes the session-scoped App state
//     via that RPC; the remote facade caches it (the methods are synchronous and
//     cannot block on an RPC) and refreshes it on setSession and on turn-boundary
//     events. Fields the daemon does not expose (full workflow state, MCP server
//     lists, grounding, cost tracker, endpoint list, TranscriptSize) remain
//     zero-valued. The conversation is populated on resume via the LoadSession
//     RPC (which returns the transcript from disk) and on live turns via the
//     event stream (AppendConv).
//   - Slash-command dispatch (P6c) routes state-mutating commands through the
//     SessionStateService mutation RPCs (SetModel, SetBackend, SetAutoApprove,
//     …). Client-side commands (/quit, /new, /resume) are classified locally;
//     read-only /show commands project from the cached SessionState; commands
//     without a daemon RPC (endpoint switching for openai-kind, /mcp, /plan,
//     /verify, /learn, /image, /handoff, /remember, /recall) return a Notice
//     explaining they are not available remotely. Unknown commands return
//     Handled=false so the TUI submits them as a regular turn (the daemon's
//     agent treats them as input).

package remote

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	v1alpha1 "github.com/treeol/wakil/api/gen/wakil/v1alpha1"
	wakilv1alpha1connect "github.com/treeol/wakil/api/gen/wakil/v1alpha1/wakilv1alpha1connect"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionclient"
	"github.com/treeol/wakil/internal/diag"
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

	// refreshSeq is a monotonically increasing counter for refreshState
	// calls. Each refreshState invocation captures a ticket; only the
	// response with the highest ticket is installed, preventing stale
	// overwrites when concurrent refreshes complete out of order.
	refreshSeq uint64

	// state is the cached SessionState from the daemon, projected into
	// Snapshot()/Consent()/Info()/CompletionSource(). Since those methods are
	// synchronous (no ctx, no error), they cannot block on an RPC; the cache
	// is refreshed asynchronously (on setSession and on turn-boundary events
	// from the pump) and read under mu.
	state *v1alpha1.SessionState

	// startupNote is the pending one-shot startup note (e.g. the repo-state
	// restore summary) surfaced to the TUI via ConsumeStartupNote, mirroring the
	// embedded App.StartupNote. Set by RestoreRepoState (fresh-boot path).
	startupNote string

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
// Once the session ID is known it kicks off an asynchronous GetSessionState
// fetch so Snapshot()/Consent()/Info() report non-zero state on the first
// render (they are synchronous and cannot block on an RPC).
func (f *RemoteFacade) setSession(sid event.SessionID) {
	f.mu.Lock()
	f.sessionID = sid
	f.mu.Unlock()
	go f.refreshState(context.Background())
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

	// Wrap deliver so the facade refreshes its cached daemon state on
	// turn-boundary events (where consent/context/model can change) before
	// forwarding to the TUI. State-change events that are not turn boundaries
	// pass through unchanged.
	refreshDeliver := func(ev event.Event) {
		if ev.Seq > 0 && isStateRefreshEvent(ev.Kind) {
			// Refresh asynchronously — do not block event delivery. The TUI
			// re-reads Snapshot() after each event batch, so a refresh landing
			// one batch later is acceptable (bounded staleness).
			go f.refreshState(context.Background())
		}
		deliver(ev)
	}

	pump := NewRemoteEventPump(f.clients, sessionID, head, refreshDeliver)
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

// refreshState fetches the daemon's SessionState and stores it under mu. It is
// the single write path for the cached state read by Snapshot()/Consent()/
// Info()/CompletionSource(). Safe to call from any goroutine; failures leave
// the previous cache intact (stale-but-valid).
//
// A monotonic ticket (refreshSeq) prevents stale overwrites: if two
// refreshState calls run concurrently, only the one with the higher ticket
// installs its result, so an older fetch that completes after a newer one
// is discarded.
func (f *RemoteFacade) refreshState(ctx context.Context) {
	f.mu.Lock()
	sid := f.sessionID
	clients := f.clients
	closed := f.closed
	ticket := f.refreshSeq + 1
	f.refreshSeq = ticket
	f.mu.Unlock()
	if closed || clients == nil || clients.SessionState == nil {
		return
	}
	if sid == "" {
		return
	}

	fetchCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := clients.SessionState.GetSessionState(fetchCtx, connect.NewRequest(&v1alpha1.GetSessionStateRequest{
		SessionId: string(sid),
	}))
	if err != nil {
		return
	}

	f.mu.Lock()
	// Only install if no newer refresh has already landed.
	if ticket >= f.refreshSeq {
		f.state = resp.Msg
	}
	f.mu.Unlock()
	f.bumpVersion()
}

// Snapshot returns the client-visible projection of the session state. The
// projection is built from the cached SessionState fetched via GetSessionState
// (see refreshState) plus the locally-projected conversation. Since Snapshot is
// called on the render path and cannot block, it reads the cache — if the
// daemon's state hasn't arrived yet (first render), it returns zero values for
// the daemon-backed fields with the local session ID/title/conv still present.
func (f *RemoteFacade) Snapshot() sessionclient.ClientSnapshot {
	f.mu.Lock()
	version := f.snapshotVersion
	sid := f.sessionID
	title := f.title
	conv := f.conv
	st := f.state
	f.mu.Unlock()

	snap := sessionclient.ClientSnapshot{
		SessionID: sid,
		Title:     title,
		Conv:      append([]proxy.Message(nil), conv...),
		Version:   version,
	}
	if st == nil {
		// No daemon state yet: ChatID/Workspace default to empty. The session
		// identity (SessionID) is still authoritative from the local create.
		return snap
	}

	snap.ChatID = st.ChatId
	snap.Workspace = st.Workspace
	snap.Backend = st.SelectedBackend
	snap.Model = st.SelectedModel
	if snap.Model == "" {
		snap.Model = st.EffectiveModel
	}
	snap.ContextLimit = contextLimitFromProto(st.ContextLimit)
	snap.ModelList = append([]string(nil), st.ModelList...)
	snap.BackendList = backendsFromProto(st.BackendList)
	snap.RawTools = st.RawTools
	// Workflow: the daemon exposes only the sidebar label (WorkflowLabel), not
	// the full WorkflowState (Task/Phase/StepCount/StepIdx/PlanPath). Leave
	// snap.Workflow nil rather than fabricate a partial snapshot — Info()
	// surfaces WorkflowLabel for the sidebar.
	// Title from the daemon is authoritative if the local title is unset.
	if snap.Title == "" {
		snap.Title = st.Title
	}
	return snap
}

// Consent returns the daemon-owned consent state from the cached SessionState.
// Returns zero consent until the first GetSessionState lands (the daemon owns
// consent; the remote facade cannot read it synchronously otherwise).
func (f *RemoteFacade) Consent() sessionclient.Consent {
	f.mu.Lock()
	st := f.state
	f.mu.Unlock()
	if st == nil {
		return sessionclient.Consent{}
	}
	return sessionclient.Consent{
		AutoApprove:      st.AutoApprove,
		AllowDestructive: st.AllowDestructive,
		AllowReads:       st.AllowReads,
	}
}

func (f *RemoteFacade) CompletionSource() sessionclient.CompletionSource {
	return &remoteCompletionSource{f: f}
}

// Info returns the deep-state view the info panel and status line render,
// projected from the cached SessionState. Fields the daemon does not expose
// (Image, Oracle* labels, SearXngURL, MentionBase, Endpoints, MCPServers,
// SearxngTools, Grounding, Costs, TranscriptSize) remain zero-valued — the
// same P2e limitation, now narrowed: the state the daemon DOES expose is
// populated instead of everything being empty.
func (f *RemoteFacade) Info() sessionclient.InfoSnapshot {
	f.mu.Lock()
	st := f.state
	convLen := len(f.conv)
	f.mu.Unlock()
	if st == nil {
		return sessionclient.InfoSnapshot{}
	}

	info := sessionclient.InfoSnapshot{
		ChatID:          st.ChatId,
		BaseURL:         st.BaseUrl,
		LastBackend:     st.LastBackend,
		Cwd:             st.Cwd,
		ExecMode:        st.ExecMode,
		SelectedBackend: st.SelectedBackend,
		ConfigBackend:   st.ConfigBackend,
		EffectiveModel:  st.EffectiveModel,
		SubagentModel:   st.EffectiveSubagentModel,
		PromptNote:      st.PromptNote,
		ContextLimit:    contextLimitFromProto(st.ContextLimit),
		ContextUsed:     int(st.ContextUsed),
		ContextExact:    st.ContextExact,
		ConvLen:         convLen,
		WorkflowLabel:   st.WorkflowLabel,
		InfoPanelOpen:   st.InfoPanelOpen,
	}

	return info
}

// ---- proto → sessionclient conversion helpers ----

// isStateRefreshEvent reports whether an event kind can change daemon-side
// state that Snapshot()/Consent()/Info() project (consent, context, model list,
// workflow, conversation size). It returns true on turn boundaries and other
// authoritative-state events so the facade refreshes its cache at the right
// moments without hammering GetSessionState on every streaming delta.
func isStateRefreshEvent(k event.Kind) bool {
	switch k {
	case event.KindTurnStarted,
		event.KindTurnCompleted,
		event.KindConversationCompacted,
		event.KindApprovalResolved,
		event.KindUserMessageCommitted,
		event.KindSessionCreated,
		event.KindSubagentCompleted,
		event.KindWorkflowOutcome:
		return true
	default:
		return false
	}
}

func contextLimitFromProto(cl *v1alpha1.ContextLimit) sessionclient.ContextLimit {
	if cl == nil {
		return sessionclient.ContextLimit{}
	}
	return sessionclient.ContextLimit{
		NCtx:            int(cl.NCtx),
		NCtxTrain:       int(cl.NCtxTrain),
		Source:          cl.Source,
		ContextSource:   cl.ContextSource,
		UsableCtx:       int(cl.UsableCtx),
		ReasoningBudget: int(cl.ReasoningBudget),
		AnswerMargin:    int(cl.AnswerMargin),
		ModelUnresolved: cl.ModelUnresolved,
	}
}

func backendsFromProto(list []*v1alpha1.BackendInfo) []sessionclient.Backend {
	if len(list) == 0 {
		return nil
	}
	out := make([]sessionclient.Backend, 0, len(list))
	for _, b := range list {
		if b == nil {
			continue
		}
		out = append(out, sessionclient.Backend{
			Name:     b.Name,
			External: b.External,
			Caps:     append([]string(nil), b.Caps...),
		})
	}
	return out
}

// ---- Client-initiated mutations (P6c) ----
// The TUI calls SetAutoApprove/SetAllowDestructive/RevokeAuto directly from
// its mid-turn /auto path (tui.go) and from the deferred-grant flush
// (tui_events.go). These methods have no ctx/error return, so they fire the
// corresponding SessionStateService RPC on a background goroutine with a
// bounded timeout.

func (f *RemoteFacade) bumpVersion() {
	f.mu.Lock()
	f.snapshotVersion++
	f.mu.Unlock()
}

// getRPCTargets extracts the session ID and SessionState client under mu. It
// returns (false, …) when the facade is closed, has no session yet, or the
// client is nil (tests that don't dial SessionStateService).
func (f *RemoteFacade) getRPCTargets() (bool, event.SessionID, wakilv1alpha1connect.SessionStateServiceClient) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed || f.clients == nil || f.clients.SessionState == nil {
		return false, "", nil
	}
	if f.sessionID == "" {
		return false, "", nil
	}
	return true, f.sessionID, f.clients.SessionState
}

// goMutation fires a state-mutating RPC on a background goroutine (the TUI's
// direct consent mutations are fire-and-forget). The RPC context is detached
// from any caller context (these methods carry none) and bounded by rpcTimeout.
// After the RPC completes (success or failure), it triggers a state refresh so
// the cached SessionState — and therefore Snapshot()/Consent()/Info() —
// reconverge to daemon truth. Without this refresh, the TUI could display
// consent state that disagrees with what the daemon actually enforces.
//
// If the RPC fails, the error is logged to stderr — there is no synchronous
// return path or TUI notice mechanism for these fire-and-forget mutations.
// The subsequent refreshState reconverges to daemon truth regardless.
func (f *RemoteFacade) goMutation(op string, call func(context.Context, wakilv1alpha1connect.SessionStateServiceClient, event.SessionID) error) {
	ok, sid, client := f.getRPCTargets()
	if !ok {
		return
	}
	f.bumpVersion()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
		defer cancel()
		if err := call(ctx, client, sid); err != nil {
			diag.Printf("remote: %s mutation failed: %v\n", op, err)
		}
		// Refresh cached state so the TUI's next Snapshot()/Consent() read
		// reflects the mutation that just landed (or didn't).
		f.refreshState(context.Background())
	}()
}

func (f *RemoteFacade) SetAutoApprove(v bool) {
	f.goMutation("SetAutoApprove", func(ctx context.Context, c wakilv1alpha1connect.SessionStateServiceClient, sid event.SessionID) error {
		_, err := c.SetAutoApprove(ctx, connect.NewRequest(&v1alpha1.SetAutoApproveRequest{
			SessionId: string(sid),
			Value:     v,
		}))
		return err
	})
}

func (f *RemoteFacade) SetAllowDestructive(v bool) {
	f.goMutation("SetAllowDestructive", func(ctx context.Context, c wakilv1alpha1connect.SessionStateServiceClient, sid event.SessionID) error {
		_, err := c.SetAllowDestructive(ctx, connect.NewRequest(&v1alpha1.SetAllowDestructiveRequest{
			SessionId: string(sid),
			Value:     v,
		}))
		return err
	})
}

func (f *RemoteFacade) RevokeAuto() {
	f.goMutation("RevokeAuto", func(ctx context.Context, c wakilv1alpha1connect.SessionStateServiceClient, sid event.SessionID) error {
		_, err := c.RevokeAuto(ctx, connect.NewRequest(&v1alpha1.RevokeAutoRequest{
			SessionId: string(sid),
		}))
		return err
	})
}
// SetWorkflow is a no-op in remote mode: the daemon owns workflow state and
// exposes it via GetSessionState. The workflow snapshot is projected from the
// cached state, not set locally. bumpVersion ensures the next Snapshot read
// reflects the version bump so the TUI doesn't miss the call.
func (f *RemoteFacade) SetWorkflow(wf *sessionclient.WorkflowSnapshot) { f.bumpVersion() }
func (f *RemoteFacade) AppendSystemMessage(m proxy.Message) {
	f.mu.Lock()
	f.conv = append(f.conv, m)
	f.mu.Unlock()
	f.bumpVersion()
}
// SaveSession is a no-op in remote mode: the daemon persists the transcript
// server-side at turn boundaries (SaveSession RPC is wired through the
// SessionStateService). The remote TUI never writes transcripts directly.
func (f *RemoteFacade) SaveSession() {}
func (f *RemoteFacade) ConsumeStartupNote() string {
	f.mu.Lock()
	note := f.startupNote
	f.startupNote = ""
	f.mu.Unlock()
	return note
}

// SaveRepoState projects the mutator onto the daemon's repo-state. The daemon's
// mutation RPCs (SetAutoApprove, SetModel, SetBackend, …) each persist their own
// field to repo-state server-side, and the remote TUI reaches state exclusively
// through those RPCs. The only mutator field the remote TUI sets here directly
// is AutoApprove (the deferred mid-turn /auto grant); SetAutoApprove already
// persists it, but forwarding it here again is idempotent and keeps the shim
// robust against callers that don't pair SaveRepoState with a prior RPC.
func (f *RemoteFacade) SaveRepoState(mutate func(*sessionclient.RepoStateMutator)) {
	m := &sessionclient.RepoStateMutator{}
	mutate(m)
	if m.AutoApprove {
		f.goMutation("SaveRepoState", func(ctx context.Context, c wakilv1alpha1connect.SessionStateServiceClient, sid event.SessionID) error {
			_, err := c.SetAutoApprove(ctx, connect.NewRequest(&v1alpha1.SetAutoApproveRequest{
				SessionId: string(sid),
				Value:     true,
			}))
			return err
		})
	}
	// InfoPanelOpen and the other mutator fields have no remote wire path:
	// InfoPanelOpen is TUI-local presentation state, and the rest are already
	// persisted by the daemon's own command/session RPC handlers. No-op here.
}

// SetInfoPanelOpen keeps the TUI's info-panel toggle local. There is no
// daemon-side widget to sync and no SetInfoPanelOpen RPC; the persisted
// InfoPanelOpen field is a TUI-only convenience with no remote wire path.
func (f *RemoteFacade) SetInfoPanelOpen(open bool) { f.bumpVersion() }

// RestoreRepoState applies the daemon's persisted per-workspace terminal
// settings to the active session (fresh-boot only) and surfaces the summary as
// the next ConsumeStartupNote. No-op when there is no session/client yet (the
// caller invokes it only after a fresh conversation exists).
func (f *RemoteFacade) RestoreRepoState() {
	ok, sid, client := f.getRPCTargets()
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	resp, err := client.RestoreRepoState(ctx, connect.NewRequest(&v1alpha1.RestoreRepoStateRequest{
		SessionId: string(sid),
	}))
	if err != nil {
		return
	}
	f.mu.Lock()
	f.startupNote = resp.Msg.Notice
	f.mu.Unlock()
	f.refreshStateSync()
}

// RestoreRepoStateResume restores endpoint-independent settings from repo-state
// on session resume (called by BootstrapRemote after ResumeConversation). Skips
// model/backend to avoid changing them mid-transcript. Restores AutoApprove,
// RawTools, maxpar, maxctx, subagent, mashura settings.
func (f *RemoteFacade) RestoreRepoStateResume() {
	ok, sid, client := f.getRPCTargets()
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	resp, err := client.RestoreRepoStateResume(ctx, connect.NewRequest(&v1alpha1.RestoreRepoStateResumeRequest{
		SessionId: string(sid),
	}))
	if err != nil {
		return
	}
	f.mu.Lock()
	f.startupNote = resp.Msg.Notice
	f.mu.Unlock()
	f.refreshStateSync()
}
// SetCtxLimit, SetModelList, SetTools, and PendingImages methods are no-ops
// in remote mode: these are set server-side by the daemon (via config reload,
// /model, /rawtools, /image). The remote TUI reads them from the cached
// SessionState. bumpVersion ensures the TUI doesn't miss the call.
func (f *RemoteFacade) SetCtxLimit(lim sessionclient.ContextLimit)  { f.bumpVersion() }
func (f *RemoteFacade) SetModelList(models []string)                { f.bumpVersion() }
func (f *RemoteFacade) SetTools(tools []proxy.Tool)                 { f.bumpVersion() }
func (f *RemoteFacade) ReplacePendingImages(imgs []proxy.ImagePart) { f.bumpVersion() }
func (f *RemoteFacade) AddPendingImage(img proxy.ImagePart)         { f.bumpVersion() }
func (f *RemoteFacade) ClearPendingImages()                         { f.bumpVersion() }

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
	resp, err := f.clients.SessionState.ListSavedSessions(ctx, connect.NewRequest(&v1alpha1.ListSavedSessionsRequest{
		Workspace: scope.Workspace,
		All:       scope.All,
	}))
	if err != nil {
		// Fall back to the SessionService's ListSessions for older daemons
		// that don't implement SessionStateService. The session host's
		// ListSessions returns live session IDs (ses_...); ListSavedSessions
		// returns saved chat IDs (chat_...). This fallback is a graceful
		// degradation — saved sessions won't appear, but live ones will.
		if connectCodeOf(err) == connect.CodeUnimplemented {
			return f.listSessionsFallback(ctx, scope)
		}
		return nil, 0, fmt.Errorf("remote: ListSavedSessions: %w", err)
	}
	out := make([]sessionclient.SessionSummary, 0, len(resp.Msg.Sessions))
	for _, s := range resp.Msg.Sessions {
		out = append(out, sessionclient.SessionSummary{
			ChatID:       s.ChatId,
			Model:        s.Model,
			Label:        s.Label,
			Workspace:    s.Workspace,
			Created:      time.Unix(s.Created, 0),
			Updated:      time.Unix(s.Updated, 0),
			TurnCount:    int(s.Turns),
			FirstMessage: s.FirstMessage,
		})
	}
	return out, int(resp.Msg.Hidden), nil
}

// listSessionsFallback calls the SessionService's ListSessions RPC (the
// pre-SessionStateService path). Used when the daemon doesn't implement
// SessionStateService (older builds). The SessionService's ListSessions
// has no workspace/all filtering — it returns all live sessions.
func (f *RemoteFacade) listSessionsFallback(ctx context.Context, _ sessionclient.SessionScope) ([]sessionclient.SessionSummary, int, error) {
	resp, err := f.clients.Session.ListSessions(ctx, connect.NewRequest(&v1alpha1.ListSessionsRequest{}))
	if err != nil {
		return nil, 0, fmt.Errorf("remote: ListSessions: %w", err)
	}
	out := make([]sessionclient.SessionSummary, 0, len(resp.Msg.Sessions))
	for _, s := range resp.Msg.Sessions {
		summary := sessionclient.SessionSummary{
			ChatID: s.Id,
			Label:  s.Title,
		}
		if s.CreatedAt != nil {
			summary.Created = s.CreatedAt.AsTime()
		}
		if s.ClosedAt != nil {
			summary.Updated = s.ClosedAt.AsTime()
		}
		out = append(out, summary)
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

// ---- Slash-command dispatch (P6c) ----
// In remote mode, slash commands that would mutate daemon App state are routed
// through the SessionStateService mutation RPCs; read-only /show commands
// project from the cached SessionState; client-side commands (quit, new,
// resume) are classified locally; commands with no daemon-side RPC return a
// Notice explaining they are unavailable remotely; unrecognized input returns
// Handled=false so the TUI submits it as a regular turn.
//
// CALLING CONTRACT: the TUI invokes DispatchCommand from a tea.Cmd goroutine
// (tui.go), so synchronous RPCs here are safe — matching wiringFacade's
// contract. Every recognized command returns Handled=true.
func (f *RemoteFacade) DispatchCommand(line string) sessionclient.CommandResult {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "/") {
		return sessionclient.CommandResult{}
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return sessionclient.CommandResult{}
	}

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
		return f.dispatchResume(fields)

	case "/model":
		return f.dispatchModel(fields)

	case "/backend":
		return f.dispatchBackend(fields)

	case "/auto":
		return f.dispatchAuto(fields)

	case "/subagent":
		return f.dispatchSubagentEndpoint(fields)

	case "/submodel":
		return f.dispatchSubagentModel(fields)

	case "/maxpar":
		return f.dispatchMaxParallel(fields)

	case "/maxctx":
		return f.dispatchEffectiveCtxMax(fields)

	case "/rawtools":
		return f.dispatchRawTools(fields)

	case "/counsel":
		return f.dispatchCounsel(fields)

	case "/compact":
		return f.dispatchCompact(fields)

	case "/repostate":
		return f.dispatchRepoState(fields)

	case "/session":
		return f.dispatchSessionLabel(fields)

	case "/help":
		return sessionclient.CommandResult{Handled: true, Notice: remoteHelpText}

	case "/cwd":
		// Projected from the cached state (no daemon RPC needed).
		if cwd := f.stateString(func(s *v1alpha1.SessionState) string { return s.Cwd }); cwd != "" {
			return sessionclient.CommandResult{Handled: true, Notice: "cwd: " + cwd}
		}
		return sessionclient.CommandResult{Handled: true, Notice: "cwd: (unknown — wait for the daemon state to load)"}

	case "/mode":
		if m := f.stateString(func(s *v1alpha1.SessionState) string { return s.ExecMode }); m != "" {
			return sessionclient.CommandResult{Handled: true, Notice: "exec: " + m}
		}
		return sessionclient.CommandResult{Handled: true, Notice: "exec: (unknown — wait for the daemon state to load)"}

	// ── Commands with no daemon-side RPC ──────────────────────────────
	// (/info and /queue are intercepted TUI-locally before reaching
	// DispatchCommand, so they do not appear here.)
	case "/handoff", "/learn", "/remember", "/recall", "/image", "/mcp",
		"/mashura", "/plan", "/verify", "/sessions", "/history":
		return sessionclient.CommandResult{
			Handled: true,
			Notice:  fmt.Sprintf("%s is not available remotely in daemon mode", fields[0]),
		}

	default:
		// Unknown command: return Handled=false so the TUI submits it as a
		// regular turn (the daemon's agent treats it as input). This matches
		// the embedded path where an unrecognized command is passed through.
		return sessionclient.CommandResult{}
	}
}

// dispatchResume classifies /resume: bare (or "all") opens the picker; an
// explicit id/prefix rotates through the manager.
func (f *RemoteFacade) dispatchResume(fields []string) sessionclient.CommandResult {
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

// stateString reads one string field from the cached SessionState under mu.
// Returns "" when the cache is nil (first render before GetSessionState lands).
func (f *RemoteFacade) stateString(get func(*v1alpha1.SessionState) string) string {
	f.mu.Lock()
	st := f.state
	f.mu.Unlock()
	if st == nil {
		return ""
	}
	return get(st)
}

// stateInt reads an int32 field from the cached state with the same nil-safety.
func (f *RemoteFacade) stateInt(get func(*v1alpha1.SessionState) int32) int {
	f.mu.Lock()
	st := f.state
	f.mu.Unlock()
	if st == nil {
		return 0
	}
	return int(get(st))
}

// dispatchModel handles /model [<name>] via SetModel, or shows the current
// model from the cache.
func (f *RemoteFacade) dispatchModel(fields []string) sessionclient.CommandResult {
	if len(fields) >= 2 {
		model := fields[1]
		if model == "" {
			return sessionclient.CommandResult{Handled: true, Notice: "usage: /model [<name>]"}
		}
		notice, err := f.callStateRPC("SetModel", func(ctx context.Context, c wakilv1alpha1connect.SessionStateServiceClient, sid event.SessionID) (string, error) {
			resp, err := c.SetModel(ctx, connect.NewRequest(&v1alpha1.SetModelRequest{
				SessionId: string(sid),
				Model:     model,
			}))
			if err != nil {
				return "", err
			}
			return resp.Msg.Notice, nil
		})
		if err != nil {
			return sessionclient.CommandResult{Handled: true, Notice: "model: " + err.Error()}
		}
		f.refreshStateSync()
		return sessionclient.CommandResult{Handled: true, Notice: notice}
	}
	// Show current model.
	cur := f.stateString(func(s *v1alpha1.SessionState) string { return s.SelectedModel })
	if cur == "" {
		cur = f.stateString(func(s *v1alpha1.SessionState) string { return s.EffectiveModel })
	}
	if cur == "" {
		cur = "(unknown — wait for the daemon state to load)"
	}
	return sessionclient.CommandResult{Handled: true, Notice: "model: " + cur}
}

// dispatchBackend handles /backend [<name>[/<model-path>]] via SetBackend, or
// shows the current selection from the cache.
func (f *RemoteFacade) dispatchBackend(fields []string) sessionclient.CommandResult {
	if len(fields) >= 2 {
		arg := fields[1]
		notice, err := f.callStateRPC("SetBackend", func(ctx context.Context, c wakilv1alpha1connect.SessionStateServiceClient, sid event.SessionID) (string, error) {
			resp, err := c.SetBackend(ctx, connect.NewRequest(&v1alpha1.SetBackendRequest{
				SessionId: string(sid),
				Backend:   arg,
			}))
			if err != nil {
				return "", err
			}
			return resp.Msg.Notice, nil
		})
		if err != nil {
			return sessionclient.CommandResult{Handled: true, Notice: "backend: " + err.Error()}
		}
		f.refreshStateSync()
		return sessionclient.CommandResult{Handled: true, Notice: notice}
	}
	cur := f.stateString(func(s *v1alpha1.SessionState) string { return s.SelectedBackend })
	if cur == "" {
		cur = "(proxy default)"
	}
	used := f.stateString(func(s *v1alpha1.SessionState) string { return s.LastBackend })
	if used == "" {
		used = "(none yet)"
	}
	msg := "backend: selected=" + cur + " · last-used=" + used
	if m := f.stateString(func(s *v1alpha1.SessionState) string { return s.SelectedModel }); m != "" {
		msg += " · model=" + m
	}
	return sessionclient.CommandResult{Handled: true, Notice: msg}
}

// dispatchAuto handles /auto and /auto destructive via SetAutoApprove/
// SetAllowDestructive/RevokeAuto, mirroring the embedded toggle semantics.
func (f *RemoteFacade) dispatchAuto(fields []string) sessionclient.CommandResult {
	if len(fields) > 1 {
		if fields[1] != "destructive" {
			return sessionclient.CommandResult{Handled: true, Notice: "usage: /auto | /auto destructive"}
		}
		consent := f.Consent()
		if !consent.AutoApprove {
			return sessionclient.CommandResult{Handled: true, Notice: "auto mode is OFF — enable /auto first, then /auto destructive"}
		}
		next := !consent.AllowDestructive
		if _, err := f.callStateRPC("SetAllowDestructive", func(ctx context.Context, c wakilv1alpha1connect.SessionStateServiceClient, sid event.SessionID) (string, error) {
			resp, err := c.SetAllowDestructive(ctx, connect.NewRequest(&v1alpha1.SetAllowDestructiveRequest{
				SessionId: string(sid),
				Value:     next,
			}))
			if err != nil {
				return "", err
			}
			return resp.Msg.Notice, nil
		}); err != nil {
			return sessionclient.CommandResult{Handled: true, Notice: "auto destructive: " + err.Error()}
		}
		f.refreshStateSync()
		return sessionclient.CommandResult{Handled: true, Notice: f.destructiveToggleNotice(next)}
	}
	// Bare /auto: toggle AutoApprove.
	next := !f.Consent().AutoApprove
	if next {
		if _, err := f.callStateRPC("SetAutoApprove", func(ctx context.Context, c wakilv1alpha1connect.SessionStateServiceClient, sid event.SessionID) (string, error) {
			resp, err := c.SetAutoApprove(ctx, connect.NewRequest(&v1alpha1.SetAutoApproveRequest{
				SessionId: string(sid),
				Value:     true,
			}))
			if err != nil {
				return "", err
			}
			return resp.Msg.Notice, nil
		}); err != nil {
			return sessionclient.CommandResult{Handled: true, Notice: "auto: " + err.Error()}
		}
	} else {
		if _, err := f.callStateRPC("RevokeAuto", func(ctx context.Context, c wakilv1alpha1connect.SessionStateServiceClient, sid event.SessionID) (string, error) {
			resp, err := c.RevokeAuto(ctx, connect.NewRequest(&v1alpha1.RevokeAutoRequest{
				SessionId: string(sid),
			}))
			if err != nil {
				return "", err
			}
			return resp.Msg.Notice, nil
		}); err != nil {
			return sessionclient.CommandResult{Handled: true, Notice: "auto: " + err.Error()}
		}
	}
	f.refreshStateSync()
	return sessionclient.CommandResult{Handled: true, Notice: f.autoToggleNotice(next)}
}

// autoToggleNotice returns the status notice for a bare /auto toggle.
func (f *RemoteFacade) autoToggleNotice(on bool) string {
	if on {
		return "auto mode: ON — tool calls approved without prompting\n" +
			"  still confirmed: destructive shell commands (opt in with /auto destructive), external-backend egress"
	}
	return "auto mode: OFF — tool calls require confirmation"
}

// destructiveToggleNotice returns the status notice for a /auto destructive toggle.
func (f *RemoteFacade) destructiveToggleNotice(on bool) string {
	if on {
		return "⚠ destructive auto-approve: ON — rm, mv, git reset, … run without prompting\n" +
			"  still confirmed: external-backend egress; /auto destructive again to revoke"
	}
	return "destructive auto-approve: OFF — destructive commands require confirmation again"
}

// dispatchSubagentEndpoint handles /subagent [<name>|inherit] via SetSubagentEndpoint.
func (f *RemoteFacade) dispatchSubagentEndpoint(fields []string) sessionclient.CommandResult {
	if len(fields) >= 2 {
		name := fields[1]
		notice, err := f.callStateRPC("SetSubagentEndpoint", func(ctx context.Context, c wakilv1alpha1connect.SessionStateServiceClient, sid event.SessionID) (string, error) {
			resp, err := c.SetSubagentEndpoint(ctx, connect.NewRequest(&v1alpha1.SetSubagentEndpointRequest{
				SessionId: string(sid),
				Endpoint:  name,
			}))
			if err != nil {
				return "", err
			}
			return resp.Msg.Notice, nil
		})
		if err != nil {
			return sessionclient.CommandResult{Handled: true, Notice: "subagent: " + err.Error()}
		}
		f.refreshStateSync()
		return sessionclient.CommandResult{Handled: true, Notice: notice}
	}
	cur := f.stateString(func(s *v1alpha1.SessionState) string { return s.SubagentEndpoint })
	if cur == "" {
		return sessionclient.CommandResult{Handled: true, Notice: "subagent endpoint: inherit (parent endpoint)"}
	}
	return sessionclient.CommandResult{Handled: true, Notice: "subagent endpoint: " + cur}
}

// dispatchSubagentModel handles /submodel [<name>|inherit] via SetSubagentModel.
func (f *RemoteFacade) dispatchSubagentModel(fields []string) sessionclient.CommandResult {
	if len(fields) >= 2 {
		name := fields[1]
		notice, err := f.callStateRPC("SetSubagentModel", func(ctx context.Context, c wakilv1alpha1connect.SessionStateServiceClient, sid event.SessionID) (string, error) {
			resp, err := c.SetSubagentModel(ctx, connect.NewRequest(&v1alpha1.SetSubagentModelRequest{
				SessionId: string(sid),
				Model:     name,
			}))
			if err != nil {
				return "", err
			}
			return resp.Msg.Notice, nil
		})
		if err != nil {
			return sessionclient.CommandResult{Handled: true, Notice: "submodel: " + err.Error()}
		}
		f.refreshStateSync()
		return sessionclient.CommandResult{Handled: true, Notice: notice}
	}
	cur := f.stateString(func(s *v1alpha1.SessionState) string { return s.EffectiveSubagentModel })
	if cur == "" {
		cur = "(unknown — wait for the daemon state to load)"
	}
	return sessionclient.CommandResult{Handled: true, Notice: "subagent model: " + cur}
}

// dispatchMaxParallel handles /maxpar [<N>] via SetMaxParallelSubagents.
func (f *RemoteFacade) dispatchMaxParallel(fields []string) sessionclient.CommandResult {
	if len(fields) >= 2 {
		n, err := strconv.Atoi(fields[1])
		if err != nil || n < 1 {
			return sessionclient.CommandResult{Handled: true, Notice: "maxpar: must be a positive integer (1 = sequential)"}
		}
		notice, rerr := f.callStateRPC("SetMaxParallelSubagents", func(ctx context.Context, c wakilv1alpha1connect.SessionStateServiceClient, sid event.SessionID) (string, error) {
			resp, rerr := c.SetMaxParallelSubagents(ctx, connect.NewRequest(&v1alpha1.SetMaxParallelSubagentsRequest{
				SessionId: string(sid),
				Value:     int32(n),
			}))
			if rerr != nil {
				return "", rerr
			}
			return resp.Msg.Notice, nil
		})
		if rerr != nil {
			return sessionclient.CommandResult{Handled: true, Notice: "maxpar: " + rerr.Error()}
		}
		f.refreshStateSync()
		return sessionclient.CommandResult{Handled: true, Notice: notice}
	}
	cur := f.stateInt(func(s *v1alpha1.SessionState) int32 { return s.MaxParallelSubagents })
	if cur < 1 {
		cur = 1
	}
	return sessionclient.CommandResult{Handled: true, Notice: fmt.Sprintf("max parallel subagents: %d", cur)}
}

// dispatchEffectiveCtxMax handles /maxctx [<chars>] via SetEffectiveCtxMax.
func (f *RemoteFacade) dispatchEffectiveCtxMax(fields []string) sessionclient.CommandResult {
	if len(fields) >= 2 {
		n, err := strconv.Atoi(fields[1])
		if err != nil || n < 0 {
			return sessionclient.CommandResult{Handled: true, Notice: "maxctx: must be a non-negative integer (0 = disabled)"}
		}
		notice, rerr := f.callStateRPC("SetEffectiveCtxMax", func(ctx context.Context, c wakilv1alpha1connect.SessionStateServiceClient, sid event.SessionID) (string, error) {
			resp, rerr := c.SetEffectiveCtxMax(ctx, connect.NewRequest(&v1alpha1.SetEffectiveCtxMaxRequest{
				SessionId: string(sid),
				Value:     int32(n),
			}))
			if rerr != nil {
				return "", rerr
			}
			return resp.Msg.Notice, nil
		})
		if rerr != nil {
			return sessionclient.CommandResult{Handled: true, Notice: "maxctx: " + rerr.Error()}
		}
		f.refreshStateSync()
		return sessionclient.CommandResult{Handled: true, Notice: notice}
	}
	cap := f.stateInt(func(s *v1alpha1.SessionState) int32 { return s.EffectiveCtxMax })
	if cap <= 0 {
		return sessionclient.CommandResult{Handled: true, Notice: "effective context cap: disabled (using full model context)"}
	}
	return sessionclient.CommandResult{Handled: true, Notice: fmt.Sprintf("effective context cap: %d chars", cap)}
}

// dispatchRawTools handles /rawtools via SetRawTools (toggle).
func (f *RemoteFacade) dispatchRawTools(fields []string) sessionclient.CommandResult {
	next := !f.stateBool(func(s *v1alpha1.SessionState) bool { return s.RawTools })
	notice, err := f.callStateRPC("SetRawTools", func(ctx context.Context, c wakilv1alpha1connect.SessionStateServiceClient, sid event.SessionID) (string, error) {
		resp, err := c.SetRawTools(ctx, connect.NewRequest(&v1alpha1.SetRawToolsRequest{
			SessionId: string(sid),
			Value:     next,
		}))
		if err != nil {
			return "", err
		}
		return resp.Msg.Notice, nil
	})
	if err != nil {
		return sessionclient.CommandResult{Handled: true, Notice: "rawtools: " + err.Error()}
	}
	f.refreshStateSync()
	return sessionclient.CommandResult{Handled: true, Notice: notice}
}

// stateBool reads a bool field from the cached state with nil-safety.
func (f *RemoteFacade) stateBool(get func(*v1alpha1.SessionState) bool) bool {
	f.mu.Lock()
	st := f.state
	f.mu.Unlock()
	if st == nil {
		return false
	}
	return get(st)
}

// dispatchCounsel handles /counsel [auto|suggest|off] via SetCounselMode, or
// shows the current mode.
func (f *RemoteFacade) dispatchCounsel(fields []string) sessionclient.CommandResult {
	if len(fields) >= 2 {
		mode := fields[1]
		notice, err := f.callStateRPC("SetCounselMode", func(ctx context.Context, c wakilv1alpha1connect.SessionStateServiceClient, sid event.SessionID) (string, error) {
			resp, err := c.SetCounselMode(ctx, connect.NewRequest(&v1alpha1.SetCounselModeRequest{
				SessionId: string(sid),
				Mode:      mode,
			}))
			if err != nil {
				return "", err
			}
			return resp.Msg.Notice, nil
		})
		if err != nil {
			return sessionclient.CommandResult{Handled: true, Notice: "counsel: " + err.Error()}
		}
		f.refreshStateSync()
		return sessionclient.CommandResult{Handled: true, Notice: notice}
	}
	mode := f.stateString(func(s *v1alpha1.SessionState) string { return s.CounselMode })
	if mode == "" {
		mode = "suggest"
	}
	msg := "counsel mode: " + mode
	if mode == "auto" {
		msg += fmt.Sprintf(" (cap: %d/turn)", f.stateInt(func(s *v1alpha1.SessionState) int32 { return s.MaxCounsel }))
	}
	return sessionclient.CommandResult{Handled: true, Notice: msg}
}

// dispatchCompact handles /compact via the Compact RPC.
func (f *RemoteFacade) dispatchCompact(fields []string) sessionclient.CommandResult {
	res, err := f.callStateRPCResult("Compact", func(ctx context.Context, c wakilv1alpha1connect.SessionStateServiceClient, sid event.SessionID) (bool, string, error) {
		resp, err := c.Compact(ctx, connect.NewRequest(&v1alpha1.CompactRequest{
			SessionId: string(sid),
		}))
		if err != nil {
			return false, "", err
		}
		return resp.Msg.Compacted, resp.Msg.Notice, nil
	})
	if err != nil {
		return sessionclient.CommandResult{Handled: true, Notice: "compact: " + err.Error()}
	}
	f.refreshStateSync()
	return sessionclient.CommandResult{Handled: true, Compacted: res.compacted, Notice: res.notice}
}

// dispatchRepoState handles /repostate [clear] via the SaveRepoState RPC.
func (f *RemoteFacade) dispatchRepoState(fields []string) sessionclient.CommandResult {
	clear := len(fields) >= 2 && fields[1] == "clear"
	notice, err := f.callStateRPC("SaveRepoState", func(ctx context.Context, c wakilv1alpha1connect.SessionStateServiceClient, sid event.SessionID) (string, error) {
		resp, err := c.SaveRepoState(ctx, connect.NewRequest(&v1alpha1.SaveRepoStateRequest{
			SessionId: string(sid),
			Clear:     clear,
		}))
		if err != nil {
			return "", err
		}
		return resp.Msg.Notice, nil
	})
	if err != nil {
		return sessionclient.CommandResult{Handled: true, Notice: "repostate: " + err.Error()}
	}
	return sessionclient.CommandResult{Handled: true, Notice: notice}
}

// dispatchSessionLabel handles /session name "<label>" via SetSessionLabel.
func (f *RemoteFacade) dispatchSessionLabel(fields []string) sessionclient.CommandResult {
	if len(fields) >= 3 && fields[1] == "name" {
		label := strings.Join(fields[2:], " ")
		label = strings.Trim(label, `"'"`)
		notice, err := f.callStateRPC("SetSessionLabel", func(ctx context.Context, c wakilv1alpha1connect.SessionStateServiceClient, sid event.SessionID) (string, error) {
			resp, err := c.SetSessionLabel(ctx, connect.NewRequest(&v1alpha1.SetSessionLabelRequest{
				SessionId: string(sid),
				Label:     label,
			}))
			if err != nil {
				return "", err
			}
			return resp.Msg.Notice, nil
		})
		if err != nil {
			return sessionclient.CommandResult{Handled: true, Notice: "session: " + err.Error()}
		}
		return sessionclient.CommandResult{Handled: true, Notice: notice}
	}
	return sessionclient.CommandResult{Handled: true, Notice: `usage: /session name "<label>"`}
}

// ---- RPC helpers ----

// callStateRPC runs a SessionState mutation RPC synchronously (DispatchCommand
// is invoked from a tea.Cmd goroutine) with a bounded timeout and no session.
// It returns the RPC's Notice string. Safe when the facade has no session yet
// or the client is nil (returns a descriptive error).
func (f *RemoteFacade) callStateRPC(name string, call func(context.Context, wakilv1alpha1connect.SessionStateServiceClient, event.SessionID) (string, error)) (string, error) {
	ok, sid, client := f.getRPCTargets()
	if !ok {
		return "", fmt.Errorf("%s: not connected to the daemon's session-state service", name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	notice, err := call(ctx, client, sid)
	if err != nil {
		return "", err
	}
	f.bumpVersion()
	return notice, nil
}

type rpcResult struct {
	compacted bool
	notice    string
}

// callStateRPCResult is callStateRPC for RPCs whose response carries more than
// a Notice (Compact returns a bool too).
func (f *RemoteFacade) callStateRPCResult(name string, call func(context.Context, wakilv1alpha1connect.SessionStateServiceClient, event.SessionID) (bool, string, error)) (rpcResult, error) {
	ok, sid, client := f.getRPCTargets()
	if !ok {
		return rpcResult{}, fmt.Errorf("%s: not connected to the daemon's session-state service", name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	compacted, notice, err := call(ctx, client, sid)
	if err != nil {
		return rpcResult{}, err
	}
	f.bumpVersion()
	return rpcResult{compacted: compacted, notice: notice}, nil
}

// refreshStateSync performs a synchronous refreshState so the TUI's next
// Snapshot()/Consent()/Info() read sees the just-applied mutation. This is a
// bounded call (rpcTimeout) safe on the DispatchCommand goroutine only.
func (f *RemoteFacade) refreshStateSync() {
	f.refreshState(context.Background())
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

type remoteCompletionSource struct {
	f *RemoteFacade
}

// Models returns the daemon's model list from the cached state. Empty until
// the first GetSessionState lands — completion is best-effort and degrades
// gracefully to no candidates.
func (c *remoteCompletionSource) Models() []string {
	c.f.mu.Lock()
	st := c.f.state
	c.f.mu.Unlock()
	if st == nil {
		return nil
	}
	return append([]string(nil), st.ModelList...)
}

func (c *remoteCompletionSource) Backends() []sessionclient.Backend {
	c.f.mu.Lock()
	st := c.f.state
	c.f.mu.Unlock()
	if st == nil {
		return nil
	}
	return backendsFromProto(st.BackendList)
}

// Sessions returns nil in remote mode: session auto-completion requires the
// daemon's ListSavedSessions RPC on every keystroke, which is too expensive
// for the TUI's inline completion path. The /resume picker uses the full RPC
// path (ListSessions) instead. This is a documented P2e limitation.
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

// remoteHelpText is the /help text for daemon mode. It lists only the commands
// available remotely (the rest are classified by DispatchCommand with a
// "not available remotely" notice).
const remoteHelpText = `/new, /reset         fresh conversation (new chat_id)
/resume [<id>]      resume a saved session by id prefix; bare opens the picker
/model <name>       set the model for this session
/model              show current model
/backend <name>     set the backend for this session
/backend <name/model> set backend + model
/backend            show current backend selection and last-used backend
/subagent <name>    set which endpoint dispatch_subagent targets
/subagent inherit   reset dispatch_subagent to follow the parent's endpoint
/subagent           show current subagent endpoint selection
/submodel <name>    set the model for dispatch_subagent
/submodel inherit   reset subagent model to the endpoint's configured model
/submodel           show current subagent model
/maxpar <N>         set max concurrent dispatch_subagent workers (1 = sequential, max 64)
/maxpar             show current max parallel subagents
/maxctx <chars>     cap effective context for large models (0 = disabled)
/maxctx             show current effective context cap
/auto               toggle auto-approve of tool calls
/auto destructive   toggle auto-approve of destructive shell commands (requires /auto ON)
/rawtools           toggle full tool output in context
/counsel auto|suggest|off  auto-counsel mode
/counsel            show current counsel mode
/compact            summarize older turns now
/repostate          show terminal settings remembered for this folder
/repostate clear    delete remembered settings for this folder
/session name "..." label the current session
/cwd                show executor working directory
/mode               show execution backend
/help               this help
/quit, /exit        leave

Not available remotely: /handoff /learn /remember /recall /image /mcp
/mashura /plan /verify /sessions /history`
