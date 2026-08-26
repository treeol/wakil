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
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"connectrpc.com/connect"
	v1alpha1 "github.com/treeol/wakil/api/gen/wakil/v1alpha1"
	wakilv1alpha1connect "github.com/treeol/wakil/api/gen/wakil/v1alpha1/wakilv1alpha1connect"
	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/wiring"
)

// SessionStateHandler implements SessionStateServiceHandler by calling into
// the daemon's *agent.App.
type SessionStateHandler struct {
	app      *agent.App
	resolver principalResolver

	// coordinator serializes session transitions (LoadSession,
	// InitNewSession) against turn starts and idle maintenance (Compact).
	// When nil, transitions run without coordination (test path).
	coordinator *wiring.TransitionCoordinator

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

// SetCoordinator sets the transition coordinator that serializes session
// transitions (LoadSession, InitNewSession, Compact) against turn starts.
// Used by the daemon path; the test path leaves it nil.
func (h *SessionStateHandler) SetCoordinator(c *wiring.TransitionCoordinator) {
	h.coordinator = c
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

	// One coherent snapshot under stateMu.RLock — replaces ~40 individual
	// direct field reads that could race with RPC setters and turn goroutine
	// writes. Derived values (ContextUsage, ContextLimit, Exec, Client) are
	// computed outside the lock by SnapshotSessionState using stable copies.
	snap := app.SnapshotSessionState()
	cs := app.Consent()

	state := &v1alpha1.SessionState{
		SessionId:           req.Msg.SessionId,
		ChatId:               snap.ChatID,
		Title:                snap.SessionLabel,
		Workspace:            snap.Workspace,
		SelectedBackend:      snap.SelectedBackend,
		SelectedModel:        snap.SelectedModel,
		EffectiveModel:       snap.EffectiveModel,
		ModelList:            append([]string(nil), snap.ModelList...),
		ConfigBackend:        snap.ConfigBackend,
		SubagentEndpoint:     snap.SubagentEndpoint,
		SubagentModel:        snap.SubagentModel,
		EffectiveSubagentModel: snap.EffectiveSubagentModel,
		MaxParallelSubagents: int32(snap.MaxParallel),
		ContextUsed:          int32(snap.ContextUsed),
		ContextExact:         snap.ContextExact,
		EffectiveCtxMax:      int32(snap.CtxMaxCharsOverride),
		AutoApprove:          cs.AutoApprove,
		AllowDestructive:     cs.AllowDestructive,
		AllowReads:           cs.AllowReads,
		RawTools:             snap.RawTools,
		CounselMode:          snap.CounselMode,
		MaxCounsel:           int32(snap.MaxCounsel),
		BaseUrl:              snap.BaseURL,
		LastBackend:          snap.LastBackend,
		Cwd:                  snap.Cwd,
		ExecMode:             snap.ExecMode,
		InfoPanelOpen:        snap.InfoPanelOpen,
		ContextLimit: &v1alpha1.ContextLimit{
			NCtx:            int32(snap.CtxLimit.NCtx),
			NCtxTrain:       int32(snap.CtxLimit.NCtxTrain),
			Source:          snap.CtxLimit.Source,
			ContextSource:   snap.CtxLimit.ContextSource,
			UsableCtx:       int32(snap.CtxLimit.UsableCtx),
			ReasoningBudget: int32(snap.CtxLimit.ReasoningBudget),
			AnswerMargin:    int32(snap.CtxLimit.AnswerMargin),
			ModelUnresolved:  snap.CtxLimit.ModelUnresolved,
		},
	}
	state.WorkflowLabel = snap.WorkflowLabel
	state.BackendList = appBackendListToProto(snap.BackendList)
	state.PromptNote = snap.PromptNote

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

	// Route through the coordinator whenever it is present. This avoids a
	// TOCTOU race: checking kind outside the lock and then having
	// SetModelOverride check it again inside stateMu could disagree if an
	// endpoint switch happens concurrently. For OpenAI kind, the coordinator
	// rejects the change during an active turn (Client.Model is read by the
	// streaming turn). For ilm-proxy kind, WithTransition is still safe —
	// it acquires the lock, runs SetModelOverride (which only writes
	// SelectedModel), and releases. The turn goroutine snapshots
	// SelectedModel at prepareTurn time, so a mid-turn change is harmless.
	setModel := func() error {
		app.SetModelOverride(model)
		return nil
	}
	if h.coordinator != nil {
		if err := h.coordinator.WithTransition(setModel); err != nil {
			if errors.Is(err, wiring.ErrTurnActiveCoord) {
				return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("model: cannot switch model during an active turn: %w", err))
			}
			return nil, mapError(err)
		}
	} else {
		// Test path: no coordinator — apply directly.
		_ = setModel()
	}

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
	// SetBackendSelection handles the split and writes under stateMu.Lock.
	app.SetBackendSelection(arg)
	selected := app.SelectedBackendLocked()
	selectedModel := app.SelectedModelLocked()
	notice := "backend: set to " + selected
	if selectedModel != "" {
		notice += " · model: " + selectedModel
	}
	app.SaveRepoState(func(s *agent.RepoState) {
		s.Backend = selected
		if selectedModel != "" {
			s.Model = selectedModel
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
		app.SetSubagentEndpointOverride("")
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
	app.SetSubagentEndpointOverride(name)
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
		app.SetSubagentModelOverride("")
		app.SaveRepoState(func(s *agent.RepoState) { s.SubagentModel = "" })
		return connect.NewResponse(&v1alpha1.SetSubagentModelResponse{
			Notice: "subagent model: inherit (endpoint model)",
		}), nil
	}
	app.SetSubagentModelOverride(name)
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
	app.SetMaxParallel(n)
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
	app.SetMaxCtxOverride(n)
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
	app.SetRawToolsValue(v)
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
		cap := app.MaxCounselLocked()
		if cap <= 0 {
			cap = app.Cfg.CounselMaxPerSession
			if cap <= 0 {
				cap = 3
			}
		}
		app.SetCounselModeValue("auto", cap)
		app.SaveRepoState(func(s *agent.RepoState) {
			s.CounselMode = "auto"
			s.MaxCounsel = cap
		})
		return connect.NewResponse(&v1alpha1.SetCounselModeResponse{
			Notice: fmt.Sprintf("counsel mode: auto (cap: %d/turn)", cap),
		}), nil
	case "suggest":
		app.SetCounselModeValue("suggest", 0)
		app.SaveRepoState(func(s *agent.RepoState) { s.CounselMode = "suggest" })
		return connect.NewResponse(&v1alpha1.SetCounselModeResponse{
			Notice: "counsel mode: suggest (hint only, no auto-fire)",
		}), nil
	case "off":
		app.SetCounselModeValue("off", 0)
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

	// Run Compact inside the coordinator lock so it doesn't race with an
	// in-flight turn (convMu writes) or a session transition. The Session
	// nil-check, Compact call, and SaveSession are all inside the
	// coordinated block so the session can't change between check and use.
	var resp *connect.Response[v1alpha1.CompactResponse]
	maint := func() error {
		if app.Session == nil {
			resp = connect.NewResponse(&v1alpha1.CompactResponse{
				Compacted: false,
				Notice:    "no active session",
			})
			return nil
		}
		ok, err := app.Compact(ctx, app.SummarizeFn(), true)
		if err != nil {
			resp = connect.NewResponse(&v1alpha1.CompactResponse{
				Compacted: false,
				Notice:    "compact: " + err.Error(),
			})
			return nil
		}
		if !ok {
			resp = connect.NewResponse(&v1alpha1.CompactResponse{
				Compacted: false,
				Notice:    "nothing to compact (transcript fits within keep_bytes window)",
			})
			return nil
		}
		app.SaveSession()
		resp = connect.NewResponse(&v1alpha1.CompactResponse{
			Compacted: true,
			Notice:    "context compacted",
		})
		return nil
	}
	if h.coordinator != nil {
		if err := h.coordinator.WithIdleMaintenance(maint); err != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("compact: %w", err))
		}
	} else {
		_ = maint()
	}

	return resp, nil
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
	app.SetSessionLabelValue(label)
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

	// One-shot per session generation (guard + idempotence, not the
	// trigger): the single-App daemon serves multiple sequential sessions;
	// each fresh conversation gets its own restore. The guard is reset in
	// InitNewSession and LoadSession (via resetRestoreDone) so a subsequent
	// fresh conversation can restore again. The client drives the call
	// (fresh boot only); the guard prevents a duplicate/racing call within
	// one session.
	h.restoreMu.Lock()
	if h.restoreDone {
		h.restoreMu.Unlock()
		return connect.NewResponse(&v1alpha1.RestoreRepoStateResponse{}), nil
	}

	// Phase 1: Load from disk (no lock).
	st, err := agent.RestoreRepoStateRead(app)
	if err != nil || st == nil {
		// Missing/unreadable file counts as done — there's nothing to apply.
		h.restoreDone = true
		h.restoreMu.Unlock()
		return connect.NewResponse(&v1alpha1.RestoreRepoStateResponse{}), nil
	}
	h.restoreDone = true
	h.restoreMu.Unlock()

	// Phase 2: Apply under lock. Returns literal model/backend strings that
	// were actually applied (skipped by eligibility guards don't appear).
	result := agent.RestoreRepoStateApply(app, st)

	// Phase 3: Network probe (no lock). Re-resolve context limits server-side
	// using the literal strings Apply actually used — not raw RepoState values
	// that may have been skipped. The daemon owns app.Client.HTTP and app.Cfg;
	// the remote TUI does not.
	if (result.Model != "" || result.Backend != "") && app.Client != nil && app.Client.HTTP != nil {
		cl := agent.ResolveContextLimitForBackendModel(ctx, app.Client.HTTP, app.Cfg, result.Backend, result.Model, io.Discard)
		app.SetCtxLimit(cl)
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
	//
	// Split: Read (I/O) → Apply (lock). No network probe needed (no
	// model/backend change).
	st, err := agent.RestoreRepoStateRead(app)
	if err != nil || st == nil {
		return connect.NewResponse(&v1alpha1.RestoreRepoStateResumeResponse{}), nil
	}
	result := agent.RestoreRepoStateResumeApply(app, st)

	return connect.NewResponse(&v1alpha1.RestoreRepoStateResumeResponse{
		Notice: result.Note,
	}), nil
}

// ---- LoadSession (P6f fix: session resume) ----

func (h *SessionStateHandler) LoadSession(ctx context.Context, req *connect.Request[v1alpha1.LoadSessionRequest]) (*connect.Response[v1alpha1.LoadSessionResponse], error) {
	if _, err := resolvePrincipal(ctx, h.resolver); err != nil {
		return nil, mapError(err)
	}
	// Load and validate the target session BEFORE mutating any state, so a
	// failed load (not found, corrupt) leaves the active session untouched.
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

	// ── Session transition: all mutations below run inside the coordinator
	// lock so no turn can interleave. ───────────────────────────────────────

	transition := func() error {
		// Clear the hostTurn's session binding so the single App can serve this
		// resumed session (the previous session's binding would reject it).
		if h.resetSessionBinding != nil {
			if err := h.resetSessionBinding(); err != nil {
				return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("load_session: cannot reset session binding: %w", err))
			}
		}
		// Reset the restore-done guard so a subsequent fresh conversation after
		// this resume can trigger RestoreRepoState again.
		h.resetRestoreDone()

		// Atomically install the loaded session into the App under stateMu +
		// convMu. This replaces the individual field writes (Client.ChatID,
		// SetConv, Session, SetWorkflow) with a single locked method.
		h.app.InstallSession(s)
		return nil
	}

	if h.coordinator != nil {
		if err := h.coordinator.WithTransition(transition); err != nil {
			if ce, ok := err.(*connect.Error); ok {
				return nil, ce
			}
			if errors.Is(err, wiring.ErrTurnActiveCoord) {
				return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("load_session: %w", err))
			}
			return nil, mapError(err)
		}
	} else {
		if err := transition(); err != nil {
			if ce, ok := err.(*connect.Error); ok {
				return nil, ce
			}
			if errors.Is(err, wiring.ErrTurnActiveCoord) {
				return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("load_session: %w", err))
			}
			return nil, mapError(err)
		}
	}

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
	chatID := agent.NewChatID()

	transition := func() error {
		// Clear the hostTurn's session binding so the single App can serve this
		// new session (the previous session's binding would reject the new one).
		if h.resetSessionBinding != nil {
			if err := h.resetSessionBinding(); err != nil {
				return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("init_new_session: cannot reset session binding: %w", err))
			}
		}
		// Reset the restore-done guard so this fresh conversation can trigger
		// RestoreRepoState (the client calls it after InitNewSession on /new).
		h.resetRestoreDone()
		// Atomically start a fresh session under stateMu.Lock.
		h.app.NewConversationTransition(chatID)
		return nil
	}

	if h.coordinator != nil {
		if err := h.coordinator.WithTransition(transition); err != nil {
			if ce, ok := err.(*connect.Error); ok {
				return nil, ce
			}
			if errors.Is(err, wiring.ErrTurnActiveCoord) {
				return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("init_new_session: %w", err))
			}
			return nil, mapError(err)
		}
	} else {
		if err := transition(); err != nil {
			if ce, ok := err.(*connect.Error); ok {
				return nil, ce
			}
			if errors.Is(err, wiring.ErrTurnActiveCoord) {
				return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("init_new_session: %w", err))
			}
			return nil, mapError(err)
		}
	}

	return connect.NewResponse(&v1alpha1.InitNewSessionResponse{
		ChatId: chatID,
	}), nil
}

// ---- Helpers ----

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
