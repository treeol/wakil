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

	// A different symptom fires a second round: rewrites on answer.txt,
	// then a failing shell command, both in one turn. The "repeated
	// failures" and "repeat" rules now share one command identity, so a
	// failing command counts once however many of its rules fire: the
	// first round fires for the rewrite identity, the third write is the
	// same identity and stays silent, and the command fires once (at its
	// first failing run via "repeated failures", with its later match
	// withheld). Both are within the cap of 5.
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
	if *count2/2 != 2 {
		t.Fatalf("distinct symptoms: panel fired %d rounds, want 2", *count2/2)
	}
	if strings.Count(outD.String(), "starting mashura debug") != 2 {
		t.Fatalf("distinct symptoms: announce lines = %d, want 2; out=%q",
			strings.Count(outD.String(), "starting mashura debug"), outD.String())
	}
}

// TestStruggleKeyIdentity pins the struggle identity keys: writes to one
// file share one rewrite identity at any count, while a failing command
// shares one command identity across its rules.
func TestStruggleKeyIdentity(t *testing.T) {
	w := ToolTraceEntry{Abbrev: "write", Command: "answer.txt"}
	for _, n := range []int{2, 3, 4} {
		traces := make([]ToolTraceEntry, n)
		for i := range traces {
			traces[i] = w
		}
		_, d, _, k := detectStruggleScan(traces)
		if !d || k != "rewrite:answer.txt" {
			t.Fatalf("%d writes: got detected=%v key=%q, want rewrite:answer.txt", n, d, k)
		}
	}
	other := ToolTraceEntry{Abbrev: "write", Command: "other.txt"}
	if _, d, _, k := detectStruggleScan([]ToolTraceEntry{other, other}); !d || k != "rewrite:other.txt" {
		t.Fatalf("other.txt: got detected=%v key=%q, want rewrite:other.txt", d, k)
	}
	f := ToolTraceEntry{Abbrev: "shell", Command: "./check.sh", ExitErr: true, FirstLine: "FAIL"}
	if _, d, _, k := detectStruggleScan([]ToolTraceEntry{f, f}); !d || k != "command:shell:./check.sh" {
		t.Fatalf("failing check: got detected=%v key=%q, want command:shell:./check.sh", d, k)
	}
	if _, d, _, _ := detectStruggleScan([]ToolTraceEntry{f, f}, func(_, k string) bool { return k == "command:shell:./check.sh" }); d {
		t.Fatal("skipping the command identity must withhold both of its rules")
	}
	// A withheld identity must not starve a different one.
	traces := []ToolTraceEntry{w, w, w, f, f}
	if _, d, _, k := detectStruggleScan(traces, func(_, k string) bool { return k == "rewrite:answer.txt" }); !d || k != "command:shell:./check.sh" {
		t.Fatalf("withheld rewrite: got detected=%v key=%q, want command:shell:./check.sh", d, k)
	}
}

// TestAutoCounsel_ReadOnlyExplorationNeverFires reproduces the release-session
// regression where plain file reading fired two automatic Mashūra rounds at
// level high: paginated read_file calls on two files, identical re-reads,
// repeated list_dir/search_files, and two missing-file read_file probes must
// all complete with zero automatic Mashūra rounds.
func TestAutoCounsel_ReadOnlyExplorationNeverFires(t *testing.T) {
	t.Setenv("MASHURA_TEST_OR_KEY", "x")
	t.Setenv("MASHURA_TEST_KEY", "x")

	_, count := counselServer(t)
	frames := [][]string{
		// Paginated reads of file a.go (previously collapsed to identical Command "a.go" → round 1).
		toolCallFrames("r1", "read_file", `{"path":"a.go","offset":1,"limit":2}`),
		toolCallFrames("r2", "read_file", `{"path":"a.go","offset":3,"limit":2}`),
		// Paginated reads of file b.go (previously collapsed to identical Command "b.go" → round 2).
		toolCallFrames("r3", "read_file", `{"path":"b.go","offset":1,"limit":2}`),
		toolCallFrames("r4", "read_file", `{"path":"b.go","offset":3,"limit":2}`),
		// Plain identical re-read of a.go on the parent agent (no ToolCache).
		toolCallFrames("r5", "read_file", `{"path":"a.go"}`),
		toolCallFrames("r6", "read_file", `{"path":"a.go"}`),
		// Repeated list_dir and search_files calls.
		toolCallFrames("l1", "list_dir", `{"path":"."}`),
		toolCallFrames("l2", "list_dir", `{"path":"."}`),
		toolCallFrames("s1", "search_files", `{"pattern":"package","path":"."}`),
		toolCallFrames("s2", "search_files", `{"pattern":"package","path":"."}`),
		// Two missing-file probes during exploration (ExitErr on read_file).
		toolCallFrames("m1", "read_file", `{"path":"does-not-exist-1.go"}`),
		toolCallFrames("m2", "read_file", `{"path":"does-not-exist-2.go"}`),
		[]string{contentChunk("done reading")},
	}
	srv := sseServer(t, frames...)
	t.Cleanup(srv.Close)

	exe := newFakeExecutor()
	exe.files["a.go"] = "package a\n// line 2\n// line 3\n// line 4\n"
	exe.files["b.go"] = "package b\n// line 2\n// line 3\n// line 4\n"

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
	app.CounselMaxSet = true
	app.MaxCounsel = 5
	out := new(strings.Builder)
	app.Out = out
	app.prepareTurn()

	var traceToolCalls []trace.ToolTrace
	if _, _, err := app.streamTurn(context.Background(), "read a.go and b.go", nil, &traceToolCalls); err != nil {
		t.Fatalf("streamTurn: %v", err)
	}
	if *count != 0 {
		t.Fatalf("read-only exploration fired %d Mashūra HTTP calls, want 0; out=%q", *count, out.String())
	}
	if strings.Contains(out.String(), "starting mashura debug") || strings.Contains(out.String(), "struggle detected") {
		t.Errorf("read-only exploration should not emit struggle/auto-counsel lines, got:\n%s", out.String())
	}
}

// TestDetectStruggle_ReadOnlyProbeAfterFailureDoesNotFire verifies that a
// failed read-only probe immediately following a failed write/shell does not
// fire the "repeated failures" rule against the read-only probe.
func TestDetectStruggle_ReadOnlyProbeAfterFailureDoesNotFire(t *testing.T) {
	traces := []ToolTraceEntry{
		{Abbrev: "edit", Command: "a.go", ExitErr: true, FirstLine: "old_string not found"},
		{Abbrev: "read", Command: "missing.go", ExitErr: true, ReadOnly: true, FirstLine: "open missing.go: no such file"},
	}
	if s, d, r, _ := detectStruggleScan(traces); d {
		t.Errorf("read-only probe after failed edit must not fire struggle detector; got rule=%q symptom=%q", r, s)
	}
}
