package agent

import (
	"context"
	"fmt"
	"strings"
)

// TakeLearnNudge computes and clears the end-of-turn learn nudge (7b3 m4).
//
// It replicates exactly the nudge block the old TUI's RunTurn Cmd ran after
// each turn (tui_cmds.go): a nudge fires when (a) the learn-candidate log
// fired this turn (learnNudgePending is set), (b) at least one web or oracle
// grounding entry was added client-side during the turn, and (c) this query
// hasn't been nudged already this session. Calling it clears the pending
// query (always — same as RunTurn) and records a shown nudge (when fired).
//
// The wiring adapter calls it after DriveTurnWithResilience so the wiring path
// produces the same nudge the TUI path did, delivered as an ephemeral
// learn_nudge event instead of an AgentDoneMsg field.
func (a *App) TakeLearnNudge() string {
	pendingQuery := a.learnNudgePending
	a.learnNudgePending = "" // always clear
	if pendingQuery == "" {
		return ""
	}
	for _, e := range a.Client.Grounding() {
		if e.Type == "web" || e.Type == "oracle" {
			if a.learnNudgedQueries == nil {
				a.learnNudgedQueries = make(map[string]bool)
			}
			if !a.learnNudgedQueries[pendingQuery] {
				a.learnNudgedQueries[pendingQuery] = true
				return "· low grounding + external sources used — /learn to save this for next time"
			}
			break
		}
	}
	return ""
}

// runTurnToFinal drives one user turn to its FINAL outcome, transparently
// handling Phase 2 suspension: when the turn suspends on pending
// async work, it awaits a completion (WaitForAsyncCompletion) and resumes until
// the turn reaches a Final outcome. Used by both the TUI (RunTurn) and headless
// (cmd/wakil/run.go, which has its own equivalent in package main).
func runTurnToFinal(ctx context.Context, app *App, userText string) (TurnOutcome, error) {
	out, err := app.SendOutcome(ctx, userText)
	if err != nil {
		return out, err
	}
	if out.Kind != TurnFinal && out.Kind != TurnSuspended {
		return out, fmt.Errorf("turn aborted: %s", out.Cause)
	}
	for out.Kind == TurnSuspended {
		app.sendEvent(TurnSuspendedSignal{})
		hbCtx, hbCancel := context.WithCancel(ctx)
		app.startAsyncHeartbeat(hbCtx)
		ok, werr := app.WaitForAsyncCompletion(ctx)
		hbCancel()
		if werr != nil {
			return out, werr
		}
		if !ok {
			if app.asyncIsStopping() {
				return TurnOutcome{Kind: TurnAborted, Text: out.Text, Cause: StopCauseCancelled}, fmt.Errorf("async operations stopped while turn was suspended")
			}
			return TurnOutcome{Kind: TurnAborted, Text: out.Text, Cause: StopCauseAsyncLost}, fmt.Errorf("async completion was lost before the suspended turn could resume")
		}
		app.sendEvent(TurnResumedSignal{})
		out, err = app.Resume(ctx)
		if err != nil {
			return out, err
		}
		if out.Kind != TurnFinal && out.Kind != TurnSuspended {
			return out, fmt.Errorf("turn aborted: %s", out.Cause)
		}
	}
	return out, nil
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
	app.convMu.Lock()
	app.Conv = s.Conv
	app.convMu.Unlock()
	app.SetChatID(s.ChatID)
	app.Session = s
	app.SetWorkflow(s.SavedWorkflow)
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
