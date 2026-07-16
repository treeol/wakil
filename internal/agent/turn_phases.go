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
func (a *App) prepareTurn() {
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
func (a *App) checkEgressConsent() bool {
	if a.SelectedBackend == "" || !IsExternalBackend(a.BackendList, a.Cfg, a.SelectedBackend) {
		return true
	}
	if a.consentedBackends != nil && a.consentedBackends[a.SelectedBackend] {
		return true
	}
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
		return false
	}
	if a.consentedBackends == nil {
		a.consentedBackends = make(map[string]bool)
	}
	a.consentedBackends[a.SelectedBackend] = true
	return true
}

// streamTurn runs the stream-and-tool-call loop: stream a response, execute
// tool calls, feed results back, repeat until a final text answer or
// force-finish. Returns the final assistant text and any stream error.
// traceToolCalls is appended to for the deferred trace flush in the caller.
func (a *App) streamTurn(ctx context.Context, userText string, rsink proxy.Sink, traceToolCalls *[]trace.ToolTrace) (string, error) {
	var final string
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
				a.Conv = append(a.Conv, proxy.Message{Role: "user", Content: StrPtr(confinementBreakerPrompt(confinementPaths))})
			} else {
				a.stopReason = "iteration_limit"
				a.Conv = append(a.Conv, proxy.Message{Role: "user", Content: StrPtr(ToolLimitPrompt)})
			}
		}

		// Conv[0] already carries the day-stable preamble (ensurePreamble, run
		// once at Send entry) when InjectDate is on — no per-iteration rebuild.
		msgs := a.Conv

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

			// Check the RAW result (before CapOrStub can touch it) against the
			// path-confinement breaker — ConfinePath error text is short and
			// never capped, so this ordering is only for clarity/robustness.
			trackConfinement(result)

			// Capture pre-cap size before CapOrStub so the trace reflects actual
			// tool output, not the truncated version the model sees.
			text := result.text
			preCapBytes := len(text)
			if !a.RawTools {
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
			a.Conv = append(a.Conv, proxy.Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content:    StrPtr(text),
				Pinned:     pinned,
			})
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
			finalizeToolResult(tc, result)
			ti++
		}
		// After a round of tool calls, offer mashura__debug if the rolling trace
		// shows a struggle signal. In auto-counsel mode this fires the call
		// directly (up to MaxCounsel times); otherwise it only prints a hint.
		a.maybeSuggestDebug(ctx)
	}
	return final, nil
}

// finalizeTurn runs post-loop cleanup: compaction, hard-max enforcement, and
// context pressure warning. Called after the stream loop completes successfully.
func (a *App) finalizeTurn(ctx context.Context) {
	if ok, err := a.Compact(ctx, a.summarizeFn(), false); err == nil && ok {
		fmt.Fprintln(a.Out, Dim("· compacted earlier turns into a summary"))
	}
	_, _, hm := a.activeThresholds()
	a.enforceHardMax(ctx, hm)
	a.WarnContextPressure()
}
