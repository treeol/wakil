package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/treeol/wakil/internal/proxy"
)

// FinalizeGoalStatus is a coordinator proposal. The model can propose an
// outcome, but the continuous coordinator remains the authority that decides
// whether it is accepted and whether verification passed.
type FinalizeGoalStatus string

const (
	FinalizeGoalComplete FinalizeGoalStatus = "complete"
	FinalizeGoalContinue FinalizeGoalStatus = "continue"
	FinalizeGoalBlocked  FinalizeGoalStatus = "blocked"
)

// GoalFinalization is the validated payload of a continuous-mode finalize_goal
// tool call. String fields and list sizes are bounded at dispatch.
type GoalFinalization struct {
	Status             FinalizeGoalStatus `json:"status"`
	Summary            string             `json:"summary"`
	RemainingWork      []string           `json:"remaining_work,omitempty"`
	RequiresUser       bool               `json:"requires_user"`
	RequiredUserAction string             `json:"required_user_action,omitempty"`
}

// FinalizeGoalFunc captures one structured finalization proposal. It is
// installed only by the experimental continuous headless coordinator.
type FinalizeGoalFunc func(GoalFinalization) error

// FinalizeGoalCallback records the coordinator's validated proposal callback.
// nil in all ordinary modes. Wiring installs it only for --continue;
// subagents and TUI one-shot behavior never see the finalize_goal tool.
type FinalizeGoalCallback FinalizeGoalFunc

// SetFinalizeGoal installs the continuous-mode finalization callback.
func (a *App) SetFinalizeGoal(fn FinalizeGoalFunc) { a.finalizeGoal = FinalizeGoalCallback(fn) }

const (
	maxGoalSummaryChars        = 4000
	maxGoalRemainingWorkItems  = 20
	maxGoalItemChars           = 1000
	maxGoalRequiredActionChars = 2000
)

func (a *App) handleFinalizeGoal(tc proxy.ToolCall) toolResult {
	if a.finalizeGoal == nil {
		return errResult("finalize_goal is only available in continuous mode")
	}
	var p GoalFinalization
	dec := json.NewDecoder(strings.NewReader(tc.Function.Arguments))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return errResult("finalize_goal: " + err.Error())
	}
	switch p.Status {
	case FinalizeGoalComplete, FinalizeGoalContinue, FinalizeGoalBlocked:
	default:
		return errResult(`finalize_goal: status must be "complete", "continue", or "blocked"`)
	}
	if strings.TrimSpace(p.Summary) == "" || len(p.Summary) > maxGoalSummaryChars {
		return errResult(fmt.Sprintf("finalize_goal: summary must be 1-%d characters", maxGoalSummaryChars))
	}
	if len(p.RemainingWork) > maxGoalRemainingWorkItems {
		return errResult(fmt.Sprintf("finalize_goal: remaining_work exceeds %d items", maxGoalRemainingWorkItems))
	}
	for i, item := range p.RemainingWork {
		if strings.TrimSpace(item) == "" || len(item) > maxGoalItemChars {
			return errResult(fmt.Sprintf("finalize_goal: remaining_work[%d] must be 1-%d characters", i, maxGoalItemChars))
		}
	}
	if p.RequiresUser && strings.TrimSpace(p.RequiredUserAction) == "" {
		return errResult("finalize_goal: required_user_action is required when requires_user=true")
	}
	if !p.RequiresUser && strings.TrimSpace(p.RequiredUserAction) != "" {
		return errResult("finalize_goal: required_user_action requires requires_user=true")
	}
	if len(p.RequiredUserAction) > maxGoalRequiredActionChars {
		return errResult(fmt.Sprintf("finalize_goal: required_user_action exceeds %d characters", maxGoalRequiredActionChars))
	}
	if p.Status == FinalizeGoalComplete && (len(p.RemainingWork) > 0 || p.RequiresUser) {
		return errResult("finalize_goal: complete cannot have remaining_work or require user input")
	}
	if p.Status == FinalizeGoalBlocked && !p.RequiresUser {
		return errResult("finalize_goal: blocked must set requires_user=true and required_user_action")
	}
	if p.Status == FinalizeGoalContinue && p.RequiresUser {
		return errResult("finalize_goal: use blocked, not continue, when user input is required")
	}
	if err := a.finalizeGoal(p); err != nil {
		return errResult("finalize_goal: " + err.Error())
	}
	return toolResult{ok: true, text: "goal finalization proposal recorded; coordinator will evaluate it", endTurn: true}
}
