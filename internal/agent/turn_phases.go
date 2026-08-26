package agent

// turn_phases.go contains the phase methods extracted from Send (WP-6.2).
// Send orchestrates: prepareTurn → checkEgressConsent → streamTurn → finalizeTurn.
//
// The defers (SaveSession, flushTraceTurn) remain in Send because they are
// function-scoped and need to see retErr and the trace accumulators.

import (
	"context"
	"fmt"
	"strings"

	"github.com/treeol/wakil/internal/proxy"
	wtools "github.com/treeol/wakil/internal/tools"
	"github.com/treeol/wakil/internal/trace"
)

// prepareTurn resets per-turn state and applies model/backend selection at
// request build time. Called at the top of Send before any request is made.
//
// Holds stateMu.Lock for the field assignments only — no I/O, no callbacks.
// This is a write lock because the turn goroutine writes Client.Model/Backend
// (turn-stable writes). GetSessionState takes stateMu.RLock and sees a
// consistent state. Per-turn reset fields (exhausted, stopReason, etc.) are
// turn-scoped — only the turn goroutine reads them — but are reset here under
// the lock for consistency.
func (a *App) prepareTurn() {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()

	// Reset per-turn exhaustion flag. Set by forceFinish or enforceHardMax
	// during this turn. dispatchSubagent captures the first-Send value before
	// the retry, then ORs it with the retry's value, so resetting here
	// doesn't mask first-Send exhaustion.
	a.exhausted = false
	a.stopReason = ""
	a.turnBudgetStubbed = false
	// Reset per-turn confinement-breaker flags, same rationale as exhausted
	// above: dispatchSubagent captures the first-Send value before the retry.
	a.confinementTripped = false
	a.confinementPathsHit = nil

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
}

// checkEgressConsent prompts the user before the first request to an external
// backend. Returns false if the user declines (SelectedBackend is reverted to
// the proxy default and a notice is printed); the caller should return ("", nil)
// immediately. Returns true when no consent is needed or consent is granted.
//
// Gated even in /auto mode — the SuspendAuto hook in tuiConfirmer ensures the
// prompt always fires.
//
// TOCTOU fix: the backend is snapshotted under stateMu.RLock before the
// (blocking) Confirm call. If the user declines, the revert is a conditional
// write under stateMu.Lock — it only reverts if the backend hasn't changed
// during the prompt. This prevents a /backend RPC that lands mid-prompt from
// being silently reverted.
func (a *App) checkEgressConsent() bool {
	a.stateMu.RLock()
	backend := a.SelectedBackend
	a.stateMu.RUnlock()

	if backend == "" || !IsExternalBackend(a.BackendList, a.Cfg, backend) {
		return true
	}
	if a.consentedBackends != nil && a.consentedBackends[backend] {
		return true
	}
	detail := fmt.Sprintf(
		"This session's context (memory, grounding, learned notes) will be sent to "+
			"external backend %q. Proceed?\n\n"+
			"(The proxy also enforces ILM_ALLOW_EXTERNAL; this gate makes the decision "+
			"visible at the moment it happens.)", backend)
	if !a.Confirm("external_backend",
		"⚠ Send session context to external backend "+backend+"?",
		detail, false) {
		// Decline: conditional write — only revert if backend hasn't changed
		// during the prompt. If a /backend RPC changed it mid-prompt, the
		// new selection stands.
		reverted := false
		a.stateMu.Lock()
		if a.SelectedBackend == backend {
			a.SelectedBackend = ""
			a.Client.Backend = ""
			reverted = true
		}
		a.stateMu.Unlock()
		if reverted {
			fmt.Fprintf(a.Out, "\n· backend %q declined — selection reverted to proxy default\n", backend)
		} else {
			fmt.Fprintf(a.Out, "\n· backend %q declined — selection changed during prompt\n", backend)
		}
		return false
	}
	// Record consent for the target backend. consentedBackends is turn-scoped
	// (only the turn goroutine writes it), so no lock is needed here — the
	// turn goroutine is single-threaded for this map.
	if a.consentedBackends == nil {
		a.consentedBackends = make(map[string]bool)
	}
	a.consentedBackends[backend] = true
	return true
}

// streamTurn runs the stream-and-tool-call loop: stream a response, execute
// tool calls, feed results back, repeat until a final text answer or
// force-finish. Returns the final assistant text and any stream error.
// traceToolCalls is appended to for the deferred trace flush in the caller.
func (a *App) streamTurn(ctx context.Context, userText string, rsink proxy.Sink, traceToolCalls *[]trace.ToolTrace) (string, bool, error) {
	var final string
	var suspended bool    // card #122 Phase 2: true when the turn idled with async work pending
	var wantsSuspend bool // wait_for_completion requested a suspension this round
	var turnToolBytes int
	firstStream := true
	// Path-confinement circuit breaker state (see confinementBreakerThreshold):
	// confinementFailures counts ConfinePath rejections per distinct path across
	// the whole turn; confinementPaths preserves first-seen order for the honest
	// final message; confinementTrip is set once any path crosses the threshold
	// and forces an early, precise wrap-up on the NEXT iteration — well before
	// MaxToolIterations would otherwise exhaust the budget on a foregone
	// conclusion (the same unreachable path can never resolve differently).
	confinementFailures := map[string]int{}
	var confinementPaths []string
	confinementTrip := false
	for iter := 0; ; iter++ {
		wantsSuspend = false // one wait_for_completion per round
		// Card #121: drain completed async operations (mashūra panels, detached
		// shell jobs) into the conversation BEFORE the model request, so the
		// model sees them as a ping. Turn goroutine only; Conv mutated under
		// convMu. Empty inbox → no message, no cost.
		if envelope := a.drainAsyncInbox(); envelope != "" {
			a.convMu.Lock()
			a.Conv = append(a.Conv, proxy.Message{Role: "user", Content: StrPtr(envelope)})
			a.convMu.Unlock()
			fmt.Fprintln(a.Out, Dim("· async results delivered"))
		}
		// Hard backstop against runaway tool loops: on the final allowed iteration
		// drop the tools and force the model to answer from what it already has.
		// 0 = unlimited (the parent's default; a human gates each tool there).
		forceFinish := (a.Cfg.MaxToolIterations > 0 && iter >= a.Cfg.MaxToolIterations) || confinementTrip
		tools := a.Tools
		if forceFinish {
			tools = nil
			a.exhausted = true // signal to dispatchSubagent: iteration limit hit
			if confinementTrip {
				// Precise, honest wrap-up: name the unreachable path(s) instead of
				// the generic ToolLimitPrompt, and record the reason so
				// dispatchSubagent can report Skipped{Reason:"inaccessible"}
				// rather than a bare "budget-exhausted".
				a.confinementTripped = true
				a.confinementPathsHit = confinementPaths
				a.stopReason = "confinement_breaker"
				a.convMu.Lock()
				a.Conv = append(a.Conv, proxy.Message{Role: "user", Content: StrPtr(confinementBreakerPrompt(confinementPaths))})
				a.convMu.Unlock()
			} else {
				a.stopReason = "iteration_limit"
				a.convMu.Lock()
				a.Conv = append(a.Conv, proxy.Message{Role: "user", Content: StrPtr(ToolLimitPrompt)})
				a.convMu.Unlock()
			}
		}

		// Conv[0] already carries the day-stable preamble (ensurePreamble, run
		// once at Send entry) when InjectDate is on — no per-iteration rebuild.
		a.convMu.RLock()
		msgs := a.Conv
		a.convMu.RUnlock()

		sink := a.streamSink()
		msg, err := a.Client.Stream(ctx, msgs, tools, sink, rsink)
		if err != nil {
			return "", false, err
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
		a.convMu.Lock()
		a.Conv = append(a.Conv, msg)
		a.convMu.Unlock()
		final = DerefStr(msg.Content)

		if len(msg.ToolCalls) == 0 || forceFinish {
			// Card #122 Phase 2: a genuine idle point is when the model produced
			// final text with NO tool calls AND async work is still pending. NOT a
			// suspension when forceFinish stripped tools (limit/breaker backstop —
			// that is a definitive stop, not an idle). Return suspended so the
			// caller can wake on the next completion instead of ending the turn.
			suspended = !forceFinish && a.isIdle(len(msg.ToolCalls) == 0)
			break
		}
		// Circuit breaker (checked pre-cap, on the raw tool result): a
		// ConfinePath rejection is deterministic per resolved path — it cannot
		// succeed on retry, so repeated hits on the SAME path (not the same
		// call; the model retries with varied quoting/tool/relative-vs-absolute
		// form) signal a doomed loop rather than progress, not a transient
		// failure worth spending the rest of MaxToolIterations on. Trips after
		// confinementBreakerThreshold distinct hits on one path.
		trackConfinement := func(result toolResult) {
			if !isConfinementError(result) {
				return
			}
			p := confinementPathQuoted(result.text)
			if confinementFailures[p] == 0 {
				confinementPaths = append(confinementPaths, p)
			}
			confinementFailures[p]++
			if confinementFailures[p] >= confinementBreakerThreshold {
				confinementTrip = true
			}
		}

		// finalizeToolResult runs the shared per-result bookkeeping on the main
		// goroutine: progress line, breaker check, cap/stub, trace, budget, Conv
		// append.
		finalizeToolResult := func(tc proxy.ToolCall, result toolResult) {
			// Show a one-line summary (path/command + a result digest). The full
			// result still goes into the transcript below for the model to read.
			fmt.Fprintln(a.Out, Dim(toolLine(tc, result)))

			// Emit a tool-result event to clear the TUI's running-tool indicator.
			// Truncate the result at emission — the full result is already rendered
			// via a.Out (ProgWriter → StreamChunkMsg); the event only needs enough
			// text for the indicator-clear match, not the full payload.
			a.sendEvent(ToolResultMsg{
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Result:     Truncate(result.text, 2000),
			})

			// Check the RAW result (before CapOrStub can touch it) against the
			// path-confinement breaker — ConfinePath error text is short and
			// never capped, so this ordering is only for clarity/robustness.
			trackConfinement(result)

			// Capture pre-cap size before CapOrStub so the trace reflects actual
			// tool output, not the truncated version the model sees.
			text := result.text
			preCapBytes := len(text)
			// RawTools is LIVE — re-read under stateMu.RLock per tool result.
			a.stateMu.RLock()
			rawTools := a.RawTools
			a.stateMu.RUnlock()
			if !rawTools {
				text = a.CapOrStub(text, tc.Function.Name, turnToolBytes)
			}
			if a.Trace != nil {
				*traceToolCalls = append(*traceToolCalls, trace.ToolTrace{
					Name:         tc.Function.Name,
					PreCapBytes:  preCapBytes,
					PostCapBytes: len(text),
					Capped:       len(text) != preCapBytes,
				})
			}

			turnToolBytes += len(text)
			// dispatch_subagent(s) results carry durable on-disk summary paths.
			// Pin the tool message so the parent's compaction never dissolves
			// the breadcrumb — the model must always be able to read_file the
			// full structured findings from the path marker in the content.
			pinned := wtools.IsSubagentResult(tc.Function.Name)
			a.convMu.Lock()
			a.Conv = append(a.Conv, proxy.Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content:    StrPtr(text),
				Pinned:     pinned,
			})
			a.convMu.Unlock()
		}

		// Walk tool calls in order. A maximal contiguous run of ≥2
		// dispatch_subagent calls executes concurrently (bounded — see
		// runParallelSubagentBlock); everything else, including single
		// dispatches, runs sequentially exactly as before. Non-subagent tools
		// act as ordering barriers: [dispatch, shell, dispatch] never runs the
		// second dispatch before the shell. Results are finalized in original
		// call order either way, so every tool_call_id is answered in sequence.
		for ti := 0; ti < len(msg.ToolCalls); {
			tc := msg.ToolCalls[ti]
			tj := ti
			for tj < len(msg.ToolCalls) && msg.ToolCalls[tj].Function.Name == "dispatch_subagent" {
				tj++
			}
			if tj-ti >= 2 {
				block := msg.ToolCalls[ti:tj]
				// Card #122 Phase 1: runParallelSubagentBlock routes pure-discovery
				// blocks through the async funnel and runs mixed/non-discovery blocks
				// synchronously. Returns one result per call in block order.
				blockResults := a.runParallelSubagentBlock(ctx, block)
				for bi, btc := range block {
					br := stringToToolResult(blockResults[bi])
					a.captureToolTrace(btc, br)
					a.recordRecentTrace(btc, br)
					finalizeToolResult(btc, br)
				}
				ti = tj
				continue
			}
			result := a.handleToolCall(ctx, tc)
			if result.text == waitForCompletionToken {
				wantsSuspend = true
			}
			finalizeToolResult(tc, result)
			ti++
		}
		// Card #122 Phase 2: wait_for_completion hands control back. If the model
		// explicitly requested a wait and async work is pending, suspend so the
		// caller awaits a completion and resumes (instead of ending or spinning).
		if wantsSuspend && a.countActiveAsyncOps() > 0 {
			suspended = true
			break
		}
		// After a round of tool calls, offer mashura__debug if the rolling trace
		// shows a struggle signal. In auto-counsel mode this fires the call
		// directly (up to MaxCounsel times); otherwise it only prints a hint.
		a.maybeSuggestDebug(ctx)
	}
	return final, suspended, nil
}

// finalizeTurn runs post-loop cleanup: compaction, hard-max enforcement, and
// context pressure warning. Called after the stream loop completes successfully.
func (a *App) finalizeTurn(ctx context.Context) {
	ok, err := a.Compact(ctx, a.summarizeFn(), false)
	if err != nil {
		// Warn once per session on compaction failure — the hot path falls
		// to lossy enforceHardMax, but the operator should know.
		if !a.compactFailed {
			a.compactFailed = true
			fmt.Fprintln(a.Out, Yellow("⚠ compaction failed: "+err.Error()+" — falling to hard-max enforcement"))
		}
	} else if ok {
		fmt.Fprintln(a.Out, Dim("· compacted earlier turns into a summary"))
	}
	_, _, hm := a.activeThresholds()
	a.enforceHardMax(ctx, hm)
	a.WarnContextPressure()
}
