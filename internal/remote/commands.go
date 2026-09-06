// commands.go: slash-command dispatch for the remote facade (P6c).

package remote

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	v1alpha1 "github.com/treeol/wakil/api/gen/wakil/v1alpha1"
	wakilv1alpha1connect "github.com/treeol/wakil/api/gen/wakil/v1alpha1/wakilv1alpha1connect"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionclient"
)

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
