package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/trace"
)

// TestAutoCounselOnePerSymptom drives the real turn loop (streamTurn with
// the scripted fake model) through four identical writes at level high:
// the rewrites rule fires at the third write and would fire again at the
// fourth, but one automatic round per symptom per turn (rule 4) holds the
// second one back — the panel is hit once, and the withheld round is
// explainable through bare /counsel's diagnostics. A different symptom
// (a different target) in the same turn fires a second round.
func TestAutoCounselOnePerSymptom(t *testing.T) {
	t.Setenv("MASHURA_TEST_OR_KEY", "x")
	t.Setenv("MASHURA_TEST_KEY", "x")

	run := func(t *testing.T, writes []string, checkCmd string) (*App, *strings.Builder, *int) {
		t.Helper()
		_, count := counselServer(t)
		// counselServer counts per member HTTP call, not per round: two
		// members = 2 calls per round. Divide to get rounds.
		rounds := count
		_ = rounds
		frames := struggleFrames(writes, checkCmd)
		srv := sseServer(t, frames...)
		t.Cleanup(srv.Close)
		exe := newFakeExecutor()
		exe.shellErr = fmt.Errorf("check failed")
		app := newTestApp(srv.URL, exe, func(_, _, _ string, _ bool) bool { return true })
		app.Cfg.Mashura = &config.MashuraConfig{
			Level:   "high",
			Members: []string{"openrouter:test-model-a", "openrouter:test-model-b"},
		}
		app.Cfg.OpenRouterAPIKeyEnv = "MASHURA_TEST_OR_KEY"
		app.Cfg.OracleAPIKeyEnv = "MASHURA_TEST_KEY"
		app.Cfg.OracleModel = "test-model"
		app.IsSubagent = false
		app.SetAutoApprove(true)
		out := new(strings.Builder)
		app.Out = out
		// prepareTurn is what Send runs before the loop; the real loop
		// entry is Send, so run it here to reset the per-turn cap.
		app.prepareTurn()
		var traceToolCalls []trace.ToolTrace
		if _, _, err := app.streamTurn(context.Background(), "write answer", nil, &traceToolCalls); err != nil {
			t.Fatalf("streamTurn: %v", err)
		}
		return app, out, count
	}

	// Four identical writes: the rewrites rule fires after the third write
	// (one round) and must not fire again for the fourth (same symptom).
	// The second trace of the level's struggle sequence is the failing
	// check, so "repeated failures" also fires — one round per symptom,
	// counted against the two-member cap of 2, so the four writes produce
	// at most two rounds total. counselServer counts per-member HTTP
	// calls (2 per round), so divide by the member count.
	_, out, n := run(t, []string{"same", "same", "same", "same"}, "./check.sh")
	if *n/2 != 2 {
		t.Fatalf("four identical writes: panel fired %d rounds, want 2 (cap)", *n/2)
	}
	if strings.Count(out.String(), "starting mashura debug") != 2 {
		t.Fatalf("four identical writes: announce lines = %d, want 2; out=%q",
			strings.Count(out.String(), "starting mashura debug"), out.String())
	}
	// The header is present exactly once per round in what was injected:
	// count it in the conversation projection (two rounds at the cap).
	app4, _, _ := run(t, []string{"same", "same", "same", "same"}, "./check.sh")
	hdr := 0
	for _, m := range app4.contextProjection() {
		if m.Content != nil {
			hdr += strings.Count(*m.Content, autoCounselResultHeader)
		}
	}
	if hdr != 2 {
		t.Fatalf("four identical writes: header present %d times in the projection, want 2", hdr)
	}

	// Bare /counsel shows the withheld reason after the run: the last
	// recorded evaluation names the rule and the withheld round. After a
	// single-symptom run the last evaluation is the one-round-per-symptom
	// withholding itself.
	app4b, _, _ := run(t, []string{"same", "same", "same"}, "./check.sh")
	handled, _, cmd := HandleTUICommand("/counsel", app4b)
	if !handled {
		t.Fatal("/counsel not handled")
	}
	msg := ""
	if cmd != nil {
		msg = cmd().(SysNoteMsg).Text
	}
	if !strings.Contains(msg, "automatic round withheld") {
		t.Fatalf("bare /counsel must show the withheld round after a repeat symptom:\n%s", msg)
	}

	// A different symptom (different target) fires a second round: rule
	// "rewrites" on answer.txt, then rule "repeated failures" on a failing
	// shell command, both in one turn.
	frames := [][]string{
		toolCallFrames("w1", "write_file", `{"path":"answer.txt","content":"same"}`),
		toolCallFrames("w2", "write_file", `{"path":"answer.txt","content":"same"}`),
		toolCallFrames("w3", "write_file", `{"path":"answer.txt","content":"same"}`),
		toolCallFrames("s1", "run_shell", `{"command":"./check.sh"}`),
		toolCallFrames("s2", "run_shell", `{"command":"./check.sh"}`),
		[]string{contentChunk("done")},
	}
	_, count2 := counselServer(t)
	srv2 := sseServer(t, frames...)
	t.Cleanup(srv2.Close)
	exe2 := newFakeExecutor()
	exe2.shellErr = fmt.Errorf("check failed")
	appD := newTestApp(srv2.URL, exe2, func(_, _, _ string, _ bool) bool { return true })
	appD.Cfg.Mashura = &config.MashuraConfig{
		Level:   "high",
		Members: []string{"openrouter:test-model-a", "openrouter:test-model-b"},
	}
	appD.Cfg.OpenRouterAPIKeyEnv = "MASHURA_TEST_OR_KEY"
	appD.Cfg.OracleAPIKeyEnv = "MASHURA_TEST_KEY"
	appD.Cfg.OracleModel = "test-model"
	appD.IsSubagent = false
	appD.SetAutoApprove(true)
	outD := new(strings.Builder)
	appD.Out = outD
	appD.prepareTurn()
	var traceToolCalls []trace.ToolTrace
	if _, _, err := appD.streamTurn(context.Background(), "write answer", nil, &traceToolCalls); err != nil {
		t.Fatalf("streamTurn (two symptoms): %v", err)
	}
	if *count2/2 != 2 {
		t.Fatalf("two different symptoms: panel fired %d rounds, want 2", *count2/2)
	}
	if strings.Count(outD.String(), "starting mashura debug") != 2 {
		t.Fatalf("two different symptoms: announce lines = %d, want 2; out=%q",
			strings.Count(outD.String(), "starting mashura debug"), outD.String())
	}
}
