package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/treeol/wakil/internal/proxy"
)

// RunTurn returns a Cmd that runs the agent turn. When the TUI executes it
// (via AdaptCmd → tea.Cmd), the body runs off the event loop — all progress
// is posted into the TUI via EventSink (sendEvent), and the Cmd itself
// returns nil (no message). Safe because it runs outside the Bubble Tea
// event loop.
func RunTurn(app *App, ctx context.Context, userText string) Cmd {
	return func() Msg {
		app.Out = NewProgWriter(func(m StreamChunkMsg) { app.sendEvent(m) })
		app.Confirm = tuiConfirmer(app)
		app.OnTokRate = func(tps float64) { app.sendEvent(TokRateMsg{Tps: tps}) }
		app.OnReasoning = func(s string) { app.sendEvent(ReasoningChunkMsg{Text: s}) }
		// Dedup cache is for subagents only (dispatchSubagent sets it). The main
		// agent is interactive — the user can cancel loops via Ctrl+C, and
		// legitimate re-reads of the same file must not be silently suppressed.
		app.ToolCache = nil
		// Reset per-turn step evidence so each IMPLEMENT turn starts with a
		// clean trace. Consumed by handleWorkflowTransition on %%STEP_DONE%%.
		app.WorkflowStepTrace = nil
		// Clear grounding so proxy entries from the previous turn don't persist.
		// Header entries for this turn land at first Stream; client-side entries
		// (web/oracle) accumulate during tool execution.
		app.Client.ResetGrounding()

		_, err := app.Send(ctx, userText)

		// Retry transient backend failures in auto mode; surface fatal errors and
		// exhausted-retry state as tidy ⚠ lines rather than raw error traces.
		err = HandleStreamError(ctx, app, err)
		streamWarn := ""
		if errors.Is(err, proxy.ErrBackendStream) {
			streamWarn = "⚠ backend unreachable"
			if app.NearContextLimit() {
				streamWarn += " (near context limit — try /compact)"
			}
			if id := ShortID(app.Client.ChatID); id != "" {
				streamWarn += " — session saved, /resume " + id + " to continue"
			}
		} else if errors.Is(err, proxy.ErrBackendFatal) {
			// 4xx: surface the full message so the user can diagnose (bad request,
			// auth failure, etc.) — not a stream warn, it needs the detail.
			streamWarn = "⚠ " + err.Error()
			err = nil // don't double-render as AgentDoneMsg.Err
		}

		// Detect empty completions (no content, no tool calls) — likely token-limit
		// truncation. In IMPLEMENT phases retry once automatically; otherwise just
		// warn. Must run before handleWorkflowTransition so the transition sees the
		// retry response rather than the empty one.
		if err == nil {
			HandleEmptyResponse(ctx, app)
		}

		// Workflow phase detection: check for completion sentinels in the last
		// assistant message. All transitions run here in the goroutine so that
		// oracle calls can use the normal confirm gate before AgentDoneMsg fires.
		var wfNext *WFStartTurnMsg
		if app.Workflow != nil && err == nil {
			wfNext = HandleWorkflowTransition(ctx, app)
		}

		// Build end-of-turn nudge when: (a) the learn-candidate log fired this
		// turn, (b) at least one web or oracle grounding entry was added client-
		// side, and (c) this query hasn't been nudged already this session.
		nudge := ""
		pendingQuery := app.learnNudgePending
		app.learnNudgePending = "" // always clear
		if pendingQuery != "" && err == nil {
			for _, e := range app.Client.Grounding() {
				if e.Type == "web" || e.Type == "oracle" {
					if app.learnNudgedQueries == nil {
						app.learnNudgedQueries = make(map[string]bool)
					}
					if !app.learnNudgedQueries[pendingQuery] {
						app.learnNudgedQueries[pendingQuery] = true
						nudge = "· low grounding + external sources used — /learn to save this for next time"
					}
					break
				}
			}
		}
		// A surfaced stream error renders as the tidy warn line, not a raw err.
		doneErr := err
		if streamWarn != "" {
			doneErr = nil
		}
		app.sendEvent(AgentDoneMsg{Err: doneErr, LearnNudge: nudge, Warn: streamWarn, WorkflowWillContinue: wfNext != nil})
		if wfNext != nil {
			app.sendEvent(*wfNext)
		}
		return nil
	}
}

// runFinalReview is the Cmd that the TUI fires when WFFinalReviewMsg arrives.
// It sets up the oracle/confirm infrastructure, runs handleFinalReview, then
// signals completion via AgentDoneMsg.
func RunFinalReview(app *App, ctx context.Context) Cmd {
	return func() Msg {
		app.Out = NewProgWriter(func(m StreamChunkMsg) { app.sendEvent(m) })
		app.Confirm = tuiConfirmer(app)
		app.Client.ResetGrounding()
		HandleFinalReview(ctx, app)
		app.sendEvent(AgentDoneMsg{Err: nil})
		return nil
	}
}

// resolveBackendCtxCmd returns a Cmd that probes the new backend+model's
// context window in a goroutine and delivers the result as a BackendCtxLimitMsg.
// The TUI event loop applies the update to app.CtxLimit when the msg is handled,
// keeping it out of the goroutine to avoid races with concurrent agent turns.
func resolveBackendCtxCmd(app *App, backend, model string) Cmd {
	return func() Msg {
		var buf strings.Builder
		lim := ResolveContextLimitForBackendModel(context.Background(), app.Client.HTTP, app.Cfg, backend, model, &buf)
		return BackendCtxLimitMsg{Limit: lim, Note: strings.TrimSpace(buf.String())}
	}
}

// fetchModelListCmd returns a Cmd that fetches the model list for the
// current endpoint (after an endpoint switch) and delivers it as a
// ModelListUpdatedMsg. Like resolveBackendCtxCmd, the HTTP call runs in a
// goroutine and the result is applied to app.ModelList in the TUI event loop.
func fetchModelListCmd(app *App) Cmd {
	return func() Msg {
		models := FetchModelListForEndpoint(context.Background(), app.Client.HTTP, app.Cfg)
		return ModelListUpdatedMsg{Models: models}
	}
}

// ResumeSessionMsg applies a resumed session's state onto app (transcript,
// chat_id, workflow) and builds the NewConvMsg the TUI uses to rebuild its
// viewport. Shared by /resume's direct-id path and the resume picker's Enter
// action (internal/tui) so both apply the exact same mutation.
func ResumeSessionMsg(app *App, s *Session) Msg {
	app.Conv = s.Conv
	app.Client.ChatID = s.ChatID
	app.Session = s
	app.Workflow = s.SavedWorkflow
	msg := "resumed session " + ShortID(s.ChatID)
	if s.Label != "" {
		msg += " [" + s.Label + "]"
	}
	msg += fmt.Sprintf(" — %d messages", len(s.Conv))
	if s.SavedWorkflow != nil {
		msg += " · workflow restored: " + s.SavedWorkflow.PhaseName()
	}
	return NewConvMsg{Note: msg, RebuildConv: true}
}
