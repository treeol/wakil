package connect

// session_state_handler.go: daemon-side handler for SessionStateService (P6a).
//
// The daemon owns the *agent.App. This handler translates SessionStateService
// RPCs into App method calls, exposing the session-scoped state over the wire
// that the TUI's RemoteFacade needs for Snapshot(), Consent(), Info(),
// CompletionSource(), and DispatchCommand.
//
// The handler holds a *agent.App pointer (the daemon builds it once at startup
// and passes it in). The App's own synchronization (atomic.Value for consent,
// convMu for Conv, etc.) makes reads safe. Mutations follow the same patterns
// as the embedded HandleTUICommand path (commands.go): direct field writes for
// fields that are safe under the single-App-single-turn constraint, plus
// saveRepoState for persistence.
//
// All RPCs resolve the principal from the request context via the same
// principalResolver used by every other handler. The session_id in each
// request is accepted but not validated against the App's session — the
// single-App daemon serves one active session at a time, and the host's
// session enforcement (claimSession in hostturn.go) already rejects turns
// for wrong sessions. State mutations (SetModel etc.) are safe regardless of
// which session ID the caller passes: they affect the shared App state, and
// the single-App constraint means only one session's turns are running.

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"connectrpc.com/connect"
	v1alpha1 "github.com/treeol/wakil/api/gen/wakil/v1alpha1"
	wakilv1alpha1connect "github.com/treeol/wakil/api/gen/wakil/v1alpha1/wakilv1alpha1connect"
	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
)

// SessionStateHandler implements SessionStateServiceHandler by calling into
// the daemon's *agent.App.
type SessionStateHandler struct {
	app      *agent.App
	resolver principalResolver

	// resetSessionBinding, when non-nil, is called before InitNewSession and
	// LoadSession to clear the hostTurn's session binding so the single App
	// can serve a new session after the previous one closes. The daemon path
	// sets this; the test path leaves it nil.
	resetSessionBinding func() error

	// restoreMu guards restoreDone: RestoreRepoState applies at most once per
	// session generation (not per App lifetime). The daemon serves multiple
	// sequential sessions on one App; each fresh conversation should get its
	// own repo-state restore. The guard is reset in InitNewSession and
	// LoadSession (via resetRestoreDone) so a subsequent fresh conversation
	// can restore again. The client drives the call (fresh boot only); the
	// guard prevents a duplicate/racing call within one session.
	restoreMu   sync.Mutex
	restoreDone bool
}

// Compile-time assertion.
var _ wakilv1alpha1connect.SessionStateServiceHandler = (*SessionStateHandler)(nil)

// NewSessionStateHandler creates a handler backed by the given App.
// The resolver is the principal resolver used by all handlers (same as
// SessionHandler, EventHandler, etc.).
func NewSessionStateHandler(app *agent.App, resolver principalResolver) *SessionStateHandler {
	return &SessionStateHandler{app: app, resolver: resolver}
}

// SetResetSessionBinding sets the callback used to clear the hostTurn's
// session binding before a new session is initialized. Used by the daemon
// path so the single agent.App can serve multiple sequential sessions across
// TUI reconnections.
func (h *SessionStateHandler) SetResetSessionBinding(fn func() error) {
	h.resetSessionBinding = fn
}

// resetRestoreDone clears the restore-done guard so a subsequent fresh
// conversation can trigger RestoreRepoState again. Called from InitNewSession
// and LoadSession — each new session generation gets its own restore chance.
func (h *SessionStateHandler) resetRestoreDone() {
	h.restoreMu.Lock()
	h.restoreDone = false
	h.restoreMu.Unlock()
}

// App returns the handler's bound *agent.App. Used by callers that need
// direct App access (e.g. the daemon's shutdown path, which already holds
// the App pointer directly).
func (h *SessionStateHandler) App() *agent.App { return h.app }

// ---- GetSessionState ----

func (h *SessionStateHandler) GetSessionState(ctx context.Context, req *connect.Request[v1alpha1.GetSessionStateRequest]) (*connect.Response[v1alpha1.SessionState], error) {
	if _, err := resolvePrincipal(ctx, h.resolver); err != nil {
		return nil, mapError(err)
	}
	app := h.app

	used, exact := app.ContextUsage()
	cl := app.ContextLimit()
	cs := app.Consent()

	state := &v1alpha1.SessionState{
		SessionId:           req.Msg.SessionId,
		ChatId:             appChatID(app),
		Title:              appSessionLabel(app),
		Workspace:          app.SessionWorkspace(),
		SelectedBackend:     app.SelectedBackend,
		SelectedModel:       app.SelectedModel,
		EffectiveModel:      app.EffectiveModel(),
		ModelList:           append([]string(nil), app.ModelList...),
		ConfigBackend:       app.Cfg.Backend,
		SubagentEndpoint:    app.SubagentEndpointOverride,
		SubagentModel:       app.SubagentModelOverride,
		EffectiveSubagentModel: app.EffectiveSubagentModel(),
		MaxParallelSubagents: int32(app.Cfg.MaxParallelSubagents),
		ContextUsed:         int32(used),
		ContextExact:        exact,
		EffectiveCtxMax:     int32(app.EffectiveCtxMaxCharsOverride),
		AutoApprove:         cs.AutoApprove,
		AllowDestructive:    cs.AllowDestructive,
		AllowReads:          cs.AllowReads,
		RawTools:            app.RawTools,
		CounselMode:         app.CounselMode,
		MaxCounsel:          int32(app.MaxCounsel),
		BaseUrl:             appClientBaseURL(app),
		LastBackend:         appClientLastBackend(app),
		Cwd:                 appExecCwd(app),
		ExecMode:            appExecDescribe(app),
		InfoPanelOpen:       app.InfoPanelOpen,
		ContextLimit: &v1alpha1.ContextLimit{
			NCtx:            int32(cl.NCtx),
			NCtxTrain:       int32(cl.NCtxTrain),
			Source:          cl.Source,
			ContextSource:   cl.ContextSource,
			UsableCtx:       int32(cl.UsableCtx),
			ReasoningBudget: int32(cl.ReasoningBudget),
			AnswerMargin:    int32(cl.AnswerMargin),
			ModelUnresolved:  cl.ModelUnresolved,
		},
	}
	if app.Workflow != nil {
		state.WorkflowLabel = app.Workflow.SidebarLabel()
	}
	state.BackendList = appBackendListToProto(app.BackendList)
	state.PromptNote = app.AgentPromptNote()

	return connect.NewResponse(state), nil
}

// ---- SetModel ----

func (h *SessionStateHandler) SetModel(ctx context.Context, req *connect.Request[v1alpha1.SetModelRequest]) (*connect.Response[v1alpha1.SetModelResponse], error) {
	if _, err := resolvePrincipal(ctx, h.resolver); err != nil {
		return nil, mapError(err)
	}
	app := h.app
	model := req.Msg.Model
	if model == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("model must not be empty"))
	}
	agent.ApplyModelOverride(app, model)
	app.SaveRepoState(func(s *agent.RepoState) {
		s.Model = model
		s.EndpointName = app.Cfg.EndpointName
	})
	return connect.NewResponse(&v1alpha1.SetModelResponse{
		Notice: "model: set to " + model,
	}), nil
}

// ---- SetBackend ----

func (h *SessionStateHandler) SetBackend(ctx context.Context, req *connect.Request[v1alpha1.SetBackendRequest]) (*connect.Response[v1alpha1.SetBackendResponse], error) {
	if _, err := resolvePrincipal(ctx, h.resolver); err != nil {
		return nil, mapError(err)
	}
	app := h.app
	arg := req.Msg.Backend
	if arg == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("backend must not be empty"))
	}

	// OpenAI-kind: /backend is the endpoint switcher.
	if app.Cfg.ActiveEndpoint().Kind == config.EndpointKindOpenAI {
		// Not supported over RPC — the endpoint switch involves reconfiguring
		// the client (kind/base_url/auth/sampling) which requires the full
		// handleEndpointSwitch path with its Batch Cmd return type.
		// Degrade: report that the endpoint switch is not available remotely.
		return connect.NewResponse(&v1alpha1.SetBackendResponse{
			Notice: "backend switching is not available remotely for openai-kind endpoints — use /backend in the TUI directly",
		}), nil
	}

	// ilm-proxy kind: parse "<name>" or "<name>/<model-path>".
	if idx := strings.Index(arg, "/"); idx >= 0 {
		app.SelectedBackend = arg[:idx]
		app.SelectedModel = arg
	} else {
		app.SelectedBackend = arg
		app.SelectedModel = ""
	}
	selected := app.SelectedBackend
	notice := "backend: set to " + selected
	if app.SelectedModel != "" {
		notice += " · model: " + app.SelectedModel
	}
	app.SaveRepoState(func(s *agent.RepoState) {
		s.Backend = app.SelectedBackend
		if app.SelectedModel != "" {
			s.Model = app.SelectedModel
		}
		s.EndpointName = app.Cfg.EndpointName
	})
	return connect.NewResponse(&v1alpha1.SetBackendResponse{
		Notice: notice,
	}), nil
}

// ---- SetAutoApprove ----

func (h *SessionStateHandler) SetAutoApprove(ctx context.Context, req *connect.Request[v1alpha1.SetAutoApproveRequest]) (*connect.Response[v1alpha1.SetAutoApproveResponse], error) {
	if _, err := resolvePrincipal(ctx, h.resolver); err != nil {
		return nil, mapError(err)
	}
	app := h.app
	v := req.Msg.Value
	app.SaveRepoState(func(s *agent.RepoState) { s.AutoApprove = v })
	if v {
		app.SetAutoApprove(true)
		return connect.NewResponse(&v1alpha1.SetAutoApproveResponse{
			Notice: "auto mode: ON — tool calls approved without prompting\n  still confirmed: destructive shell commands (opt in with /auto destructive), external-backend egress",
		}), nil
	}
	app.RevokeAuto()
	return connect.NewResponse(&v1alpha1.SetAutoApproveResponse{
		Notice: "auto mode: OFF — tool calls require confirmation",
	}), nil
}

// ---- SetAllowDestructive ----

func (h *SessionStateHandler) SetAllowDestructive(ctx context.Context, req *connect.Request[v1alpha1.SetAllowDestructiveRequest]) (*connect.Response[v1alpha1.SetAllowDestructiveResponse], error) {
	if _, err := resolvePrincipal(ctx, h.resolver); err != nil {
		return nil, mapError(err)
	}
	app := h.app
	v := req.Msg.Value
	if v {
		// Enable: use the atomic conditional mutator to prevent the
		// check-then-act race where a concurrent RevokeAuto between the
		// AutoApprove check and the SetAllowDestructive call could produce
		// AutoApprove=false + AllowDestructive=true.
		if !app.EnableDestructiveIfAuto() {
			return connect.NewResponse(&v1alpha1.SetAllowDestructiveResponse{
				Notice: "auto mode is OFF — enable /auto first, then /auto destructive",
			}), nil
		}
		return connect.NewResponse(&v1alpha1.SetAllowDestructiveResponse{
			Notice: "⚠ destructive auto-approve: ON — rm, mv, git reset, … run without prompting\n  still confirmed: external-backend egress; /auto destructive again to revoke",
		}), nil
	}
	app.SetAllowDestructive(false)
	return connect.NewResponse(&v1alpha1.SetAllowDestructiveResponse{
		Notice: "destructive auto-approve: OFF — destructive commands require confirmation again",
	}), nil
}

// ---- RevokeAuto ----

func (h *SessionStateHandler) RevokeAuto(ctx context.Context, req *connect.Request[v1alpha1.RevokeAutoRequest]) (*connect.Response[v1alpha1.RevokeAutoResponse], error) {
	if _, err := resolvePrincipal(ctx, h.resolver); err != nil {
		return nil, mapError(err)
	}
	app := h.app
	// Persist the OFF toggle so /auto never silently resurrects on a later
	// fresh-conversation restore (parity with SaveRepoState in SetAutoApprove).
	app.SaveRepoState(func(s *agent.RepoState) { s.AutoApprove = false })
	app.RevokeAuto()
	return connect.NewResponse(&v1alpha1.RevokeAutoResponse{
		Notice: "auto mode: OFF — tool calls require confirmation",
	}), nil
}

// ---- SetSubagentEndpoint ----

func (h *SessionStateHandler) SetSubagentEndpoint(ctx context.Context, req *connect.Request[v1alpha1.SetSubagentEndpointRequest]) (*connect.Response[v1alpha1.SetSubagentEndpointResponse], error) {
	if _, err := resolvePrincipal(ctx, h.resolver); err != nil {
		return nil, mapError(err)
	}
	app := h.app
	name := req.Msg.Endpoint
	if name == "" || name == "inherit" {
		app.SubagentEndpointOverride = ""
		app.SaveRepoState(func(s *agent.RepoState) { s.SubagentEndpoint = "" })
		return connect.NewResponse(&v1alpha1.SetSubagentEndpointResponse{
			Notice: "subagent endpoint: inherit (parent endpoint)",
		}), nil
	}
	if _, err := app.Cfg.NormalizeEndpoint(name); err != nil {
		return connect.NewResponse(&v1alpha1.SetSubagentEndpointResponse{
			Notice: fmt.Sprintf("subagent endpoint %q: %v — not set", name, err),
		}), nil
	}
	app.SubagentEndpointOverride = name
	app.SaveRepoState(func(s *agent.RepoState) { s.SubagentEndpoint = name })
	return connect.NewResponse(&v1alpha1.SetSubagentEndpointResponse{
		Notice: "subagent endpoint: set to " + name,
	}), nil
}

// ---- SetSubagentModel ----

func (h *SessionStateHandler) SetSubagentModel(ctx context.Context, req *connect.Request[v1alpha1.SetSubagentModelRequest]) (*connect.Response[v1alpha1.SetSubagentModelResponse], error) {
	if _, err := resolvePrincipal(ctx, h.resolver); err != nil {
		return nil, mapError(err)
	}
	app := h.app
	name := req.Msg.Model
	if name == "" || name == "inherit" {
		app.SubagentModelOverride = ""
		app.SaveRepoState(func(s *agent.RepoState) { s.SubagentModel = "" })
		return connect.NewResponse(&v1alpha1.SetSubagentModelResponse{
			Notice: "subagent model: inherit (endpoint model)",
		}), nil
	}
	app.SubagentModelOverride = name
	app.SaveRepoState(func(s *agent.RepoState) { s.SubagentModel = name })
	return connect.NewResponse(&v1alpha1.SetSubagentModelResponse{
		Notice: "subagent model: set to " + name,
	}), nil
}

// ---- SetMaxParallelSubagents ----

func (h *SessionStateHandler) SetMaxParallelSubagents(ctx context.Context, req *connect.Request[v1alpha1.SetMaxParallelSubagentsRequest]) (*connect.Response[v1alpha1.SetMaxParallelSubagentsResponse], error) {
	if _, err := resolvePrincipal(ctx, h.resolver); err != nil {
		return nil, mapError(err)
	}
	app := h.app
	n := int(req.Msg.Value)
	if n < 1 {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("maxpar: must be a positive integer (1 = sequential)"))
	}
	const maxParCap = 64
	if n > maxParCap {
		n = maxParCap
	}
	app.Cfg.MaxParallelSubagents = n
	app.SaveRepoState(func(s *agent.RepoState) { s.MaxParallelSubagents = n })
	notice := fmt.Sprintf("max parallel subagents: set to %d", n)
	if n == maxParCap {
		notice = fmt.Sprintf("max parallel subagents: set to %d (capped at %d)", n, maxParCap)
	}
	return connect.NewResponse(&v1alpha1.SetMaxParallelSubagentsResponse{
		Notice: notice,
	}), nil
}

// ---- SetEffectiveCtxMax ----

func (h *SessionStateHandler) SetEffectiveCtxMax(ctx context.Context, req *connect.Request[v1alpha1.SetEffectiveCtxMaxRequest]) (*connect.Response[v1alpha1.SetEffectiveCtxMaxResponse], error) {
	if _, err := resolvePrincipal(ctx, h.resolver); err != nil {
		return nil, mapError(err)
	}
	app := h.app
	n := int(req.Msg.Value)
	if n < 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("maxctx: must be a non-negative integer (0 = disabled)"))
	}
	app.EffectiveCtxMaxCharsOverride = n
	nVal := n
	app.SaveRepoState(func(s *agent.RepoState) { s.EffectiveCtxMaxChars = &nVal })
	if n == 0 {
		return connect.NewResponse(&v1alpha1.SetEffectiveCtxMaxResponse{
			Notice: "effective context cap: disabled (using full model context)",
		}), nil
	}
	return connect.NewResponse(&v1alpha1.SetEffectiveCtxMaxResponse{
		Notice: fmt.Sprintf("effective context cap: %d chars", n),
	}), nil
}

// ---- SetRawTools ----

func (h *SessionStateHandler) SetRawTools(ctx context.Context, req *connect.Request[v1alpha1.SetRawToolsRequest]) (*connect.Response[v1alpha1.SetRawToolsResponse], error) {
	if _, err := resolvePrincipal(ctx, h.resolver); err != nil {
		return nil, mapError(err)
	}
	app := h.app
	v := req.Msg.Value
	app.RawTools = v
	app.SaveRepoState(func(s *agent.RepoState) { s.RawTools = v })
	var notice string
	if v {
		notice = "raw tool results: ON — full output kept in context (cap disabled)"
	} else {
		cap := app.Cfg.ToolResultCap
		if cap <= 0 {
			notice = "raw tool results: OFF — cap is set to unlimited in config"
		} else {
			notice = fmt.Sprintf("raw tool results: OFF — results capped at %d chars", cap)
		}
	}
	return connect.NewResponse(&v1alpha1.SetRawToolsResponse{
		Notice: notice,
	}), nil
}

// ---- SetCounselMode ----

func (h *SessionStateHandler) SetCounselMode(ctx context.Context, req *connect.Request[v1alpha1.SetCounselModeRequest]) (*connect.Response[v1alpha1.SetCounselModeResponse], error) {
	if _, err := resolvePrincipal(ctx, h.resolver); err != nil {
		return nil, mapError(err)
	}
	app := h.app
	mode := req.Msg.Mode
	switch mode {
	case "auto":
		cap := app.MaxCounsel
		if cap <= 0 {
			cap = app.Cfg.CounselMaxPerSession
			if cap <= 0 {
				cap = 3
			}
		}
		app.CounselMode = "auto"
		app.MaxCounsel = cap
		app.SaveRepoState(func(s *agent.RepoState) {
			s.CounselMode = "auto"
			s.MaxCounsel = cap
		})
		return connect.NewResponse(&v1alpha1.SetCounselModeResponse{
			Notice: fmt.Sprintf("counsel mode: auto (cap: %d/turn)", cap),
		}), nil
	case "suggest":
		app.CounselMode = "suggest"
		app.SaveRepoState(func(s *agent.RepoState) { s.CounselMode = "suggest" })
		return connect.NewResponse(&v1alpha1.SetCounselModeResponse{
			Notice: "counsel mode: suggest (hint only, no auto-fire)",
		}), nil
	case "off":
		app.CounselMode = "off"
		app.SaveRepoState(func(s *agent.RepoState) { s.CounselMode = "off" })
		return connect.NewResponse(&v1alpha1.SetCounselModeResponse{
			Notice: "counsel mode: off (struggle detected silently)",
		}), nil
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("usage: /counsel auto|suggest|off"))
	}
}

// ---- Compact ----

func (h *SessionStateHandler) Compact(ctx context.Context, req *connect.Request[v1alpha1.CompactRequest]) (*connect.Response[v1alpha1.CompactResponse], error) {
	if _, err := resolvePrincipal(ctx, h.resolver); err != nil {
		return nil, mapError(err)
	}
	app := h.app
	if app.Session == nil {
		return connect.NewResponse(&v1alpha1.CompactResponse{
			Compacted: false,
			Notice:    "no active session",
		}), nil
	}
	ok, err := app.Compact(ctx, app.SummarizeFn(), true)
	if err != nil {
		return connect.NewResponse(&v1alpha1.CompactResponse{
			Compacted: false,
			Notice:    "compact: " + err.Error(),
		}), nil
	}
	if !ok {
		return connect.NewResponse(&v1alpha1.CompactResponse{
			Compacted: false,
			Notice:    "nothing to compact (transcript fits within keep_bytes window)",
		}), nil
	}
	app.SaveSession()
	return connect.NewResponse(&v1alpha1.CompactResponse{
		Compacted: true,
		Notice:    "context compacted",
	}), nil
}

// ---- SaveRepoState ----

func (h *SessionStateHandler) SaveRepoState(ctx context.Context, req *connect.Request[v1alpha1.SaveRepoStateRequest]) (*connect.Response[v1alpha1.SaveRepoStateResponse], error) {
	if _, err := resolvePrincipal(ctx, h.resolver); err != nil {
		return nil, mapError(err)
	}
	app := h.app
	if req.Msg.Clear {
		if err := agent.ClearRepoState(app); err != nil {
			return connect.NewResponse(&v1alpha1.SaveRepoStateResponse{
				Notice: "repostate: clear failed: " + err.Error(),
			}), nil
		}
		return connect.NewResponse(&v1alpha1.SaveRepoStateResponse{
			Notice: "repostate: cleared for " + app.SessionWorkspace() +
				" (this session's current values are unchanged)",
		}), nil
	}
	return connect.NewResponse(&v1alpha1.SaveRepoStateResponse{
		Notice: agent.DescribeRepoState(app),
	}), nil
}

// ---- SetSessionLabel ----

func (h *SessionStateHandler) SetSessionLabel(ctx context.Context, req *connect.Request[v1alpha1.SetSessionLabelRequest]) (*connect.Response[v1alpha1.SetSessionLabelResponse], error) {
	if _, err := resolvePrincipal(ctx, h.resolver); err != nil {
		return nil, mapError(err)
	}
	app := h.app
	label := strings.Trim(req.Msg.Label, `"'"`)
	if app.Session == nil {
		return connect.NewResponse(&v1alpha1.SetSessionLabelResponse{
			Notice: "no active session",
		}), nil
	}
	app.Session.Label = label
	app.SaveSession()
	return connect.NewResponse(&v1alpha1.SetSessionLabelResponse{
		Notice: "session labeled: " + label,
	}), nil
}

// ---- RestoreRepoState (P6d) ----

func (h *SessionStateHandler) RestoreRepoState(ctx context.Context, req *connect.Request[v1alpha1.RestoreRepoStateRequest]) (*connect.Response[v1alpha1.RestoreRepoStateResponse], error) {
	if _, err := resolvePrincipal(ctx, h.resolver); err != nil {
		return nil, mapError(err)
	}
	app := h.app

	// One-shot per App lifetime (guard + idempotence, not the trigger): the
	// single-App daemon cannot itself distinguish fresh-boot from resume, so the
	// remote TUI drives this call on a fresh conversation only. This guard makes
	// a duplicate/racing call a no-op that still reports the live state safely.
	h.restoreMu.Lock()
	if h.restoreDone {
		h.restoreMu.Unlock()
		return connect.NewResponse(&v1alpha1.RestoreRepoStateResponse{}), nil
	}
	h.restoreDone = true
	h.restoreMu.Unlock()

	result := agent.RestoreRepoState(app)

	// Re-resolve context limits server-side (the daemon owns app.Client.HTTP and
	// app.Cfg; the remote TUI does not). Uses the literal restored strings, not
	// post-restore App fields, mirroring BootstrapTUI's convention exactly.
	if (result.Model != "" || result.Backend != "") && app.Client != nil && app.Client.HTTP != nil {
		app.CtxLimit = agent.ResolveContextLimitForBackendModel(ctx, app.Client.HTTP, app.Cfg, result.Backend, result.Model, io.Discard)
	}

	return connect.NewResponse(&v1alpha1.RestoreRepoStateResponse{
		Notice: result.Note,
	}), nil
}

// ---- RestoreRepoStateResume ----

func (h *SessionStateHandler) RestoreRepoStateResume(ctx context.Context, req *connect.Request[v1alpha1.RestoreRepoStateResumeRequest]) (*connect.Response[v1alpha1.RestoreRepoStateResumeResponse], error) {
	if _, err := resolvePrincipal(ctx, h.resolver); err != nil {
		return nil, mapError(err)
	}
	app := h.app

	// Restore endpoint-independent settings only (no model/backend changes).
	// This is called by the remote TUI after LoadSession (resume) to restore
	// AutoApprove, RawTools, maxpar, maxctx, subagent, mashura settings
	// without changing the model/backend mid-transcript.
	result := agent.RestoreRepoStateResume(app)

	return connect.NewResponse(&v1alpha1.RestoreRepoStateResumeResponse{
		Notice: result.Note,
	}), nil
}

// ---- LoadSession (P6f fix: session resume) ----

func (h *SessionStateHandler) LoadSession(ctx context.Context, req *connect.Request[v1alpha1.LoadSessionRequest]) (*connect.Response[v1alpha1.LoadSessionResponse], error) {
	if _, err := resolvePrincipal(ctx, h.resolver); err != nil {
		return nil, mapError(err)
	}
	// Clear the hostTurn's session binding so the single App can serve this
	// resumed session (the previous session's binding would reject it).
	if h.resetSessionBinding != nil {
		if err := h.resetSessionBinding(); err != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("load_session: cannot reset session binding: %w", err))
		}
	}
	// Reset the restore-done guard so a subsequent fresh conversation after
	// this resume can trigger RestoreRepoState again.
	h.resetRestoreDone()
	app := h.app

	// Reset ephemeral consent grants before restoring the session, so a
	// previous session's /auto or /auto destructive grant does not leak into
	// the resumed one. The workspace-level AutoApprove preference is restored
	// separately (RestoreRepoState, driven by the client on fresh boots).
	app.RevokeAuto()
	app.SetAllowReads(false)

	// Load the session from disk (same path as embedded ResumeConversation).
	idOrPrefix := req.Msg.IdOrPrefix
	if idOrPrefix == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("load_session: id_or_prefix must not be empty"))
	}
	s, err := agent.LoadSessionScoped(idOrPrefix, agent.SessionScope{All: true})
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("load_session: %w", err))
	}
	if s == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("load_session: session %q not found", idOrPrefix))
	}

	// Restore the loaded session's state into the App — mirroring the embedded
	// ResumeConversation path (conversation_manager.go:172-176).
	if app.Client != nil {
		app.Client.ChatID = s.ChatID
	}
	app.SetConv(s.Conv)
	app.Session = s
	app.SetWorkflow(s.SavedWorkflow)

	// Build the display-only conv projection for the TUI.
	conv := make([]*v1alpha1.ConvMessage, 0, len(s.Conv))
	for _, m := range s.Conv {
		cm := &v1alpha1.ConvMessage{Role: m.Role, Name: m.Name}
		if m.Content != nil {
			cm.Content = m.Content
		}
		conv = append(conv, cm)
	}

	resp := &v1alpha1.LoadSessionResponse{
		ChatId:  s.ChatID,
		Title:   s.Label,
		Conv:    conv,
	}
	if s.SavedWorkflow != nil {
		resp.WorkflowLabel = s.SavedWorkflow.PhaseName()
	}
	return connect.NewResponse(resp), nil
}

// ---- ListSavedSessions ----

func (h *SessionStateHandler) ListSavedSessions(ctx context.Context, req *connect.Request[v1alpha1.ListSavedSessionsRequest]) (*connect.Response[v1alpha1.ListSavedSessionsResponse], error) {
	if _, err := resolvePrincipal(ctx, h.resolver); err != nil {
		return nil, mapError(err)
	}
	scope := agent.SessionScope{
		Workspace: req.Msg.Workspace,
		All:       req.Msg.All,
	}
	sessions, hidden, err := agent.ListSessionsScoped(scope)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list_saved_sessions: %w", err))
	}
	out := make([]*v1alpha1.SavedSession, 0, len(sessions))
	for _, s := range sessions {
		turns, first := agent.SessionTurns(s)
		if len(first) > 40 {
			first = first[:40] + "…"
		}
		out = append(out, &v1alpha1.SavedSession{
			ChatId:       s.ChatID,
			Model:        s.Model,
			Label:        s.Label,
			Workspace:   s.Workspace,
			Created:      s.Created.Unix(),
			Updated:      s.Updated.Unix(),
			Turns:        int32(turns),
			FirstMessage: first,
		})
	}
	return connect.NewResponse(&v1alpha1.ListSavedSessionsResponse{
		Sessions: out,
		Hidden:   int32(hidden),
	}), nil
}

// ---- InitNewSession ----

func (h *SessionStateHandler) InitNewSession(ctx context.Context, req *connect.Request[v1alpha1.InitNewSessionRequest]) (*connect.Response[v1alpha1.InitNewSessionResponse], error) {
	if _, err := resolvePrincipal(ctx, h.resolver); err != nil {
		return nil, mapError(err)
	}
	// Clear the hostTurn's session binding so the single App can serve this
	// new session (the previous session's binding would reject the new one).
	if h.resetSessionBinding != nil {
		if err := h.resetSessionBinding(); err != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("init_new_session: cannot reset session binding: %w", err))
		}
	}
	// Reset the restore-done guard so this fresh conversation can trigger
	// RestoreRepoState (the client calls it after InitNewSession on /new).
	h.resetRestoreDone()
	app := h.app
	chatID := agent.NewChatID()
	app.NewConversation(chatID)
	return connect.NewResponse(&v1alpha1.InitNewSessionResponse{
		ChatId: chatID,
	}), nil
}

// ---- Helpers ----
// These wrap App field reads that may be nil (Client, Exec, Session) in
// early-init or test paths. They are the same nil-safety the wiringFacade
// applies, but concentrated here so the handler never panics.

func appChatID(app *agent.App) string {
	if app.Session != nil && app.Session.ChatID != "" {
		return app.Session.ChatID
	}
	if app.Client != nil {
		return app.Client.ChatID
	}
	return ""
}

func appSessionLabel(app *agent.App) string {
	if app.Session != nil {
		return app.Session.Label
	}
	return ""
}

func appClientBaseURL(app *agent.App) string {
	if app.Client != nil {
		return app.Client.BaseURL
	}
	return ""
}

func appClientLastBackend(app *agent.App) string {
	if app.Client != nil {
		return app.Client.LastUsedBackend()
	}
	return ""
}

func appExecCwd(app *agent.App) string {
	if app.Exec != nil {
		return app.Exec.Cwd()
	}
	return ""
}

func appExecDescribe(app *agent.App) string {
	if app.Exec != nil {
		return app.Exec.Describe()
	}
	return ""
}

func appBackendListToProto(list []agent.BackendInfo) []*v1alpha1.BackendInfo {
	if len(list) == 0 {
		return nil
	}
	out := make([]*v1alpha1.BackendInfo, 0, len(list))
	for _, b := range list {
		out = append(out, &v1alpha1.BackendInfo{
			Name:     b.Name,
			External: b.External,
			Caps:     append([]string(nil), b.Caps...),
		})
	}
	return out
}
