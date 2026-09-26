package wiring

// headless_continuous_test.go covers the experimental bounded continuous
// coordinator: completion ownership, verification gating, bounds, and the
// non-resumable terminal contract. The helpers (fakeApp, sseServer,
// contentChunk, toolCallFrames, outputEvents, findEvent) are shared with
// headless_host_test.go and hostturn_test.go in this package.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/proxy"
)

// finalizeFrame builds a finalize_goal tool-call frame. "continue" adds one
// remaining-work item (required by the protocol); "blocked" sets requires_user
// plus required_user_action. The two are mutually exclusive, so a plain
// "complete" proposal carries neither.
func finalizeFrame(id, status, summary string) []string {
	p := agent.GoalFinalization{
		Status:  agent.FinalizeGoalStatus(status),
		Summary: summary,
	}
	switch status {
	case "continue":
		p.RemainingWork = []string{"more work"}
	case "blocked":
		p.RequiresUser = true
		p.RequiredUserAction = "export API_KEY"
	}
	args, err := json.Marshal(p)
	if err != nil {
		panic(err)
	}
	return toolCallFrames(id, "finalize_goal", string(args))
}

func contOpts(maxTurns int, verifyCmds ...string) ContinuousOptions {
	return ContinuousOptions{MaxTurns: maxTurns, MaxTime: 2 * time.Minute, VerifyCommands: verifyCmds}
}

// TestContinuousCompleteWithoutVerifierIsNotSuccess: a completion proposal with
// no declared verifier contract must NOT exit 0 — "all declared checks passed"
// cannot be asserted when nothing was declared.
func TestContinuousCompleteWithoutVerifierIsNotSuccess(t *testing.T) {
	srv := sseServer(t, finalizeFrame("f1", "complete", "did the work"))
	defer srv.Close()

	app := fakeApp(srv.URL)
	var out strings.Builder
	code := runContinuousTask(context.Background(), app, "go", HeadlessOptions{Auto: true}, contOpts(3), &out)

	if code != ExitIncomplete {
		t.Fatalf("exit = %d, want %d (complete_unverified); out: %s", code, ExitIncomplete, out.String())
	}
	done := findEvent(outputEvents(t, out.String()), "done")
	if done == nil || done["outcome"] != "complete_unverified" {
		t.Fatalf("want done{complete_unverified}; got %v", done)
	}
	if done["resumable"] != false {
		t.Errorf("terminal record must be resumable:false; got %v", done["resumable"])
	}
}

// TestContinuousCompleteVerified: complete proposal + passing declared verifier
// is the only path to a zero exit and a coordinator-owned verification receipt.
func TestContinuousCompleteVerified(t *testing.T) {
	srv := sseServer(t, finalizeFrame("f1", "complete", "did the work"))
	defer srv.Close()

	app := fakeApp(srv.URL)
	var out strings.Builder
	// The fake executor returns "" with a nil error, so runOneVerifyCommand
	// records StatusPass.
	code := runContinuousTask(context.Background(), app, "go", HeadlessOptions{Auto: true},
		contOpts(3, "true"), &out)

	if code != ExitOK {
		t.Fatalf("exit = %d, want %d; out: %s", code, ExitOK, out.String())
	}
	evs := outputEvents(t, out.String())
	done := findEvent(evs, "done")
	if done == nil || done["outcome"] != "complete_verified" || done["reason"] != "goal_complete" {
		t.Fatalf("want done{complete_verified, goal_complete}; got %v", done)
	}
	ver, _ := done["verification"].(map[string]any)
	if ver == nil || ver["status"] != "passed" {
		t.Fatalf("want verification{passed}; got %v", done["verification"])
	}
	if findEvent(evs, "verify") == nil {
		t.Error("want a verify event")
	}
}

// TestContinuousExperimentalWarningFirst: consumers must see the experimental
// caveat before any result is claimed.
func TestContinuousExperimentalWarningFirst(t *testing.T) {
	srv := sseServer(t, finalizeFrame("f1", "complete", "w"))
	defer srv.Close()

	app := fakeApp(srv.URL)
	var out strings.Builder
	runContinuousTask(context.Background(), app, "go", HeadlessOptions{Auto: true}, contOpts(2, "true"), &out)

	evs := outputEvents(t, out.String())
	if len(evs) == 0 || evs[0]["type"] != "warning" {
		t.Fatalf("first event should be the experimental warning; got %v", evs)
	}
}

// TestContinuousBlockedRequiresUser: a blocked proposal terminates the run and
// surfaces the required user action.
func TestContinuousBlockedRequiresUser(t *testing.T) {
	srv := sseServer(t, []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"f1","function":{"name":"finalize_goal","arguments":"{\"status\":\"blocked\",\"summary\":\"need a key\",\"requires_user\":true,\"required_user_action\":\"export API_KEY\"}"}}]},"finish_reason":null}]}`,
	})
	defer srv.Close()

	app := fakeApp(srv.URL)
	var out strings.Builder
	code := runContinuousTask(context.Background(), app, "go", HeadlessOptions{Auto: true}, contOpts(2), &out)

	if code != ExitDeclined {
		t.Fatalf("exit = %d, want %d; out: %s", code, ExitDeclined, out.String())
	}
	done := findEvent(outputEvents(t, out.String()), "done")
	if done == nil || done["outcome"] != "blocked" {
		t.Fatalf("want done{blocked}; got %v", done)
	}
	if done["required_user_action"] != "export API_KEY" {
		t.Errorf("required_user_action = %v", done["required_user_action"])
	}
}

// TestContinuousMaxTurnsBoundary: reaching the turn bound prohibits the NEXT
// admission and never discards a completion from the last allowed turn.
func TestContinuousMaxTurnsBoundary(t *testing.T) {
	// Every turn proposes continue, so the loop runs out of turns.
	srv := sseServer(t, finalizeFrame("f1", "continue", "still working"))
	defer srv.Close()

	app := fakeApp(srv.URL)
	var out strings.Builder
	code := runContinuousTask(context.Background(), app, "go", HeadlessOptions{Auto: true}, contOpts(2), &out)

	if code != ExitIncomplete {
		t.Fatalf("exit = %d, want %d; out: %s", code, ExitIncomplete, out.String())
	}
	done := findEvent(outputEvents(t, out.String()), "done")
	if done == nil || done["reason"] != "max_turns_reached" {
		t.Fatalf("want done{reason:max_turns_reached}; got %v", done)
	}
	if turns, _ := done["turns"].(float64); int(turns) != 2 {
		t.Errorf("turns = %v, want 2", done["turns"])
	}
}

// TestContinuousProseWithoutProposalIsNotSuccess: a turn that never calls
// finalize_goal gets one corrective turn, then fails closed.
func TestContinuousProseWithoutProposalIsNotSuccess(t *testing.T) {
	srv := sseServer(t, []string{contentChunk("I think I am done!")})
	defer srv.Close()

	app := fakeApp(srv.URL)
	var out strings.Builder
	code := runContinuousTask(context.Background(), app, "go", HeadlessOptions{Auto: true}, contOpts(5), &out)

	if code == ExitOK {
		t.Fatalf("prose without finalize_goal must not exit 0; out: %s", out.String())
	}
	done := findEvent(outputEvents(t, out.String()), "done")
	if done == nil || done["reason"] != "finalization_invalid" {
		t.Fatalf("want done{reason:finalization_invalid}; got %v", done)
	}
	if done["resumable"] != false {
		t.Errorf("terminal record must be resumable:false")
	}
}

// TestContinuousSingleTokenRecordAndTerminalLast: the terminal record is
// emitted once and the tokens summary stays last, matching headless ordering.
func TestContinuousSingleTokenRecordAndTerminalLast(t *testing.T) {
	srv := sseServer(t, finalizeFrame("f1", "complete", "w"))
	defer srv.Close()

	app := fakeApp(srv.URL)
	app.Costs = proxy.NewCostTracker()
	var out strings.Builder
	runContinuousTask(context.Background(), app, "go", HeadlessOptions{Auto: true}, contOpts(2, "true"), &out)

	evs := outputEvents(t, out.String())
	var doneCount int
	for _, ev := range evs {
		if ev["type"] == "done" {
			doneCount++
		}
	}
	if doneCount != 1 {
		t.Errorf("want exactly one terminal record, got %d", doneCount)
	}
	if evs[len(evs)-1]["type"] != "tokens" {
		t.Errorf("last event should be tokens; got %v", evs[len(evs)-1])
	}
}
