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
	"os"
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

// Subscribe returns a live event stream for the session and starts the
// facade-owned event pump (7b3 m4): every event — durable and ephemeral — is
// delivered to the given callback (typically tea.Program.Send). The pump is
// stopped and the subscription closed by Close; rotation stops it via Stop.
//
// The subscription is also retained on the facade (f.subscription) for
// callers that read it directly.
func (f *wiringFacade) Subscribe(ctx context.Context, principal core.Principal, sessionID event.SessionID, after event.Seq, deliver func(event.Event)) (core.EventSubscription, error) {
	f.mu.Lock()
	closed := f.closed
	f.mu.Unlock()
	if closed {
		return nil, fmt.Errorf("facade closed")
	}
	sub, err := f.host.Subscribe(ctx, principal, sessionID, after)
	if err != nil {
		return nil, err
	}
	pump := NewEventPump(sub, f.host, principal, sessionID, after, deliver)
	f.mu.Lock()
	f.subscription = sub
	f.pump = pump
	f.mu.Unlock()
	return sub, nil
}

// StartEventPump runs the facade's event pump in a goroutine until it is
// stopped (Close or rotation). No-op when no pump exists (Subscribe not
// called). It is separate from Subscribe so the caller controls when
// delivery begins (e.g. after the TUI program is constructed).
func (f *wiringFacade) StartEventPump(ctx context.Context) {
	f.mu.Lock()
	pump := f.pump
	f.mu.Unlock()
	if pump != nil {
		go pump.Run(ctx)
	}
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
	info.InfoPanelOpen = app.InfoPanelOpen

	// Oracle label with the "no key" fallback the old TUI computed inline
	// (env checks belong wiring-side, not in the render path).
	if app.Cfg.OracleEnabled {
		anthropicOk := os.Getenv(app.Cfg.OracleAPIKeyEnv) != ""
		openrouterOk := os.Getenv("OPENROUTER_API_KEY") != ""
		if anthropicOk || openrouterOk {
			info.OracleLabel = mashuraPanelLabel(app.Cfg)
		} else {
			info.OracleLabel = "no key"
		}
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

// bumpVersion increments the snapshot revision counter. Called AFTER every
// facade-mediated mutation (mutate-then-bump, never bump-then-mutate: a
// reader between the bump and the mutation would see a new version with
// stale data, and the later fresh fetch would carry the SAME version — the
// staleness check would miss it forever) so a client can detect a stale
// snapshot by version.
func (f *wiringFacade) bumpVersion() {
	f.mu.Lock()
	f.snapshotVersion++
	f.mu.Unlock()
}

func (f *wiringFacade) SetAutoApprove(v bool)      { f.app.SetAutoApprove(v); f.bumpVersion() }
func (f *wiringFacade) SetAllowDestructive(v bool) { f.app.SetAllowDestructive(v); f.bumpVersion() }
func (f *wiringFacade) RevokeAuto()                { f.app.RevokeAuto(); f.bumpVersion() }

func (f *wiringFacade) SetWorkflow(wf *sessionclient.WorkflowSnapshot) {
	if wf == nil {
		f.app.SetWorkflow(nil)
		f.bumpVersion()
		return
	}
	f.app.SetWorkflow(&workflow.WorkflowState{
		Task:      wf.Task,
		Phase:     workflowPhaseFromName(wf.Phase),
		StepCount: wf.StepCount,
		StepIdx:   wf.StepIdx,
		PlanPath:  wf.PlanPath,
	})
	f.bumpVersion()
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
	f.app.AppendSystemMessage(m)
	f.bumpVersion()
}

func (f *wiringFacade) SaveSession() { f.app.SaveSession() }

func (f *wiringFacade) ConsumeStartupNote() string {
	note := f.app.StartupNote
	f.app.StartupNote = ""
	return note
}

func (f *wiringFacade) SaveRepoState(mutate func(*sessionclient.RepoStateMutator)) {
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
	f.bumpVersion()
}

func (f *wiringFacade) SetInfoPanelOpen(open bool) { f.app.SetInfoPanelOpen(open); f.bumpVersion() }

func (f *wiringFacade) SetCtxLimit(lim sessionclient.ContextLimit) {
	f.app.CtxLimit = toAgentContextLimit(lim)
	f.bumpVersion()
}

func (f *wiringFacade) SetModelList(models []string) {
	f.app.ModelList = append([]string(nil), models...)
	f.bumpVersion()
}

func (f *wiringFacade) SetTools(tools []proxy.Tool) {
	f.app.Tools = append([]proxy.Tool(nil), tools...)
	f.bumpVersion()
}

func (f *wiringFacade) ReplacePendingImages(imgs []proxy.ImagePart) {
	f.app.PendingImages = append([]proxy.ImagePart(nil), imgs...)
	f.bumpVersion()
}

func (f *wiringFacade) AddPendingImage(img proxy.ImagePart) {
	f.app.PendingImages = append(f.app.PendingImages, img)
	f.bumpVersion()
}

func (f *wiringFacade) ClearPendingImages() {
	f.app.PendingImages = nil
	f.bumpVersion()
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
	s := agent.SessionScope{Workspace: scope.Workspace, All: scope.All, IncludeLegacy: true}
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

// DispatchCommand classifies and executes a slash command (the agent-free
// replacement for agent.HandleTUICommand + AdaptCmd).
//
// CALLING CONTRACT (7b3 m4, per review): some commands' work is slow —
// /handoff runs a summarizer pipeline (up to 120s), /remember and /recall run
// 30s memory searches, /compact runs a summarizer. The facade executes them
// SYNCHRONOUSLY; the caller (the TUI) must invoke DispatchCommand from a
// worker goroutine and deliver the CommandResult back through its event loop
// (the same AdaptCmd pattern the old TUI used — a tea.Cmd goroutine). Fast
// commands (arg parsing + state mutation) may also be called synchronously;
// the old TUI ran HandleTUICommand's classification on the event loop and only
// the returned Cmd async.
//
// /handoff is special: it does NOT execute the pipeline here (that would
// duplicate the work HandoffConversation performs). It validates arguments
// and returns Rotate{Type:"handoff"} — the caller drives the rotation through
// ConversationManager.HandoffConversation, which runs the pipeline exactly
// once, asynchronously.
func (f *wiringFacade) DispatchCommand(line string) sessionclient.CommandResult {
	if fields := strings.Fields(line); len(fields) > 0 {
		switch fields[0] {
		case "/handoff":
			return f.dispatchHandoff(fields)
		case "/new", "/reset":
			// Intercepted (m4b): agent.HandleTUICommand's /new & /reset call
			// app.NewConversation + finalizeSessionHistory on the OLD App —
			// wrong target on the wiring path. Rotation must build a fresh
			// App/facade through the manager; session-history finalize runs
			// there against the old session (it belongs with "old session is
			// final"). Here we only classify.
			return sessionclient.CommandResult{
				Handled: true,
				Rotate:  &sessionclient.RotateRequest{Type: "new"},
				Rotating: true,
			}
		case "/resume":
			return f.dispatchResume(fields)
		}
	}
	handled, quit, cmd := agent.HandleTUICommand(line, f.app)
	if !handled {
		return sessionclient.CommandResult{}
	}
	// Commands can mutate authoritative App state repo-side (/backend,
	// /model, /mcp reconnect, /auto, …) with no corresponding event (D24:
	// query-state). Bump the snapshot revision so the TUI's post-command
	// re-fetch sees fresh state.
	f.bumpVersion()
	if cmd == nil {
		return sessionclient.CommandResult{Handled: true, Quit: quit}
	}
	msg := cmd()
	return f.interpretAgentMsg(msg, quit)
}

// dispatchResume classifies /resume (m4b): bare /resume opens the interactive
// picker (TUI-local, session list via ListSessions); /resume <id> rotates to
// that session through the manager. The agent's /resume Cmds either load the
// session into the OLD App (ResumeSessionMsg — wrong target on the wiring
// path) or open the picker; both are replaced here.
func (f *wiringFacade) dispatchResume(fields []string) sessionclient.CommandResult {
	if len(fields) == 1 || (len(fields) == 2 && fields[1] == "all") {
		return sessionclient.CommandResult{Handled: true, ResumePicker: true}
	}
	return sessionclient.CommandResult{
		Handled: true,
		Rotate: &sessionclient.RotateRequest{
			Type:    "resume",
			Session: nil, // resolved by the manager from the id/prefix
		},
		Rotating: true,
	}
}

// interpretAgentMsg translates an agent.Msg (returned by agent.HandleTUICommand)
// into a sessionclient.CommandResult. It type-switches on the message type
// and maps each to the corresponding CommandResult field.
//
// Batch recursion: commands like /backend return agent.BatchMsg{note, probe}.
// Every sub-Cmd is executed and interpreted; side effects (ctx-limit probes,
// model-list fetches) are applied to the App HERE, on the facade's side of
// the boundary, because no event carries them (D24: query-state, not events)
// and the TUI has no way to apply agent messages. First applicable action
// (Submit/Rotate/…) wins; notices concatenate.
func (f *wiringFacade) interpretAgentMsg(msg any, quit bool) sessionclient.CommandResult {
	cr := sessionclient.CommandResult{Handled: true, Quit: quit}
	if msg == nil {
		return cr
	}
	if batch, ok := msg.(agent.BatchMsg); ok {
		for _, c := range batch.Cmds {
			if c == nil {
				continue
			}
			sub := f.interpretAgentMsg(c(), false)
			if sub.Quit {
				cr.Quit = true
			}
			if sub.Notice != "" {
				if cr.Notice != "" {
					cr.Notice += "\n" + sub.Notice
				} else {
					cr.Notice = sub.Notice
				}
			}
			if sub.Compacted {
				cr.Compacted = true
			}
			if sub.ClipboardImage {
				cr.ClipboardImage = true
			}
			if sub.Rotating {
				cr.Rotating = true
			}
			// First action field wins (Batch carries note+probe, never two actions).
			if sub.Submit != "" && cr.Submit == "" {
				cr.Submit = sub.Submit
			}
			if sub.Rotate != nil && cr.Rotate == nil {
				cr.Rotate = sub.Rotate
			}
			if sub.ResumePicker {
				cr.ResumePicker = true
			}
		}
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
		// The picker itself is TUI-local display state; the TUI opens it from
		// this flag and reads the session list through the facade (D6).
		cr.ResumePicker = true
	case agent.WFFinalReviewMsg:
		// Workflow final review (from /plan verify, or /plan approve closing
		// the last step). The old TUI ran RunFinalReview — a dedicated Cmd that
		// drove HandleFinalReview with the turn goroutine's callbacks. On the
		// wiring path the final review runs INSIDE HandleWorkflowTransition
		// after each IMPLEMENT turn (and on remediation turns), so this
		// message reaching DispatchCommand means a user-typed command wants
		// the review NOW: submit a plain continuation turn, which the adapter
		// runs; HandleWorkflowTransition at its end re-runs the review (the
		// verify-state remediation path re-runs it on every completed turn).
		cr.Submit = "continue"
	case agent.WFStartTurnMsg:
		cr.Submit = m.UserText
		if cr.Submit == "" {
			cr.Submit = "continue"
		}
	case agent.LearnTurnMsg:
		// The old TUI's LearnTurnMsg handler started a RunTurn with this exact
		// literal text (the proxy recognizes it as the learn trigger). NOT
		// "/learn" — that would re-dispatch the command and loop.
		cr.Submit = "learn this for next time"
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
		// Apply the rebuilt tool list HERE (the old TUI did m.apply.SetTools in
		// its MCPReconnectedMsg handler). No event carries it — the TUI
		// re-reads Snapshot() after every DispatchCommand and would otherwise
		// see stale Tools.
		f.bumpVersion()
		f.app.SetTools(m.Tools)
		cr.Notice = "mcp reconnected: " + m.Name
	case agent.BackendCtxLimitMsg:
		// D24 query-state: applied facade-side, no event. The TUI re-reads
		// Snapshot() after the command completes.
		f.bumpVersion()
		f.app.SetCtxLimit(m.Limit)
		if m.Note != "" {
			cr.Notice = m.Note
		}
	case agent.ModelListUpdatedMsg:
		f.bumpVersion()
		f.app.SetModelList(m.Models)
	default:
		// Clipboard sentinel VALUE: agent.ClipboardImageRequest is a var of an
		// unexported struct type, so a type switch cannot case on it — match
		// by equality. The agent cannot read the clipboard; the TUI
		// substitutes its own clipboard-reading command (readClipboardCmd).
		if msg == any(agent.ClipboardImageRequest) {
			cr.ClipboardImage = true
			return cr
		}
		// Unknown message — best effort: if it's a string, use as notice.
		if s, ok := msg.(fmt.Stringer); ok {
			cr.Notice = s.String()
		}
	}
	return cr
}

// dispatchHandoff classifies /handoff WITHOUT executing the pipeline: arg
// validation + the quick-fail emptiness guards run here (fast), and the
// rotation intent is returned for the caller to drive through
// ConversationManager.HandoffConversation — which runs the real pipeline
// (save, summarize, index, record) exactly once.
func (f *wiringFacade) dispatchHandoff(fields []string) sessionclient.CommandResult {
	proceed := false
	if len(fields) > 2 {
		return sessionclient.CommandResult{Handled: true, Notice: "usage: /handoff [proceed|stop]"}
	}
	if len(fields) == 2 {
		switch fields[1] {
		case "proceed":
			proceed = true
		case "stop":
			proceed = false
		default:
			return sessionclient.CommandResult{Handled: true, Notice: "usage: /handoff [proceed|stop]"}
		}
	}
	// Quick-fail guards identical to RunHandoffPipeline's, so a hopeless
	// handoff errors immediately instead of after a rotation attempt.
	if len(f.app.Conv) == 0 {
		return sessionclient.CommandResult{Handled: true, Notice: "handoff failed: nothing to hand off (empty conversation)"}
	}
	hasUser := false
	for _, m := range f.app.Conv {
		if m.Role == "user" {
			hasUser = true
			break
		}
	}
	if !hasUser {
		return sessionclient.CommandResult{Handled: true, Notice: "handoff failed: nothing to hand off (no user messages in conversation)"}
	}
	return sessionclient.CommandResult{
		Handled: true,
		Rotate: &sessionclient.RotateRequest{
			Type:    "handoff",
			Proceed: proceed,
		},
	}
}

// interpretAgentMsg's logic now lives on the facade (method) so Batch
// recursion can apply side effects through f — see (*wiringFacade).
// interpretAgentMsg above.

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
	sessionID := f.sessionID
	principal := f.principal
	f.mu.Unlock()

	// Close the host session FIRST when one exists: requestClose cancels the
	// in-flight turn ctx, which unblocks a Parked approval with a forced
	// decline (m4b review finding — without this, a rotation while an
	// approval was parked left the turn goroutine blocked forever: the TUI
	// had swapped facades, so nobody could answer the old session's approval)
	// and lets the executor emit TurnCompleted{cancelled} + SessionClosed.
	// The 10s bound covers a turn that ignores cancellation.
	if f.host != nil && sessionID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = f.host.CloseSession(ctx, principal, sessionID)
		cancel()
	}

	// Stop the event pump and DRAIN it: a non-blocking probe lets events be
	// delivered after Close returned (m4b review finding). The pump exits
	// once the subscription Next fails (Close'd above) or Stop wins; Done
	// is closed either way. Bounded wait so a wedged pump can't hang Close.
	if pump != nil {
		pump.Stop()
		select {
		case <-pump.Done():
		case <-time.After(5 * time.Second):
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

	// Persist the session transcript on close — belt-and-suspenders for state
	// mutated after the last turn's defer SaveSession (e.g. /label, /model,
	// repo-state). SaveSession is a no-op when app.Session is nil (no
	// conversation was started) or when Conv is empty (nothing to save).
	f.app.SaveSession()

	// Release the App ownership claim. The turn may still be winding down
	// from the CloseSession cancellation above (the executor goroutine clears
	// turnActive in its defer after finishTurn); retry briefly so a normal
	// rotation doesn't leak the appOwners entry on a lost race. After the
	// retries the App pointer stays claimed — harmless (rotation builds a
	// fresh App; the claim only prevents THIS App from being re-hosted), but
	// the map entry leaks, so log-worthy in principle. Best-effort.
	if f.handle != nil {
		deadline := time.Now().Add(5 * time.Second)
		for {
			err := f.handle.Release()
			if err == nil || time.Now().After(deadline) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
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
