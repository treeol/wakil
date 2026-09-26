package agent

import (
	"errors"
	"testing"

	"github.com/treeol/wakil/internal/proxy"
)

func finalizeCall(args string) proxy.ToolCall {
	return proxy.ToolCall{ID: "f1", Function: proxy.FunctionCall{Name: "finalize_goal", Arguments: args}}
}

func TestHandleFinalizeGoalValidComplete(t *testing.T) {
	var got GoalFinalization
	app := &App{}
	app.SetFinalizeGoal(func(p GoalFinalization) error { got = p; return nil })
	res := app.handleFinalizeGoal(finalizeCall(`{"status":"complete","summary":"done","requires_user":false}`))
	if !res.ok || !res.endTurn {
		t.Fatalf("result = %+v, want successful end-turn", res)
	}
	if got.Status != FinalizeGoalComplete || got.Summary != "done" {
		t.Fatalf("proposal = %+v", got)
	}
}

func TestHandleFinalizeGoalRejectsContradictions(t *testing.T) {
	app := &App{}
	app.SetFinalizeGoal(func(GoalFinalization) error { return nil })
	cases := []string{
		`{"status":"complete","summary":"done","remaining_work":["x"],"requires_user":false}`,
		`{"status":"blocked","summary":"need key","requires_user":false}`,
		`{"status":"continue","summary":"ask","requires_user":true,"required_user_action":"key"}`,
		`{"status":"complete","summary":"","requires_user":false}`,
		`{"status":"complete","summary":"done","requires_user":true}`,
		`{"status":"complete","summary":"done","requires_user":false,"unknown":1}`,
		`{"status":"continue","summary":"more","requires_user":false}`,
		`{"status":"complete","summary":"done"}`,
		`{"summary":"done","requires_user":false}`,
		`{"status":"complete","requires_user":false}`,
		`{"status":"complete","summary":"done","requires_user":false} {"status":"blocked"}`,
	}
	for _, args := range cases {
		if res := app.handleFinalizeGoal(finalizeCall(args)); res.ok || res.endTurn {
			t.Errorf("args %s: result = %+v, want rejection", args, res)
		}
	}
}

func TestHandleFinalizeGoalPropagatesCallbackError(t *testing.T) {
	app := &App{}
	app.SetFinalizeGoal(func(GoalFinalization) error { return errors.New("duplicate") })
	res := app.handleFinalizeGoal(finalizeCall(`{"status":"continue","summary":"more","remaining_work":["test"],"requires_user":false}`))
	if res.ok || res.endTurn || res.text != "finalize_goal: duplicate" {
		t.Fatalf("result = %+v", res)
	}
}

func TestHandleFinalizeGoalRequiresContinuousMode(t *testing.T) {
	app := &App{}
	res := app.handleFinalizeGoal(finalizeCall(`{"status":"complete","summary":"done","requires_user":false}`))
	if res.ok || res.endTurn {
		t.Fatalf("result = %+v, want unavailable", res)
	}
}
