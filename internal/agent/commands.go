package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/tools"
	"github.com/treeol/wakil/internal/workflow"
)

// tuiConfirmer pauses the agent goroutine and posts a ConfirmReqMsg into the
// TUI event loop. It blocks on the response channel until the user answers.
// Picking "allow all reads" flips app.AllowReads so later read-only commands
// skip the gate. Safe: runs in the agent goroutine, not in the event loop.
// suspendAuto returns a human-readable reason string when auto mode must be
// suspended for this tool call and the interactive gate must fire instead.
// Returns "" when auto mode may proceed without gating.
//
// Every carve-out routes through here so no fall-through can occur without
// a reason, making all auto-suspensions visible and auditable.
//
// Mashūra calls are NOT suspended: opting into /auto covers counsel calls the
// same way headless --auto does (see headlessConfirmer). The ⚡ auto note still
// announces the panel, question, and briefing before the call fires, so cost
// stays visible — it just no longer blocks.
func SuspendAuto(toolName string, app *App, detail string) string {
	switch toolName {
	case "external_backend":
		// Egress consent gate: session context would be sent to an external backend.
		// Always requires explicit approval — never auto-approved, even in /auto.
		return "external backend egress (privacy gate)"
	case "run_shell", "run_background":
		// run_background detail lines are "$ <cmd> (background)" — the trailing
		// marker is harmless: the destructive check matches on segment-leading
		// tokens. Gating both mirrors headlessConfirmer's carve-out.
		cmd := ShellCmdFromDetail(detail)
		// AllowDestructive (/auto destructive) is the TUI counterpart of the
		// headless --allow-destructive flag: an explicit second opt-in that
		// covers destructive shell commands. Never covers the egress gate.
		if IsDestructiveShell(cmd) && !app.AllowDestructive {
			return "destructive command"
		}
		// Pre-implementation phases gate only commands that could write: the
		// write-containment invariant is enforced separately by wfPhaseBlock
		// (write_file/edit_file/run_background are rejected outright), so
		// read-only investigative commands (ls, grep, git status) may proceed
		// in auto mode without a prompt.
		if toolName == "run_shell" &&
			app.Workflow != nil && workflow.IsPreImplementPhase(app.Workflow.Phase) && !IsReadOnlyShell(cmd) {
			return "pre-implementation phase (" + app.Workflow.PhaseName() + ")"
		}
	}
	return ""
}

// shouldGateEvenWithAutoApprove is a thin predicate wrapper around suspendAuto
// for callers that only need a boolean.
func ShouldGateEvenWithAutoApprove(toolName string, app *App, detail string) bool {
	return SuspendAuto(toolName, app, detail) != ""
}

func tuiConfirmer(app *App) Confirmer {
	return func(toolName, headline, detail string, readAction bool) bool {
		if app.AutoApprove {
			reason := SuspendAuto(toolName, app, detail)
			if reason == "" {
				app.sendEvent(SysNoteMsg{Text: "⚡ auto: " + headline + "\n" + Indent(detail)})
				return true
			}
			// Auto suspended — prefix the headline so the first line of the
			// confirm prompt states the cause. headline is a local copy; the
			// tool name and detail passed to the gate are unchanged.
			headline = "⚡ auto suspended: " + reason + " — " + headline
		}
		ch := make(chan ConfirmChoice, 1)
		app.sendEvent(ConfirmReqMsg{
			ToolName:   toolName,
			Headline:   headline,
			Detail:     detail,
			ReadAction: readAction,
			RespCh:     ch,
		})
		switch <-ch {
		case ChoiceAllowReads:
			app.AllowReads = true
			return true
		case ChoiceApprove:
			return true
		default:
			return false
		}
	}
}

// handleEndpointSwitch switches the session to the named endpoint from
// cfg.Endpoints: reconfigures the client in place (kind, base_url, model,
// auth, sampling) and re-resolves context limits. Subagent clients are built
// from the live parent Client fields at dispatch time, so they inherit the
// new endpoint automatically — nothing is snapshotted at startup.
func handleEndpointSwitch(app *App, name string, note func(string) Cmd) (handled, quit bool, cmd Cmd) {
	ep, ok := app.Cfg.Endpoints[name]
	if !ok {
		return true, false, note(fmt.Sprintf("endpoint %q not found — %s", name, listEndpoints(app)))
	}
	// Apply the same defaulting rules as config load (validation errors on
	// malformed entries were already caught there for the startup endpoint;
	// a switched-to entry may not have been validated, so repeat the checks).
	if ep.Kind == "" {
		ep.Kind = config.EndpointKindOpenAI
	}
	switch ep.Kind {
	case config.EndpointKindOpenAI:
		if ep.Model == "" {
			return true, false, note(fmt.Sprintf("endpoint %q: model is required for kind %q — not switching", name, config.EndpointKindOpenAI))
		}
	case config.EndpointKindIlmProxy:
		if ep.Model == "" {
			ep.Model = "ilm"
		}
	default:
		return true, false, note(fmt.Sprintf("endpoint %q: unknown kind %q — not switching", name, ep.Kind))
	}
	if ep.BaseURL == "" {
		return true, false, note(fmt.Sprintf("endpoint %q: base_url is required — not switching", name))
	}

	// Commit: config mirror first (AuthHeader() reads Cfg.Endpoint), then client.
	app.Cfg.Endpoint = ep
	app.Cfg.EndpointName = name
	app.Cfg.BaseURL = ep.BaseURL
	app.Cfg.Model = ep.Model

	app.Client.BaseURL = strings.TrimRight(ep.BaseURL, "/")
	app.Client.Kind = ep.Kind
	app.Client.Model = ep.Model
	app.Client.ConfiguredModel = ep.Model
	app.Client.AuthHeader = app.Cfg.AuthHeader()
	app.Client.Temperature = ep.Temperature
	app.Client.TopP = ep.TopP
	app.Client.MaxTokens = ep.MaxTokens

	// Session model/backend overrides belong to the previous endpoint.
	app.SelectedModel = ""
	app.SelectedBackend = ""
	app.defaultModel = ep.Model

	msg := fmt.Sprintf("endpoint: switched to %q (kind %s, %s, model %s)", name, ep.Kind, ep.BaseURL, ep.Model)
	return true, false, Batch(note(msg), resolveBackendCtxCmd(app, "", ep.Model), fetchModelListCmd(app))
}

// listEndpoints renders the configured endpoints with the active one marked.
func listEndpoints(app *App) string {
	if len(app.Cfg.Endpoints) == 0 {
		return "no endpoints configured — add an \"endpoints\" block to config; /backend <endpoint-name> switches between them"
	}
	names := make([]string, 0, len(app.Cfg.Endpoints))
	for n := range app.Cfg.Endpoints {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("endpoints:")
	for _, n := range names {
		ep := app.Cfg.Endpoints[n]
		kind := ep.Kind
		if kind == "" {
			kind = config.EndpointKindOpenAI
		}
		marker := "  "
		if n == app.Cfg.EndpointName {
			marker = "* "
		}
		fmt.Fprintf(&b, "\n%s%s  (%s, %s, model %s)", marker, n, kind, ep.BaseURL, ep.Model)
	}
	return b.String()
}

// ApplyModelOverride sets the effective model for the session, branching on
// endpoint kind exactly as the live /model command does. Shared by the
// /model command handler and RestoreRepoState (repostate.go) so both paths
// stay in sync — a behavior-preserving extraction of what was previously
// inlined in the /model case.
//
// kind=openai: ConfiguredModel is forced into every request, which would
// make a plain SelectedModel override a silent no-op (apparent success,
// zero effect). Instead this updates the endpoint's effective model for the
// session — the literal string the client sends. No server-side
// validation: a bad name surfaces as a request error, which is honest.
func ApplyModelOverride(app *App, model string) {
	if app.Cfg.ActiveEndpoint().Kind == config.EndpointKindOpenAI {
		app.Client.ConfiguredModel = model
		app.Client.Model = model
		app.Cfg.Endpoint.Model = model
		app.SelectedModel = "" // openai mode: ConfiguredModel is the single source
		app.defaultModel = model
		return
	}
	app.SelectedModel = model
}

// shellCmdFromDetail extracts the raw shell command from the detail string that
// app.go passes to Confirmer for run_shell calls. The format is:
//
//	"$ <command>\n  (<exec>, cwd=<path>)"
//
// In pre-IMPLEMENT workflow phases a "⚠ workflow phase: …" line precedes the
// "$ <command>" line, so scan for the first line with the "$ " marker rather
// than assuming it is line one. Falls back to the first line for robustness.
func ShellCmdFromDetail(detail string) string {
	for _, line := range strings.Split(detail, "\n") {
		if cmd, ok := strings.CutPrefix(strings.TrimSpace(line), "$ "); ok {
			return cmd
		}
	}
	line, _, _ := strings.Cut(detail, "\n")
	return strings.TrimSpace(line)
}

// handleTUICommand processes slash commands locally without touching the agent.
// Returns (handled, quit, cmd) where cmd is a Cmd that produces the
// response message. All messages are returned as Cmds — never via EventSink —
// because this function is called from within Update, and calling Send from
// inside the event loop risks a deadlock.
func HandleTUICommand(line string, app *App) (handled, quit bool, cmd Cmd) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "/") {
		return false, false, nil
	}
	fields := strings.Fields(line)

	note := func(text string) Cmd {
		return NoteCmd(text)
	}

	switch fields[0] {
	case "/new", "/reset":
		app.NewConversation(NewChatID())
		chatID := ShortID(app.Client.ChatID)
		return true, false, func() Msg {
			return NewConvMsg{Note: "fresh conversation: " + chatID}
		}

	case "/cwd":
		return true, false, note("cwd: " + app.Exec.Cwd())

	case "/mode":
		return true, false, note("exec: " + app.Exec.Describe())

	case "/history":
		return true, false, note(fmt.Sprintf("%d messages, ~%d chars (max %d)",
			len(app.Conv), TranscriptSize(app.Conv), app.Cfg.MaxChars))

	case "/auto":
		// /auto destructive — separate explicit opt-in for destructive shell
		// commands (TUI counterpart of headless --allow-destructive). Requires
		// auto mode to already be ON so the grant is always a deliberate second
		// step, never part of the first toggle.
		if len(fields) > 1 {
			if fields[1] != "destructive" {
				return true, false, note("usage: /auto | /auto destructive")
			}
			if !app.AutoApprove {
				return true, false, note("auto mode is OFF — enable /auto first, then /auto destructive")
			}
			app.AllowDestructive = !app.AllowDestructive
			if app.AllowDestructive {
				return true, false, note("⚠ destructive auto-approve: ON — rm, mv, git reset, … run without prompting\n" +
					"  still confirmed: external-backend egress; /auto destructive again to revoke")
			}
			return true, false, note("destructive auto-approve: OFF — destructive commands require confirmation again")
		}
		app.AutoApprove = !app.AutoApprove
		// Persist the toggle (TUI-only: HandleTUICommand is never invoked from
		// the headless "wakil run" path — see repostate.go's RestoreRepoState
		// doc comment for why AutoApprove restore must stay TUI-only).
		// Deliberately NOT reached from the /auto destructive branch above,
		// and RepoState has no field for AllowDestructive regardless — that
		// grant can never be written to disk from here.
		app.saveRepoState(func(s *RepoState) { s.AutoApprove = app.AutoApprove })
		if app.AutoApprove {
			return true, false, note("auto mode: ON — tool calls approved without prompting\n" +
				"  still confirmed: destructive shell commands (opt in with /auto destructive), external-backend egress")
		}
		// The destructive grant never outlives the auto session it was given for.
		app.AllowDestructive = false
		return true, false, note("auto mode: OFF — tool calls require confirmation")

	case "/rawtools":
		app.RawTools = !app.RawTools
		app.saveRepoState(func(s *RepoState) { s.RawTools = app.RawTools })
		if app.RawTools {
			return true, false, note("raw tool results: ON — full output kept in context (cap disabled)")
		}
		cap := app.Cfg.ToolResultCap
		if cap <= 0 {
			return true, false, note("raw tool results: OFF — cap is set to unlimited in config")
		}
		return true, false, note(fmt.Sprintf("raw tool results: OFF — results capped at %d chars", cap))

	case "/compact":
		return true, false, func() Msg {
			ok, err := app.Compact(context.Background(), app.summarizeFn(), true)
			if err != nil {
				return SysNoteMsg{Text: "compact: " + err.Error()}
			}
			if !ok {
				return SysNoteMsg{Text: "nothing to compact (transcript fits within keep_bytes window)"}
			}
			app.SaveSession()
			return CompactedMsg{}
		}

	case "/sessions":
		// "/sessions all" shows every session regardless of workspace; bare
		// "/sessions" is scoped to the current workspace (with a hidden-count
		// hint when other-workspace sessions exist).
		all := len(fields) > 1 && fields[1] == "all"
		return true, false, note(SessionListText(app.Client.ChatID, SessionScope{Workspace: app.SessionWorkspace(), All: all}))

	case "/resume":
		arg := ""
		if len(fields) > 1 {
			arg = fields[1]
		}
		scope := SessionScope{Workspace: app.SessionWorkspace()}
		if arg == "all" {
			scope.All = true
			arg = ""
		}
		// Bare "/resume" (no id/prefix, no "all") opens the interactive picker
		// instead of silently loading the most recent session — the deliberate
		// UX change: browsing/selecting is now the default, not a fallback.
		// An explicit id/prefix still resumes directly without the picker.
		if arg == "" {
			return true, false, func() Msg {
				sessions, hidden, err := ListSessionsScoped(scope)
				if err != nil {
					return SysNoteMsg{Text: "resume: " + err.Error()}
				}
				return OpenResumePickerMsg{Sessions: sessions, Scope: scope, Hidden: hidden}
			}
		}
		return true, false, func() Msg {
			s, err := LoadSession(arg)
			if err != nil {
				return SysNoteMsg{Text: "resume: " + err.Error()}
			}
			return ResumeSessionMsg(app, s)
		}

	case "/session":
		if len(fields) >= 3 && fields[1] == "name" {
			label := strings.Join(fields[2:], " ")
			label = strings.Trim(label, `"'`)
			if app.Session == nil {
				return true, false, note("no active session")
			}
			app.Session.Label = label
			app.SaveSession()
			return true, false, note("session labeled: " + label)
		}
		return true, false, note(`usage: /session name "<label>"`)

	case "/mcp":
		args := fields[1:]
		// /mcp reconnect NAME — blocking network call; run in the Cmd goroutine.
		if len(args) >= 2 && args[0] == "reconnect" {
			name := strings.Join(args[1:], " ")
			return true, false, func() Msg {
				if app.MCP == nil {
					return SysNoteMsg{Text: "no MCP servers configured"}
				}
				if err := app.MCP.Reconnect(context.Background(), name); err != nil {
					return SysNoteMsg{Text: fmt.Sprintf("reconnect %q: %v", name, err)}
				}
				toolList := BuildTools(app.Cfg, app.Exec.Cwd(), app.MCP)
				return MCPReconnectedMsg{Name: name, Tools: toolList}
			}
		}
		// /mcp list — fast, compute in-line but still return as Cmd.
		return true, false, note(mcpStatus(args, app))

	case "/backend":
		// kind=openai: /backend is repurposed as the endpoint switcher —
		// proxy backends don't exist on a plain endpoint, but named endpoints
		// from config do. /backend <endpoint-name> reconfigures the client
		// (kind, base_url, model, auth, sampling) and re-resolves limits;
		// no argument lists configured endpoints. The proxy-prefix routing
		// below applies only while the ACTIVE endpoint is kind ilm-proxy.
		if app.Cfg.ActiveEndpoint().Kind == config.EndpointKindOpenAI {
			if len(fields) >= 2 {
				return handleEndpointSwitch(app, fields[1], note)
			}
			return true, false, note(listEndpoints(app))
		}
		// /backend [<name>[/<model-path>]] — set or show the current backend selection.
		// When <name> contains a slash (e.g. "openrouter/anthropic/claude-opus-4-8"),
		// the part before the first slash is the backend name and the full string is
		// sent as the model field so the proxy can route by model prefix.
		if len(fields) >= 2 {
			arg := fields[1]
			if idx := strings.Index(arg, "/"); idx >= 0 {
				app.SelectedBackend = arg[:idx]
				app.SelectedModel = arg
			} else {
				app.SelectedBackend = arg
				app.SelectedModel = ""
			}
			selected := app.SelectedBackend
			msg := "backend: set to " + selected
			if app.SelectedModel != "" {
				msg += " · model: " + app.SelectedModel
			}
			// Persist (ilm-proxy kind only — this branch is unreachable for
			// kind=openai, which is repurposed above as the endpoint switcher).
			app.saveRepoState(func(s *RepoState) {
				s.Backend = app.SelectedBackend
				if app.SelectedModel != "" {
					s.Model = app.SelectedModel
				}
				s.EndpointName = app.Cfg.EndpointName
			})
			// Re-probe context limits for the new backend so dynamic thresholds
			// (compact_at_frac etc.) scale to the new window. The result arrives
			// as BackendCtxLimitMsg and is applied safely in the TUI event loop.
			return true, false, Batch(note(msg), resolveBackendCtxCmd(app, selected, app.SelectedModel))
		}
		// No arg: report current selection and last-used.
		cur := app.SelectedBackend
		if cur == "" {
			cur = "(proxy default)"
		}
		used := ""
		if app.Client != nil {
			used = app.Client.LastUsedBackend()
		}
		if used == "" {
			used = "(none yet)"
		}
		msg := "backend: selected=" + cur + " · last-used=" + used
		if app.SelectedModel != "" {
			msg += " · model=" + app.SelectedModel
		}
		return true, false, note(msg)

	case "/model":
		// /model [<name>] — set or show the model override for this session.
		// Unlike /backend <name/model>, /model acts on the model field only,
		// leaving the current backend selection unchanged. A model switch also
		// re-resolves context limits so compaction thresholds scale to the
		// new model's real window (not the previous model's).
		//
		// kind=openai: pass A forces ConfiguredModel into every request, which
		// would make /model a silent no-op (apparent success, zero effect).
		// Instead, update the endpoint's effective model for the session —
		// the literal string the client sends. No server-side validation: a
		// bad name surfaces as a request error, which is honest.
		if len(fields) >= 2 {
			model := fields[1]
			isOpenAI := app.Cfg.ActiveEndpoint().Kind == config.EndpointKindOpenAI
			ApplyModelOverride(app, model)
			// Persist: repo-state stores the literal model string regardless
			// of endpoint kind, since ApplyModelOverride clears SelectedModel
			// for openai kind — reading SelectedModel back here would lose
			// the value. See repo-state-plan.md fix #2.
			app.saveRepoState(func(s *RepoState) {
				s.Model = model
				s.EndpointName = app.Cfg.EndpointName
			})
			if isOpenAI {
				msg := "model: set to " + model + " (endpoint " + app.Cfg.EndpointName + ")"
				return true, false, Batch(note(msg), resolveBackendCtxCmd(app, "", model))
			}
			msg := "model: set to " + model
			// Re-resolve limits for the selected backend + new model.
			return true, false, Batch(note(msg), resolveBackendCtxCmd(app, app.SelectedBackend, model))
		}
		cur := app.SelectedModel
		if cur == "" {
			cur = app.Client.Model
		}
		return true, false, note("model: " + cur)

	case "/subagent":
		// /subagent [<endpoint-name>|inherit] — set, show, or reset which
		// endpoint dispatch_subagent targets for this session. Deliberately a
		// distinct command, not an overload of /model or /backend: both of
		// those parse only fields[1] and silently drop any further token, so
		// a scope-modifier form like "/model subagent <name>" would set the
		// model to the literal string "subagent" and silently discard the
		// intended endpoint name — see the discovery scan's parsing-trap note.
		// Session-scoped, like /model; the config file's subagent_endpoint is
		// the persistent default this overrides.
		if len(fields) >= 2 {
			name := fields[1]
			if name == "inherit" {
				app.SubagentEndpointOverride = ""
				app.saveRepoState(func(s *RepoState) { s.SubagentEndpoint = "" })
				return true, false, note("subagent endpoint: inherit (parent endpoint)")
			}
			if _, err := app.Cfg.NormalizeEndpoint(name); err != nil {
				return true, false, note(fmt.Sprintf("subagent endpoint %q: %v — not set", name, err))
			}
			app.SubagentEndpointOverride = name
			app.saveRepoState(func(s *RepoState) { s.SubagentEndpoint = name })
			return true, false, note("subagent endpoint: set to " + name)
		}
		epName := resolveSubagentEndpointName(app)
		if epName == "" {
			return true, false, note("subagent endpoint: inherit (parent endpoint)")
		}
		view, _ := app.resolveSubagentEndpointView(epName)
		return true, false, note(fmt.Sprintf("subagent endpoint: %s (kind %s, model %s)", epName, view.kind, view.model))

	case "/submodel":
		// /submodel [<name>|inherit] — set, show, or reset the model override
		// for dispatch_subagent, mirroring /model's semantics but scoped to
		// subagents. Overrides only the model string; kind/base_url/auth are
		// left to /subagent (or inherit). Composes with /subagent: set the
		// endpoint with /subagent, then the model with /submodel. No
		// server-side validation — a bad name surfaces as a request error.
		// Session-scoped, like /model.
		if len(fields) >= 2 {
			name := fields[1]
			if name == "inherit" {
				app.SubagentModelOverride = ""
				// Clear the limits cache so the next dispatch re-probes with
				// the endpoint's original model, not the stale override.
				app.subagentLimitsCachePtr = nil
				app.saveRepoState(func(s *RepoState) { s.SubagentModel = "" })
				return true, false, note("subagent model: inherit (endpoint model)")
			}
			app.SubagentModelOverride = name
			// Clear the limits cache so the next dispatch probes the new
			// model's context window rather than returning a stale cached
			// limit from the previous model.
			app.subagentLimitsCachePtr = nil
			app.saveRepoState(func(s *RepoState) { s.SubagentModel = name })
			return true, false, note("subagent model: set to " + name)
		}
		cur := app.SubagentModelOverride
		if cur == "" {
			// Show what the child will actually use: the override if set,
			// else the resolved endpoint's model.
			epName := resolveSubagentEndpointName(app)
			view, _ := app.resolveSubagentEndpointView(epName)
			cur = view.model
		}
		return true, false, note("subagent model: " + cur)

	case "/maxpar":
		// /maxpar [<N>] — set or show the max concurrent dispatch_subagent
		// workers. Values ≤ 1 mean sequential; default is 2. Capped at 64 to
		// prevent huge semaphore allocation. Session-scoped, persisted to
		// repo-state like /model and /submodel.
		if len(fields) >= 2 {
			n, err := strconv.Atoi(fields[1])
			if err != nil || n < 1 {
				return true, false, note("maxpar: must be a positive integer (1 = sequential)")
			}
			const maxParCap = 64
			if n > maxParCap {
				n = maxParCap
			}
			app.Cfg.MaxParallelSubagents = n
			app.saveRepoState(func(s *RepoState) { s.MaxParallelSubagents = n })
			if n == maxParCap {
				return true, false, note(fmt.Sprintf("max parallel subagents: set to %d (capped at %d)", n, maxParCap))
			}
			return true, false, note(fmt.Sprintf("max parallel subagents: set to %d", n))
		}
		cur := app.Cfg.MaxParallelSubagents
		if cur < 1 {
			cur = 1
		}
		return true, false, note(fmt.Sprintf("max parallel subagents: %d", cur))

	case "/counsel":
		// /counsel [auto|suggest|off] — set or show the auto-counsel mode.
		if len(fields) < 2 {
			mode := app.CounselMode
			if mode == "" {
				mode = "suggest"
			}
			msg := "counsel mode: " + mode
			if mode == "auto" {
				msg += fmt.Sprintf(" (cap: %d/turn)", app.MaxCounsel)
			}
			return true, false, note(msg)
		}
		switch fields[1] {
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
			return true, false, note(fmt.Sprintf("counsel mode: auto (cap: %d/turn)", cap))
		case "suggest":
			app.CounselMode = "suggest"
			return true, false, note("counsel mode: suggest (hint only, no auto-fire)")
		case "off":
			app.CounselMode = "off"
			return true, false, note("counsel mode: off (struggle detected silently)")
		default:
			return true, false, note("usage: /counsel auto|suggest|off")
		}

	case "/plan":
		return HandlePlanCommand(fields, app)

	case "/learn":
		// /learn asks the PROXY to synthesize and persist a fact — a plain
		// OpenAI endpoint has no such machinery and a bare model would
		// improvise "understood, I'll remember that": a fabricated success.
		// Hard-fail client-side; the request must never reach the model.
		if app.Cfg.ActiveEndpoint().Kind == config.EndpointKindOpenAI {
			epName := app.Cfg.EndpointName
			if epName == "" {
				epName = "(unnamed)"
			}
			return true, false, note(fmt.Sprintf(
				"/learn requires an ilm-proxy endpoint — current endpoint %q is kind %q (nothing was sent; no memory exists to write to)",
				epName, config.EndpointKindOpenAI))
		}
		return true, false, func() Msg { return LearnTurnMsg{} }

	case "/repostate":
		// /repostate [clear] — show or clear the per-folder terminal settings
		// remembered for this workspace (model/backend/subagent/rawtools/auto).
		if len(fields) >= 2 && fields[1] == "clear" {
			if err := ClearRepoState(app); err != nil {
				return true, false, note("repostate: clear failed: " + err.Error())
			}
			return true, false, note("repostate: cleared for " + app.SessionWorkspace() +
				" (this session's current values are unchanged)")
		}
		return true, false, note(DescribeRepoState(app))

	case "/help":
		return true, false, note(helpTextTUI)

	case "/quit", "/exit":
		return true, true, nil

	default:
		return true, false, note("unknown command — /help for the list")
	}
}

// handlePlanCommand processes all /plan subcommands. Called from handleTUICommand.
func HandlePlanCommand(fields []string, app *App) (handled, quit bool, cmd Cmd) {
	note := func(text string) Cmd {
		return NoteCmd(text)
	}

	if len(fields) < 2 {
		return true, false, note("usage: /plan <task> | /plan status | /plan abort | /plan approve")
	}

	switch fields[1] {
	case "status":
		if app.Workflow == nil {
			return true, false, note("no active workflow")
		}
		return true, false, note("workflow:\n" + app.Workflow.StatusString())

	case "abort":
		app.Workflow = nil
		return true, false, note("workflow aborted")

	case "verify":
		if app.Workflow == nil ||
			app.Workflow.Phase != workflow.WFImplement ||
			app.Workflow.StepIdx <= app.Workflow.StepCount {
			return true, false, note("no active workflow in verify state (/plan verify is for after all steps complete)")
		}
		return true, false, func() Msg { return WFFinalReviewMsg{} }

	case "review":
		// Phase acknowledgments (/plan approve, /plan review) are ONLY ever
		// user-typed commands — handlePlanCommand is only called from handleKey
		// which requires a physical tea.KeyMsg (from the TUI layer). Auto mode never invokes these
		// commands; its only workflow shortcut is in HandleReviewOracle, which
		// auto-advances PAST a review only when that review ran successfully
		// (a failed/unavailable review always parks and waits for the user).
		if app.Workflow == nil ||
			(app.Workflow.Phase != workflow.WFReview && app.Workflow.Phase != workflow.WFPresent) {
			return true, false, note("no active workflow in review state (/plan review works from WFReview or WFPresent)")
		}
		// Transition to WFReview so the WFReview auto-retry path in
		// handleWorkflowTransition picks up and calls handleReviewOracle when
		// the turn completes. For WFReview this is a no-op; for WFPresent it
		// enables the voluntary re-review that refreshes ReviewPlanHash.
		app.Workflow.Phase = workflow.WFReview
		return true, false, func() Msg {
			return WFStartTurnMsg{Note: "running oracle plan review", UserText: "continue"}
		}

	case "approve":
		// Phase acknowledgments are only ever user-typed — see /plan review above.
		if app.Workflow == nil {
			return true, false, note("no active workflow (use /plan <task> to start one)")
		}
		switch app.Workflow.Phase {
		case workflow.WFReview:
			// User force-skips the oracle review. Log the reason so the plan file
			// is an honest record: "REVIEW skipped with reason: <why oracle failed>".
			reason := app.Workflow.ReviewSkipReason
			if reason == "" {
				reason = "oracle review was unavailable"
			}
			app.Workflow.ReviewSkipReason = ""
			WFWriteReviewSkipForce(app, reason)
			app.Workflow.Phase = workflow.WFPresent
			stepLabel := strconv.Itoa(app.Workflow.StepCount)
			return true, false, note(fmt.Sprintf(
				"· oracle review skipped (logged) — plan ready (%s steps)\n"+
					"  type /plan approve again to begin step-by-step implementation", stepLabel))

		case workflow.WFPresent:
			// Stale-review detection: if ## Plan changed since the oracle reviewed it,
			// warn and require a second approve. Phase acknowledgments are user-only
			// (see /plan review comment above) so this gate cannot be auto-bypassed.
			stepLabel := strconv.Itoa(app.Workflow.StepCount)
			return true, false, func() Msg {
				wf := app.Workflow
				if wf == nil {
					return SysNoteMsg{Text: "no active workflow"}
				}
				// Check for plan modification since last review.
				if wf.ReviewPlanHash != "" && app.Exec != nil {
					if content, err := app.Exec.ReadFile(context.Background(), wf.PlanPath); err == nil {
						if workflow.HashPlanSection(content) != wf.ReviewPlanHash && !wf.ReviewStaleWarned {
							wf.ReviewStaleWarned = true
							return SysNoteMsg{Text: "⚠ plan modified since last review — " +
								"/plan review recommended (approve again to proceed anyway)"}
						}
					}
				}
				// Second approve (warned) or no hash stored — proceed.
				wf.ReviewStaleWarned = false
				wf.Phase = workflow.WFImplement
				wf.StepIdx = 1
				return WFStartTurnMsg{
					Note:     "approved — starting implementation: step 1/" + stepLabel,
					UserText: "continue",
				}
			}

		case workflow.WFImplement:
			wf := app.Workflow
			if wf.StepIdx > wf.StepCount {
				// Force-close from verify state. Log that flags were not resolved
				// so the step log is an honest record of the workflow outcome.
				wfWriteFinalLog(app, "FINAL REVIEW: workflow force-closed with unresolved flags.")
				app.Workflow = nil
				return true, false, note("· workflow force-closed (unresolved flags logged to step log)")
			}
			// Paused by every-step oracle critique — advance to next step.
			wf.StepIdx++
			if wf.StepIdx > wf.StepCount {
				// The paused step was the last — run the final review now.
				if app.Cfg.WFFinalReview {
					return true, false, func() Msg { return WFFinalReviewMsg{} }
				}
				app.Workflow = nil
				return true, false, note("· workflow complete — all steps done")
			}
			stepLabel := strconv.Itoa(wf.StepCount)
			return true, false, func() Msg {
				return WFStartTurnMsg{
					Note:     fmt.Sprintf("oracle critique acknowledged — step %d/%s", wf.StepIdx, stepLabel),
					UserText: "continue",
				}
			}

		default:
			return true, false, note("no workflow awaiting approval (use /plan status)")
		}

	default:
		// /plan [--oracle=MODE] <task text>
		// Parse optional --oracle=VALUE flag; remaining tokens form the task.
		var oracleMode string
		var taskParts []string
		for _, f := range fields[1:] {
			if strings.HasPrefix(f, "--oracle=") {
				oracleMode = strings.TrimPrefix(f, "--oracle=")
			} else {
				taskParts = append(taskParts, f)
			}
		}
		if oracleMode != "" {
			switch oracleMode {
			case "every-step", "on-deviation", "phases-only":
				// valid
			default:
				return true, false, note("unknown oracle mode " + strconv.Quote(oracleMode) +
					" — use every-step, on-deviation, or phases-only")
			}
		}
		if len(taskParts) == 0 {
			return true, false, note("usage: /plan [--oracle=MODE] <task>")
		}
		task := strings.Join(taskParts, " ")
		capturedOracleMode := oracleMode
		return true, false, func() Msg {
			content := workflow.WFInitPlanContent(task)
			// Resolve the plan path to absolute once, using the executor's cwd at
			// workflow start. All subsequent readers use this absolute path so that
			// later cwd changes inside the executor cannot misroute a read or write.
			planPath := ".wakil/plan.md"
			if app.Exec != nil {
				planPath = filepath.Join(app.Exec.Cwd(), ".wakil", "plan.md")
				if _, err := app.Exec.RunShell(context.Background(), "mkdir -p .wakil"); err != nil {
					return SysNoteMsg{Text: "workflow: could not create .wakil dir: " + err.Error()}
				}
				if _, err := app.Exec.WriteFile(context.Background(), planPath, content); err != nil {
					return SysNoteMsg{Text: "workflow: could not write plan.md: " + err.Error()}
				}
			}
			app.Workflow = &workflow.WorkflowState{
				Task:       task,
				Phase:      workflow.WFGather,
				PlanPath:   planPath,
				OracleMode: capturedOracleMode,
			}
			note := "workflow started: gather — " + Truncate(task, 60)
			if capturedOracleMode != "" {
				note += " (oracle: " + capturedOracleMode + ")"
			}
			return WFStartTurnMsg{Note: note, UserText: "continue"}
		}
	}
}

// mcpStatus builds the /mcp listing string.
func mcpStatus(args []string, app *App) string {
	hasSomething := (app.MCP != nil && len(app.MCP.Servers()) > 0) || app.Cfg.SearXngURL != "" || (app.Cfg.GoogleAPIKey != "" && app.Cfg.GoogleCX != "")
	if !hasSomething {
		return "no tool servers configured (add mcp_servers, searxng_url, or google_api_key+google_cx to config)"
	}

	var sb strings.Builder
	if app.MCP != nil {
		for _, srv := range app.MCP.Servers() {
			icon := "✓"
			if srv.Status == "failed" {
				icon = "✗"
			} else if srv.Status == "connecting" {
				icon = "…"
			}
			sb.WriteString(fmt.Sprintf("%s %s [%s] (mcp)", icon, srv.Cfg.Name, srv.Status))
			if srv.Err != nil {
				sb.WriteString(": " + srv.Err.Error())
			}
			sb.WriteByte('\n')
			for _, t := range srv.Tools {
				sb.WriteString(fmt.Sprintf("    • %s%s%s: %s\n", srv.Cfg.Name, mcpNS, t.Name, t.Description))
			}
		}
	}
	if app.Cfg.SearXngURL != "" {
		sb.WriteString("✓ searxng [connected] (native)\n")
		for _, t := range tools.SearxngTools() {
			sb.WriteString(fmt.Sprintf("    • %s: %s\n", t.Function.Name, Truncate(t.Function.Description, 70)))
		}
	}
	if app.Cfg.GoogleAPIKey != "" && app.Cfg.GoogleCX != "" {
		sb.WriteString("✓ google [connected] (native)\n")
		for _, t := range tools.GoogleTools() {
			sb.WriteString(fmt.Sprintf("    • %s: %s\n", t.Function.Name, Truncate(t.Function.Description, 70)))
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

const helpTextTUI = `/new, /reset         fresh conversation (new chat_id, clears viewport)
/backend <name>      set the backend for this session (X-Ilm-Backend header)
/backend <name/model> set backend + model (e.g. openrouter/anthropic/claude-opus-4-8)
/backend             show current backend selection and last-used backend
/model <name>        set the model for this session (overrides backend default)
/model               show current model
/subagent <name>     set which endpoint dispatch_subagent targets this session
/subagent inherit    reset dispatch_subagent to follow the parent's endpoint
/subagent            show current subagent endpoint selection
/submodel <name>     set the model for dispatch_subagent (overrides endpoint model)
/submodel inherit    reset subagent model to the endpoint's configured model
/submodel            show current subagent model
/maxpar <N>          set max concurrent dispatch_subagent workers (1 = sequential, max 64)
/maxpar              show current max parallel subagents
/plan <task>         start a gather→plan→review→implement workflow for <task>
/plan --oracle=MODE  set per-run oracle schedule (every-step|on-deviation|phases-only)
/plan status         show current workflow phase and step
/plan approve        approve the plan; force-skip review (logged); advance past pauses
/plan review         retry the oracle plan review (when review is pending/unavailable)
/plan verify         re-run the final oracle review (in verify state after gaps flagged)
/plan abort          cancel the active workflow
/compact             summarize older turns now (frees context, improves performance)
/learn               send "learn this for next time" — proxy synthesises a fact to save
/counsel auto|suggest|off  auto-counsel mode: auto=fire mashura__debug on struggle, suggest=hint, off=silent
/counsel                   show current counsel mode and per-turn cap
/auto                toggle: auto-approve tool calls without prompting (shown as AUTO in status)
                     still confirmed: destructive shell commands, external-backend egress
                     in /plan: a successful review auto-approves the plan and starts implementation
/auto destructive    toggle: also auto-approve destructive shell commands (rm, mv, git reset, …)
                     requires /auto ON; cleared when /auto goes OFF; shown as AUTO! in status
/rawtools            toggle: include full tool output in context (default: capped at 8k chars)
/repostate           show terminal settings remembered for this folder (model/backend/auto/…)
/repostate clear     delete the remembered settings for this folder
/cwd                 show executor working directory
/mode                show execution backend
/history             transcript size
/sessions            list saved sessions (★ = current)
/resume [<id>]       resume a saved session by id prefix; omit for most recent
/session name "..."  label the current session (shown in /sessions listing)
/mcp                 list tool servers and tools
/mcp reconnect NAME  reconnect a named MCP server
/help                this help
/quit, /exit         leave (ctrl+c in idle also quits)

sessions are saved automatically; resume with: wakil --resume  (or --resume-id <id>)
or switch sessions mid-run with /resume inside the TUI

@path                attach a file/folder for context (picker pops up after "@")
                     reads host files for context; editing them needs --exec direct

scroll the conversation with the mouse wheel or PgUp/PgDn
drag with the mouse to select text — it's copied to the clipboard on release`
