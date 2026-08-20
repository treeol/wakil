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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/format"
	"github.com/treeol/wakil/internal/core/id"
	"github.com/treeol/wakil/internal/core/sessionclient"
	"github.com/treeol/wakil/internal/core/sessionhost"
	"github.com/treeol/wakil/internal/proxy"
	wtools "github.com/treeol/wakil/internal/tools"
	"github.com/treeol/wakil/internal/workflow"
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

	// pump is the event pump driving the subscription. Owned by the facade;
	// stopped and drained on Close.
	pump *EventPump

	// sideQuestions maps facade-minted OpIDs to their cancel functions. The
	// agent's StartSideQuestion returns only a CancelFunc (its internal
	// SideQuestionID never leaves the agent), so the facade mints its own
	// domain OpID per start and registers the cancel func. CancelSideQuestion
	// looks up by OpID, cancels, and removes the entry. Cleared on Close
	// (detached-job policy: cancel on close).
	sideQuestions map[sessionclient.OpID]context.CancelFunc

	// snapshotVersion increments on every facade-mediated mutation so a client
	// can detect a stale snapshot by comparing versions. It is a facade-local
	// revision counter (not the durable event seq — that is the host's
	// identity for turn events; snapshot fields like ModelList change through
	// facade mutations that may not produce events).
	snapshotVersion uint64

	// closed is true after Close; subsequent calls return ErrSessionClosed.
	closed bool
}

// newWiringFacade creates a facade from the given App, handle, host, and
// principal. It does NOT create a session — the caller (ConversationManager)
// creates the session and calls setSession before returning the facade to the
// TUI.
func newWiringFacade(app *agent.App, handle *HostTurnHandle, host *sessionhost.Host, res *AppResources, principal core.Principal) *wiringFacade {
	return &wiringFacade{
		app:          app,
		handle:       handle,
		host:         host,
		resources:    res,
		principal:    principal,
		sideQuestions: make(map[sessionclient.OpID]context.CancelFunc),
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
	f.mu.Lock()
	version := f.snapshotVersion
	f.mu.Unlock()
	app := f.app
	title := ""
	if app.Session != nil {
		title = app.Session.Label
	}
	return sessionclient.ClientSnapshot{
		SessionID:    f.sessionID,
		ChatID:       app.Client.ChatID,
		Title:        title,
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
		Version:      version,
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

// Info returns the deep-state view for the TUI's info panel and status line
// (7b3 m4). All slices are defensive copies. Mirrors the reads the old TUI
// made directly on *agent.App (tui_view.go ctxSegment/headerStatusInput,
// info_panel.go info*Segments).
func (f *wiringFacade) Info() sessionclient.InfoSnapshot {
	app := f.app
	used, exact := app.ContextUsage()

	info := sessionclient.InfoSnapshot{
		ChatID:         app.Client.ChatID,
		BaseURL:        app.Client.BaseURL,
		LastBackend:    app.Client.LastUsedBackend(),
		Cwd:            app.Exec.Cwd(),
		ExecMode:       app.Exec.Describe(),
		SelectedBackend: app.SelectedBackend,
		ConfigBackend:   app.Cfg.Backend,
		EffectiveModel:  app.EffectiveModel(),
		SubagentModel:   app.EffectiveSubagentModel(),
		PromptNote:      app.AgentPromptNote(),
		Image:           app.Cfg.Image,
		OracleLabel:     mashuraPanelLabel(app.Cfg),
		OracleOn:        app.Cfg.OracleEnabled,
		SearXngURL:      app.Cfg.SearXngURL,
		MentionBase:     app.Cfg.MentionBase,
		ContextLimit:    toClientContextLimit(app.ContextLimit()),
		ContextUsed:     used,
		ContextExact:    exact,
		ConvLen:         len(app.Conv),
		TranscriptSize:  format.TranscriptSize(app.Conv),
		Costs:           app.Costs,
	}

	if app.Workflow != nil {
		info.WorkflowLabel = app.Workflow.SidebarLabel()
	}

	// Endpoint names for completion ("inherit" first, sorted rest).
	endpoints := make([]string, 0, len(app.Cfg.Endpoints)+1)
	endpoints = append(endpoints, "inherit")
	for name := range app.Cfg.Endpoints {
		endpoints = append(endpoints, name)
	}
	sort.Strings(endpoints[1:])
	info.Endpoints = endpoints

	// MCP servers.
	if app.MCP != nil {
		servers := app.MCP.Servers()
		mcpInfo := make([]sessionclient.MCPServerInfo, 0, len(servers))
		for _, srv := range servers {
			mcpInfo = append(mcpInfo, sessionclient.MCPServerInfo{
				Name:   srv.Cfg.Name,
				Status: srv.Status,
				ToolN:  len(srv.Tools),
			})
		}
		info.MCPServers = mcpInfo
	}

	// SearXNG tools (names only — the panel shows tool availability).
	if app.Cfg.SearXngURL != "" {
		tools := wtools.SearxngTools()
		names := make([]string, 0, len(tools))
		for _, t := range tools {
			names = append(names, t.Function.Name)
		}
		info.SearxngTools = names
	}

	// Grounding entries.
	for _, g := range app.Client.Grounding() {
		info.Grounding = append(info.Grounding, sessionclient.GroundingEntry{
			Type:  g.Type,
			Label: g.Label,
		})
	}

	return info
}

// mashuraPanelLabel returns a short display string for the active mashura
// panel — moved from the TUI (info_panel.go) so the info snapshot can carry
// it without the TUI reading config internals.
func mashuraPanelLabel(cfg config.Config) string {
	name := "default"
	if cfg.MashuraToolPanels != nil {
		if p := cfg.MashuraToolPanels["review"]; p != "" {
			name = p
		}
	}
	if cfg.MashuraPanels != nil {
		if p, ok := cfg.MashuraPanels[name]; ok && len(p.Models) > 0 {
			switch p.Mode {
			case "fusion":
				return fmt.Sprintf("fusion (%d models)", len(p.Models))
			case "fallback":
				return fmt.Sprintf("%s +fallback", mashuraShortModel(p.Models[0]))
			default:
				if len(p.Models) == 1 {
					return mashuraShortModel(p.Models[0])
				}
				return fmt.Sprintf("%d models", len(p.Models))
			}
		}
	}
	return cfg.OracleModel
}

// mashuraShortModel strips the "provider:" prefix for compact display.
func mashuraShortModel(s string) string {
	if i := strings.IndexByte(s, ':'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// ---- Client-initiated mutations ----

// bumpVersion increments the snapshot revision counter. Called by every
// facade-mediated mutation so a client can detect a stale snapshot by version.
func (f *wiringFacade) bumpVersion() {
	f.mu.Lock()
	f.snapshotVersion++
	f.mu.Unlock()
}

func (f *wiringFacade) SetAutoApprove(v bool)      { f.bumpVersion(); f.app.SetAutoApprove(v) }
func (f *wiringFacade) SetAllowDestructive(v bool) { f.bumpVersion(); f.app.SetAllowDestructive(v) }
func (f *wiringFacade) RevokeAuto()                { f.bumpVersion(); f.app.RevokeAuto() }

func (f *wiringFacade) SetWorkflow(wf *sessionclient.WorkflowSnapshot) {
	f.bumpVersion()
	if wf == nil {
		f.app.SetWorkflow(nil)
		return
	}
	f.app.SetWorkflow(&workflow.WorkflowState{
		Task:      wf.Task,
		Phase:     workflowPhaseFromName(wf.Phase),
		StepCount: wf.StepCount,
		StepIdx:   wf.StepIdx,
		PlanPath:  wf.PlanPath,
	})
}

// workflowPhaseFromName maps a WorkflowSnapshot phase name back to the
// workflow.WorkflowPhase value. Snapshot phases round-trip through
// PhaseName() ("gather".."done"); an unknown name maps to WFGather — the
// workflow engine's initial phase — rather than silently clearing the
// workflow (a misnamed snapshot should still produce a live workflow object
// the user can /plan abort).
func workflowPhaseFromName(name string) workflow.WorkflowPhase {
	switch name {
	case "plan":
		return workflow.WFPlan
	case "review":
		return workflow.WFReview
	case "present":
		return workflow.WFPresent
	case "implement":
		return workflow.WFImplement
	case "done":
		return workflow.WFDone
	default: // "gather" and anything unknown
		return workflow.WFGather
	}
}

func (f *wiringFacade) AppendSystemMessage(m proxy.Message) {
	f.bumpVersion()
	f.app.Conv = append(f.app.Conv, m)
}

func (f *wiringFacade) SaveSession() { f.app.SaveSession() }

func (f *wiringFacade) ConsumeStartupNote() string {
	note := f.app.StartupNote
	f.app.StartupNote = ""
	return note
}

func (f *wiringFacade) SaveRepoState(mutate func(*sessionclient.RepoStateMutator)) {
	f.bumpVersion()
	f.app.SaveRepoState(func(s *agent.RepoState) {
		// Initialize the mutator from the current repo state so the caller
		// only needs to set the fields it wants to change — unset fields
		// preserve the existing values (no accidental zeroing).
		m := sessionclient.RepoStateMutator{
			Model:                s.Model,
			Backend:              s.Backend,
			SubagentEndpoint:     s.SubagentEndpoint,
			SubagentModel:        s.SubagentModel,
			RawTools:             s.RawTools,
			MaxParallelSubagents: s.MaxParallelSubagents,
			AutoApprove:          s.AutoApprove,
			InfoPanelOpen:        s.InfoPanelOpen,
		}
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

func (f *wiringFacade) SetInfoPanelOpen(open bool) { f.bumpVersion(); f.app.SetInfoPanelOpen(open) }

func (f *wiringFacade) SetCtxLimit(lim sessionclient.ContextLimit) {
	f.bumpVersion()
	f.app.CtxLimit = toAgentContextLimit(lim)
}

func (f *wiringFacade) SetModelList(models []string) {
	f.bumpVersion()
	f.app.ModelList = append([]string(nil), models...)
}

func (f *wiringFacade) SetTools(tools []proxy.Tool) {
	f.bumpVersion()
	f.app.Tools = append([]proxy.Tool(nil), tools...)
}

func (f *wiringFacade) ReplacePendingImages(imgs []proxy.ImagePart) {
	f.bumpVersion()
	f.app.PendingImages = append([]proxy.ImagePart(nil), imgs...)
}

func (f *wiringFacade) AddPendingImage(img proxy.ImagePart) {
	f.bumpVersion()
	f.app.PendingImages = append(f.app.PendingImages, img)
}

func (f *wiringFacade) ClearPendingImages() {
	f.bumpVersion()
	f.app.PendingImages = nil
}

// ---- Side questions ----

// StartSideQuestion starts a concurrent side-question stream and returns a
// facade-minted OpID plus the cancel function. The agent's own SideQuestionID
// never crosses the boundary; the facade's OpID is the client-facing identity
// (used for display routing only — the progress/completion events the agent
// emits still carry the agent-internal ID, projected to a domain OpID by the
// event projection, which may differ from this one. The TUI correlates side-
// question output by "the active side question", not by OpID — matching the
// old TUI, which ignored the ID entirely and rendered the single active
// stream).
func (f *wiringFacade) StartSideQuestion(ctx context.Context, question string) (sessionclient.OpID, context.CancelFunc) {
	rawID, err := id.NewOpID()
	if err != nil {
		// ID generation failure is not recoverable — mint a degraded unique ID
		// from the wall clock so the registry still works.
		rawID = event.OpID(fmt.Sprintf("op_%d", time.Now().UnixNano()))
	}
	opID := sessionclient.OpID(rawID)
	cancel := f.app.StartSideQuestion(ctx, question)
	f.mu.Lock()
	if f.sideQuestions == nil {
		f.sideQuestions = make(map[sessionclient.OpID]context.CancelFunc)
	}
	f.sideQuestions[opID] = cancel
	f.mu.Unlock()
	return opID, cancel
}

// CancelSideQuestion cancels the side question started under the given OpID
// and removes it from the registry. Unknown or already-cancelled IDs are a
// no-op (the agent's cancel is idempotent; a stale ID simply maps to nothing).
func (f *wiringFacade) CancelSideQuestion(opID sessionclient.OpID) {
	f.mu.Lock()
	cancel, ok := f.sideQuestions[opID]
	if ok {
		delete(f.sideQuestions, opID)
	}
	f.mu.Unlock()
	if ok && cancel != nil {
		cancel()
	}
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
	pump := f.pump
	sub := f.subscription
	sideQuestions := f.sideQuestions
	f.sideQuestions = nil
	f.mu.Unlock()

	// Stop the event pump first; it closes the subscription internally.
	if pump != nil {
		pump.Stop()
		select {
		case <-pump.Done():
		default:
			// Non-blocking; the pump will finish on its own.
		}
	} else if sub != nil {
		_ = sub.Close()
	}

	// Cancel any running side questions (detached-job policy: cancel on close).
	for _, cancel := range sideQuestions {
		if cancel != nil {
			cancel()
		}
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
