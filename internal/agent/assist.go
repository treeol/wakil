package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/treeol/wakil/internal/ilm"
	"github.com/treeol/wakil/internal/proxy"
)

// assistCanAct returns true if assist mode is enabled, the assist client
// is available, and tools are not disabled (forceFinish). Called before
// each model call in streamTurn to decide whether to query /v1/assist first.
func (a *App) assistCanAct(forceFinish bool) bool {
	if forceFinish {
		return false
	}
	return a.AssistEnabled && a.Assist != nil
}

// tryAssist queries /v1/assist and, if the server returns an action that
// passes Wakil's allowlist, returns a synthetic assistant message with the
// proposed tool call. streamTurn's normal dispatch path then executes the
// tool call exactly once — tryAssist does NOT execute the tool, append to
// Conv, or emit tool_call/tool_result events (those happen through the
// normal path).
//
// Per C1: any non-200, timeout, or abstain → proceed as without assist.
// Per C2: the server has already applied its gate; Wakil does not re-evaluate
// probabilities. Wakil checks the action against its OWN read-only allowlist.
func (a *App) tryAssist(ctx context.Context) (proxy.Message, bool) {
	if a.ILM == nil || a.Assist == nil {
		return proxy.Message{}, false
	}

	sessionID := a.ILM.SessionID()
	seq := a.ILM.Seq()

	resp, err := a.Assist.Query(ctx, sessionID, seq)
	if err != nil {
		// A 400 (AssistRejectedError) means the server rejected the seq —
		// it was not a valid decision point (e.g. an assistant_turn seq).
		// The call-count contract still requires an assist_event, but with
		// decision "rejected" instead of "error".
		var rejErr *ilm.AssistRejectedError
		if errors.As(err, &rejErr) {
			a.emitAssistEvent("rejected", nil, resp, "", rejErr.Error())
			return proxy.Message{}, false
		}
		// Transport error / non-200 / timeout → emit event and fall through.
		a.emitAssistEvent("error", nil, resp, "", err.Error())
		return proxy.Message{}, false
	}

	// Abstain → emit event and fall through to the model.
	if resp.Abstain {
		a.emitAssistEvent("abstain", nil, resp, "", "")
		return proxy.Message{}, false
	}

	// Parse the proposed action.
	action, err := resp.ParseAction()
	if err != nil || action == nil {
		a.emitAssistEvent("error", nil, resp, "", "malformed action")
		return proxy.Message{}, false
	}

	// Check the action against Wakil's OWN read-only allowlist (C2).
	if !ilm.IsAssistAllowed(action.Name, action.Args) {
		a.emitAssistEvent("not_applicable", action, resp, "", "")
		return proxy.Message{}, false
	}

	// The proposal passes the allowlist. In assist_auto=false mode, show a
	// one-line prompt and wait for y/n. This uses a DEDICATED gate that
	// always asks — it is NOT bypassed by /auto or session grants (C2:
	// "show the proposal in the TUI as a one-line prompt and wait for y/n").
	if !a.AssistAuto {
		approved := a.assistConfirm(action.Name, action.Args, resp.GateProbability, resp.CandidateProvenance)
		if !approved {
			// User declined — emit event and call the main model.
			a.emitAssistEvent("ignored", action, resp, "", "")
			return proxy.Message{}, false
		}
	}

	// Build a synthetic assistant message with the proposed tool call.
	// streamTurn's normal dispatch path will execute it exactly once:
	// append to Conv, emit tool_call, handleToolCall, finalizeToolResult,
	// emit tool_result — the same path as a model-initiated tool call.
	tc := proxy.ToolCall{
		ID:   fmt.Sprintf("assist-%d-%d", time.Now().UnixNano(), seq),
		Type: "function",
		Function: proxy.FunctionCall{
			Name:      action.Name,
			Arguments: string(action.Args),
		},
	}

	assistantMsg := proxy.Message{
		Role:      "assistant",
		Content:   StrPtr(""),
		ToolCalls: []proxy.ToolCall{tc},
	}

	// Emit the assist_event with the decision. The tool_call and tool_result
	// events are emitted by the normal dispatch path in streamTurn.
	decision := "took"
	if a.AssistAuto {
		decision = "auto"
	}
	a.emitAssistEvent(decision, action, resp, "", "")

	return assistantMsg, true
}

// assistConfirm shows a one-line prompt in the TUI and waits for y/n.
// This is a DEDICATED gate that always asks — it is NOT the same as the
// Confirmer used for tool-call confirmation, which can be bypassed by
// /auto or session grants. The assist proposal must always require
// explicit y/n per C2.
//
// When no confirmer is installed (headless, tests), it returns false —
// the safe default is to decline.
func (a *App) assistConfirm(toolName string, args json.RawMessage, gateProb float64, provenance json.RawMessage) bool {
	if a.Confirm == nil {
		return false
	}
	headline := fmt.Sprintf("assist: %s", toolName)
	detail := fmt.Sprintf("gate=%.2f", gateProb)
	if len(provenance) > 0 && string(provenance) != "null" {
		var cp struct {
			Selected struct {
				SessionID string `json:"session_id"`
				Seq       int    `json:"seq"`
			} `json:"selected"`
		}
		if json.Unmarshal(provenance, &cp) == nil && cp.Selected.SessionID != "" {
			detail += fmt.Sprintf(", provenance: session=%s seq=%d", cp.Selected.SessionID, cp.Selected.Seq)
		}
	}
	if len(args) > 0 && string(args) != "null" {
		detail += ", args: " + string(args)
	}
	// Use readAction=true since the allowlist only permits read-only tools.
	return a.Confirm(toolName, headline, detail, true)
}

// emitAssistEvent emits an assist_event (C3) recording the assist response
// and Wakil's decision.
func (a *App) emitAssistEvent(decision string, action *ilm.AssistAction, resp *ilm.AssistResponse, mainModelAction string, reason string) {
	if a.ILM == nil {
		return
	}
	payload := ilm.AssistEventPayload{
		Decision:        decision,
		MainModelAction: mainModelAction,
		GateProbability: 0,
		Reason:          reason,
	}
	if resp != nil {
		payload.GateProbability = resp.GateProbability
		payload.Abstain = resp.Abstain
		if resp.Reason != "" && reason == "" {
			payload.Reason = resp.Reason
		}
		payload.CandidateProvenance = resp.CandidateProvenance
		payload.LatencyMS = resp.LatencyMS
	}
	if action != nil {
		payload.Tool = action.Name
		payload.Args = action.Args
	}
	a.ILM.Emit(ilm.EventAssist, payload)
}
