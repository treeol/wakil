package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"wakil/internal/config"
	"wakil/internal/counsel"
	"wakil/internal/exec"
	"wakil/internal/proxy"
	"wakil/internal/trace"
	wtools "wakil/internal/tools"
	"wakil/internal/workflow"
)

// Confirmer is the safety gate: it is shown the exact action and returns whether
// to proceed. readAction reports that the action is a read-only one, so the
// implementation may offer an "allow all reads" choice. The default returns no.
type Confirmer func(toolName, headline, detail string, readAction bool) bool

// ConfirmChoice is what the user picked at a confirm prompt.
type ConfirmChoice int

const (
	ChoiceDecline    ConfirmChoice = iota // do not run
	ChoiceApprove                         // run this one
	ChoiceAllowReads                      // run this one and auto-approve future reads
)

// App owns the single continuous conversation, the executor, and the agent loop.
type App struct {
	Cfg     config.Config
	Client  *proxy.Client
	Exec    exec.Executor
	MCP     *MCPManager // nil if no MCP servers configured
	Tools   []proxy.Tool
	Conv    []proxy.Message
	Confirm Confirmer
	Out     io.Writer // assistant text + status sink

	// CtxLimit is the authoritative per-slot context window, resolved from the
	// backend at startup (see resolveContextLimit). The zero value means "not yet
	// resolved" — contextLimit() then synthesizes a fallback from Cfg so the
	// sidebar and pressure checks always have a positive ceiling (tests, subagents).
	CtxLimit ContextLimit

	// AllowReads, once the user picks "allow all reads" at a confirm prompt,
	// auto-approves read-only shell commands for the rest of the session.
	AllowReads bool

	// AutoApprove skips all confirmation prompts for the session, approving every
	// tool call automatically. Toggled by /auto or set via --auto flag.
	AutoApprove bool

	// IsHeadless marks wakil-run sessions where no human is present to re-send a
	// failed turn. Backend stream errors are retried automatically regardless of
	// workflow phase. Set by RunHeadless before the first Send call.
	IsHeadless bool

	// IsSubagent marks dispatch_subagent child Apps. Distinct from ToolCache,
	// which exists for deduplication and is not a reliable presence signal.
	IsSubagent bool

	// RetryDelay overrides the exponential backoff schedule for backend retries.
	// Nil uses the standard 1s/2s/4s schedule. Set in tests for speed.
	RetryDelay func(attempt int) time.Duration

	// RawTools disables the ToolResultCap for the rest of the session when true.
	// Toggled by the /rawtools command.
	RawTools bool

	// ToolCache, when non-nil, deduplicates tool calls within the session.
	// A repeated (name, args) pair returns a short notice instead of re-executing.
	// Enabled for subagents (which should never need to read the same file twice).
	ToolCache map[string]bool

	// OnTokRate, when set, receives a live token/sec estimate of the assistant's
	// decode speed during streaming (output chars ÷ 4 ÷ elapsed). Set only for the
	// parent's TUI turn; nil for subagents, CLI, and tests.
	OnTokRate func(tps float64)

	// OnReasoning, when set, receives extended-thinking (reasoning_content) deltas
	// as they arrive. Reasoning is visual-only: it is never written to Conv history,
	// the session transcript, compaction input, or any tool payload.
	OnReasoning func(string)

	// AgentPrompt is the agent operating instructions loaded once at startup from
	// AgentPromptPath. It is prepended to every request as the first part of the
	// system message. Empty string = no agent system prompt (proxy-side injections
	// are unaffected).
	AgentPrompt string

	// InjectDate prepends a system message stating the current date to every
	// request, so the model doesn't fall back to its training-era guess about
	// "now". Enabled for the parent; off for subagents and tests.
	InjectDate bool

	// Session is the on-disk record persisted after each turn. nil disables
	// persistence (e.g. in tests).
	Session *Session

	// Summarize is injectable for tests; nil falls back to the proxy.
	Summarize summarizer

	// learnNudgePending holds the whitespace-normalised query when the current
	// turn fired the learn-candidate log; cleared by runTurn after the turn.
	learnNudgePending string

	// learnNudgedQueries is a per-session (process-lifetime) set of queries for
	// which the end-of-turn nudge has already been shown — prevents repeats.
	learnNudgedQueries map[string]bool

	// B3: background process registry.
	bgProcs   map[string]*bgEntry
	bgCounter int

	// Workflow is set while a /plan workflow is active. Nil when no workflow is
	// running. Cleared when the workflow reaches WFDone or the user aborts it.
	Workflow *workflow.WorkflowState

	// WorkflowStepTrace accumulates tool-call evidence during an IMPLEMENT turn.
	// Reset to nil at the start of each turn in runTurn; consumed by
	// handleWorkflowTransition when %%STEP_DONE%% is detected.
	WorkflowStepTrace []ToolTraceEntry

	// Costs accumulates per-source cost estimates for the session, rendered in the
	// sidebar. Nil disables tracking (subagents, headless runs, tests) — every
	// CostTracker method is nil-safe, so call sites need no guard.
	Costs *proxy.CostTracker

	// recentTraces is a rolling buffer of the most recent tool-call evidence
	// records across all turns and phases (unlike WorkflowStepTrace, which is
	// IMPLEMENT-only and resets each turn). mashura__debug reads it, and the
	// struggle detector scans it. Capped at mashuraRecentTraceCap entries.
	recentTraces []ToolTraceEntry

	// struggleSuggested dedupes the struggle-trigger hint so the same symptom is
	// offered at most once per session.
	struggleSuggested map[string]bool

	// CtxPressureWarned tracks whether the usable-budget pressure notice has
	// already been shown for the current high-occupancy stretch; it re-arms once
	// occupancy drops back under the usable budget so the warning fires once per
	// crossing rather than every turn.
	CtxPressureWarned bool

	// EventSink, when set, receives events the agent goroutine posts to the TUI
	// (stream chunks, done signals, confirm requests, etc.). Set by main to
	// globalProg.Send; nil in tests that don't need TUI events.
	EventSink func(interface{})

	// AutoCounsel, when true, fires mashura__debug automatically whenever the
	// struggle detector triggers, instead of just printing a hint the user would
	// need to act on. Designed for headless benchmark runs where no human is
	// present to read the hint. Gated by MaxCounsel.
	// Note: requires --auto (headless mashura approval) to actually run the call;
	// without it the call is declined and the slot is still consumed.
	AutoCounsel bool

	// MaxCounsel caps the total number of auto-counsel calls per session.
	// Prevents a stuck benchmark task from generating unbounded paid
	// consultations. Zero means auto-counsel is effectively disabled even if
	// AutoCounsel=true. Typical benchmark value: 3.
	MaxCounsel int

	// counselCalls counts auto-counsel calls fired this turn (TUI path) or
	// this session (headless path). Reset at the start of each Send() when
	// CounselMode is set.
	counselCalls int

	// CounselMode is the TUI session counsel mode: "suggest" | "auto" | "off".
	// When empty, the effective mode is derived from AutoCounsel (headless path).
	// Set by /counsel command; initialized from cfg.AutoCounsel at startup.
	CounselMode string

	// autoCounselSkipGate, when true, tells the next handleMashura call to
	// bypass the a.Confirm gate. Set immediately before an auto-counsel fire
	// when AutoApprove=true; consumed (cleared) by handleMashura.
	autoCounselSkipGate bool

	// SelectedBackend is the current session's requested backend name, sent as
	// X-Ilm-Backend on every chat request. Empty = use the proxy's default (no
	// header sent). Set by /backend; initialized from Cfg.Backend at startup.
	// Read at request build time — never snapshot into a closure.
	SelectedBackend string

	// SelectedModel overrides the model field sent in the request body.
	// When set, it is used instead of Client.Model; the original model is
	// preserved in defaultModel for restoration when cleared.
	// Format: "<backend>/<model-path>" e.g. "openrouter/anthropic/claude-opus-4-8".
	// Set by /backend <backend>/<model>; cleared by /backend <name> (no model).
	SelectedModel string

	// defaultModel is Client.Model at construction time, used to restore the
	// model when SelectedModel is cleared.
	defaultModel string

	// consentedBackends is the per-session set of external backends the user has
	// already consented to. nil until the first consented external request.
	consentedBackends map[string]bool

	// BackendList is the list of available backends fetched from /v1/ilm/backends
	// at startup. nil if the endpoint is absent; callers fall back to
	// Cfg.ExternalBackends via IsExternalBackend.
	BackendList []BackendInfo

	// ModelList is the list of model names fetched from /v1/ilm/models at startup.
	// Used for /model <name> completion; empty when the endpoint is unavailable.
	ModelList []string

	// Trace, when non-nil, receives a rich JSONL record for every turn.
	// Opened at startup when cfg.Trace is true; nil disables trace capture.
	Trace *trace.Store
}

// ToolTraceEntry is one tool call's compact evidence record for the step log.
type ToolTraceEntry struct {
	Abbrev    string // short name: "shell", "read", "write", "edit", etc.
	Command   string // run_shell command or key arg (path, pattern)
	ExitErr   bool   // true when the tool returned an error
	OutputLen int    // raw output length in bytes
	FirstLine string // first non-empty output line, truncated to 80 chars
	LastLine  string // last non-empty output line if different, truncated
	// errorTail holds a generous tail (last ~15 non-empty lines) of the output,
	// populated only when exitErr is true. The step log ignores it; mashura__debug
	// uses it to give the diagnosis a fuller view of failing output than the normal
	// 4-line cap allows.
	ErrorTail string
}

// bgEntry tracks a single background process started with run_background.
type bgEntry struct {
	id         string
	pid        int
	pgid       int
	label      string
	logPath    string
	startedAt  time.Time
	generation int // executor generation at time of creation
}

// CounselCallsCount returns how many auto-counsel calls have fired this session.
// Exported so benchmark tests in cmd/wakil can assert the cap is enforced.
func (a *App) CounselCallsCount() int { return a.counselCalls }

// EffectiveModel returns the model that will be sent in the next request:
// SelectedModel if set, otherwise Client.Model.
func (a *App) EffectiveModel() string {
	if a.SelectedModel != "" {
		return a.SelectedModel
	}
	return a.Client.Model
}

// sendEvent delivers msg to the EventSink if one is set (nil-safe).
func (a *App) sendEvent(msg interface{}) {
	if a.EventSink != nil {
		a.EventSink(msg)
	}
}

// sessionWorkspace is the host directory associated with this session: the bind
// mount in docker mode, or the working directory in direct mode.
func (a *App) SessionWorkspace() string {
	if a.Cfg.ExecMode == "direct" {
		return a.Cfg.WorkDir
	}
	return a.Cfg.HostWorkDir
}

// chatID returns the current session's chat ID, or empty string if no session.
func (a *App) chatID() string {
	if a.Session != nil {
		return a.Session.ChatID
	}
	return ""
}

// saveSession persists the current transcript. Best-effort: persistence failures
// must never interrupt a turn, so errors are swallowed.
func (a *App) SaveSession() {
	if a.Session == nil || len(a.Conv) == 0 {
		return
	}
	a.Session.Conv = a.Conv
	a.Session.Updated = time.Now()
	if a.Session.Workspace == "" {
		a.Session.Workspace = a.SessionWorkspace()
	}
	a.Session.SavedWorkflow = a.Workflow
	_ = WriteSession(a.Session)
}

func (a *App) summarizeFn() summarizer {
	if a.Summarize != nil {
		return a.Summarize
	}
	return a.proxySummarizer
}

// NewConversation resets the running transcript and rotates the chat_id, starting
// a fresh persisted session.
func (a *App) NewConversation(chatID string) {
	a.Conv = nil
	a.Client.ChatID = chatID
	a.Session = &Session{
		ChatID:    chatID,
		Model:     a.Client.Model,
		Created:   time.Now(),
		Workspace: a.SessionWorkspace(),
	}
}

// Send runs one user turn through the agent loop: stream a response, and while
// the proxy returns tool_calls, gate+execute each and feed results back until a
// final text answer. Plain-text responses (memory/learn/meta acks, answers) are
// just streamed and returned — no special-casing needed.
//
// The named return retErr lets the deferred trace flush read the error without
// an extra variable; it does not change the calling convention.
func (a *App) Send(ctx context.Context, userText string) (_ string, retErr error) {
	// Lazy-initialize defaultModel so it can be restored when SelectedModel is cleared.
	if a.defaultModel == "" {
		a.defaultModel = a.Client.Model
	}
	// Apply model override (or restore default) at request build time.
	if a.SelectedModel != "" {
		a.Client.Model = a.SelectedModel
	} else {
		a.Client.Model = a.defaultModel
	}

	// Apply the current backend selection at request build time (never snapshot).
	a.Client.Backend = a.SelectedBackend

	// Set the aux model header only when explicitly configured. When absent the
	// proxy resolves aux on its own (ILM_OR_AUX_MODEL env or follows main).
	a.Client.AuxModel = a.Cfg.AuxModel

	// Per-turn reset: counsel cap and per-symptom dedup reset on each user
	// message so the cap is effectively per-turn in TUI mode. Only active when
	// CounselMode is explicitly set (TUI path); the headless AutoCounsel path
	// keeps a session-lifetime counter.
	if a.CounselMode != "" {
		a.counselCalls = 0
		a.struggleSuggested = nil
	}

	// Egress consent gate: before the first request in a session that would route
	// to an external backend, prompt the user. Gated even in /auto mode — the
	// SuspendAuto hook in tuiConfirmer ensures the prompt always fires.
	if a.SelectedBackend != "" && IsExternalBackend(a.BackendList, a.Cfg, a.SelectedBackend) {
		if a.consentedBackends == nil || !a.consentedBackends[a.SelectedBackend] {
			detail := fmt.Sprintf(
				"This session's context (memory, grounding, learned notes) will be sent to "+
					"external backend %q. Proceed?\n\n"+
					"(The proxy also enforces ILM_ALLOW_EXTERNAL; this gate makes the decision "+
					"visible at the moment it happens.)", a.SelectedBackend)
			if !a.Confirm("external_backend",
				"⚠ Send session context to external backend "+a.SelectedBackend+"?",
				detail, false) {
				prev := a.SelectedBackend
				a.SelectedBackend = ""
				a.Client.Backend = ""
				fmt.Fprintf(a.Out, "\n· backend %q declined — selection reverted to proxy default\n", prev)
				return "", nil
			}
			if a.consentedBackends == nil {
				a.consentedBackends = make(map[string]bool)
			}
			a.consentedBackends[a.SelectedBackend] = true
		}
	}

	// Persist on every exit path (success, stream error, or cancellation) so a
	// completed turn — and at minimum the user's message — is never lost.
	defer a.SaveSession()

	// Prepend the workflow phase directive so the model knows its current
	// obligation. The combined text is stored in Conv (not ephemeral like the
	// context preamble) so it participates in caching and compaction normally.
	stored := userText
	if a.Workflow != nil {
		if d := a.Workflow.Directive(); d != "" {
			stored = d + "\n\n" + userText
		}
	}
	// P36 downshift guard: if /backend switched to a smaller-context model since
	// the last turn, Conv may already exceed the new hard ceiling. Compact+drop
	// before appending the user message so we never deliver an over-window Conv.
	a.fitConvToWindow(ctx)

	a.Conv = append(a.Conv, proxy.Message{Role: "user", Content: StrPtr(stored)})

	// P38 trace: accumulate per-turn state written to the JSONL store on exit.
	// retErr is captured by the defer closure — it reflects whichever error path
	// (or nil on success) the function returns through.
	var (
		traceReasoningChars int
		traceToolCalls      []trace.ToolTrace
		traceTurnIndex      int
	)
	if a.Trace != nil {
		for _, m := range a.Conv {
			if m.Role == "user" {
				traceTurnIndex++
			}
		}
		defer func() {
			a.flushTraceTurn(traceTurnIndex, traceReasoningChars, traceToolCalls, retErr)
		}()
	}

	// Build a reasoning sink that always accumulates chars for the trace and
	// also forwards to OnReasoning when set (TUI path). Created once so the
	// counter is shared across all iterations of the tool loop.
	rsink := a.traceReasoningSink(&traceReasoningChars)

	var final string
	var turnToolBytes int
	firstStream := true
	for iter := 0; ; iter++ {
		// Hard backstop against runaway tool loops: on the final allowed iteration
		// drop the tools and force the model to answer from what it already has.
		// 0 = unlimited (the parent's default; a human gates each tool there).
		forceFinish := a.Cfg.MaxToolIterations > 0 && iter >= a.Cfg.MaxToolIterations
		tools := a.Tools
		if forceFinish {
			tools = nil
			a.Conv = append(a.Conv, proxy.Message{Role: "user", Content: StrPtr(ToolLimitPrompt)})
		}

		// Prepend a fresh context preamble at send time (never stored in Conv,
		// so it can't go stale, duplicate, or be compacted away).
		msgs := a.Conv
		if a.InjectDate {
			msgs = append([]proxy.Message{{Role: "system", Content: StrPtr(a.contextPreamble())}}, a.Conv...)
		}

		sink := a.streamSink()
		msg, err := a.Client.Stream(ctx, msgs, tools, sink, rsink)
		if err != nil {
			return "", err
		}
		a.RecordInferenceCost() // main inference for this iteration
		if firstStream {
			// Retrieval telemetry for the user's query is set by this first call;
			// log a learn candidate if retrieval ran but coverage was low.
			attempted, maxScore, _ := a.Client.GroundingState()
			if a.maybeLogLearnCandidate(userText, attempted, maxScore) {
				// Store the normalised query so runTurn can decide whether to nudge.
				a.learnNudgePending = strings.Join(strings.Fields(wtools.UserQueryText(userText)), " ")
			}
			firstStream = false
		}
		if DerefStr(msg.Content) != "" {
			fmt.Fprintln(a.Out)
		}
		if forceFinish {
			// Tools were stripped this turn; discard any the model emitted anyway so
			// no dangling tool_calls (without responses) are left in the transcript.
			msg.ToolCalls = nil
		}
		a.Conv = append(a.Conv, msg)
		final = DerefStr(msg.Content)

		if len(msg.ToolCalls) == 0 || forceFinish {
			break
		}
		for _, tc := range msg.ToolCalls {
			result := a.handleToolCall(ctx, tc)
			// Show a one-line summary (path/command + a result digest). The full
			// result still goes into the transcript below for the model to read.
			fmt.Fprintln(a.Out, Dim(toolLine(tc, result)))

			// Capture pre-cap size before CapOrStub so the trace reflects actual
			// tool output, not the truncated version the model sees.
			preCapBytes := len(result)
			if !a.RawTools {
				result = a.CapOrStub(result, tc.Function.Name, turnToolBytes)
			}
			if a.Trace != nil {
				traceToolCalls = append(traceToolCalls, trace.ToolTrace{
					Name:         tc.Function.Name,
					PreCapBytes:  preCapBytes,
					PostCapBytes: len(result),
					Capped:       len(result) != preCapBytes,
				})
			}

			turnToolBytes += len(result)
			a.Conv = append(a.Conv, proxy.Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content:    StrPtr(result),
			})
		}
		// After a round of tool calls, offer mashura__debug if the rolling trace
		// shows a struggle signal. In auto-counsel mode this fires the call
		// directly (up to MaxCounsel times); otherwise it only prints a hint.
		a.maybeSuggestDebug(ctx)
	}

	if ok, err := a.Compact(ctx, a.summarizeFn(), false); err == nil && ok {
		fmt.Fprintln(a.Out, Dim("· compacted earlier turns into a summary"))
	}
	_, _, hm := a.activeThresholds()
	a.enforceHardMax(ctx, hm)
	a.WarnContextPressure()
	return final, nil
}

// traceReasoningSink returns a Sink that accumulates reasoning_content chars
// into *chars for the turn's trace record AND forwards to a.OnReasoning when
// set. When tracing is off (a.Trace == nil), falls back to a.reasoningSink()
// so the TUI reasoning display is unaffected.
func (a *App) traceReasoningSink(chars *int) proxy.Sink {
	if a.Trace == nil {
		return a.reasoningSink()
	}
	onR := a.OnReasoning
	return func(s string) {
		*chars += len(s)
		if onR != nil {
			onR(s)
		}
	}
}

// flushTraceTurn writes the per-turn JSONL record to a.Trace. Called via defer
// from Send so it always fires, even on error. sendErr is the value of retErr
// captured at the defer site.
func (a *App) flushTraceTurn(turnIdx, reasoningChars int, toolCalls []trace.ToolTrace, sendErr error) {
	u := a.Client.LastUsage()
	turnType := "final"
	if len(toolCalls) > 0 {
		turnType = "tool_loop"
	}
	outcome := "complete"
	if sendErr != nil {
		outcome = "stream_error"
	} else if IsEmptyTurn(a.Conv) {
		outcome = "empty"
	}
	var grounding []string
	for _, g := range a.Client.Grounding() {
		grounding = append(grounding, g.Type+":"+g.Label)
	}
	a.Trace.Write(trace.Record{
		Type:            "turn",
		SessionID:       a.Client.ChatID,
		TurnIndex:       turnIdx,
		TurnType:        turnType,
		ReasoningChars:  reasoningChars,
		ToolCalls:       toolCalls,
		Backend:         a.Client.LastUsedBackend(),
		InputTokens:     u.InputTok,
		OutputTokens:    u.OutputTok,
		ReasoningTokens: u.ReasoningTok,
		Outcome:         outcome,
		Grounding:       grounding,
	})
}

// warnContextPressure emits a one-line notice when the real prompt occupancy
// (the backend's last prompt_tokens) crosses the usable budget — n_ctx minus the
// reasoning and answer headroom. This catches pressure the byte-based compaction
// can't see: the system prompt, tool schemas, and injected retrieval all consume
// the window without growing the stored transcript. It keys off the usable number
// (never raw n_ctx) and fires at most once per crossing, re-arming when occupancy
// falls back under the budget. No-op when occupancy is only an estimate (the
// proxy reported no usage), so it never cries wolf on a length guess.
func (a *App) WarnContextPressure() {
	lim := a.ContextLimit()
	usable := lim.Usable()
	used := a.ContextTokensUsed()
	if usable <= 0 || used < usable {
		a.CtxPressureWarned = false
		return
	}
	// Only warn on a measured occupancy, not the ~4-chars/token fallback estimate.
	if a.Client == nil || a.Client.LastUsage().InputTok == 0 {
		return
	}
	if a.CtxPressureWarned {
		return
	}
	a.CtxPressureWarned = true
	fmt.Fprintln(a.Out, Yellow(fmt.Sprintf(
		"⚠ context pressure: ~%dk of %dk usable tokens in use (n_ctx %dk) — consider /compact or a fresh chat",
		used/1000, usable/1000, lim.NCtx/1000)))
}

// ToolLimitPrompt is injected on the final allowed iteration when MaxToolIterations
// is reached; tools are dropped for that turn so the model must answer with this.
const ToolLimitPrompt = "You have reached the tool-call limit for this turn. Stop calling tools and produce your final response now, using only the information you already have."

// RecordInferenceCost records the most recent Stream call's token usage. When
// the proxy reports which backend handled the request (X-Ilm-Backend-Used),
// the source key is split per-backend: external backends use ConfExact (real
// provider tokens) and per-backend/model pricing; local backends use ConfModeled
// with the configured compute rate. When no backend is known the legacy
// "inference" aggregate key is used for backward compatibility.
// nil tracker / client → no-op.
func (a *App) RecordInferenceCost() {
	if a.Costs == nil || a.Client == nil {
		return
	}
	u := a.Client.LastUsage()
	if u.InputTok == 0 && u.OutputTok == 0 {
		return // nothing measured (e.g. injected/test client)
	}

	usedBackend := a.Client.LastUsedBackend()
	isExternal := usedBackend != "" && IsExternalBackend(a.BackendList, a.Cfg, usedBackend)

	// When the model field already carries the backend prefix (e.g.
	// "openrouter/anthropic/claude-opus-4-8"), strip it so the cost key is
	// "openrouter/anthropic/claude-opus-4-8" rather than the doubled
	// "openrouter/openrouter/anthropic/claude-opus-4-8".
	modelForCost := a.Client.Model
	if isExternal && strings.HasPrefix(modelForCost, usedBackend+"/") {
		modelForCost = strings.TrimPrefix(modelForCost, usedBackend+"/")
	}

	var source string
	switch {
	case usedBackend == "":
		source = proxy.CostSourceInference // no backend routing; legacy aggregate
	case isExternal:
		source = proxy.CostSourceInfPrefix + usedBackend + "/" + modelForCost
	default:
		source = proxy.CostSourceInfPrefix + usedBackend
	}

	var usd float64
	var priced bool
	if isExternal {
		usd, priced = a.Cfg.Costs.ExternalInferenceCost(usedBackend+"/"+modelForCost, u.InputTok, u.OutputTok)
	} else {
		usd, priced = a.Cfg.Costs.InferenceCost(u.InputTok + u.OutputTok)
	}

	conf := proxy.ConfModeled
	if isExternal {
		if u.Exact {
			conf = proxy.ConfExact
		} else {
			conf = proxy.ConfApprox
		}
	} else if !u.Exact {
		conf = proxy.ConfApprox
	}

	a.Costs.Record(source, u.InputTok, u.OutputTok, usd, priced, conf)
}

// RecordOracleCostFor records one panel member's exact token usage under the
// per-model source key "mashura·<model>". Usage is exact (from the API
// response), so confidence is always ConfExact even when no rate is configured
// (cost then renders "—"). nil tracker → no-op.
func (a *App) RecordOracleCostFor(model string, u counsel.OracleUsage) {
	usd, priced := a.Cfg.Costs.MashuraCost(model, u.InputTokens, u.OutputTokens)
	source := proxy.CostSourceMashuraPrefix + model
	a.Costs.Record(source, u.InputTokens, u.OutputTokens, usd, priced, proxy.ConfExact)
}

// recordOracleCost records one oracle call using the legacy OracleModel field.
// Kept for callers that pre-date the multi-model panel; new code should use
// RecordOracleCostFor with an explicit model string.
func (a *App) RecordOracleCost(u counsel.OracleUsage) {
	a.RecordOracleCostFor(a.Cfg.OracleModel, u)
}

// recordSearchCost records one search query against the "search" source, priced
// per-query. Confidence is modeled. nil tracker → no-op.
func (a *App) RecordSearchCost() {
	usd, priced := a.Cfg.Costs.SearchCost()
	a.Costs.Record(proxy.CostSourceSearch, 0, 0, usd, priced, proxy.ConfModeled)
}

// contextPreamble builds the ephemeral system message prepended to every
// request. It leads with the static agent operating instructions (loaded once
// from AgentPromptPath at startup), then appends the per-turn context: current
// date/time, working directory, and available sandbox tools.
func (a *App) contextPreamble() string {
	date := "Current date and time: " + time.Now().Format("Monday, 2 January 2006, 15:04 MST") +
		". Treat this as the present moment for any reasoning about dates, recency, or current events."

	cwd := ""
	if a.Exec != nil {
		cwd = a.Exec.Cwd()
	} else if a.Cfg.WorkDir != "" {
		cwd = a.Cfg.WorkDir
	}

	var parts []string
	if a.AgentPrompt != "" {
		parts = append(parts, a.AgentPrompt)
	}
	parts = append(parts, date)
	if cwd != "" {
		loc := "Working directory: " + cwd
		if a.Cfg.ExecMode == "docker" && a.Cfg.HostWorkDir != "" && a.Cfg.HostWorkDir != cwd {
			loc += " (mounted from host: " + a.Cfg.HostWorkDir + ")"
		}
		parts = append(parts, loc+".")
	}
	// A2: sandbox tool inventory — lazy-probed once, cached on the executor.
	// SandboxTools() degrades to "" on failure; never panics.
	if a.Exec != nil {
		if tools := a.Exec.SandboxTools(); tools != "" {
			parts = append(parts, tools+".")
		}
	}
	return strings.Join(parts, "\n")
}

// streamSink returns the per-call SSE content sink. Beyond forwarding deltas to
// a.Out it estimates decode throughput (chars ÷ 4 ≈ tokens, over elapsed wall
// time) and reports it via a.OnTokRate, throttled to ~5 Hz. A fresh sink per
// loop iteration means the rate reflects the current assistant message, not a
// turn-wide average dragged down by tool-execution gaps.
func (a *App) streamSink() proxy.Sink {
	var chars int
	var start, lastEmit time.Time
	return func(s string) {
		fmt.Fprint(a.Out, s)
		if a.OnTokRate == nil {
			return
		}
		now := time.Now()
		if start.IsZero() {
			start = now
		}
		chars += len(s)
		if el := now.Sub(start).Seconds(); el >= 0.1 && now.Sub(lastEmit) >= 200*time.Millisecond {
			a.OnTokRate(float64(chars) / 4.0 / el)
			lastEmit = now
		}
	}
}

// agentPromptNote returns a one-line TUI status message describing where the
// agent system prompt was loaded from. Shown once at startup in the viewport.
func (a *App) AgentPromptNote() string {
	path := a.Cfg.AgentPromptPath
	if path == "" || a.AgentPrompt == "" {
		return "· agent prompt: built-in fallback (no path configured)"
	}
	return fmt.Sprintf("· agent prompt: %s (%d bytes)", path, len(a.AgentPrompt))
}

// reasoningSink returns a Sink that forwards reasoning deltas to OnReasoning.
// Returns nil when OnReasoning is not set (reasoning is silently discarded).
func (a *App) reasoningSink() proxy.Sink {
	if a.OnReasoning == nil {
		return nil
	}
	return func(s string) { a.OnReasoning(s) }
}

// capOrStub applies the per-turn budget gate to a single tool result.
// While the turn budget is not yet exhausted, the normal ToolResultCap applies.
// Once the budget is exhausted the result is fully spilled to disk and only a
// ~50-char pointer stub enters ctx — zero raw bytes, deterministic bound.
//
// PART-2 HOOK: budget exhaustion here is the natural dispatch_subagent trigger.
// Replace (or augment) stubToolResult with dispatch_subagent(result, toolName)
// so the subagent processes overflow content independently, keeping the parent
// ctx lean. The stub pointer becomes the handoff token.
func (a *App) CapOrStub(result, toolName string, turnToolBytesSoFar int) string {
	// Mashūra responses are precision-crafted answers; truncating them mid-sentence
	// produces a worse outcome than no counsel call at all — always keep in full.
	//
	// dispatch_subagent results are already a ≤4k structured JSON digest of dozens
	// of internal tool iterations — re-truncating or stubbing a digest discards
	// the work. Same exemption rationale.
	if wtools.IsMashuraTool(toolName) || wtools.IsSubagentResult(toolName) {
		return result
	}
	if a.Cfg.TurnToolBudget > 0 && turnToolBytesSoFar >= a.Cfg.TurnToolBudget {
		return wtools.StubToolResult(result, toolName, a.chatID())
	}
	return wtools.CapToolResult(result, toolName, a.chatID(), a.Cfg.ToolResultCap)
}

// evictStaleToolResults replaces the content of large tool-result messages
// that are older than ToolResultTTL turns with a compact stub. The model
// already extracted what it needed during the turn; keeping verbatim bytes
// in ctx beyond that wastes context budget.
//
// Only messages whose content exceeds ToolResultCap are touched — small
// results are cheap to keep. Eviction is skipped when ToolResultTTL < 0
// or ToolResultCap <= 0 (unlimited mode).
func (a *App) evictStaleToolResults() {
	ttl := a.Cfg.ToolResultTTL
	cap := a.Cfg.ToolResultCap
	if ttl < 0 || cap <= 0 {
		return
	}
	// Keep the most recent (ttl+1) user turns verbatim; evict tool results
	// in anything older. boundary==0 means not enough turns yet.
	boundary := turnBoundary(a.Conv, ttl+1)
	if boundary <= 0 {
		return
	}
	for i := range a.Conv[:boundary] {
		m := &a.Conv[i]
		if m.Role != "tool" || len(DerefStr(m.Content)) <= cap {
			continue
		}
		stub := wtools.MakeEvictionStub(m.Name, DerefStr(m.Content))
		m.Content = &stub
	}
}

// toolDedupKey builds a normalized cache key for (tool, args). Path arguments
// are resolved to an absolute, cleaned form so ".", "./", "/work" and trailing
// slashes all collapse to one entry; marshaling the parsed map also makes the
// key insensitive to argument key ordering and whitespace.
func (a *App) toolDedupKey(name, argsJSON string) string {
	var m map[string]interface{}
	if json.Unmarshal([]byte(argsJSON), &m) != nil {
		return name + "|" + strings.TrimSpace(argsJSON)
	}
	if p, ok := m["path"].(string); ok && p != "" {
		m["path"] = a.normPath(p)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return name + "|" + strings.TrimSpace(argsJSON)
	}
	return name + "|" + string(b)
}

// normPath resolves p against the workspace root (when relative) and cleans it.
// After the stateless-cwd change, the workspace root is always the project root.
func (a *App) normPath(p string) string {
	p = strings.TrimSpace(p)
	if a.Exec != nil && !filepath.IsAbs(p) {
		p = filepath.Join(a.Exec.WorkspaceRoot(), p)
	}
	return filepath.Clean(p)
}

func (a *App) handleToolCall(ctx context.Context, tc proxy.ToolCall) string {
	name := tc.Function.Name

	// Dedup: if an equivalent (tool, args) pair has been called before in this
	// session, skip re-execution. Prevents infinite loops in subagents where the
	// model keeps re-reading the same file. The key is normalized so trivial
	// path variants (".", "./", "/work", trailing slash) collapse to one entry.
	//
	// The key is recorded AFTER execution so declined or transiently-failed calls
	// can be retried — recording before would permanently poison them.
	var cacheKey string
	if a.ToolCache != nil {
		cacheKey = a.toolDedupKey(name, tc.Function.Arguments)
		if a.ToolCache[cacheKey] {
			return fmt.Sprintf("[already called %s with equivalent arguments — that result is already above. Do not repeat it: use what you have, try a different path/pattern, or produce your final answer.]", name)
		}
	}

	result := a.ExecuteToolCall(ctx, tc)

	// Capture tool-call evidence for the IMPLEMENT step trace and the rolling
	// cross-turn buffer. Called with the pre-cap result so evidence reflects
	// actual output size.
	a.captureToolTrace(tc, result)
	a.recordRecentTrace(tc, result)

	if cacheKey != "" &&
		!strings.HasPrefix(result, "[declined by user]") &&
		!strings.HasPrefix(result, "ERROR:") {
		a.ToolCache[cacheKey] = true
	}
	return result
}

// captureToolTrace appends a trace entry to WorkflowStepTrace when an IMPLEMENT
// step is in progress. No-op outside IMPLEMENT turns.
func (a *App) captureToolTrace(tc proxy.ToolCall, result string) {
	if a.Workflow == nil || a.Workflow.Phase != workflow.WFImplement {
		return
	}
	a.WorkflowStepTrace = append(a.WorkflowStepTrace, MakeTraceEntry(tc, result))
}

func MakeTraceEntry(tc proxy.ToolCall, result string) ToolTraceEntry {
	e := ToolTraceEntry{
		Abbrev: toolAbbrev(tc.Function.Name),
		OutputLen: len(result),
		ExitErr: strings.HasPrefix(result, "ERROR:") ||
			strings.Contains(result, "\nERROR:"),
	}
	// Extract the primary identifier (command or path).
	switch tc.Function.Name {
	case "run_shell":
		var a struct {
			Command string `json:"command"`
		}
		if json.Unmarshal([]byte(tc.Function.Arguments), &a) == nil {
			e.Command = a.Command
		}
	default:
		var a struct {
			Path    string `json:"path"`
			Pattern string `json:"pattern"`
		}
		if json.Unmarshal([]byte(tc.Function.Arguments), &a) == nil {
			e.Command = a.Path
			if e.Command == "" {
				e.Command = a.Pattern
			}
		}
	}
	// Extract first output line and a tail of output lines.
	lines := strings.Split(strings.TrimSpace(result), "\n")
	for _, l := range lines {
		if l = strings.TrimSpace(l); l != "" {
			e.FirstLine = Truncate(l, 80)
			break
		}
	}
	// run_shell: capture last 4 non-empty distinct lines so benchmark ns/op rows,
	// test summary lines, and error tails all appear in the evidence.
	// Other tools: one last line is sufficient.
	if tc.Function.Name == "run_shell" {
		var tail []string
		for i := len(lines) - 1; i >= 0; i-- {
			l := strings.TrimSpace(lines[i])
			if l == "" || l == e.FirstLine {
				continue
			}
			tail = append(tail, Truncate(l, 80))
			if len(tail) == 4 {
				break
			}
		}
		// tail is newest-first; reverse to chronological order.
		for i, j := 0, len(tail)-1; i < j; i, j = i+1, j-1 {
			tail[i], tail[j] = tail[j], tail[i]
		}
		e.LastLine = strings.Join(tail, "\n")
	} else {
		for i := len(lines) - 1; i >= 0; i-- {
			if l := strings.TrimSpace(lines[i]); l != "" && l != e.FirstLine {
				e.LastLine = Truncate(l, 80)
				break
			}
		}
	}
	// On failure, keep a generous tail for mashura__debug — the last ~15 non-empty
	// lines, each truncated, so a diagnosis sees more than the 4-line step-log cap.
	if e.ExitErr {
		var tail []string
		for i := len(lines) - 1; i >= 0 && len(tail) < 15; i-- {
			if l := strings.TrimSpace(lines[i]); l != "" {
				tail = append(tail, Truncate(l, 120))
			}
		}
		for i, j := 0, len(tail)-1; i < j; i, j = i+1, j-1 {
			tail[i], tail[j] = tail[j], tail[i]
		}
		e.ErrorTail = strings.Join(tail, "\n")
	}
	return e
}

// mashuraRecentTraceCap bounds the rolling tool-trace buffer used by
// mashura__debug and the struggle detector.
const mashuraRecentTraceCap = 24

// recordRecentTrace appends a trace entry to the rolling buffer (all tools, all
// phases), trimming to the most recent mashuraRecentTraceCap entries.
func (a *App) recordRecentTrace(tc proxy.ToolCall, result string) {
	// Don't record mashura calls themselves — they are not part of the work the
	// model is debugging, and counting them would pollute the struggle signal.
	if wtools.IsMashuraTool(tc.Function.Name) {
		return
	}
	a.recentTraces = append(a.recentTraces, MakeTraceEntry(tc, result))
	if n := len(a.recentTraces); n > mashuraRecentTraceCap {
		a.recentTraces = a.recentTraces[n-mashuraRecentTraceCap:]
	}
}

func toolAbbrev(name string) string {
	switch name {
	case "run_shell":
		return "shell"
	case "read_file":
		return "read"
	case "write_file":
		return "write"
	case "edit_file":
		return "edit"
	case "find_files":
		return "find"
	case "search_files":
		return "search"
	case "list_dir":
		return "list"
	case "dispatch_subagent":
		return "subagent"
	default:
		if len([]rune(name)) > 8 {
			return string([]rune(name)[:8])
		}
		return name
	}
}

func FormatTraceEntry(e ToolTraceEntry) string {
	status := "ok"
	if e.ExitErr {
		status = "EXIT≠0" // EXIT≠0
	}
	cmd := e.Command
	if len([]rune(cmd)) > 60 {
		cmd = string([]rune(cmd)[:57]) + "…"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "[ev] %s %s %s", e.Abbrev, status, compactBytes(e.OutputLen))
	if cmd != "" {
		fmt.Fprintf(&sb, " %q", cmd)
	}
	// Show output digest only when it adds information (not for path-echoing tools).
	if e.FirstLine != "" && e.FirstLine != cmd && e.FirstLine != e.Command {
		fmt.Fprintf(&sb, " → %q", e.FirstLine)
	}
	if e.LastLine != "" {
		fmt.Fprintf(&sb, " … %q", e.LastLine)
	}
	return sb.String()
}

func compactBytes(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%dKB", n/1024)
	default:
		return fmt.Sprintf("%dMB", n/(1024*1024))
	}
}

// formatStepEvidence formats the per-step tool trace into a block of [ev] lines
// capped at ~1 KB. When there are too many entries the middle is summarised so
// the most important entries — the first (context) and last (most recent result) —
// are preserved.
func FormatStepEvidence(trace []ToolTraceEntry) string {
	if len(trace) == 0 {
		return ""
	}
	const maxBytes = 1000

	lines := make([]string, len(trace))
	for i, e := range trace {
		lines[i] = FormatTraceEntry(e)
	}

	// Fast path: everything fits.
	full := strings.Join(lines, "\n")
	if len(full) <= maxBytes {
		return full
	}

	// Over budget: keep first 2 + last 4 entries, summarise the gap.
	const keepFront = 2
	const keepBack = 4
	if len(lines) > keepFront+keepBack {
		dropped := len(lines) - keepFront - keepBack
		kept := make([]string, 0, keepFront+1+keepBack)
		kept = append(kept, lines[:keepFront]...)
		kept = append(kept, fmt.Sprintf("[ev] … %d entries omitted …", dropped))
		kept = append(kept, lines[len(lines)-keepBack:]...)
		return strings.Join(kept, "\n")
	}

	// Very verbose individual lines: hard-cap with a trailing note.
	var sb strings.Builder
	for i, l := range lines {
		if sb.Len()+len(l)+1 > maxBytes {
			fmt.Fprintf(&sb, "\n[ev] … %d entries omitted", len(lines)-i)
			break
		}
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(l)
	}
	return sb.String()
}

// wfPhaseBlock returns a tool-error string when the current workflow phase
// prohibits the requested tool call, or "" if the call is allowed.
//
// Enforcement rules (in evaluation order):
//  1. ALL phases: write_file to plan.md is always rejected — the scaffold is
//     written once by Wakil; only targeted edit_file changes are permitted.
//  2. GATHER/PLAN/REVIEW/PRESENT: write_file and edit_file to paths outside
//     .wakil/ are rejected; run_background is rejected unconditionally.
func (a *App) wfPhaseBlock(toolName, argsJSON string) string {
	wf := a.Workflow
	if wf == nil {
		return ""
	}

	// Rule 1: write_file to plan.md is rejected in every workflow phase.
	// The scaffold is created once by Wakil; the model must use edit_file for
	// any subsequent modifications so the section structure is preserved.
	if toolName == "write_file" && wf.PlanPath != "" {
		var args struct {
			Path string `json:"path"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) == nil && workflow.IsPlanFilePath(args.Path, wf.PlanPath) {
			return "workflow: write_file on plan.md is not permitted — " +
				"use edit_file for targeted changes to preserve the workflow structure"
		}
	}

	// Rule 2: pre-IMPLEMENT writes and background processes.
	const writeRejected = ": implementation writes are not permitted before plan approval; finish this phase and emit %%PHASE_DONE%%"
	if workflow.IsPreImplementPhase(wf.Phase) {
		switch toolName {
		case "write_file", "edit_file":
			var args struct {
				Path string `json:"path"`
			}
			if json.Unmarshal([]byte(argsJSON), &args) == nil && args.Path != "" && !workflow.IsWakilPath(args.Path) {
				return "workflow phase " + wf.PhaseName() + writeRejected
			}
		case "run_background":
			return "workflow phase " + wf.PhaseName() + writeRejected
		}
	}

	return ""
}

func (a *App) ExecuteToolCall(ctx context.Context, tc proxy.ToolCall) string {
	name := tc.Function.Name

	// Phase enforcement: reject tool calls that violate the workflow write-containment
	// invariant. The error is a regular tool result so the model reads it and can
	// self-correct within the same turn.
	if a.Workflow != nil {
		if msg := a.wfPhaseBlock(name, tc.Function.Arguments); msg != "" {
			return msg
		}
	}

	switch name {
	case "run_shell":
		var args struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
		}
		// Read-only commands are auto-approved once the user has allowed reads;
		// otherwise prompt, offering the "allow all reads" choice for reads.
		readAction := IsReadOnlyShell(args.Command)

		// In pre-IMPLEMENT workflow phases, always show the confirm prompt —
		// AllowReads shortcut is suppressed so the user sees the phase warning.
		preImpl := a.Workflow != nil && workflow.IsPreImplementPhase(a.Workflow.Phase)
		detail := fmt.Sprintf("$ %s\n  (%s)", args.Command, a.Exec.Describe())
		if preImpl {
			detail = fmt.Sprintf("⚠ workflow phase: %s (read-only expected — is this command investigative?)\n%s",
				a.Workflow.PhaseName(), detail)
		}
		if preImpl || !(readAction && a.AllowReads) {
			if !a.Confirm("run_shell", "Run shell command?", detail, readAction) {
				return "[declined by user]"
			}
		}
		out, err := a.Exec.RunShell(ctx, args.Command)
		return formatResult(out, err)

	case "open_url":
		var args struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
		}
		// Always runs on the host (not via a.Exec), so it reaches the user's
		// desktop even when shell commands are sandboxed.
		detail := fmt.Sprintf("xdg-open %s\n  (on the host desktop)", args.URL)
		if !a.Confirm("open_url", "Open in browser?", detail, false) {
			return "[declined by user]"
		}
		out, err := wtools.OpenOnHost(args.URL)
		result := formatResult(out, err)
		if !strings.HasPrefix(result, "ERROR:") {
			label := args.URL
			label = Truncate(label, 79)
			a.Client.AddGrounding(proxy.GroundingEntry{Type: "web", Label: label})
		}
		return result

	case "read_file":
		var args struct {
			Path   string `json:"path"`
			Offset int    `json:"offset"`
			Limit  int    `json:"limit"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
		}
		out, err := a.Exec.ReadFile(args.Path)
		// Redirect a directory read to the right tool instead of returning a raw
		// errno the model tends to retry against (a known subagent loop trigger).
		if err != nil && strings.Contains(strings.ToLower(err.Error()), "is a directory") {
			return fmt.Sprintf("ERROR: %q is a directory, not a file — use list_dir to see its contents or search_files to search within it.", args.Path)
		}
		if err != nil {
			return formatResult(out, err)
		}
		return formatFileView(out, args.Offset, args.Limit)

	case "list_dir":
		var args struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
		}
		out, err := a.Exec.ListDir(args.Path)
		return formatResult(out, err)

	case "find_files":
		var args struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
		}
		if args.Pattern == "" {
			return "ERROR: pattern is required"
		}
		path := args.Path
		if path == "" {
			path = "."
		}
		// Constrained find: model-supplied values are single-quoted so no shell
		// metacharacters leak. Errors (permission denied) are dropped and the
		// list is capped so a huge tree can't flood ctx.
		const findCap = 200
		cmd := "find " + shellQuote(path) + " -type f -name " + shellQuote(args.Pattern) +
			fmt.Sprintf(" 2>/dev/null | head -n %d", findCap)
		out, err := a.Exec.RunShell(ctx, cmd)
		if err == nil && strings.TrimSpace(out) == "" {
			return "(no files found)"
		}
		if n := strings.Count(strings.TrimRight(out, "\n"), "\n") + 1; n >= findCap {
			out = strings.TrimRight(out, "\n") + fmt.Sprintf("\n… [capped at %d files — narrow the pattern or path]", findCap)
		}
		return formatResult(out, err)

	case "edit_file":
		return a.handleEditFile(tc)

	case "search_files":
		var args struct {
			Pattern         string `json:"pattern"`
			Path            string `json:"path"`
			FilePattern     string `json:"file_pattern"`
			CaseInsensitive bool   `json:"case_insensitive"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
		}
		if args.Pattern == "" || args.Path == "" {
			return "ERROR: pattern and path are required"
		}
		// Build a controlled grep command. All model-supplied values are
		// single-quoted so the model cannot inject shell metacharacters.
		cmd := "grep -rn"
		if args.CaseInsensitive {
			cmd += " -i"
		}
		if args.FilePattern != "" {
			cmd += " --include=" + shellQuote(args.FilePattern)
		}
		cmd += " -- " + shellQuote(args.Pattern) + " " + shellQuote(args.Path)
		out, err := a.Exec.RunShell(ctx, cmd)
		// grep exits 1 when it finds zero matches — not an error.
		if err != nil && strings.TrimSpace(out) == "" && strings.Contains(err.Error(), "exit status 1") {
			return "(no matches)"
		}
		return formatResult(out, err)

	case "write_file":
		var args struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
		}
		preview := args.Content
		if len(preview) > 280 {
			preview = preview[:280] + "…"
		}
		detail := fmt.Sprintf("write_file %s (%d bytes) in %s\n--- content ---\n%s",
			args.Path, len(args.Content), a.Exec.Describe(), preview)
		if !a.Confirm("write_file", "Write file?", detail, false) {
			return "[declined by user]"
		}
		out, err := a.Exec.WriteFile(args.Path, args.Content)
		return formatResult(out, err)

	case "searxng_search":
		var args struct {
			Query      string `json:"query"`
			Categories string `json:"categories"`
			TimeRange  string `json:"time_range"`
			Engines    string `json:"engines"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
		}
		result, urls := wtools.CallSearxng(a.Cfg.SearXngURL, args.Query, args.Categories, args.TimeRange, args.Engines)
		a.RecordSearchCost()
		for i, u := range urls {
			if i >= 5 {
				break
			}
			label := u
			label = Truncate(label, 79)
			a.Client.AddGrounding(proxy.GroundingEntry{Type: "web", Label: label})
		}
		return result

	case "searxng_url_read":
		var args struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
		}
		result := wtools.FetchURL(args.URL)
		if !strings.HasPrefix(result, "ERROR:") {
			label := args.URL
			label = Truncate(label, 79)
			a.Client.AddGrounding(proxy.GroundingEntry{Type: "web", Label: label})
		}
		return result

	case "google_search":
		var args struct {
			Query  string `json:"query"`
			Num    int    `json:"num"`
			Start  int    `json:"start"`
			After  string `json:"after"`
			Before string `json:"before"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
		}
		result, urls := wtools.CallGoogle(a.Cfg.GoogleAPIKey, a.Cfg.GoogleCX, args.Query, args.Num, args.Start, args.After, args.Before)
		a.RecordSearchCost()
		for i, u := range urls {
			if i >= 5 {
				break
			}
			label := Truncate(u, 79)
			a.Client.AddGrounding(proxy.GroundingEntry{Type: "web", Label: label})
		}
		return result

	case "google_fetch_url":
		var args struct {
			URL      string `json:"url"`
			MaxChars int    `json:"max_chars"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
		}
		result := wtools.GoogleFetchURL(args.URL, args.MaxChars)
		if !strings.HasPrefix(result, "ERROR:") {
			label := Truncate(args.URL, 79)
			a.Client.AddGrounding(proxy.GroundingEntry{Type: "web", Label: label})
		}
		return result

	case "delete_file":
		var args struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
		}
		if args.Path == "" {
			return "ERROR: path is required"
		}
		canonical, err := a.Exec.ConfinePath(ctx, args.Path)
		if err != nil {
			return "ERROR: " + err.Error()
		}
		rel, _ := filepath.Rel(a.Exec.WorkspaceRoot(), canonical)
		detail := fmt.Sprintf("Delete file: %s\n  (%s)", rel, a.Exec.Describe())
		if !a.Confirm("delete_file", "Delete file?", detail, false) {
			return "[declined by user]"
		}
		if err := a.Exec.DeletePath(ctx, canonical); err != nil {
			return "ERROR: " + err.Error()
		}
		return "deleted: " + rel

	case "move_file":
		var args struct {
			Src string `json:"src"`
			Dst string `json:"dst"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
		}
		if args.Src == "" || args.Dst == "" {
			return "ERROR: src and dst are required"
		}
		canonSrc, err := a.Exec.ConfinePath(ctx, args.Src)
		if err != nil {
			return "ERROR: src — " + err.Error()
		}
		canonDst, err := a.Exec.ConfinePath(ctx, args.Dst)
		if err != nil {
			return "ERROR: dst — " + err.Error()
		}
		root := a.Exec.WorkspaceRoot()
		relSrc, _ := filepath.Rel(root, canonSrc)
		relDst, _ := filepath.Rel(root, canonDst)
		detail := fmt.Sprintf("Move: %s → %s\n  (%s)", relSrc, relDst, a.Exec.Describe())
		if !a.Confirm("move_file", "Move file?", detail, false) {
			return "[declined by user]"
		}
		if err := a.Exec.MovePath(ctx, canonSrc, canonDst); err != nil {
			return "ERROR: " + err.Error()
		}
		return fmt.Sprintf("moved: %s → %s", relSrc, relDst)

	case "run_background":
		var args struct {
			Command string `json:"command"`
			Label   string `json:"label"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
		}
		if args.Command == "" {
			return "ERROR: command is required"
		}
		if args.Label == "" {
			args.Label = "bg"
		}
		if a.bgProcs == nil {
			a.bgProcs = make(map[string]*bgEntry)
		}
		// Count live processes (those matching the current generation).
		live := 0
		for _, e := range a.bgProcs {
			if e.generation == a.Exec.Generation() && a.Exec.IsProcessAlive(ctx, e.pid) {
				live++
			}
		}
		if live >= 5 {
			return "ERROR: maximum of 5 concurrent background processes reached — kill one first"
		}
		a.bgCounter++
		n := a.bgCounter
		logPath := fmt.Sprintf("/tmp/wakil-bg-%d.log", n)
		if a.Cfg.ExecMode == "direct" {
			logPath = filepath.Join(os.TempDir(), fmt.Sprintf("wakil-bg-%d.log", n))
		}
		bgID := fmt.Sprintf("bg%d", n)
		detail := fmt.Sprintf("$ %s (background)\n  label=%s, log=%s\n  (%s)",
			args.Command, args.Label, logPath, a.Exec.Describe())
		if !a.Confirm("run_background", "Start background process?", detail, false) {
			a.bgCounter-- // reclaim the counter slot on decline
			return "[declined by user]"
		}
		pid, pgid, err := a.Exec.StartBackground(ctx, args.Command, logPath)
		if err != nil {
			a.bgCounter--
			return "ERROR: " + err.Error()
		}
		a.bgProcs[bgID] = &bgEntry{
			id:         bgID,
			pid:        pid,
			pgid:       pgid,
			label:      args.Label,
			logPath:    logPath,
			startedAt:  time.Now(),
			generation: a.Exec.Generation(),
		}
		return fmt.Sprintf("id: %s\npid: %d\nlog: %s\nlabel: %s", bgID, pid, logPath, args.Label)

	case "kill_process":
		var args struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
		}
		entry, ok := a.bgProcs[args.ID]
		if !ok {
			return fmt.Sprintf("ERROR: no background process with id %q", args.ID)
		}
		if entry.generation != a.Exec.Generation() {
			delete(a.bgProcs, args.ID)
			return fmt.Sprintf("[%s] process lost (container restarted)", args.ID)
		}
		detail := fmt.Sprintf("kill_process %s (%s) pgid=%d\n  (%s)", args.ID, entry.label, entry.pgid, a.Exec.Describe())
		if !a.Confirm("kill_process", "Kill background process?", detail, false) {
			return "[declined by user]"
		}
		if !a.Exec.IsProcessAlive(ctx, entry.pid) {
			delete(a.bgProcs, args.ID)
			return fmt.Sprintf("[%s] already exited", args.ID)
		}
		_ = a.Exec.KillPgid(ctx, entry.pgid, 15) // SIGTERM
		// Wait up to 5s for the group to exit, then SIGKILL.
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			time.Sleep(200 * time.Millisecond)
			if !a.Exec.IsProcessAlive(ctx, entry.pid) {
				delete(a.bgProcs, args.ID)
				return fmt.Sprintf("[%s] terminated (SIGTERM)", args.ID)
			}
		}
		_ = a.Exec.KillPgid(ctx, entry.pgid, 9) // SIGKILL
		delete(a.bgProcs, args.ID)
		return fmt.Sprintf("[%s] killed (SIGKILL after 5s timeout)", args.ID)

	case "read_process_log":
		var args struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
		}
		entry, ok := a.bgProcs[args.ID]
		if !ok {
			return fmt.Sprintf("ERROR: no background process with id %q", args.ID)
		}
		if entry.generation != a.Exec.Generation() {
			delete(a.bgProcs, args.ID)
			return fmt.Sprintf("[%s] process lost (container restarted)", args.ID)
		}
		alive := a.Exec.IsProcessAlive(ctx, entry.pid)
		status := "running"
		if !alive {
			status = "exited"
		}
		header := fmt.Sprintf("[%s %s] %s pid=%d\n", args.ID, entry.label, status, entry.pid)
		const maxLogBytes = 8 * 1024
		tail, err := a.Exec.ReadFileTail(ctx, entry.logPath, maxLogBytes)
		if err != nil {
			return header + "(log not yet available)"
		}
		// Enforce hard cap: header + tail must not exceed 8KB + small overhead.
		result := header + tail
		if len(result) > maxLogBytes+256 {
			result = result[:maxLogBytes+256]
		}
		return result

	case "dispatch_subagent":
		var args struct {
			Task string `json:"task"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
		}
		if args.Task == "" {
			return "ERROR: task is required"
		}
		fmt.Fprintln(a.Out, Dim("· subagent: "+Truncate(args.Task, 60)))
		subChatID := NewChatID()
		subBackend := ResolveSubagentBackend(a.SelectedBackend, a.Cfg.SubagentBackend)
		a.sendEvent(SubagentStartMsg{Task: args.Task, ChatID: subChatID, Backend: subBackend})
		summary, grounding, ctxSize, usedBackend := a.dispatchSubagent(ctx, args.Task, subagentProgressOut(a), subBackend, subChatID)
		a.sendEvent(SubagentDoneMsg{
			Grounding:    grounding,
			CtxSize:      ctxSize,
			HardMaxBytes: subagentHardMaxBytes,
			UsedBackend:  usedBackend,
		})
		return summary.Render()

	// The mashura__* counsel family (and the legacy oracle__ask alias) all route
	// through one handler: the model supplies intent, Wakil deterministically
	// assembles the briefing. Each is gated and never auto-approved.
	case "mashura__review", "mashura__debug", "mashura__decide", "mashura__check", "oracle__ask":
		return a.handleMashura(ctx, name, tc)

	default:
		// MCP tool — namespaced as "{server}__{tool}".
		// URL extraction from arbitrary MCP result payloads would require a fragile
		// scraper; emit one opaque provenance entry per successful call instead.
		if a.MCP != nil && strings.Contains(name, mcpNS) {
			result := a.MCP.CallTool(ctx, name, tc.Function.Arguments, a.Confirm, a.AllowReads)
			if !strings.HasPrefix(result, "ERROR:") && result != "[declined by user]" {
				label := Truncate(name+" result", 79)
				a.Client.AddGrounding(proxy.GroundingEntry{Type: "web", Label: label})
			}
			return result
		}
		return fmt.Sprintf("ERROR: unknown tool %q", name)
	}
}

// stopAllBackgroundProcs sends SIGTERM to all live background process groups and
// waits up to 2 seconds as a grace period before returning. Called on shutdown.
// In docker mode the container removal (Close) will kill everything anyway;
// this is primarily meaningful for direct mode.
func (a *App) StopAllBackgroundProcs() {
	if len(a.bgProcs) == 0 {
		return
	}
	bg := context.Background()
	any := false
	for _, entry := range a.bgProcs {
		if entry.generation != a.Exec.Generation() {
			continue
		}
		if a.Exec.IsProcessAlive(bg, entry.pid) {
			_ = a.Exec.KillPgid(bg, entry.pgid, 15)
			any = true
		}
	}
	if any {
		time.Sleep(2 * time.Second)
	}
}

func formatResult(out string, err error) string {
	out = strings.TrimRight(out, "\n")
	if err != nil {
		if out == "" {
			return "ERROR: " + err.Error()
		}
		return out + "\nERROR: " + err.Error()
	}
	if out == "" {
		return "(no output)"
	}
	return out
}

// formatFileView renders file content with cat -n style line numbers, optionally
// restricted to a line range. offset is 1-based; limit caps the line count from
// offset. When a range is applied a "[lines a-b of N]" header is prepended so the
// model knows it is seeing a partial view.
func formatFileView(content string, offset, limit int) string {
	lines := strings.Split(content, "\n")
	// A trailing newline yields a final empty element; drop it so the line count
	// matches what an editor would show.
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	total := len(lines)
	if total == 0 {
		return "(empty file)"
	}
	start := 0
	if offset > 0 {
		start = offset - 1
	}
	if start >= total {
		return fmt.Sprintf("(offset %d is past end of file — file has %d lines)", offset, total)
	}
	end := total
	if limit > 0 && start+limit < end {
		end = start + limit
	}
	var b strings.Builder
	if start > 0 || end < total {
		fmt.Fprintf(&b, "[lines %d-%d of %d]\n", start+1, end, total)
	}
	for i := start; i < end; i++ {
		fmt.Fprintf(&b, "%6d\t%s\n", i+1, lines[i])
	}
	return strings.TrimRight(b.String(), "\n")
}

// handleEditFile applies an exact-substring replacement to a file, gated behind a
// confirmation that shows a -/+ diff preview. old_string must be present (and
// unique unless replace_all) or a corrective error is returned for the model.
func (a *App) handleEditFile(tc proxy.ToolCall) string {
	var args struct {
		Path       string `json:"path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	if args.Path == "" || args.OldString == "" {
		return "ERROR: path and old_string are required"
	}
	if args.OldString == args.NewString {
		return "ERROR: old_string and new_string are identical — nothing to change"
	}
	cur, err := a.Exec.ReadFile(args.Path)
	if err != nil {
		return "ERROR: could not read " + args.Path + ": " + err.Error()
	}
	count := strings.Count(cur, args.OldString)
	if count == 0 {
		return fmt.Sprintf("ERROR: old_string not found in %s — read the file and copy the exact text (including whitespace, without the line-number prefix).", args.Path)
	}
	if count > 1 && !args.ReplaceAll {
		return fmt.Sprintf("ERROR: old_string appears %d times in %s — add surrounding context to make it unique, or set replace_all=true.", count, args.Path)
	}
	var updated string
	if args.ReplaceAll {
		updated = strings.ReplaceAll(cur, args.OldString, args.NewString)
	} else {
		updated = strings.Replace(cur, args.OldString, args.NewString, 1)
	}

	if !a.Confirm("edit_file", "Apply edit?", editDiffPreview(args.Path, args.OldString, args.NewString, count, args.ReplaceAll, a.Exec.Describe()), false) {
		return "[declined by user]"
	}
	out, err := a.Exec.WriteFile(args.Path, updated)
	if err != nil {
		return formatResult(out, err)
	}
	n := 1
	if args.ReplaceAll {
		n = count
	}
	return fmt.Sprintf("edited %s — %d replacement(s)", args.Path, n)
}

// editDiffPreview renders the confirm-gate detail for edit_file as a compact
// -/+ hunk (each side truncated so a huge replacement can't flood the prompt).
func editDiffPreview(path, oldS, newS string, count int, all bool, execDesc string) string {
	trunc := func(s string) string {
		if len(s) > 400 {
			return s[:400] + "…"
		}
		return s
	}
	scope := "1 occurrence"
	if all {
		scope = fmt.Sprintf("all %d occurrences", count)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "edit_file %s (%s) in %s\n", path, scope, execDesc)
	for _, ln := range strings.Split(trunc(oldS), "\n") {
		b.WriteString("- " + ln + "\n")
	}
	for _, ln := range strings.Split(trunc(newS), "\n") {
		b.WriteString("+ " + ln + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// toolLine renders a single-line summary of a tool call and its result, e.g.
//
//	· read_file main.go → 412 lines · 9.8KB
//	· run_shell tail -n200 app.log → 200 lines · 14.1KB
//	· run_shell pwd → /root
//
// The full result is still recorded in the transcript for the model; this only
// governs what the user sees, so large outputs (file reads, log tails, command
// output) collapse to one line instead of flooding the conversation.
func toolLine(tc proxy.ToolCall, result string) string {
	head := tc.Function.Name
	if arg := toolPrimaryArg(tc); arg != "" {
		head += " " + arg
	}
	return "· " + head + " → " + resultSummary(result)
}

// toolPrimaryArg pulls the most meaningful argument from a tool call (the file
// path, shell command, search query, …) for the summary line, flattened to a
// single line and truncated.
func toolPrimaryArg(tc proxy.ToolCall) string {
	var m map[string]interface{}
	if json.Unmarshal([]byte(tc.Function.Arguments), &m) != nil {
		return ""
	}
	for _, k := range []string{"path", "command", "query", "url", "pattern", "file"} {
		if v, ok := m[k].(string); ok && v != "" {
			return Truncate(strings.Join(strings.Fields(v), " "), 50)
		}
	}
	return ""
}

// resultSummary digests a tool result for the one-line view: a short single-line
// result is shown verbatim; anything larger collapses to a line/size count.
// Declines and errors are flagged rather than dumped.
func resultSummary(result string) string {
	r := strings.TrimRight(result, "\n")
	switch {
	case r == "[declined by user]":
		return "declined"
	case r == "" || r == "(no output)":
		return "ok"
	case strings.HasPrefix(r, "ERROR:"):
		return "✗ " + Truncate(firstLine(strings.TrimSpace(r[len("ERROR:"):])), 60)
	}
	lines := strings.Count(r, "\n") + 1
	if lines == 1 && len([]rune(r)) <= 60 {
		return r // short scalar result — the value is more useful than a count
	}
	unit := "lines"
	if lines == 1 {
		unit = "line"
	}
	summary := fmt.Sprintf("%d %s · %s", lines, unit, humanBytes(len(result)))
	if strings.Contains(r, "\nERROR:") {
		summary += " ✗"
	}
	return summary
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

func humanBytes(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1fKB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1fMB", float64(n)/(1024*1024))
	}
}
