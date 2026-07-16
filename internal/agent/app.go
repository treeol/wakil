package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/counsel"
	"github.com/treeol/wakil/internal/exec"
	"github.com/treeol/wakil/internal/lsp"
	"github.com/treeol/wakil/internal/memory"
	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/staging"
	wtools "github.com/treeol/wakil/internal/tools"
	"github.com/treeol/wakil/internal/trace"
	"github.com/treeol/wakil/internal/workflow"
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

	// AllowDestructive, when true alongside AutoApprove, auto-approves
	// destructive shell commands (rm, mv, git reset, …) instead of suspending
	// auto mode for them. Toggled by /auto destructive — a separate explicit
	// opt-in, mirroring headless --allow-destructive. Cleared whenever /auto
	// is switched off so the grant never outlives the auto session it was
	// given for. Has no effect outside auto mode. The external-backend egress
	// gate is NOT covered — that always prompts.
	AllowDestructive bool

	// IsHeadless marks wakil-run sessions where no human is present to re-send a
	// failed turn. Backend stream errors are retried automatically regardless of
	// workflow phase. Set by RunHeadless before the first Send call.
	IsHeadless bool

	// IsSubagent marks dispatch_subagent child Apps. Distinct from ToolCache,
	// which exists for deduplication and is not a reliable presence signal.
	IsSubagent bool

	// AgentPrefix is the staging key prefix for this agent ("main" or "sub-<id>").
	// The staging tool layer unconditionally prepends this to the agent-supplied
	// key on staging_put and staging_delete. Set by the main agent ("main") and
	// by dispatchSubagent ("sub-" + first 8 chars of the child's ChatID).
	AgentPrefix string

	// StagingClient is the kvr wire protocol client, or nil if kvr is
	// unavailable (disabled, direct mode, or failed readiness). When nil,
	// all staging_* tools return "staging unavailable". Set by the host
	// startup code after the executor is ready.
	StagingClient *staging.Client

	// MemoryStore is the durable host-side memory store, or nil if
	// unavailable (init failed, no workspace). When nil, all memory_*
	// tools return "memory unavailable". Shared between parent and
	// subagents (thread-safe via internal mutex). Set by the host startup
	// code after the workspace is resolved.
	MemoryStore *memory.Store

	// touchedExternal is a sticky per-App flag set when the agent's
	// grounding records web/oracle content. Used for the session-cumulative
	// taint signal (A1): once the agent touches untrusted external content,
	// all subsequent memory writes are tainted=true. Never reset.
	touchedExternal bool

	// exhausted is set by Send when the subagent hit MaxToolIterations
	// (forceFinish) or enforceHardMax dropped content during the turn. It is
	// read by dispatchSubagent after Send returns to produce a truthful
	// Status:"incomplete" summary instead of relying on the model's final
	// response — which may be a lobotomized generic message if compaction
	// fired. Only meaningful for subagents; the parent ignores it.
	exhausted bool

	// stopReason records why the subagent stopped, set at the exact site where
	// exhaustion occurs. Values: "iteration_limit", "hard_max_shed",
	// "confinement_breaker". Empty = no stop reason (normal completion).
	// Captured before the retry Send (which resets it) and ORed across both
	// Sends, exactly like exhausted. Only meaningful for subagents.
	stopReason string

	// turnBudgetStubbed is a sticky per-App flag set inside CapOrStub when the
	// per-turn tool budget is exhausted and a result is stubbed to a spill
	// pointer. Used by dispatchSubagent to set StopReason="turn_budget_exhausted"
	// when no other stop reason fired (the model may stop naturally after being
	// starved of content, without hitting the iteration cap).
	turnBudgetStubbed bool

	// filesChanged is the recorder for edit-tier subagents: tracks canonical
	// paths touched by successful edit-category tool calls during the child's
	// Send loop. nil for the parent and discovery-tier children. Populated by
	// ExecuteToolCall when it detects an edit-tool success; read by
	// dispatchSubagent after Send returns to produce the mechanical
	// files_changed list on the done message.
	filesChanged *filesChangedRecorder

	// externalActions is the recorder for tools-tier subagents: tracks every
	// MCP tool call the child makes (server, tool, status). nil for the parent
	// and discovery/edit-tier children. Populated by ExecuteToolCall when it
	// routes an MCP tool call; read by dispatchSubagent after Send returns to
	// fold the mechanical external_calls list into the summary.
	externalActions *externalActionsRecorder

	// confinementTripped is set by Send when the path-confinement circuit
	// breaker fires: confinementBreakerThreshold consecutive ConfinePath
	// rejections within the turn. ConfinePath failures are a deterministic
	// error class — the same path fails identically on every retry — so this
	// is distinct from generic budget exhaustion: the model is stopped early
	// (well before MaxToolIterations) and told plainly which path(s) are
	// unreachable, rather than being left to burn its whole iteration budget
	// retrying a doomed path. Read by dispatchSubagent to attach a precise
	// "inaccessible" Skipped entry. confinementPathsHit holds the distinct
	// path arguments observed during the tripped streak, for reporting.
	confinementTripped  bool
	confinementPathsHit []string

	// pinUserMessage marks the user message appended by Send as Pinned, so it
	// survives compaction and hard-max dropping. Set by dispatchSubagent for
	// the subagent's task instruction — the subagent must never forget its own
	// task mid-run. The parent does not set this.
	pinUserMessage bool

	// subMaxToolIter overrides the subagentMaxToolIter constant when non-zero.
	// Used by tests to force exhaustion through the real dispatchSubagent path
	// without changing the package-level constant. Zero = use the constant.
	subMaxToolIter int

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

	// InjectDate keeps a day-stable system message (agent prompt + date + cwd
	// + tool inventory) as Conv[0], so the model doesn't fall back to its
	// training-era guess about "now". Enabled for the parent; off for
	// subagents and tests. See ensurePreamble.
	InjectDate bool

	// preambleDay is the calendar day (format "Monday, 2 January 2006") that
	// Conv[0]'s content currently reflects. Empty means "no preamble stored
	// yet" (fresh App, or Conv was just reset by NewConversation). Read/written
	// only by ensurePreamble, called once per turn at Send entry — never per
	// tool-loop iteration — so the request's leading bytes are byte-identical
	// across every Stream call within one calendar day, the single largest
	// lever on prompt-cache prefix stability (see internal/proxy/client.go
	// Stream: this is messages[0], the earliest possible byte position).
	preambleDay string

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
	// bgMu protects bgProcs and bgCounter. Written in turn handlers (run_background,
	// kill_process, read_process_log), read in shutdown (StopAllBackgroundProcs).
	// Do NOT hold the lock while waiting on process exit — copy references under
	// lock, then signal/wait outside.
	bgMu      sync.RWMutex
	bgProcs   map[string]*bgEntry
	bgCounter int
	bgLogDir  string // per-session temp dir for bg process logs; cleaned up in StopAllBackgroundProcs

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
	// the TUI program's Send; nil in tests that don't need TUI events.
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

	// StartupNote, when non-empty, is a message the TUI's Init() should emit
	// as a SysNoteMsg on the very first update — used by RestoreRepoState
	// (repostate.go) to surface "restored from last session in this folder"
	// through the normal message pipeline instead of stderr, which would be
	// invisible once tea.WithAltScreen() engages. Consumed (cleared) by
	// tuiModel.Init() so it never re-fires. Unused headless.
	StartupNote string

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

	// SubagentEndpointOverride is the session-scoped override for which
	// endpoint dispatch_subagent targets, set by /subagent <name> (cleared by
	// /subagent inherit). Takes precedence over Cfg.SubagentEndpoint when
	// non-empty. Empty (the default) falls through to the config value, then
	// to inheriting the parent's live endpoint — see resolveSubagentEndpoint.
	SubagentEndpointOverride string

	// SubagentModelOverride is the session-scoped model override for
	// dispatch_subagent, set by /submodel <name> (cleared by /submodel inherit).
	// Mirrors /model's semantics but scoped to subagents: overrides only the
	// model string the child sends, leaving kind/base_url/auth unchanged.
	// Applied AFTER endpoint resolution (inherit or named override), so it
	// composes with /subagent: /subagent picks the endpoint, /submodel picks
	// the model within it. Empty = use the resolved endpoint's model.
	SubagentModelOverride string

	// subagentLimitsCachePtr backs a singleflight cache for context-limit
	// probes against overridden subagent endpoints: concurrent dispatch_subagent
	// workers targeting the same endpoint+backend fire at most one /props or
	// /v1/ilm/limits request. A pointer field (not an embedded mutex value)
	// keeps App itself safe to copy by value, as some tests do.
	//
	// MAIN GOROUTINE ONLY to set: ensureSubagentLimitsCache() populates it
	// before any worker goroutine spawns (Phase A of runParallelSubagentBlock,
	// or inline in the sequential dispatch_subagent handler — both run on the
	// main goroutine only). Go's memory model guarantees a value written on
	// the main goroutine before a `go` statement is visible inside that
	// goroutine without extra synchronization, so workers may safely read
	// (never write) this field after being spawned. Once set, the cache's own
	// internal mutex guards all concurrent access to its map. Callers that
	// invoke dispatchSubagent directly without going through either wrapper
	// (most existing tests) see a nil pointer here; resolveChildCtxLimit falls
	// back to an unshared, call-local cache in that case — correct but without
	// cross-call dedup, since there is only one call to dedup against anyway.
	subagentLimitsCachePtr *subagentLimitsCache

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

	// LSP is the language server manager for code-intelligence tools.
	// nil when LSPEnabled is false.
	LSP *lsp.Manager
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

	// done is closed by a reaper goroutine when the process exits. Used by
	// StopAllBackgroundProcs to wait for clean shutdown without a fixed sleep.
	// nil when the entry was constructed by test code (not via run_background).
	done chan struct{}
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

// chatID returns the current session's chat ID. When Session is nil (e.g.
// subagents, which have no on-disk persistence), it falls back to the Client's
// ChatID so that spill-to-disk (StubToolResult, CapToolResult, SpillFullResult)
// produces real recoverable paths inside a subagent instead of silently
// no-op'ing with an empty directory. The subagent's Client.ChatID is minted
// per-dispatch-unique (a fresh UUID v4 via NewChatID at the dispatch call site),
// so two concurrent subagents never collide in the toolcache directory.
func (a *App) chatID() string {
	if a.Session != nil && a.Session.ChatID != "" {
		return a.Session.ChatID
	}
	if a.Client != nil {
		return a.Client.ChatID
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
	// Force ensurePreamble to re-insert Conv[0] on the next Send — otherwise
	// a same-day preambleDay would read as "already up to date" against the
	// now-empty Conv and silently leave the new conversation with no preamble.
	a.preambleDay = ""
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
//
// Send orchestrates four phases (WP-6.2): prepareTurn (reset + model/backend
// selection), checkEgressConsent (external backend gate), streamTurn (stream +
// tool loop), finalizeTurn (compaction + hard-max + pressure warning).
func (a *App) Send(ctx context.Context, userText string) (_ string, retErr error) {
	a.prepareTurn()

	if !a.checkEgressConsent() {
		return "", nil
	}

	// Persist on every exit path (success, stream error, or cancellation) so a
	// completed turn — and at minimum the user's message — is never lost.
	defer a.SaveSession()

	// Prepend the workflow phase directive so the model knows its current
	// obligation. The combined text is stored in Conv so it participates in
	// caching and compaction normally.
	stored := userText
	if a.Workflow != nil {
		if d := a.Workflow.Directive(); d != "" {
			stored = d + "\n\n" + userText
		}
	}

	// Keep Conv[0]'s day-stable preamble current before anything below reads
	// or resizes Conv. Once per turn, not per tool-loop iteration — see
	// ensurePreamble.
	a.ensurePreamble()

	// P36 downshift guard: if /backend switched to a smaller-context model since
	// the last turn, Conv may already exceed the new hard ceiling. Compact+drop
	// before appending the user message so we never deliver an over-window Conv.
	a.fitConvToWindow(ctx)

	a.Conv = append(a.Conv, proxy.Message{Role: "user", Content: StrPtr(stored), Pinned: a.pinUserMessage})

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

	final, err := a.streamTurn(ctx, userText, rsink, &traceToolCalls)
	if err != nil {
		return "", err
	}

	a.finalizeTurn(ctx)
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
	// Exact is the authoritative marker: the provisional pre-send usage (published
	// so the ctx meter moves mid-turn) and the length-based fallback both carry
	// Exact=false and must never trip this warning.
	if a.Client == nil || !a.Client.LastUsage().Exact || a.Client.LastUsage().InputTok == 0 {
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

// confinementBreakerThreshold is how many times the SAME path may fail path
// confinement (ConfinePath: outside workspace / unresolvable) within one turn
// before the circuit breaker trips and force-finishes the turn early.
//
// Rationale: unlike most tool failures, a ConfinePath rejection is a
// deterministic error class — given the same resolved path, the outcome can
// never change on retry (the path is either inside the workspace root or it
// isn't; that doesn't flip mid-turn). A model that keeps retrying variants of
// an unreachable path (different quoting, relative vs absolute, a different
// read tool) is not making progress and, left alone, burns its entire
// MaxToolIterations budget on a foregone conclusion — the exact
// "ran out of budget" failure mode this breaker exists to short-circuit.
// 2 strikes (not 1) tolerates one legitimate retry with a corrected path.
const confinementBreakerThreshold = 2

// confinementErrorMarkers are substrings unique to Executor.ConfinePath's
// error text (see internal/exec/exec_ops.go: DockerExecutor/DirectExecutor
// ConfinePath). Any tool result containing one of these is a deterministic,
// non-retryable path-confinement rejection, never a transient failure —
// matching on these lets the breaker fire on the ERROR CLASS rather than on
// byte-identical retries, which real model retries rarely are (they vary
// quoting, tool choice, and path form call to call).
var confinementErrorMarkers = []string{
	"outside workspace",      // both executors: "... is outside workspace %q — traversal not allowed"
	"resolving path",         // Docker: readlink -f failed
	"could not resolve path", // Docker: readlink -f produced empty output
}

// isConfinementError reports whether a tool result is a path-confinement
// rejection from Executor.ConfinePath, as opposed to any other tool error.
func isConfinementError(result toolResult) bool {
	if result.ok {
		return false
	}
	for _, m := range confinementErrorMarkers {
		if strings.Contains(result.text, m) {
			return true
		}
	}
	return false
}

// confinementPathQuoted extracts the first double-quoted path segment from a
// ConfinePath error (all its error strings quote the offending path with %q).
// Used both as the breaker's per-path counter key and for the honest final
// message. Falls back to the full result string if no quoted segment is found
// so distinct-but-unparseable errors don't collapse into one counter bucket.
func confinementPathQuoted(result string) string {
	start := strings.IndexByte(result, '"')
	if start < 0 {
		return result
	}
	end := strings.IndexByte(result[start+1:], '"')
	if end < 0 {
		return result
	}
	return result[start+1 : start+1+end]
}

// confinementBreakerPrompt builds the honest, specific final-answer directive
// shown to the model when the breaker trips — named paths, named reason —
// instead of the generic ToolLimitPrompt, which never explains why the model
// is being cut off. paths is the de-duplicated list of unreachable paths hit.
func confinementBreakerPrompt(paths []string) string {
	var b strings.Builder
	b.WriteString("The following path(s) are outside the accessible workspace and cannot be read, listed, or searched no matter how they are re-specified (this will not change on retry):\n")
	for _, p := range paths {
		fmt.Fprintf(&b, "  - %s\n", p)
	}
	b.WriteString("Stop retrying them. Produce your final response now: state plainly that these paths are unreachable from this sandbox, and answer using only what you have already gathered.")
	return b.String()
}

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
		usd, priced = a.Cfg.Costs.ExternalInferenceCost(usedBackend+"/"+modelForCost, u.InputTok, u.OutputTok, config.TokenDetail{
			CachedTok:     u.CachedTok,
			CacheWriteTok: u.CacheWriteTok,
		})
	} else {
		// Local/modeled tier has one flat combined rate (no separate input
		// rate to split a cache discount against) — cached tokens stay folded
		// into InputTok exactly as before; only the exact/external tier above
		// gets split-rate cache pricing.
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

	a.Costs.Record(source, u.InputTok, u.OutputTok, usd, priced, conf, config.TokenDetail{
		CachedTok:     u.CachedTok,
		CacheWriteTok: u.CacheWriteTok,
	})
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

// buildPreamble builds the day-stable system message stored at Conv[0] (see
// ensurePreamble). It leads with the static agent operating instructions
// (loaded once from AgentPromptPath at startup), then appends the per-day
// context: current date, working directory, and available sandbox tools.
//
// today is deliberately day-granularity (the caller formats it without a
// time-of-day component) — this string becomes the request's leading system
// message, the earliest possible byte position in the serialized body, so
// any change here invalidates the whole prompt-cache prefix from position
// zero. Time-of-day is not worth paying that cost on every request; a
// same-day timestamp accuracy is enough for the "treat this as now" framing.
func (a *App) buildPreamble(today string) string {
	date := "Current date: " + today +
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
	// LSP code intelligence inventory.
	if a.LSP != nil {
		parts = append(parts, "LSP code intelligence available: lsp_definition, lsp_references, "+
			"lsp_hover, lsp_symbols. Pass a file path + 1-based line number (as shown by read_file) "+
			"+ symbol name — the client resolves the position. Prefer these over search_files for "+
			"symbol-level navigation (definition lookup, finding callers).")
	}
	// Memory digest: compact session-start snapshot. NOT live — entries
	// written mid-session won't appear until the next day rollover or
	// session start. The agent uses memory_search for live data. This is
	// a heads-up, not a source of truth. Invalidating the preamble on
	// every memory mutation would destroy prompt-cache prefix stability.
	if a.MemoryStore != nil {
		statsCtx, statsCancel := context.WithTimeout(context.Background(), 3*time.Second)
		stats, err := a.MemoryStore.Stats(statsCtx, 5)
		statsCancel()
		if err == nil && (stats.ActiveDurable > 0 || stats.ActiveMid > 0 || stats.PendingProposed > 0) {
			var memParts []string
			memParts = append(memParts, fmt.Sprintf("Memory: %d active durable entries", stats.ActiveDurable))
			if stats.ActiveMid > 0 {
				memParts = append(memParts, fmt.Sprintf("%d mid-tier", stats.ActiveMid))
			}
			if stats.PendingProposed > 0 {
				memParts = append(memParts, fmt.Sprintf("%d pending proposals", stats.PendingProposed))
			}
			memLine := strings.Join(memParts, ", ") + "."
			if len(stats.RecentKeys) > 0 {
				memLine += " Recent: " + strings.Join(stats.RecentKeys, ", ") + "."
			}
			memLine += " Use memory_search or memory_get to retrieve entries."
			parts = append(parts, memLine)
		}
	}
	return strings.Join(parts, "\n")
}

// ensurePreamble keeps Conv[0] as a day-stable pinned system message carrying
// the agent prompt, date, cwd, and tool inventory — the request's leading
// bytes, and therefore the dominant lever on prompt-cache prefix stability.
// Called once per turn at Send entry, never per tool-loop iteration, so every
// Stream call within one calendar day sends a byte-identical messages[0].
//
// A day rollover mutates Conv[0].Content in place — one deliberate prefix
// invalidation per day, no worse than a compaction event, and never more
// than once per day even if a turn happens to straddle midnight (the check
// runs once, at turn start; mid-turn iterations keep whatever day was
// current when the turn began).
//
// No-op when InjectDate is off (subagents, tests) — those paths are
// unaffected and continue to get no preamble at all.
func (a *App) ensurePreamble() {
	if !a.InjectDate {
		return
	}
	today := time.Now().Format("Monday, 2 January 2006")
	if a.preambleDay == today {
		return
	}
	text := a.buildPreamble(today)
	if a.preambleDay != "" && len(a.Conv) > 0 && a.Conv[0].Role == "system" {
		// Same session, day rolled over — refresh the existing slot in place.
		a.Conv[0].Content = StrPtr(text)
	} else {
		// First turn this session, or Conv was just reset (NewConversation) —
		// insert fresh. Pinned so compaction/hard-max never dissolve or drop
		// it (see compact.go's leading-system-message handling in
		// oldestTurnRange/dropOldestTurn, which already special-cases Conv[0]
		// being a system message).
		a.Conv = append([]proxy.Message{{Role: "system", Content: StrPtr(text), Pinned: true}}, a.Conv...)
	}
	a.preambleDay = today
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
		a.turnBudgetStubbed = true
		return wtools.StubToolResult(result, toolName, a.chatID())
	}
	// read_file_full: return full content (size already checked at the tool layer
	// against max_full_read_bytes). Spill to disk and embed the path so eviction
	// and pre-send trim leave a recoverable pointer. Exempt from ToolResultCap
	// (the 8K windowing that causes re-read churn) but NOT from TurnToolBudget —
	// when the per-turn budget is exhausted, the result is stubbed to prevent
	// context overflow. MaxRequestBytes remains the final backstop.
	if toolName == "read_file_full" {
		return wtools.SpillFullResult(result, toolName, a.chatID())
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

// recordFileChanged appends a canonical path to the filesChanged recorder when
// the App has one (edit-tier subagents). No-op for the parent and discovery-tier
// children (filesChanged is nil). Called after a successful edit-category tool call.
func (a *App) recordFileChanged(canonical string) {
	if a.filesChanged != nil {
		a.filesChanged.record(canonical)
	}
}

// recordExternalAction appends one MCP tool call to the externalActions recorder
// when the App has one (tools-tier subagents). No-op for the parent and
// discovery/edit-tier children (externalActions is nil). Called after an MCP
// tool call completes, whether success or error.
func (a *App) recordExternalAction(server, tool, status string) {
	if a.externalActions != nil {
		a.externalActions.record(server, tool, status)
	}
}

// buildSubagentTools assembles the toolset for a tools-tier subagent: discovery
// tools (the shared prefix for cache stability) + web search + LSP + filtered
// MCP tools (only servers in the SubagentMCPServers allowlist). Never includes
// run_shell, dispatch_subagent, run_background, kill_process, open_url, or
// mashura__* — those stay parent-only. Called only when capability == "tools".
func (a *App) buildSubagentTools() []proxy.Tool {
	cwd := a.Exec.Cwd()
	t := wtools.DiscoveryTools(cwd)
	// Web search — only if the parent has it configured.
	if a.Cfg.SearXngURL != "" {
		t = append(t, wtools.SearxngTools()...)
	}
	if a.Cfg.GoogleAPIKey != "" && a.Cfg.GoogleCX != "" {
		t = append(t, wtools.GoogleTools()...)
	}
	// LSP — only if the parent has it enabled.
	if a.Cfg.LSPEnabled {
		t = append(t, lsp.LSPTools(cwd)...)
	}
	// MCP — only servers in the allowlist.
	if a.MCP != nil && len(a.Cfg.SubagentMCPServers) > 0 {
		allowed := make(map[string]bool, len(a.Cfg.SubagentMCPServers))
		for _, s := range a.Cfg.SubagentMCPServers {
			allowed[s] = true
		}
		t = append(t, a.MCP.OpenAIToolsForServers(allowed)...)
	}
	return t
}

// subagentToolNames returns the tool name list for the sidebar display.
// Returns nil for discovery and edit tiers (the TUI hardcodes those lists);
// returns the actual tool names for the "tools" tier (which is dynamic —
// depends on MCP server config, LSP, search).
func (a *App) subagentToolNames(capability string) []string {
	if capability != wtools.CapabilityTools {
		return nil
	}
	tools := a.buildSubagentTools()
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Function.Name)
	}
	return names
}

func (a *App) handleToolCall(ctx context.Context, tc proxy.ToolCall) toolResult {
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
			return errResult(fmt.Sprintf("[already called %s with equivalent arguments — that result is already above. Do not repeat it: use what you have, try a different path/pattern, or produce your final answer.]", name))
		}
	}

	result := a.ExecuteToolCall(ctx, tc)

	// Capture tool-call evidence for the IMPLEMENT step trace and the rolling
	// cross-turn buffer. Called with the pre-cap result so evidence reflects
	// actual output size.
	a.captureToolTrace(tc, result)
	a.recordRecentTrace(tc, result)

	if cacheKey != "" && result.ok {
		a.ToolCache[cacheKey] = true
	}
	return result
}

// captureToolTrace appends a trace entry to WorkflowStepTrace when an IMPLEMENT
// step is in progress. No-op outside IMPLEMENT turns.
func (a *App) captureToolTrace(tc proxy.ToolCall, result toolResult) {
	if a.Workflow == nil || a.Workflow.Phase != workflow.WFImplement {
		return
	}
	a.WorkflowStepTrace = append(a.WorkflowStepTrace, MakeTraceEntry(tc, result))
}

func MakeTraceEntry(tc proxy.ToolCall, result toolResult) ToolTraceEntry {
	e := ToolTraceEntry{
		Abbrev:    toolAbbrev(tc.Function.Name),
		OutputLen: len(result.text),
		ExitErr:   !result.ok,
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
	case "move_file":
		var a struct {
			Src string `json:"src"`
			Dst string `json:"dst"`
		}
		if json.Unmarshal([]byte(tc.Function.Arguments), &a) == nil {
			switch {
			case a.Src != "" && a.Dst != "":
				e.Command = a.Src + " → " + a.Dst
			case a.Src != "":
				e.Command = a.Src
			case a.Dst != "":
				e.Command = a.Dst
			}
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
	lines := strings.Split(strings.TrimSpace(result.text), "\n")
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
func (a *App) recordRecentTrace(tc proxy.ToolCall, result toolResult) {
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
	case "read_file_full":
		return "rfull"
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
	case "delete_file":
		return "delete"
	case "move_file":
		return "move"
	case "dispatch_subagent":
		return "subagent"
	case "dispatch_subagents":
		return "subagents"
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

// ExecuteToolCall dispatches a tool call to the appropriate handler method.
// Each tool case is implemented as a separate *App method in tool_handlers.go
// (or the original file for pre-existing handlers like handleEditFile,
// handleMashura, handleStaging*, handleMemory*). This function runs the shared
// pre-dispatch gate (workflow phase enforcement), then routes to the handler.
//
// Returns toolResult (WP-6.8): the typed boundary replaces the former string
// protocol. Handlers still return string and are wrapped via stringToToolResult
// at this dispatch boundary. Callers classify success/failure via result.ok,
// never by prefix-sniffing "ERROR:".
func (a *App) ExecuteToolCall(ctx context.Context, tc proxy.ToolCall) toolResult {
	name := tc.Function.Name

	// Phase enforcement: reject tool calls that violate the workflow write-containment
	// invariant. The error is a regular tool result so the model reads it and can
	// self-correct within the same turn.
	if a.Workflow != nil {
		if msg := a.wfPhaseBlock(name, tc.Function.Arguments); msg != "" {
			return errResult(msg)
		}
	}

	switch name {
	case "run_shell":
		return stringToToolResult(a.handleRunShell(ctx, tc))
	case "open_url":
		return stringToToolResult(a.handleOpenURL(ctx, tc))
	case "read_file":
		return stringToToolResult(a.handleReadFile(ctx, tc))
	case "read_file_full":
		return stringToToolResult(a.handleReadFileFull(ctx, tc))
	case "list_dir":
		return stringToToolResult(a.handleListDir(ctx, tc))
	case "find_files":
		return stringToToolResult(a.handleFindFiles(ctx, tc))
	case "search_files":
		return stringToToolResult(a.handleSearchFiles(ctx, tc))
	case "write_file":
		return stringToToolResult(a.handleWriteFile(ctx, tc))
	case "edit_file":
		return stringToToolResult(a.handleEditFile(ctx, tc))
	case "delete_file":
		return stringToToolResult(a.handleDeleteFile(ctx, tc))
	case "move_file":
		return stringToToolResult(a.handleMoveFile(ctx, tc))
	case "searxng_search":
		return stringToToolResult(a.handleSearxngSearch(ctx, tc))
	case "searxng_url_read":
		return stringToToolResult(a.handleSearxngURLRead(ctx, tc))
	case "google_search":
		return stringToToolResult(a.handleGoogleSearch(ctx, tc))
	case "google_fetch_url":
		return stringToToolResult(a.handleGoogleFetchURL(ctx, tc))
	case "run_background":
		return stringToToolResult(a.handleRunBackground(ctx, tc))
	case "kill_process":
		return stringToToolResult(a.handleKillProcess(ctx, tc))
	case "read_process_log":
		return stringToToolResult(a.handleReadProcessLog(ctx, tc))
	case "dispatch_subagent":
		return stringToToolResult(a.handleDispatchSubagent(ctx, tc))
	case "dispatch_subagents":
		return stringToToolResult(a.handleDispatchSubagents(ctx, tc))
	// The mashura__* counsel family (and the legacy oracle__ask alias) all route
	// through one handler: the model supplies intent, Wakil deterministically
	// assembles the briefing. Each is gated through the normal confirm flow
	// (auto-approved in /auto mode with a visible ⚡ auto note).
	case "mashura__review", "mashura__debug", "mashura__decide", "mashura__check", "oracle__ask":
		return stringToToolResult(a.handleMashura(ctx, name, tc))
	// LSP code-intelligence tools (read-only, no confirmation needed).
	case "lsp_definition", "lsp_references", "lsp_hover", "lsp_symbols":
		return stringToToolResult(a.handleLSPReadOnly(ctx, tc))
	// Staging tools (ungated by design — the gate lives at promotion).
	case "staging_put":
		return stringToToolResult(a.handleStagingPut(ctx, tc))
	case "staging_get":
		return stringToToolResult(a.handleStagingGet(ctx, tc))
	case "staging_delete":
		return stringToToolResult(a.handleStagingDelete(ctx, tc))
	case "staging_list":
		return stringToToolResult(a.handleStagingList(ctx, tc))
	case "staging_get_many":
		return stringToToolResult(a.handleStagingGetMany(ctx, tc))
	// Memory tools (tier-gating at dispatch time via a.IsSubagent).
	case "memory_put":
		return stringToToolResult(a.handleMemoryPut(ctx, tc))
	case "memory_promote":
		return stringToToolResult(a.handleMemoryPromote(ctx, tc))
	case "memory_reject":
		return stringToToolResult(a.handleMemoryReject(ctx, tc))
	case "memory_get":
		return stringToToolResult(a.handleMemoryGet(ctx, tc))
	case "memory_search":
		return stringToToolResult(a.handleMemorySearch(ctx, tc))
	case "memory_list":
		return stringToToolResult(a.handleMemoryList(ctx, tc))
	case "memory_forget":
		return stringToToolResult(a.handleMemoryForget(ctx, tc))
	case "memory_promote_from_staging":
		return stringToToolResult(a.handleMemoryPromoteFromStaging(ctx, tc))
	default:
		return stringToToolResult(a.handleMCPTool(ctx, tc))
	}
}

// stopAllBackgroundProcs sends SIGTERM to all live background process groups and
// waits for them to exit, with a 2-second grace period ceiling before returning.
// Called on shutdown. In docker mode the container removal (Close) will kill
// everything anyway; this is primarily meaningful for direct mode.
func (a *App) StopAllBackgroundProcs() {
	a.bgMu.RLock()
	if len(a.bgProcs) == 0 {
		a.bgMu.RUnlock()
		return
	}
	// Copy entries under lock, then operate outside lock to avoid
	// holding the lock during KillPgid/IsProcessAlive/wait.
	entries := make([]*bgEntry, 0, len(a.bgProcs))
	for _, entry := range a.bgProcs {
		entries = append(entries, entry)
	}
	a.bgMu.RUnlock()

	bg := context.Background()
	type liveProc struct {
		entry *bgEntry
		done  chan struct{}
	}
	var live []liveProc
	for _, entry := range entries {
		if entry.generation != a.Exec.Generation() {
			continue
		}
		if a.Exec.IsProcessAlive(bg, entry.pid) {
			_ = a.Exec.KillPgid(bg, entry.pgid, 15) // SIGTERM
			live = append(live, liveProc{entry: entry, done: entry.done})
		}
	}
	if len(live) > 0 {
		// Wait for all processes to exit, with a 2s ceiling. If the timeout
		// expires, SIGKILL the remaining processes.
		deadline := time.After(2 * time.Second)
		timedOut := false
		for _, p := range live {
			if p.done == nil {
				continue // no reaper (test-constructed entry) — skip wait
			}
			select {
			case <-p.done:
				// process exited cleanly
			case <-deadline:
				timedOut = true
			}
			if timedOut {
				break // stop waiting once the deadline has passed
			}
		}
		if timedOut {
			// Force-kill any processes that are still alive.
			for _, p := range live {
				if a.Exec.IsProcessAlive(bg, p.entry.pid) {
					_ = a.Exec.KillPgid(bg, p.entry.pgid, 9) // SIGKILL
				}
			}
		}
	}
	// Clean up the per-session bg log directory.
	if a.bgLogDir != "" {
		os.RemoveAll(a.bgLogDir)
		a.bgLogDir = ""
	}
}

// renderFilesChanged appends the mechanical files_changed list (ground truth) to
// the subagent result string. When the model's self-reported list disagrees with
// the mechanical record, both are rendered with the discrepancy noted — the
// mechanical list is ground truth, the model's list is a claim.
func renderFilesChanged(modelClaim, mechanical []string) string {
	var b strings.Builder
	b.WriteString("\n[files_changed (mechanical)]")
	for _, p := range mechanical {
		b.WriteString("\n  " + p)
	}
	// Check for discrepancy: model claims a file not in the mechanical record,
	// or the mechanical record has files the model didn't claim.
	mechSet := make(map[string]bool, len(mechanical))
	for _, p := range mechanical {
		mechSet[p] = true
	}
	claimSet := make(map[string]bool, len(modelClaim))
	for _, p := range modelClaim {
		claimSet[p] = true
	}
	var extra []string // model claimed but not mechanically recorded
	for _, p := range modelClaim {
		if !mechSet[p] {
			extra = append(extra, p)
		}
	}
	var missing []string // mechanically recorded but model didn't claim
	for _, p := range mechanical {
		if !claimSet[p] {
			missing = append(missing, p)
		}
	}
	if len(extra) > 0 || len(missing) > 0 {
		b.WriteString("\n[discrepancy: model self-report differs from mechanical record]")
		if len(extra) > 0 {
			b.WriteString("\n  model claimed (not mechanically confirmed):")
			for _, p := range extra {
				b.WriteString("\n  + " + p)
			}
		}
		if len(missing) > 0 {
			b.WriteString("\n  mechanical record (model did not report):")
			for _, p := range missing {
				b.WriteString("\n  - " + p)
			}
		}
	}
	return b.String()
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

// hostCacheReadResult reads a wakil toolcache spill file directly from the
// host filesystem (see wtools.IsToolCacheHostPath / ReadHostCacheFile) and
// applies the same size-guard and binary-sniff checks the executor-backed
// read_file/read_file_full paths apply, so a spilled artifact is held to the
// same guarantees as a normal workspace file. Returns (content, "") on
// success, or ("", errorResult) when the read should be refused/failed —
// callers pass errorResult straight back as the tool result.
//
// sizeLimit <= 0 disables the pre-read size guard (mirrors the read_file
// behavior when an explicit Limit/offset already bounds the read).
// adviceSuffix is appended to the size-guard error message so read_file and
// read_file_full keep their existing distinct wording ("specify a line/byte
// range..." vs "use read_file with an offset/limit range instead.").
func hostCacheReadResult(path string, sizeLimit int64, kind, adviceSuffix string) (content, errorResult string) {
	if sizeLimit > 0 {
		if fileSize, serr := wtools.StatHostCacheFile(path); serr == nil && fileSize > sizeLimit {
			return "", fmt.Sprintf(
				"ERROR: file is %.2f MB, exceeds %s limit of %.2f MB — %s",
				float64(fileSize)/(1<<20), kind, float64(sizeLimit)/(1<<20), adviceSuffix)
		}
	}
	out, err := wtools.ReadHostCacheFile(path)
	if err != nil {
		return "", formatResult(out, err)
	}
	if strings.ContainsRune(out, 0) {
		return "", fmt.Sprintf("ERROR: binary file, %.2f MB — not readable as text.", float64(len(out))/(1<<20))
	}
	return out, ""
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
func (a *App) handleEditFile(ctx context.Context, tc proxy.ToolCall) string {
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
	// Confine the path to the workspace (P0-3: path confinement).
	canonical, err := a.Exec.ConfinePath(context.Background(), args.Path)
	if err != nil {
		return "ERROR: " + err.Error()
	}
	cur, err := a.Exec.ReadFile(ctx, canonical)
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
	out, err := a.Exec.WriteFile(ctx, canonical, updated)
	if err != nil {
		return formatResult(out, err)
	}
	// LSP file-sync: notify gopls of the change (didChange).
	if a.LSP != nil {
		a.LSP.NotifyChange(context.Background(), canonical)
	}
	a.recordFileChanged(canonical)
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
func toolLine(tc proxy.ToolCall, result toolResult) string {
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
	for _, k := range []string{"path", "command", "query", "url", "pattern", "file", "src"} {
		if v, ok := m[k].(string); ok && v != "" {
			return Truncate(strings.Join(strings.Fields(v), " "), 50)
		}
	}
	return ""
}

// resultSummary digests a tool result for the one-line view: a short single-line
// result is shown verbatim; anything larger collapses to a line/size count.
// Declines and errors are flagged rather than dumped.
func resultSummary(result toolResult) string {
	r := strings.TrimRight(result.text, "\n")
	switch {
	case r == "[declined by user]":
		return "declined"
	case r == "" || r == "(no output)":
		return "ok"
	case !result.ok && !strings.Contains(r, "\n"):
		// Single-line error: strip the "ERROR:" prefix and show the message.
		// A !ok single-line result is always "ERROR: …" (stringToToolResult
		// guarantees it), so the prefix is always present.
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
	summary := fmt.Sprintf("%d %s · %s", lines, unit, humanBytes(len(result.text)))
	if !result.ok {
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
