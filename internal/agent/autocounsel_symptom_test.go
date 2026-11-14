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
// the rewrites rule fires at the third write and must not fire again at
// the fourth (one automatic round per symptom per turn, rule 4). The
// cap test below proves the cap: four writes produce at most two rounds
// total, and — discriminatingly — the survivor set pins the rewrites
// symptom fired exactly once. A different symptom (a different target)
// in the same turn may still fire a second round.
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
		// High cap so the per-turn cap cannot mask rule 4: one round
		// per symptom must hold even with room to fire.
		app.CounselMaxSet = true
		app.MaxCounsel = 5
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

	// Pure rewrites (no failing check): a third write fires the rewrites
	// rule once; a fourth write of the same file is the same symptom and
	// must not re-fire, even with the cap at 5.
	_, out, n := run(t, []string{"same", "same", "same", "same"}, "true")
	if *n/2 != 1 {
		t.Fatalf("pure rewrites: panel fired %d rounds, want exactly 1", *n/2)
	}
	if strings.Count(out.String(), "starting mashura debug") != 1 {
		t.Fatalf("pure rewrites: announce lines = %d, want 1; out=%q",
			strings.Count(out.String(), "starting mashura debug"), out.String())
	}
	// The header is present exactly once per round in what was injected:
	// count it in the conversation projection.
	app4, _, _ := run(t, []string{"same", "same", "same", "same"}, "true")
	hdr := 0
	for _, m := range app4.contextProjection() {
		if m.Content != nil {
			hdr += strings.Count(*m.Content, autoCounselResultHeader)
		}
	}
	if hdr != 1 {
		t.Fatalf("pure rewrites: header present %d times in the projection, want 1", hdr)
	}

	// Bare /counsel shows the withheld reason after the run: the last
	// recorded evaluation is the one-round-per-symptom withholding itself.
	app4b, _, _ := run(t, []string{"same", "same", "same", "same"}, "true")
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

	// A different symptom (different target) fires a second round: rewrites
	// on answer.txt, then repeated failures on a failing shell command,
	// both in one turn. The "repeat" rule also fires on the second
	// identical ./check.sh — a third, distinct symptom, which is allowed
	// (rule 4 only withholds the same symptom). All three are within the
	// cap of 5.
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
	appD.CounselMaxSet = true
	appD.MaxCounsel = 5
	outD := new(strings.Builder)
	appD.Out = outD
	appD.prepareTurn()
	var traceToolCalls []trace.ToolTrace
	if _, _, err := appD.streamTurn(context.Background(), "write answer", nil, &traceToolCalls); err != nil {
		t.Fatalf("streamTurn (two symptoms): %v", err)
	}
	if *count2/2 != 3 {
		t.Fatalf("distinct symptoms: panel fired %d rounds, want 3", *count2/2)
	}
	if strings.Count(outD.String(), "starting mashura debug") != 3 {
		t.Fatalf("distinct symptoms: announce lines = %d, want 3; out=%q",
			strings.Count(outD.String(), "starting mashura debug"), outD.String())
	}
}
