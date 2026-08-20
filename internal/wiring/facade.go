package wiring

// facade.go: the wiring-side implementation of sessionclient.Facade (card #148
// chunk 7b3 m3). It bridges the agent-free facade contract to the real
// *agent.App + *sessionhost.Host, translating facade calls into App mutations
// and host service calls.
//
// The facade is the ONLY object the TUI holds for a conversation. It owns:
//   - the *agent.App (mutable conversation state)
//   - the HostTurnHandle (App↔host binding)
//   - the *sessionhost.Host (session lifecycle)
//   - the session ID (once created)
//   - the event subscription (live event stream)
//
// The TUI never touches *agent.App directly — it goes through the facade. The
// facade constructs ClientSnapshot from the App's fields, delegates mutations
// to App methods, and routes session-service calls (SubmitInput, Interrupt,
// RespondToApproval, Close) through the host.

import (
	"context"
	"fmt"
	"sync"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionclient"
	"github.com/treeol/wakil/internal/core/sessionhost"
	"github.com/treeol/wakil/internal/proxy"
)

// Compile-time proof that wiringFacade satisfies the Facade interface.
var _ sessionclient.Facade = (*wiringFacade)(nil)

// wiringFacade implements sessionclient.Facade by bridging to *agent.App and
// *sessionhost.Host. It is created by the ConversationManager (or the bootstrap
// factory) and is the ONLY facade implementation.
type wiringFacade struct {
	mu        sync.Mutex
	app       *agent.App
	handle    *HostTurnHandle
	host      *sessionhost.Host
	resources *AppResources
	principal core.Principal

	// sessionID is set once after CreateSession. The TUI reads it from the
	// snapshot.
	sessionID event.SessionID

	// subscription is the live event stream, set after Subscribe. The event
	// pump reads from it.
	subscription core.EventSubscription

	// closed is true after Close; subsequent calls return ErrSessionClosed.
	closed bool
}

// newWiringFacade creates a facade from the given App, handle, host, and
// principal. It does NOT create a session — the caller (ConversationManager)
// creates the session and calls setSession before returning the facade to the
// TUI.
func newWiringFacade(app *agent.App, handle *HostTurnHandle, host *sessionhost.Host, res *AppResources, principal core.Principal) *wiringFacade {
	return &wiringFacade{
		app:       app,
		handle:    handle,
		host:      host,
		resources: res,
		principal: principal,
	}
}

// setSession records the session ID after CreateSession. Called once by the
// ConversationManager.
func (f *wiringFacade) setSession(sid event.SessionID) {
	f.mu.Lock()
	f.sessionID = sid
	f.mu.Unlock()
}

// ---- SessionService delegation ----

func (f *wiringFacade) CreateSession(ctx context.Context, principal core.Principal, req core.CreateSessionRequest) (core.Session, error) {
	return f.host.CreateSession(ctx, principal, req)
}

func (f *wiringFacade) SubmitInput(ctx context.Context, principal core.Principal, req core.SubmitInputRequest) (core.TurnAck, error) {
	return f.host.SubmitInput(ctx, principal, req)
}

func (f *wiringFacade) RespondToApproval(ctx context.Context, principal core.Principal, d core.ApprovalDecision) error {
	return f.host.RespondToApproval(ctx, principal, d)
}

func (f *wiringFacade) Interrupt(ctx context.Context, principal core.Principal, sessionID event.SessionID) error {
	return f.host.Interrupt(ctx, principal, sessionID)
}

func (f *wiringFacade) CloseSession(ctx context.Context, principal core.Principal, sessionID event.SessionID) error {
	return f.host.CloseSession(ctx, principal, sessionID)
}

// ---- EventReader delegation ----

func (f *wiringFacade) Subscribe(ctx context.Context, principal core.Principal, sessionID event.SessionID, after event.Seq) (core.EventSubscription, error) {
	sub, err := f.host.Subscribe(ctx, principal, sessionID, after)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.subscription = sub
	f.mu.Unlock()
	return sub, nil
}

func (f *wiringFacade) ListEvents(ctx context.Context, principal core.Principal, sessionID event.SessionID, after event.Seq, limit int) ([]event.Event, error) {
	return f.host.ListEvents(ctx, principal, sessionID, after, limit)
}

func (f *wiringFacade) SessionSnapshot(ctx context.Context, principal core.Principal, sessionID event.SessionID) (core.SessionSnapshot, error) {
	return f.host.SessionSnapshot(ctx, principal, sessionID)
}

// ---- TUI-specific surfaces ----

func (f *wiringFacade) Snapshot() sessionclient.ClientSnapshot {
	app := f.app
	return sessionclient.ClientSnapshot{
		SessionID:    f.sessionID,
		ChatID:       app.Client.ChatID,
		Title:        "",
		Workspace:    app.SessionWorkspace(),
		Backend:      app.SelectedBackend,
		Model:        app.EffectiveModel(),
		Conv:         app.ConvSnapshot(),
		ContextLimit: toClientContextLimit(app.CtxLimit),
		ModelList:    append([]string(nil), app.ModelList...),
		BackendList:  toClientBackends(app.BackendList),
		Tools:        append([]proxy.Tool(nil), app.Tools...),
		PendingImages: append([]proxy.ImagePart(nil), app.PendingImages...),
		RawTools:     app.RawTools,
		OutputMode:   app.Cfg.OutputMode,
		Costs:        app.Costs,
		Workflow:     toClientWorkflow(app),
		Version:      0, // TODO(7b3 m3): version-stamp with event seq
	}
}

func (f *wiringFacade) Consent() sessionclient.Consent {
	c := f.app.Consent()
	return sessionclient.Consent{
		AutoApprove:      c.AutoApprove,
		AllowDestructive: c.AllowDestructive,
		AllowReads:       c.AllowReads,
	}
}

func (f *wiringFacade) CompletionSource() sessionclient.CompletionSource {
	return &facadeCompletionSource{f: f}
}

// ---- Client-initiated mutations ----

func (f *wiringFacade) SetAutoApprove(v bool)      { f.app.SetAutoApprove(v) }
func (f *wiringFacade) SetAllowDestructive(v bool) { f.app.SetAllowDestructive(v) }
func (f *wiringFacade) RevokeAuto()               { f.app.RevokeAuto() }

func (f *wiringFacade) SetWorkflow(wf *sessionclient.WorkflowSnapshot) {
	if wf == nil {
		f.app.SetWorkflow(nil)
		return
	}
	// TODO: convert WorkflowSnapshot to workflow.WorkflowState — the agent's
	// SetWorkflow takes a *workflow.WorkflowState. For now, nil it.
	f.app.SetWorkflow(nil)
}

func (f *wiringFacade) AppendSystemMessage(m proxy.Message) {
	f.app.Conv = append(f.app.Conv, m)
}

func (f *wiringFacade) SaveSession() { f.app.SaveSession() }

func (f *wiringFacade) ConsumeStartupNote() string {
	note := f.app.StartupNote
	f.app.StartupNote = ""
	return note
}

func (f *wiringFacade) SaveRepoState(mutate func(*sessionclient.RepoStateMutator)) {
	f.app.SaveRepoState(func(s *agent.RepoState) {
		m := sessionclient.RepoStateMutator{}
		mutate(&m)
		if m.Model != "" {
			s.Model = m.Model
		}
		if m.Backend != "" {
			s.Backend = m.Backend
		}
		if m.SubagentEndpoint != "" {
			s.SubagentEndpoint = m.SubagentEndpoint
		}
		if m.SubagentModel != "" {
			s.SubagentModel = m.SubagentModel
		}
		s.RawTools = m.RawTools
		s.MaxParallelSubagents = m.MaxParallelSubagents
		s.AutoApprove = m.AutoApprove
		s.InfoPanelOpen = m.InfoPanelOpen
	})
}

func (f *wiringFacade) SetInfoPanelOpen(open bool) { f.app.SetInfoPanelOpen(open) }

func (f *wiringFacade) SetCtxLimit(lim sessionclient.ContextLimit) {
	f.app.CtxLimit = toAgentContextLimit(lim)
}

func (f *wiringFacade) SetModelList(models []string) {
	f.app.ModelList = append([]string(nil), models...)
}

func (f *wiringFacade) SetTools(tools []proxy.Tool) {
	f.app.Tools = append([]proxy.Tool(nil), tools...)
}

func (f *wiringFacade) ReplacePendingImages(imgs []proxy.ImagePart) {
	f.app.PendingImages = append([]proxy.ImagePart(nil), imgs...)
}

func (f *wiringFacade) AddPendingImage(img proxy.ImagePart) {
	f.app.PendingImages = append(f.app.PendingImages, img)
}

func (f *wiringFacade) ClearPendingImages() {
	f.app.PendingImages = nil
}

// ---- Side questions ----

func (f *wiringFacade) StartSideQuestion(ctx context.Context, question string) (sessionclient.OpID, context.CancelFunc) {
	cancel := f.app.StartSideQuestion(ctx, question)
	// The agent's StartSideQuestion returns only a CancelFunc, not an OpID.
	// We generate a pseudo OpID for the facade contract. The TUI uses it
	// only for display routing; the actual cancellation goes through the
	// returned CancelFunc.
	return sessionclient.OpID("op_sq"), cancel
}

func (f *wiringFacade) CancelSideQuestion(id sessionclient.OpID) {
	// The agent's side question API is cancel-only (via the CancelFunc
	// returned by StartSideQuestion). There is no separate CancelSideQuestion
	// method on *agent.App. The TUI must hold the CancelFunc from
	// StartSideQuestion and call it directly. This method is a no-op stub
	// for the interface contract.
}

// ---- Session listing ----

func (f *wiringFacade) ListSessions(scope sessionclient.SessionScope) ([]sessionclient.SessionSummary, int, error) {
	s := agent.SessionScope{Workspace: scope.Workspace, All: scope.All}
	sessions, hidden, err := agent.ListSessionsScoped(s)
	if err != nil {
		return nil, 0, err
	}
	out := make([]sessionclient.SessionSummary, len(sessions))
	for i, sess := range sessions {
		out[i] = sessionclient.SessionSummary{
			ChatID:    sess.ChatID,
			Model:     sess.Model,
			Label:     sess.Label,
			Workspace: sess.Workspace,
			Created:   sess.Created,
			Updated:   sess.Updated,
			Conv:      sess.Conv,
		}
	}
	return out, hidden, nil
}

func (f *wiringFacade) LoadSession(idOrPrefix string) (*sessionclient.SessionSummary, error) {
	s, err := agent.LoadSessionScoped(idOrPrefix, agent.SessionScope{})
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, nil
	}
	return &sessionclient.SessionSummary{
		ChatID:    s.ChatID,
		Model:     s.Model,
		Label:     s.Label,
		Workspace: s.Workspace,
		Created:   s.Created,
		Updated:   s.Updated,
		Conv:      s.Conv,
	}, nil
}

// ---- Slash-command dispatch ----

func (f *wiringFacade) DispatchCommand(line string) sessionclient.CommandResult {
	handled, quit, cmd := agent.HandleTUICommand(line, f.app)
	if !handled {
		return sessionclient.CommandResult{}
	}
	// The agent returns a Cmd (func() Msg). We interpret it to produce a
	// CommandResult. This is the transition point where agent.Cmd is
	// translated to the agent-free CommandResult.
	if cmd == nil {
		return sessionclient.CommandResult{Handled: true, Quit: quit}
	}
	msg := cmd()
	return interpretAgentMsg(msg, quit)
}

// interpretAgentMsg translates an agent.Msg (returned by agent.HandleTUICommand)
// into a sessionclient.CommandResult. It type-switches on the message type
// and maps each to the corresponding CommandResult field.
func interpretAgentMsg(msg any, quit bool) sessionclient.CommandResult {
	cr := sessionclient.CommandResult{Handled: true, Quit: quit}
	if msg == nil {
		return cr
	}
	switch m := msg.(type) {
	case agent.SysNoteMsg:
		cr.Notice = m.Text
	case agent.NewConvMsg:
		cr.Rotate = &sessionclient.RotateRequest{Type: "new"}
		if m.Note != "" {
			cr.Notice = m.Note
		}
	case agent.HandoffMsg:
		if m.Err != nil {
			cr.Notice = "handoff failed: " + m.Err.Error()
			return cr
		}
		cr.Rotate = &sessionclient.RotateRequest{
			Type:           "handoff",
			HandoffContext: m.ContinuationPrompt,
			Proceed:        m.Proceed,
		}
	case agent.OpenResumePickerMsg:
		cr.Rotate = &sessionclient.RotateRequest{Type: "resume"}
	case agent.WFFinalReviewMsg:
		// Workflow final review — the TUI submits a final-review input.
		// For now, map to Submit (the TUI submits a workflow continuation).
		cr.Submit = "continue" // TODO: proper workflow continuation text
	case agent.WFStartTurnMsg:
		cr.Submit = m.UserText
		if cr.Submit == "" {
			cr.Submit = "continue"
		}
	case agent.LearnTurnMsg:
		cr.Submit = "/learn"
	case agent.RememberTurnMsg:
		cr.Submit = m.UserText
		if m.UserText == "" {
			cr.Submit = m.Query
		}
	case agent.RecallTurnMsg:
		cr.Submit = m.UserText
	case agent.CompactedMsg:
		cr.Compacted = true
	case agent.MCPReconnectedMsg:
		cr.Notice = "mcp reconnected: " + m.Name
	default:
		// Unknown message — best effort: if it's a string, use as notice.
		if s, ok := msg.(fmt.Stringer); ok {
			cr.Notice = s.String()
		}
	}
	return cr
}

// ---- Lifecycle ----

func (f *wiringFacade) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	sub := f.subscription
	f.mu.Unlock()

	if sub != nil {
		_ = sub.Close()
	}
	// Cancel detached async jobs (detached-job policy: cancel on close).
	f.app.StopAllAsyncOps()
	f.app.StopAllBackgroundProcs()

	// Release the App ownership claim.
	if f.handle != nil {
		_ = f.handle.Release()
	}
	return nil
}

// ---- CompletionSource ----

type facadeCompletionSource struct {
	f *wiringFacade
}

func (c *facadeCompletionSource) Models() []string {
	return append([]string(nil), c.f.app.ModelList...)
}

func (c *facadeCompletionSource) Backends() []sessionclient.Backend {
	return toClientBackends(c.f.app.BackendList)
}

func (c *facadeCompletionSource) Sessions() []sessionclient.SessionSummary {
	sessions, err := agent.ListSessions()
	if err != nil {
		return nil
	}
	out := make([]sessionclient.SessionSummary, len(sessions))
	for i, s := range sessions {
		out[i] = sessionclient.SessionSummary{
			ChatID:    s.ChatID,
			Model:     s.Model,
			Label:     s.Label,
			Workspace: s.Workspace,
			Created:   s.Created,
			Updated:   s.Updated,
			Conv:      s.Conv,
		}
	}
	return out
}

// ---- conversion helpers ----

func toClientContextLimit(lim agent.ContextLimit) sessionclient.ContextLimit {
	return sessionclient.ContextLimit{
		NCtx:            lim.NCtx,
		NCtxTrain:       lim.NCtxTrain,
		Source:          lim.Source,
		ContextSource:   lim.ContextSource,
		UsableCtx:       lim.UsableCtx,
		ReasoningBudget:  lim.ReasoningBudget,
		AnswerMargin:    lim.AnswerMargin,
		ModelUnresolved:  lim.ModelUnresolved,
	}
}

func toAgentContextLimit(lim sessionclient.ContextLimit) agent.ContextLimit {
	return agent.ContextLimit{
		NCtx:            lim.NCtx,
		NCtxTrain:       lim.NCtxTrain,
		Source:          lim.Source,
		ContextSource:   lim.ContextSource,
		UsableCtx:       lim.UsableCtx,
		ReasoningBudget:  lim.ReasoningBudget,
		AnswerMargin:    lim.AnswerMargin,
		ModelUnresolved:  lim.ModelUnresolved,
	}
}

func toClientBackends(backends []agent.BackendInfo) []sessionclient.Backend {
	out := make([]sessionclient.Backend, len(backends))
	for i, b := range backends {
		out[i] = sessionclient.Backend{
			Name:     b.Name,
			External: b.External,
			Caps:     append([]string(nil), b.Caps...),
		}
	}
	return out
}

func toClientWorkflow(app *agent.App) *sessionclient.WorkflowSnapshot {
	if app.Workflow == nil {
		return nil
	}
	return &sessionclient.WorkflowSnapshot{
		Task:      app.Workflow.Task,
		Phase:     app.Workflow.PhaseName(),
		StepCount: app.Workflow.StepCount,
		StepIdx:   app.Workflow.StepIdx,
		PlanPath:  app.Workflow.PlanPath,
	}
}
